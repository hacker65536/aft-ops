package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	cptypes "github.com/aws/aws-sdk-go-v2/service/codepipeline/types"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

// UpdateAPI is the subset of CodePipeline the trigger fix writes with. Like
// StartAPI it may be a client built on a different, admin-capable profile;
// the definitions it rewrites are read through the Service's read client.
type UpdateAPI interface {
	UpdatePipeline(ctx context.Context, in *codepipeline.UpdatePipelineInput,
		opts ...func(*codepipeline.Options)) (*codepipeline.UpdatePipelineOutput, error)
}

// UpdateTriggersRequest is one guarded batch of trigger fixes.
type UpdateTriggersRequest struct {
	// Targets are the plan: rows PlanTriggerFix judged FixUpdate, each
	// carrying the expected trigger and the actual one the operator was
	// shown. A target whose trigger no longer matches that Actual is skipped.
	Targets []model.TriggerSummary
	// BackupDir receives each pipeline's trigger declaration as it was
	// before the write. Required: nothing is written without a backup.
	BackupDir string
	// Concurrency overrides the default of one pipeline at a time when > 0.
	Concurrency int
}

// UpdateTriggers replaces the trigger of each target with its expected one.
//
// UpdatePipeline replaces the whole definition, so this is a read-modify-
// write: the definition is read again immediately before each write, and
// only its Triggers field is replaced on the SDK declaration that came back.
// Nothing is round-tripped through another representation, so no field the
// tool does not know about can be dropped on the way.
//
// The default is one pipeline at a time. UpdatePipeline's throttling limit is
// unmeasured, and GetPipeline alone already tops out at three concurrent
// calls (triggerConcurrencyCap).
func (s *Service) UpdateTriggers(
	ctx context.Context,
	upd UpdateAPI,
	req UpdateTriggersRequest,
	onProgress func(batch.Progress),
) []model.TriggerFixResult {
	cfg := s.Batch
	cfg.Concurrency = 1
	if req.Concurrency > 0 {
		cfg.Concurrency = req.Concurrency
	}
	results := batch.Run(ctx, cfg, req.Targets,
		func(ctx context.Context, t model.TriggerSummary) (model.TriggerFixResult, error) {
			r := s.updateTrigger(ctx, upd, t, req.BackupDir)
			if r.Outcome == model.FixFailed {
				return r, errors.New(r.Error)
			}
			return r, nil
		}, onProgress)

	out := make([]model.TriggerFixResult, len(results))
	written := map[string][]model.PushTrigger{}
	for i, res := range results {
		r := res.Value
		if r.PipelineName == "" {
			// Batch-level failure, e.g. cancelled before this item started.
			t := req.Targets[i]
			r = model.TriggerFixResult{
				PipelineName: t.PipelineName,
				AccountID:    t.AccountID,
				AccountName:  t.AccountName,
				Outcome:      model.FixFailed,
				Before:       t.Actual,
			}
			if res.Err != nil {
				r.Error = res.Err.Error()
			}
		}
		if r.Outcome == model.FixUpdated && r.After != nil {
			written[r.PipelineName] = []model.PushTrigger{*r.After}
		}
		out[i] = r
	}
	if err := s.storeTriggers(written); err != nil {
		fmt.Fprintln(os.Stderr, "warning: failed to update trigger cache:", err)
	}
	return out
}

