// Package demo runs aft-ops against a local fixture file instead of AWS.
//
// The substitution happens at the AWS SDK client boundary: the fakes here
// implement the same narrow interfaces the core services already depend on
// (pipeline.API, pipeline.StartAPI, logs.CodeBuildAPI, logs.LogsAPI,
// account.Source), so everything above the adapter layer — normalization,
// caching, the batch engine, sorting, the CLI renderers, the TUI — runs
// exactly as it does against a real AFT account. One fixture therefore
// covers every demo, and no code path is special-cased for the demo.
//
// This is what `--demo <fixture.json>` selects. It needs no credentials, no
// network, and no AWS account, which makes it both the recording harness for
// the README GIFs (docs/demo) and a way to try the tool before pointing it
// at a real management account.
package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// SchemaVersion is the fixture format this build understands.
const SchemaVersion = 1

// Duration is a time.Duration serialized as a string ("18m", "6m12s").
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// D returns the wrapped time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Fixture is the on-disk fake-data format.
//
// Every time is relative to when the fixture is loaded rather than absolute:
// a committed fixture with absolute timestamps would read "3 weeks ago" on
// every later recording, and the point of the demo is to look like a live
// account.
type Fixture struct {
	SchemaVersion int      `json:"schema_version"`
	Identity      Identity `json:"identity"`
	// Latency is slept before every fake API call. Without it the fan-out
	// over the whole inventory finishes instantly and the progress
	// indicator — one of the things worth showing — never appears.
	Latency Duration `json:"latency"`
	// Release describes the pseudo-execution that StartPipelineExecution
	// creates, so a Release change can be demonstrated end to end.
	Release ReleaseSpec `json:"release"`
	// OtherPipelines are non-account pipelines (AFT's own) returned by
	// ListPipelines. They exist to show model.AccountPipelineRe filtering
	// them out of the inventory.
	OtherPipelines []string            `json:"other_pipelines,omitempty"`
	Accounts       []model.Account     `json:"accounts"`
	Pipelines      []Pipeline          `json:"pipelines"`
	Logs           map[string][]string `json:"logs"`
}

// Identity names the account the demo pretends to be attached to. It feeds
// the target banner every command prints.
type Identity struct {
	Account string `json:"account"`
	Region  string `json:"region"`
	Profile string `json:"profile"`
}

// ReleaseSpec configures the pseudo-execution started by a Release change.
type ReleaseSpec struct {
	Takes Duration `json:"takes"` // how long the new execution runs for
	Log   string   `json:"log"`   // Logs key its build actions serve
}

// Pipeline is one account's customizations pipeline.
type Pipeline struct {
	AccountID string `json:"account_id"`
	// Trigger is the push trigger the pipeline declares. Absent means the
	// pipeline has none — which is a state worth demonstrating, since it is
	// what `pipeline triggers` reports after AFT rewrites a pipeline.
	Trigger    *Trigger    `json:"trigger,omitempty"`
	Executions []Execution `json:"executions"` // newest first
}

// Trigger is a fixture pipeline's push trigger. It is written out in full
// rather than derived from the account so that a fixture can hold a drifted
// one (a file path pointing at the wrong directory) next to correct ones.
type Trigger struct {
	ProviderType string   `json:"provider_type,omitempty"` // default CodeStarSourceConnection
	SourceAction string   `json:"source_action"`
	Branches     []string `json:"branches"`
	FilePaths    []string `json:"file_paths"`
}

// Name returns the CodePipeline name AFT would give this account.
func (p Pipeline) Name() string { return p.AccountID + "-customizations-pipeline" }

