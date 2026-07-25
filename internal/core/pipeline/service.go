// Package pipeline implements the core services around AFT per-account
// customizations pipelines: inventory, latest-status fan-out, and release
// (StartPipelineExecution). Both the CLI and the TUI call this package —
// guards and instrumentation live here, not in the UIs.
package pipeline

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	cptypes "github.com/aws/aws-sdk-go-v2/service/codepipeline/types"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

// The status cache envelope is read with cache.Forever: its own per-entry
// FetchedAt is authoritative, so the envelope must never self-expire.
const (
	inventoryCacheKey = "pipelines"
	statusCacheKey    = "statuses"
)

// API is the subset of the CodePipeline client used by the read side.
type API interface {
	codepipeline.ListPipelinesAPIClient
	ListPipelineExecutions(ctx context.Context, in *codepipeline.ListPipelineExecutionsInput,
		opts ...func(*codepipeline.Options)) (*codepipeline.ListPipelineExecutionsOutput, error)
	GetPipelineState(ctx context.Context, in *codepipeline.GetPipelineStateInput,
		opts ...func(*codepipeline.Options)) (*codepipeline.GetPipelineStateOutput, error)
	ListActionExecutions(ctx context.Context, in *codepipeline.ListActionExecutionsInput,
		opts ...func(*codepipeline.Options)) (*codepipeline.ListActionExecutionsOutput, error)
}

// StartAPI is the subset used by the write side (may be a client built on
// a different, admin-capable profile).
type StartAPI interface {
	StartPipelineExecution(ctx context.Context, in *codepipeline.StartPipelineExecutionInput,
		opts ...func(*codepipeline.Options)) (*codepipeline.StartPipelineExecutionOutput, error)
}

// Service bundles the dependencies of the pipeline operations.
//
// The execution-history and action memos below are session-scoped (memory
// only, never disk): terminal executions' actions are immutable, and the
// executions list of a mostly-idle AFT pipeline changes rarely, so both are
// served from memory within their policy (docs/design.md §7).
type Service struct {
	Read        API
	Batch       batch.Config
	Cache       cache.Store
	PipelineTTL time.Duration
	// ExecutionsTTL bounds the executions-history memo; 0 disables it.
	ExecutionsTTL time.Duration

	mu          sync.Mutex
	execsMemo   map[string]execsEntry              // by pipeline name
	actionsMemo map[string][]model.ActionExecution // by name+"\x00"+execID; terminal executions only
}

// execsEntry is one pipeline's memoized execution history.
type execsEntry struct {
	execs     []model.Execution
	fetchedAt time.Time
}