// updateTrigger fixes one pipeline. It never returns an error: every way it
// can end is an outcome on the row.
func (s *Service) updateTrigger(
	ctx context.Context,
	upd UpdateAPI,
	t model.TriggerSummary,
	backupDir string,
) model.TriggerFixResult {
	r := model.TriggerFixResult{
		PipelineName: t.PipelineName,
		AccountID:    t.AccountID,
		AccountName:  t.AccountName,
		Outcome:      model.FixFailed,
		Before:       t.Actual,
	}
	fail := func(err error) model.TriggerFixResult {
		r.Error = err.Error()
		return r
	}
	if t.Expected == nil {
		return fail(errors.New("no expected trigger"))
	}

	got, err := s.Read.GetPipeline(ctx, &codepipeline.GetPipelineInput{Name: aws.String(t.PipelineName)})
	if err != nil {
		return fail(err)
	}
	if got.Pipeline == nil {
		return fail(errors.New("GetPipeline returned no definition"))
	}
	decl := *got.Pipeline
	r.VersionBefore = aws.ToInt32(decl.Version)

	current := make([]model.PushTrigger, 0, len(decl.Triggers))
	for _, d := range decl.Triggers {
		current = append(current, pushTriggerFromDeclaration(d))
	}
	r.Before = current
	if !model.SameTriggers(current, t.Actual) {
		r.Outcome, r.Reasons = model.FixSkipped, []string{model.SkipChangedSincePlan}
		return r
	}

	// UpdatePipeline stops an in-flight execution, so a running pipeline is
	// left for the next run rather than having its apply cut short.
	execs, err := s.Read.ListPipelineExecutions(ctx, &codepipeline.ListPipelineExecutionsInput{
		PipelineName: aws.String(t.PipelineName),
		MaxResults:   aws.Int32(1),
	})
	if err != nil {
		return fail(err)
	}
	if len(execs.PipelineExecutionSummaries) > 0 {
		switch execs.PipelineExecutionSummaries[0].Status {
		case cptypes.PipelineExecutionStatusInProgress, cptypes.PipelineExecutionStatusStopping:
			r.Outcome, r.Reasons = model.FixSkipped, []string{model.SkipInProgress}
			return r
		}
	}

	path, err := backupTriggers(backupDir, t.PipelineName, r.VersionBefore, decl.Triggers)
	if err != nil {
		return fail(fmt.Errorf("backup: %w", err))
	}
	r.BackupPath = path

	decl.Triggers = []cptypes.PipelineTriggerDeclaration{declarationFromPushTrigger(*t.Expected)}
	out, err := upd.UpdatePipeline(ctx, &codepipeline.UpdatePipelineInput{Pipeline: &decl})
	if err != nil {
		return fail(err)
	}
	if out.Pipeline != nil {
		r.VersionAfter = aws.ToInt32(out.Pipeline.Version)
		written := make([]model.PushTrigger, 0, len(out.Pipeline.Triggers))
		for _, d := range out.Pipeline.Triggers {
			written = append(written, pushTriggerFromDeclaration(d))
		}
		// The write went through, but what came back is not what was sent:
		// report it rather than call the pipeline fixed.
		if !model.SameTriggers(written, []model.PushTrigger{*t.Expected}) {
			return fail(errors.New("UpdatePipeline returned a trigger other than the one written"))
		}
	}
	want := *t.Expected
	r.Outcome, r.After = model.FixUpdated, &want
	return r
}

// storeTriggers records just-written triggers in the trigger cache, so the
// next `pipeline triggers` reports them without refetching. The entry is the
// value written, which UpdatePipeline has just echoed back.
func (s *Service) storeTriggers(written map[string][]model.PushTrigger) error {
	if len(written) == 0 {
		return nil
	}
	cached := map[string]triggerEntry{}
	if m, _, ok := cache.Get[map[string]triggerEntry](s.Cache, triggerCacheKey, cache.Forever); ok {
		cached = m
	}
	now := time.Now()
	for name, actual := range written {
		cached[name] = triggerEntry{Actual: actual, FetchedAt: now}
	}
	return cache.Put(s.Cache, triggerCacheKey, cached)
}

// declarationFromPushTrigger is the inverse of pushTriggerFromDeclaration for
// the shape an expectation has: one push filter, branch includes, file-path
// includes and excludes.
func declarationFromPushTrigger(t model.PushTrigger) cptypes.PipelineTriggerDeclaration {
	paths := &cptypes.GitFilePathFilterCriteria{Includes: t.FilePaths}
	if len(t.FilePathExcludes) > 0 {
		paths.Excludes = t.FilePathExcludes
	}
	return cptypes.PipelineTriggerDeclaration{
		ProviderType: cptypes.PipelineTriggerProviderType(t.ProviderType),
		GitConfiguration: &cptypes.GitConfiguration{
			SourceActionName: aws.String(t.SourceAction),
			Push: []cptypes.GitPushFilter{{
				Branches:  &cptypes.GitBranchFilterCriteria{Includes: t.Branches},
				FilePaths: paths,
			}},
		},
	}
}

