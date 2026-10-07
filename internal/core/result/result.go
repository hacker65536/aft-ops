// Package result reads the terraform outcome of AFT customizations runs:
// per build ("what did this terraform apply do?") and per pipeline execution
// (one result for the global layer, one for the account layer). It is the one
// path the TUI list, the TUI actions screen and `pipeline list --results` all
// take, so the three cannot reach different conclusions about the same build
// (docs/design.md §4.6).
//
// A finished build's log never changes, so its conclusion is cached on disk
// with no TTL — together with the conclusion of every terminal execution, so
// that a list whose pipelines have not run since the last look costs no API
// calls at all. Only the conclusion is stored, never the log itself.
package result

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/logs"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

// cacheKey is the on-disk cache entry holding every stored conclusion. One
// file rather than one per build: the cache scope directory stays listable
// (`cache status`), and the whole set is a few hundred small records.
const cacheKey = "terraform-results"

// ActionsSource lists the action runs of one pipeline execution in pipeline
// order (pipeline.Service.ActionExecutions).
type ActionsSource interface {
	ActionExecutions(ctx context.Context, name, execID string, done bool) ([]model.ActionExecution, error)
}

// Service reads terraform results. The zero value is not usable: Actions and
// Logs are required.
type Service struct {
	Actions ActionsSource
	Logs    *logs.Service
	Cache   cache.Store
	// Batch bounds the concurrency of reading several builds' logs at once
	// (the actions screen's builds). The rate limit itself is enforced per
	// API call by the clients (awsx.RateLimit).
	Batch batch.Config
	// MaxAge drops stored conclusions older than this when the cache is
	// written (0 keeps everything). See config cache.results_max_age.
	MaxAge time.Duration

	mu     sync.Mutex
	loaded bool
	data   stored
}

// stored is the on-disk shape of the results cache.
type stored struct {
	Builds     map[string]storedBuild `json:"builds"`     // by CodeBuild build id
	Executions map[string]storedExec  `json:"executions"` // by pipeline execution id
}

type storedBuild struct {
	Result model.TerraformResult `json:"result"`
	At     time.Time             `json:"at"`
}

type storedExec struct {
	Results model.ExecutionResults `json:"results"`
	At      time.Time              `json:"at"`
}

// ForExecution returns the terraform result of each layer of one pipeline
// execution. A terminal execution whose results were read before is served
// from the cache without a single API call; otherwise this is one
// ListActionExecutions (itself memoized for terminal executions) and one
// GetLogEvents per build.
//
// A layer whose build could not be read comes back as ResultUnknown, and the
// error says why; the other layer's result is still returned.
func (s *Service) ForExecution(ctx context.Context, name string, exec model.Execution) (model.ExecutionResults, error) {
	terminal := exec.Status.Terminal()
	if terminal {
		if r, ok := s.cachedExec(exec.ID); ok {
			return r, nil
		}
	}

	actions, err := s.Actions.ActionExecutions(ctx, name, exec.ID, terminal)
	if err != nil {
		return model.ExecutionResults{ExecutionID: exec.ID}, err
	}

	out := model.ExecutionResults{ExecutionID: exec.ID}
	var builds []model.ActionExecution
	for _, layer := range []model.Layer{model.LayerGlobal, model.LayerAccount} {
		if a := model.LayerAction(actions, layer); a != nil {
			builds = append(builds, *a)
		}
	}
	byBuild, err := s.ForActions(ctx, builds)
	for _, layer := range []model.Layer{model.LayerGlobal, model.LayerAccount} {
		var r model.TerraformResult
		switch a := model.LayerAction(actions, layer); {
		case a != nil:
			r = byBuild[a.CodeBuildID]
		case exec.Status.InFlight():
			// The run has not reached this stage yet.
			r = model.TerraformResult{Kind: model.ResultRunning}
		default:
			r = model.TerraformResult{Kind: model.ResultNotRun}
		}
		if layer == model.LayerGlobal {
			out.Global = &r
		} else {
			out.Account = &r
		}
	}

	// Store only what is final: every log was read (err == nil — an
	// unreadable one is worth retrying) and every layer reached a conclusion
	// (see final).
	if err == nil && terminal && final(out.Global) && final(out.Account) {
		s.storeExec(out)
	}
	return out, err
}

// final reports whether a result can be stored for good. Running is not. Nor
// is unknown: a log read moments after its build finished can still be
// missing its last lines (CloudWatch ingestion trails the build), and
// storing that would pin "?" to the row forever. Such builds are rare, so
// reading them again next time costs little.
func final(r *model.TerraformResult) bool {
	return r != nil && r.Kind != model.ResultRunning && r.Kind != model.ResultUnknown
}