// Execution is one run of a pipeline.
type Execution struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// StartedAgo places the run before the load time.
	StartedAgo Duration `json:"started_ago"`
	// Took is the run's span; terminal executions only.
	Took Duration `json:"took,omitempty"`
	// CompletesIn makes an in-flight execution finish this long after the
	// fixture is loaded — which is what lets `--watch` and the TUI's poll
	// be recorded doing their actual job instead of being mimed.
	CompletesIn Duration `json:"completes_in,omitempty"`
	// CompletesAs is the status it lands on (default Succeeded).
	CompletesAs string   `json:"completes_as,omitempty"`
	Revision    Revision `json:"revision,omitempty"`
	Actions     []Action `json:"actions"`
}

// Revision is the source revision that triggered the run.
type Revision struct {
	ActionName string `json:"action_name,omitempty"`
	RevisionID string `json:"revision_id,omitempty"`
	Summary    string `json:"summary,omitempty"`
	URL        string `json:"url,omitempty"`
}

// Action is one action run inside an execution.
type Action struct {
	Stage  string `json:"stage"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// StartsAfter is the offset from the execution's start; Takes is the
	// action's own span.
	StartsAfter Duration `json:"starts_after,omitempty"`
	Takes       Duration `json:"takes,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Error       string   `json:"error,omitempty"`
	// BuildID is a CodeBuild build id ("<project>:<uuid>"); empty for source
	// actions, which carry RevisionID (a bare commit SHA) instead. The colon
	// is what tells the two apart downstream, so a fixture must not put one
	// in a RevisionID.
	BuildID    string `json:"build_id,omitempty"`
	RevisionID string `json:"revision_id,omitempty"`
	URL        string `json:"url,omitempty"`
	// Log keys into Fixture.Logs. Several actions may share one body.
	Log string `json:"log,omitempty"`
}

// Env is a loaded fixture with a fixed time origin. All the fake clients
// hang off it and share its mutex: StartPipelineExecution mutates the
// pipeline list while the TUI's poll is reading it.
type Env struct {
	fx   Fixture
	path string
	base time.Time
	now  func() time.Time
	seq  int

	mu sync.Mutex
}

// Load reads a fixture and pins its time origin to now.
func Load(path string) (*Env, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read demo fixture: %w", err)
	}
	var fx Fixture
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		return nil, fmt.Errorf("parse demo fixture %s: %w", path, err)
	}
	if fx.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("demo fixture %s: schema_version %d, want %d",
			path, fx.SchemaVersion, SchemaVersion)
	}
	if len(fx.Pipelines) == 0 {
		return nil, fmt.Errorf("demo fixture %s: no pipelines", path)
	}
	if fx.Identity.Region == "" {
		return nil, fmt.Errorf("demo fixture %s: identity.region is required", path)
	}
	for _, p := range fx.Pipelines {
		if model.AccountIDFromPipeline(p.Name()) == "" {
			return nil, fmt.Errorf("demo fixture %s: %q is not a 12-digit account id",
				path, p.AccountID)
		}
		for _, e := range p.Executions {
			for _, a := range e.Actions {
				if a.Log != "" {
					if _, ok := fx.Logs[a.Log]; !ok {
						return nil, fmt.Errorf("demo fixture %s: %s/%s references unknown log %q",
							path, p.AccountID, a.Name, a.Log)
					}
				}
			}
		}
	}

	env := &Env{fx: fx, path: path, base: time.Now()}
	env.now = time.Now
	return env, nil
}

// Path is the fixture's location (used to label the account source).
func (e *Env) Path() string { return e.path }

// Identity returns the account/region/profile the demo presents.
func (e *Env) Identity() Identity { return e.fx.Identity }

// SetLatency overrides the fixture's per-call latency (AFT_OPS_DEMO_LATENCY),
// so one fixture can be paced differently per recording.
func (e *Env) SetLatency(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fx.Latency = Duration(d)
}

// execState is one execution's effective state at a moment in time.
type execState struct {
	Status     model.Status
	Start      time.Time
	LastUpdate time.Time
	// Done reports whether the execution has reached a terminal status by
	// now — including an in-flight one whose CompletesIn has elapsed.
	Done bool
}