// Inventory returns the names of all AFT account pipelines, served from
// cache when fresh — which pipelines exist changes far more rarely than
// what they are doing (statuses have their own, much shorter TTL).
func (s *Service) Inventory(ctx context.Context, refresh bool) (names []string, cachedAt time.Time, err error) {
	if !refresh {
		if names, at, ok := cache.Get[[]string](s.Cache, inventoryCacheKey, s.PipelineTTL); ok {
			return names, at, nil
		}
	}
	p := codepipeline.NewListPipelinesPaginator(s.Read, &codepipeline.ListPipelinesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("ListPipelines: %w", err)
		}
		for _, pl := range page.Pipelines {
			name := aws.ToString(pl.Name)
			if model.AccountIDFromPipeline(name) != "" {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	if err := cache.Put(s.Cache, inventoryCacheKey, names); err != nil {
		fmt.Fprintln(os.Stderr, "warning: failed to write pipeline cache:", err)
	}
	return names, time.Time{}, nil
}

// StatusEntry is one pipeline's cached latest execution with the time it was
// fetched. Statuses are cached per-entry so that a short TTL and targeted
// refresh both work at pipeline granularity (docs/design.md §4.1).
type StatusEntry struct {
	Execution *model.Execution `json:"execution"`
	FetchedAt time.Time        `json:"fetched_at"`
}

// StatusOptions controls how Statuses uses the status cache.
type StatusOptions struct {
	TTL             time.Duration   // serve a cached entry younger than this
	RefreshAll      bool            // ignore the cache; refetch every name
	RefreshOnly     map[string]bool // force-refetch these names
	RefreshInFlight bool            // always refetch cached in-flight entries
	// Prune drops cached entries for pipelines outside names. Set it only
	// when names is the full inventory (a `pl list` pass), never for a
	// targeted subset refresh — otherwise the rest of the cache is lost.
	Prune bool
}

// Statuses returns the latest execution of each pipeline, served from the
// per-entry status cache when fresh and refetched otherwise. Only the subset
// that is stale/forced/in-flight hits the API (through the batch engine), so
// a repeated call within the TTL performs no requests. Per-item fetch
// failures are reported in PipelineSummary.FetchError unless a cached value
// can stand in — the accompanying StatusStats still counts them, so a failed
// refresh behind a stale value is never silent. The merged cache is written
// back when anything was fetched or pruned.
func (s *Service) Statuses(
	ctx context.Context,
	names []string,
	resolver *account.Resolver,
	opts StatusOptions,
	onProgress func(batch.Progress),
) ([]model.PipelineSummary, model.StatusStats) {
	cached := map[string]StatusEntry{}
	if !opts.RefreshAll {
		if m, _, ok := cache.Get[map[string]StatusEntry](s.Cache, statusCacheKey, cache.Forever); ok {
			cached = m
		}
	}

	pruned := 0
	if opts.Prune {
		keep := make(map[string]bool, len(names))
		for _, n := range names {
			keep[n] = true
		}
		for n := range cached {
			if !keep[n] {
				delete(cached, n)
				pruned++
			}
		}
	}

	now := time.Now()
	var toFetch []string
	for _, name := range names {
		e, ok := cached[name]
		forced := opts.RefreshAll || opts.RefreshOnly[name]
		stale := !ok || now.Sub(e.FetchedAt) > opts.TTL
		inflight := opts.RefreshInFlight && ok && e.Execution != nil && e.Execution.Status.InFlight()
		if forced || stale || inflight {
			toFetch = append(toFetch, name)
		}
	}

	stats := model.StatusStats{TTL: opts.TTL}
	fetched := map[string]bool{}
	fetchErrs := map[string]string{}
	if len(toFetch) > 0 {
		results := batch.Run(ctx, s.Batch, toFetch,
			func(ctx context.Context, name string) (*model.Execution, error) {
				return s.latestExecution(ctx, name)
			}, onProgress)
		fetchedAt := time.Now()
		for i, res := range results {
			name := toFetch[i]
			if res.Err != nil {
				stats.Failed++
				// Retain any prior cached value; only surface an error when
				// there is nothing to fall back on.
				if _, ok := cached[name]; !ok {
					fetchErrs[name] = res.Err.Error()
				}
				continue
			}
			cached[name] = StatusEntry{Execution: res.Value, FetchedAt: fetchedAt}
			fetched[name] = true
		}
	}
	if len(fetched) > 0 || pruned > 0 {
		if err := cache.Put(s.Cache, statusCacheKey, cached); err != nil {
			fmt.Fprintln(os.Stderr, "warning: failed to write status cache:", err)
		}
	}

	summaries := make([]model.PipelineSummary, len(names))
	for i, name := range names {
		sum := model.PipelineSummary{
			PipelineName: name,
			AccountID:    model.AccountIDFromPipeline(name),
		}
		if resolver != nil {
			if a := resolver.ByID(sum.AccountID); a != nil {
				sum.AccountName = a.Name
			}
		}
		if e, ok := cached[name]; ok {
			sum.Latest = e.Execution
			at := e.FetchedAt
			sum.StatusFetchedAt = &at
			if fetched[name] {
				stats.Fetched++
			} else {
				stats.FromCache++
				if stats.Oldest.IsZero() || at.Before(stats.Oldest) {
					stats.Oldest = at
				}
			}
		}
		if msg, ok := fetchErrs[name]; ok {
			sum.FetchError = msg
		}
		summaries[i] = sum
	}
	return summaries, stats
}

// InvalidateStatuses drops the given pipelines from the status cache so the
// next Statuses call refetches them. Used after a release, whose targets
// have just transitioned and whose cached terminal status is now wrong.
func (s *Service) InvalidateStatuses(names []string) error {
	if len(names) == 0 {
		return nil
	}
	cached, _, ok := cache.Get[map[string]StatusEntry](s.Cache, statusCacheKey, cache.Forever)
	if !ok {
		return nil // nothing cached yet
	}
	for _, name := range names {
		delete(cached, name)
	}
	return cache.Put(s.Cache, statusCacheKey, cached)
}

// InvalidateExecutions drops the given pipelines' memoized execution history
// (and the action lists hanging off it). Like InvalidateStatuses, this is for
// after a release: the pipeline has a new run that the memo predates, and the
// memo's own TTL would otherwise hide it for minutes.
func (s *Service) InvalidateExecutions(names []string) {
	if len(names) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		delete(s.execsMemo, name)
		prefix := name + "\x00"
		for key := range s.actionsMemo {
			if strings.HasPrefix(key, prefix) {
				delete(s.actionsMemo, key)
			}
		}
	}
}

func (s *Service) latestExecution(ctx context.Context, name string) (*model.Execution, error) {
	out, err := s.Read.ListPipelineExecutions(ctx, &codepipeline.ListPipelineExecutionsInput{
		PipelineName: aws.String(name),
		MaxResults:   aws.Int32(1),
	})
	if err != nil {
		return nil, err
	}
	if len(out.PipelineExecutionSummaries) == 0 {
		return nil, nil // pipeline exists but never ran
	}
	exec := executionFromSummary(out.PipelineExecutionSummaries[0])
	return &exec, nil
}

// executionFromSummary normalizes an SDK execution summary into the domain
// model (SDK types must not leak past this package).
func executionFromSummary(e cptypes.PipelineExecutionSummary) model.Execution {
	exec := model.Execution{
		ID:         aws.ToString(e.PipelineExecutionId),
		Status:     model.ParseStatus(string(e.Status)),
		StartTime:  e.StartTime,
		LastUpdate: e.LastUpdateTime,
	}
	for _, r := range e.SourceRevisions {
		exec.Revisions = append(exec.Revisions, model.Revision{
			ActionName: aws.ToString(r.ActionName),
			RevisionID: aws.ToString(r.RevisionId),
			Summary:    aws.ToString(r.RevisionSummary),
			URL:        aws.ToString(r.RevisionUrl),
		})
	}
	return exec
}

// Detail returns the current stage/action state of one pipeline plus up to
// historyN recent executions (historyN <= 0 skips the history call). It is
// the read side of F2 (docs/design.md §4.2) and supplies the CodeBuild ids
// that `pipeline logs` follows.
func (s *Service) Detail(
	ctx context.Context,
	name string,
	historyN int32,
	resolver *account.Resolver,
) (*model.PipelineDetail, error) {
	state, err := s.Read.GetPipelineState(ctx, &codepipeline.GetPipelineStateInput{
		Name: aws.String(name),
	})
	if err != nil {
		return nil, fmt.Errorf("GetPipelineState(%s): %w", name, err)
	}

	d := &model.PipelineDetail{
		PipelineName: name,
		AccountID:    model.AccountIDFromPipeline(name),
	}
	if resolver != nil {
		if a := resolver.ByID(d.AccountID); a != nil {
			d.AccountName = a.Name
		}
	}
	for _, st := range state.StageStates {
		stage := model.StageState{Name: aws.ToString(st.StageName)}
		if st.LatestExecution != nil {
			stage.Status = model.ParseStatus(string(st.LatestExecution.Status))
		}
		for _, a := range st.ActionStates {
			as := model.ActionState{Name: aws.ToString(a.ActionName)}
			if le := a.LatestExecution; le != nil {
				as.Status = model.ParseStatus(string(le.Status))
				as.Summary = aws.ToString(le.Summary)
				// GetPipelineState carries no action type, so gate on the
				// id's shape: source actions put a commit SHA here, which
				// must not be treated as a CodeBuild build id.
				if id := aws.ToString(le.ExternalExecutionId); isCodeBuildID(id) {
					as.CodeBuildID = id
				}
				as.LogStreamARN = aws.ToString(le.LogStreamARN)
				as.ExternalURL = aws.ToString(le.ExternalExecutionUrl)
				as.LastChange = le.LastStatusChange
				if le.ErrorDetails != nil {
					as.ErrorMessage = aws.ToString(le.ErrorDetails.Message)
				}
			}
			stage.Actions = append(stage.Actions, as)
		}
		d.Stages = append(d.Stages, stage)
	}

	if historyN > 0 {
		out, err := s.Read.ListPipelineExecutions(ctx, &codepipeline.ListPipelineExecutionsInput{
			PipelineName: aws.String(name),
			MaxResults:   aws.Int32(historyN),
		})
		if err != nil {
			return nil, fmt.Errorf("ListPipelineExecutions(%s): %w", name, err)
		}
		for _, e := range out.PipelineExecutionSummaries {
			d.History = append(d.History, executionFromSummary(e))
		}
	}
	return d, nil
}

// Executions returns up to maxN recent executions of one pipeline, newest
// first (a single ListPipelineExecutions page; maxN is capped by the API at
// 100, plenty for the TUI's drill-down). The history is served from the
// session memo within ExecutionsTTL unless refresh forces a refetch or the
// memoized head execution is in-flight (a running pipeline moves fast, an
// idle one not at all).
func (s *Service) Executions(ctx context.Context, name string, maxN int32, refresh bool) ([]model.Execution, error) {
	if !refresh {
		s.mu.Lock()
		e, ok := s.execsMemo[name]
		s.mu.Unlock()
		if ok && time.Since(e.fetchedAt) <= s.ExecutionsTTL &&
			!(len(e.execs) > 0 && e.execs[0].Status.InFlight()) {
			return e.execs, nil
		}
	}

	out, err := s.Read.ListPipelineExecutions(ctx, &codepipeline.ListPipelineExecutionsInput{
		PipelineName: aws.String(name),
		MaxResults:   aws.Int32(maxN),
	})
	if err != nil {
		return nil, fmt.Errorf("ListPipelineExecutions(%s): %w", name, err)
	}
	execs := make([]model.Execution, 0, len(out.PipelineExecutionSummaries))
	for _, e := range out.PipelineExecutionSummaries {
		execs = append(execs, executionFromSummary(e))
	}
	if s.ExecutionsTTL > 0 {
		s.mu.Lock()
		if s.execsMemo == nil {
			s.execsMemo = map[string]execsEntry{}
		}
		s.execsMemo[name] = execsEntry{execs: execs, fetchedAt: time.Now()}
		s.mu.Unlock()
	}
	return execs, nil
}

// ActionExecutions returns the per-action run details of one pipeline
// execution. The API yields newest-first; the result is normalized to
// chronological (stage) order so it reads top-to-bottom like the pipeline
// and model.LogAction's "last build" pick lands on the final action.
// done marks the execution as terminal: its actions are then immutable and
// memoized for the session, while an in-flight execution refetches every
// time (its actions are still progressing).
func (s *Service) ActionExecutions(ctx context.Context, name, execID string, done bool) ([]model.ActionExecution, error) {
	key := name + "\x00" + execID
	if done {
		s.mu.Lock()
		memo, ok := s.actionsMemo[key]
		s.mu.Unlock()
		if ok {
			return memo, nil
		}
	}

	var actions []model.ActionExecution
	in := &codepipeline.ListActionExecutionsInput{
		PipelineName: aws.String(name),
		Filter:       &cptypes.ActionExecutionFilter{PipelineExecutionId: aws.String(execID)},
	}
	for {
		out, err := s.Read.ListActionExecutions(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("ListActionExecutions(%s): %w", name, err)
		}
		for _, d := range out.ActionExecutionDetails {
			actions = append(actions, actionExecutionFromDetail(d))
		}
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	sort.SliceStable(actions, func(i, j int) bool {
		a, b := actions[i].StartTime, actions[j].StartTime
		if a == nil || b == nil {
			return b == nil && a != nil // unknown start sinks last
		}
		return a.Before(*b)
	})
	if done {
		s.mu.Lock()
		if s.actionsMemo == nil {
			s.actionsMemo = map[string][]model.ActionExecution{}
		}
		s.actionsMemo[key] = actions
		s.mu.Unlock()
	}
	return actions, nil
}

// actionExecutionFromDetail normalizes an SDK action execution detail into
// the domain model (SDK types must not leak past this package). The
// external execution id is only kept as CodeBuildID for CodeBuild actions —
// source actions carry a commit SHA there, which must not be fed to
// BatchGetBuilds.
func actionExecutionFromDetail(d cptypes.ActionExecutionDetail) model.ActionExecution {
	a := model.ActionExecution{
		StageName:  aws.ToString(d.StageName),
		ActionName: aws.ToString(d.ActionName),
		Status:     model.ParseStatus(string(d.Status)),
		StartTime:  d.StartTime,
		LastUpdate: d.LastUpdateTime,
	}
	provider := ""
	if d.Input != nil && d.Input.ActionTypeId != nil {
		provider = aws.ToString(d.Input.ActionTypeId.Provider)
	}
	if out := d.Output; out != nil && out.ExecutionResult != nil {
		r := out.ExecutionResult
		// Source actions carry the CodeConnections JSON here — unwrap it to
		// the commit message, like the executions screen does.
		a.Summary = model.UnwrapProviderSummary(aws.ToString(r.ExternalExecutionSummary))
		a.LogStreamARN = aws.ToString(r.LogStreamARN)
		a.ExternalURL = aws.ToString(r.ExternalExecutionUrl)
		if r.ErrorDetails != nil {
			a.ErrorMessage = aws.ToString(r.ErrorDetails.Message)
		}
		id := aws.ToString(r.ExternalExecutionId)
		switch {
		case provider == "CodeBuild":
			a.CodeBuildID = id
		case provider == "" && isCodeBuildID(id):
			// No type info — fall back to the id's shape.
			a.CodeBuildID = id
		}
	}
	return a
}

// isCodeBuildID reports whether an external execution id has the CodeBuild
// build-id shape ("<project>:<uuid>"). Source revisions are bare commit
// SHAs and never contain a colon.
func isCodeBuildID(id string) bool {
	return strings.Contains(id, ":")
}

// ReleaseRequest is one guarded batch of StartPipelineExecution calls.
type ReleaseRequest struct {
	Targets        []model.PipelineSummary
	SkipInProgress bool
}

// Release triggers the targets through the batch engine. In-progress
// pipelines are skipped when SkipInProgress (re-releasing them supersedes
// the running execution, which is rarely what an operator wants).
func (s *Service) Release(
	ctx context.Context,
	start StartAPI,
	req ReleaseRequest,
	onProgress func(batch.Progress),
) []model.ReleaseResult {
	results := batch.Run(ctx, s.Batch, req.Targets,
		func(ctx context.Context, t model.PipelineSummary) (model.ReleaseResult, error) {
			r := model.ReleaseResult{
				PipelineName: t.PipelineName,
				AccountID:    t.AccountID,
				AccountName:  t.AccountName,
			}
			if req.SkipInProgress && t.Status().InFlight() {
				r.Skipped = true
				r.SkipReason = string(t.Status())
				return r, nil
			}
			out, err := start.StartPipelineExecution(ctx, &codepipeline.StartPipelineExecutionInput{
				Name: aws.String(t.PipelineName),
			})
			if err != nil {
				r.Error = err.Error()
				return r, err
			}
			r.ExecutionID = aws.ToString(out.PipelineExecutionId)
			return r, nil
		}, onProgress)

	out := make([]model.ReleaseResult, len(results))
	for i, res := range results {
		if res.Err != nil && res.Value.PipelineName == "" {
			// batch-level failure (e.g. cancelled before start)
			out[i] = model.ReleaseResult{
				PipelineName: req.Targets[i].PipelineName,
				AccountID:    req.Targets[i].AccountID,
				AccountName:  req.Targets[i].AccountName,
				Error:        res.Err.Error(),
			}
			continue
		}
		out[i] = res.Value
	}
	return out
}