// triggerBackup is the file a fix leaves behind for each pipeline it writes.
//
// It holds the trigger declarations rather than the whole definition, in
// CodePipeline's own JSON shape: the trigger is the only thing a fix changes,
// so it is the only thing there is to restore, and the SDK types have no
// serializer that produces the API's shape for a full definition. Restoring
// is a GetPipeline whose .pipeline.triggers is replaced with this file's
// triggers, sent back with UpdatePipeline. An empty list restores "no
// trigger".
type triggerBackup struct {
	PipelineName string           `json:"pipeline_name"`
	Version      int32            `json:"version"`
	SavedAt      time.Time        `json:"saved_at"`
	Triggers     []map[string]any `json:"triggers"`
}

func backupTriggers(dir, name string, version int32, decls []cptypes.PipelineTriggerDeclaration) (string, error) {
	if dir == "" {
		return "", errors.New("no backup directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b := triggerBackup{PipelineName: name, Version: version, SavedAt: time.Now(),
		Triggers: make([]map[string]any, 0, len(decls))}
	for _, d := range decls {
		b.Triggers = append(b.Triggers, triggerDeclarationJSON(d))
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// triggerDeclarationJSON renders one trigger declaration in CodePipeline's
// JSON shape (the one `aws codepipeline get-pipeline` prints), covering every
// field the trigger API defines so that a backup of a trigger the tool would
// refuse to touch is still complete.
func triggerDeclarationJSON(d cptypes.PipelineTriggerDeclaration) map[string]any {
	m := map[string]any{"providerType": string(d.ProviderType)}
	g := d.GitConfiguration
	if g == nil {
		return m
	}
	git := map[string]any{"sourceActionName": aws.ToString(g.SourceActionName)}
	if len(g.Push) > 0 {
		push := make([]map[string]any, 0, len(g.Push))
		for _, p := range g.Push {
			f := map[string]any{}
			putCriteria(f, "branches", p.Branches)
			putCriteria(f, "filePaths", p.FilePaths)
			putCriteria(f, "tags", p.Tags)
			push = append(push, f)
		}
		git["push"] = push
	}
	if len(g.PullRequest) > 0 {
		prs := make([]map[string]any, 0, len(g.PullRequest))
		for _, p := range g.PullRequest {
			f := map[string]any{}
			if len(p.Events) > 0 {
				events := make([]string, len(p.Events))
				for i, e := range p.Events {
					events[i] = string(e)
				}
				f["events"] = events
			}
			putCriteria(f, "branches", p.Branches)
			putCriteria(f, "filePaths", p.FilePaths)
			prs = append(prs, f)
		}
		git["pullRequest"] = prs
	}
	m["gitConfiguration"] = git
	return m
}

// putCriteria adds an includes/excludes filter under key when it is present.
// The three SDK criteria types share the shape but not a type.
func putCriteria(m map[string]any, key string, c any) {
	var inc, exc []string
	switch v := c.(type) {
	case *cptypes.GitBranchFilterCriteria:
		if v == nil {
			return
		}
		inc, exc = v.Includes, v.Excludes
	case *cptypes.GitFilePathFilterCriteria:
		if v == nil {
			return
		}
		inc, exc = v.Includes, v.Excludes
	case *cptypes.GitTagFilterCriteria:
		if v == nil {
			return
		}
		inc, exc = v.Includes, v.Excludes
	default:
		return
	}
	f := map[string]any{}
	if len(inc) > 0 {
		f["includes"] = inc
	}
	if len(exc) > 0 {
		f["excludes"] = exc
	}
	m[key] = f
}