// state resolves an execution's fixture description into concrete times and
// a status as of now.
//
// CodePipeline reports lastUpdateTime equal to startTime while an execution
// is in flight (see model.Execution.Elapsed), and the fakes reproduce that:
// the count-up in the list and the elapsed column exist precisely to work
// around it, so a fixture that reported a growing lastUpdateTime would
// demo a behaviour the tool never sees.
func (e *Env) state(ex Execution, now time.Time) execState {
	start := e.base.Add(-ex.StartedAgo.D())
	declared := model.ParseStatus(ex.Status)

	if !declared.InFlight() {
		return execState{
			Status:     declared,
			Start:      start,
			LastUpdate: start.Add(ex.Took.D()),
			Done:       true,
		}
	}

	if ex.CompletesIn > 0 {
		finish := e.base.Add(ex.CompletesIn.D())
		if !now.Before(finish) {
			final := model.StatusSucceeded
			if ex.CompletesAs != "" {
				final = model.ParseStatus(ex.CompletesAs)
			}
			return execState{Status: final, Start: start, LastUpdate: finish, Done: true}
		}
	}
	return execState{Status: declared, Start: start, LastUpdate: start}
}

// actionState resolves one action run against its execution's state.
// started is false for an action of a running execution that has not begun
// yet — the API would not list it, and neither do we.
func (e *Env) actionState(es execState, a Action, now time.Time) (st model.Status, start, last time.Time, started bool) {
	start = es.Start.Add(a.StartsAfter.D())
	end := start.Add(a.Takes.D())
	declared := model.ParseStatus(a.Status)

	if es.Done {
		// Clamp to the execution's end so an action can never outlive the
		// run it belongs to (a fixture's per-action spans are hand-written
		// and need not add up exactly).
		if end.After(es.LastUpdate) {
			end = es.LastUpdate
		}
		if start.After(end) {
			start = end
		}
		return declared, start, end, true
	}
	switch {
	case now.Before(start):
		return model.StatusUnknown, start, start, false
	case now.Before(end):
		return model.StatusInProgress, start, start, true
	default:
		return declared, start, end, true
	}
}

// pipelineByName finds a fixture pipeline. Callers hold e.mu.
func (e *Env) pipelineByName(name string) (int, bool) {
	for i := range e.fx.Pipelines {
		if e.fx.Pipelines[i].Name() == name {
			return i, true
		}
	}
	return 0, false
}

// buildRef locates the action that owns a CodeBuild id.
type buildRef struct {
	pipeline int
	exec     int
	action   int
}

// findBuild resolves a build id to its owning action. Callers hold e.mu.
func (e *Env) findBuild(id string) (buildRef, bool) {
	for pi := range e.fx.Pipelines {
		for xi := range e.fx.Pipelines[pi].Executions {
			acts := e.fx.Pipelines[pi].Executions[xi].Actions
			for ai := range acts {
				if acts[ai].BuildID == id {
					return buildRef{pi, xi, ai}, true
				}
			}
		}
	}
	return buildRef{}, false
}

// nextID mints a deterministic UUID-shaped id for a released execution.
// Shaped like the real thing because the UI abbreviates it to its first
// eight characters, and randomness is out because two recordings of the
// same tape should be able to produce the same frames.
func (e *Env) nextID(salt string) string {
	e.seq++
	n := uint64(e.seq) * 2654435761
	for _, c := range salt {
		n = n*31 + uint64(c)
	}
	m := n * 11400714819323198485
	return fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x",
		uint32(m), uint16(m>>32), uint16(m>>48)&0x0fff,
		uint16(m>>11)|0x8000, (m*31)&0xffffffffffff)
}

// logLines returns a log body by key (nil when absent).
func (e *Env) logLines(key string) []string { return e.fx.Logs[key] }

// base name of the fixture, for the account source label.
func (e *Env) label() string { return filepath.Base(e.path) }