// ForActions returns the terraform result of every action run that carries
// a CodeBuild build, keyed by build id. Running builds are reported as
// running without reading their log; finished builds come from the cache or
// from the tail of their log. Builds whose log could not be read are
// ResultUnknown, and the joined error names them.
func (s *Service) ForActions(ctx context.Context, actions []model.ActionExecution) (map[string]model.TerraformResult, error) {
	out := make(map[string]model.TerraformResult, len(actions))
	var need []model.ActionExecution
	for _, a := range actions {
		switch {
		case a.CodeBuildID == "":
			continue
		case !a.Status.Terminal():
			out[a.CodeBuildID] = model.TerraformResult{Kind: model.ResultRunning}
		default:
			if r, ok := s.cachedBuild(a.CodeBuildID); ok {
				out[a.CodeBuildID] = r
			} else {
				need = append(need, a)
			}
		}
	}
	if len(need) == 0 {
		return out, nil
	}

	results := batch.Run(ctx, s.Batch, need,
		func(ctx context.Context, a model.ActionExecution) (model.TerraformResult, error) {
			r, err := s.readBuild(ctx, a)
			if err != nil {
				return model.TerraformResult{Kind: model.ResultUnknown}, err
			}
			if final(&r) {
				s.storeBuild(a.CodeBuildID, r)
			}
			return r, nil
		}, nil)

	var errs []error
	for i, res := range results {
		id := need[i].CodeBuildID
		if res.Err != nil {
			out[id] = model.TerraformResult{Kind: model.ResultUnknown}
			errs = append(errs, res.Err)
			continue
		}
		out[id] = res.Value
	}
	return out, errors.Join(errs...)
}

// readBuild concludes one finished build: the tail of its log first, and
// the whole log only when the tail carries no verdict (a build whose terraform
// output was followed by more than a page of other output). The action's own
// status then decides what a missing or successful verdict means.
//
// The log is looked for where CodeBuild puts it by default, without asking
// BatchGetBuilds: that is where AFT's projects log, and BatchGetBuilds is the
// call CodeBuild throttles first (measured: one in six throttled while the
// list read every pipeline). Only when the default location does not exist
// — a project configured to log elsewhere — is the build looked up.
func (s *Service) readBuild(ctx context.Context, a model.ActionExecution) (model.TerraformResult, error) {
	b, ok := logs.DefaultLocation(a.CodeBuildID, true)
	var lines []string
	var err error
	if ok {
		lines, err = s.Logs.Tail(ctx, b)
	}
	if !ok || logs.IsNotFound(err) {
		located, lerr := s.Logs.Locate(ctx, []string{a.CodeBuildID})
		if lerr != nil {
			return model.TerraformResult{}, lerr
		}
		var found bool
		if b, found = located[a.CodeBuildID]; !found {
			return model.TerraformResult{}, fmt.Errorf("build %s not found", a.CodeBuildID)
		}
		lines, err = s.Logs.Tail(ctx, b)
	}
	if err != nil {
		return model.TerraformResult{}, err
	}
	r := logs.ParseVerdict(lines)
	if r.Kind == model.ResultUnknown {
		bl, err := s.Logs.FetchBuild(ctx, b)
		if err != nil {
			return model.TerraformResult{}, err
		}
		r = logs.ParseVerdict(bl.Lines)
	}
	// A failed action whose log shows no terraform error died outside
	// terraform (a helper script, the container). Its counts, if any, are
	// kept — they are what terraform did — but the cell must read as a
	// failure, not as a clean apply.
	if a.Status == model.StatusFailed && r.Kind != model.ResultError {
		r.Kind = model.ResultFailed
	}
	return r, nil
}

func (s *Service) cachedBuild(id string) (model.TerraformResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	e, ok := s.data.Builds[id]
	return e.Result, ok
}

func (s *Service) cachedExec(id string) (model.ExecutionResults, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	e, ok := s.data.Executions[id]
	return e.Results, ok
}

func (s *Service) storeBuild(id string, r model.TerraformResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	s.data.Builds[id] = storedBuild{Result: r, At: time.Now()}
	s.saveLocked()
}

func (s *Service) storeExec(r model.ExecutionResults) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	s.data.Executions[r.ExecutionID] = storedExec{Results: r, At: time.Now()}
	s.saveLocked()
}

// loadLocked reads the on-disk cache once per Service. A missing or
// unreadable file is an empty cache.
func (s *Service) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	if d, _, ok := cache.Get[stored](s.Cache, cacheKey, cache.Forever); ok {
		s.data = d
	}
	if s.data.Builds == nil {
		s.data.Builds = map[string]storedBuild{}
	}
	if s.data.Executions == nil {
		s.data.Executions = map[string]storedExec{}
	}
}

// saveLocked prunes entries older than MaxAge and writes the cache back.
// A write failure is ignored: the result in hand is still right, it will
// just be read again next time.
func (s *Service) saveLocked() {
	if s.MaxAge > 0 {
		cutoff := time.Now().Add(-s.MaxAge)
		for id, e := range s.data.Builds {
			if e.At.Before(cutoff) {
				delete(s.data.Builds, id)
			}
		}
		for id, e := range s.data.Executions {
			if e.At.Before(cutoff) {
				delete(s.data.Executions, id)
			}
		}
	}
	_ = cache.Put(s.Cache, cacheKey, s.data)
}
