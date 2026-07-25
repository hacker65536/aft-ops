package demo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"

	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/logs"
	"github.com/hacker65536/aft-ops/internal/core/model"
	"github.com/hacker65536/aft-ops/internal/core/pipeline"
)

// The fakes exist to be plugged into the core services, so the compiler
// should say when a core interface grows a method the fixture cannot serve.
var (
	_ pipeline.API      = (*PipelineClient)(nil)
	_ pipeline.StartAPI = (*StartClient)(nil)
	_ logs.CodeBuildAPI = (*CodeBuildClient)(nil)
	_ logs.LogsAPI      = (*LogsClient)(nil)
	_ account.Source    = (*AccountSource)(nil)
)

// bundledFixture is the fixture shipped for the README recordings. Testing
// against it (rather than a throwaway inline one) is deliberate: it is a
// committed artifact that the tapes depend on, and a silent drift in it
// would only show up as a broken GIF.
const bundledFixture = "../../docs/demo/fixture.json"

func load(t *testing.T) *Env {
	t.Helper()
	env, err := Load(bundledFixture)
	if err != nil {
		t.Fatalf("Load(%s): %v", bundledFixture, err)
	}
	return env
}

// svc wires the fakes into the real pipeline service, so every assertion
// below also covers the core path the CLI and TUI take.
func svc(t *testing.T, env *Env) *pipeline.Service {
	t.Helper()
	return &pipeline.Service{
		Read:  env.PipelineAPI(),
		Cache: cache.New(t.TempDir(), "demo", "test"),
	}
}

func TestLoadBundledFixture(t *testing.T) {
	env := load(t)

	if got := env.Identity().Region; got == "" {
		t.Error("identity.region is empty")
	}
	if n := len(env.fx.Accounts); n < 20 {
		t.Errorf("accounts = %d, want a demo-sized inventory (>=20)", n)
	}
	if len(env.fx.Pipelines) != len(env.fx.Accounts) {
		t.Errorf("pipelines = %d, accounts = %d; every account should have one",
			len(env.fx.Pipelines), len(env.fx.Accounts))
	}
	// Latency has to stay small: it is multiplied by the whole inventory on
	// every fan-out, and a slow fixture makes a tape unrecordable.
	if d := env.fx.Latency.D(); d > 250*time.Millisecond {
		t.Errorf("latency = %s, too slow for a recording", d)
	}
}

// TestInventoryFiltersNonAccountPipelines checks the fixture keeps AFT's own
// pipelines in the ListPipelines response, so the inventory filter is
// actually exercised rather than handed a pre-filtered list.
func TestInventoryFiltersNonAccountPipelines(t *testing.T) {
	env := load(t)
	if len(env.fx.OtherPipelines) == 0 {
		t.Fatal("fixture has no non-account pipelines; the inventory filter is untested")
	}

	names, _, err := svc(t, env).Inventory(context.Background(), true)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(names) != len(env.fx.Pipelines) {
		t.Fatalf("inventory = %d names, want %d", len(names), len(env.fx.Pipelines))
	}
	for _, n := range names {
		if model.AccountIDFromPipeline(n) == "" {
			t.Errorf("inventory kept a non-account pipeline: %s", n)
		}
	}
}

// TestStatusMix guards the property the demo exists for: the list has to
// show more than one kind of row.
func TestStatusMix(t *testing.T) {
	env := load(t)
	s := svc(t, env)
	ctx := context.Background()

	names, _, err := s.Inventory(ctx, true)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	summaries, stats := s.Statuses(ctx, names, nil, pipeline.StatusOptions{RefreshAll: true}, nil)
	if stats.Failed != 0 {
		t.Errorf("%d status fetches failed", stats.Failed)
	}

	counts := map[model.Status]int{}
	for _, sum := range summaries {
		counts[sum.Status()]++
	}
	for _, want := range []model.Status{
		model.StatusSucceeded, model.StatusFailed,
		model.StatusInProgress, model.StatusUnknown,
	} {
		if counts[want] == 0 {
			t.Errorf("no %s pipeline in the fixture (counts: %v)", want, counts)
		}
	}
	if counts[model.StatusSucceeded] < counts[model.StatusFailed] {
		t.Error("a demo where most pipelines are broken misrepresents the tool")
	}
}

// TestDetailAndActions walks the drill-down the TUI takes and checks the
// shapes the screens rely on: two CodeBuild logs per run, a failed action
// carrying an error, and chronological action order.
func TestDetailAndActions(t *testing.T) {
	env := load(t)
	s := svc(t, env)
	ctx := context.Background()

	name := failedPipeline(t, env)
	d, err := s.Detail(ctx, name, 5, nil)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if len(d.FailedActions()) == 0 {
		t.Fatal("the failed pipeline's state reports no failed action")
	}
	if got := d.FailedActions()[0].ErrorMessage; got == "" {
		t.Error("failed action carries no error message")
	}
	if len(d.History) < 2 {
		t.Errorf("history = %d executions, want a few to page through", len(d.History))
	}

	execs, err := s.Executions(ctx, name, 25, true)
	if err != nil {
		t.Fatalf("Executions: %v", err)
	}
	acts, err := s.ActionExecutions(ctx, name, execs[0].ID, true)
	if err != nil {
		t.Fatalf("ActionExecutions: %v", err)
	}
	if n := len(model.LogActions(acts)); n != 2 {
		t.Errorf("build actions = %d, want 2 (global + account customizations)", n)
	}
	for i := 1; i < len(acts); i++ {
		if acts[i-1].StartTime.After(*acts[i].StartTime) {
			t.Fatalf("actions are not chronological at %d", i)
		}
	}
	// A source action's commit SHA must never be mistaken for a build id.
	for _, a := range acts {
		if a.StageName == "Source" && a.CodeBuildID != "" {
			t.Errorf("source action %q was given a CodeBuild id (%s)", a.ActionName, a.CodeBuildID)
		}
	}
}

// TestLogsRoundTrip fetches a failed build's log through the real logs
// service and checks the extractors find what the log screen shows.
func TestLogsRoundTrip(t *testing.T) {
	env := load(t)
	ls := &logs.Service{CodeBuild: env.CodeBuildAPI(), Logs: env.LogsAPI()}
	ctx := context.Background()

	name := failedPipeline(t, env)
	d, err := svc(t, env).Detail(ctx, name, 0, nil)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	buildID := d.FailedActions()[0].CodeBuildID
	bl, err := ls.Fetch(ctx, buildID)
	if err != nil {
		t.Fatalf("Fetch(%s): %v", buildID, err)
	}
	// The fixture is longer than one page, so the paging loop ran.
	if len(bl.Lines) <= logPageSize {
		t.Logf("log is %d lines; paging not exercised", len(bl.Lines))
	}

	tf := logs.ExtractTerraform(bl.Lines)
	if len(tf) == 0 || len(tf) >= len(bl.Lines) {
		t.Errorf("terraform extraction returned %d of %d lines; the fixture's "+
			"CodeBuild preamble should be stripped and something should remain",
			len(tf), len(bl.Lines))
	}
	if v := logs.Verdict(bl.Lines); !strings.HasPrefix(v, "Error:") {
		t.Errorf("verdict of a failed build = %q, want an Error: line", v)
	}
	if n := len(logs.Summarize(bl.Lines)); n == 0 {
		t.Error("summary of a failed build is empty")
	}
}

// TestInFlightCompletes covers the mechanism the --watch and TUI-poll
// recordings depend on: an execution declared in-flight has to actually
// finish once its completes_in has elapsed, without the fixture being
// touched.
func TestInFlightCompletes(t *testing.T) {
	env := load(t)

	var target Execution
	var pipelineName string
	for _, p := range env.fx.Pipelines {
		for _, ex := range p.Executions {
			if ex.CompletesIn > 0 {
				target, pipelineName = ex, p.Name()
				break
			}
		}
	}
	if pipelineName == "" {
		t.Fatal("no execution with completes_in; the watch demo has nothing to show")
	}

	before := env.state(target, env.base)
	if !before.Status.InFlight() {
		t.Fatalf("before completes_in: status = %s, want in-flight", before.Status)
	}
	if !before.LastUpdate.Equal(before.Start) {
		t.Error("an in-flight execution must report lastUpdate == startTime, as CodePipeline does")
	}

	after := env.state(target, env.base.Add(target.CompletesIn.D()+time.Second))
	if !after.Status.Terminal() {
		t.Fatalf("after completes_in: status = %s, want terminal", after.Status)
	}
	if !after.LastUpdate.After(after.Start) {
		t.Error("a finished execution must report a real span")
	}

	// And the same transition must be visible through the core service.
	s := svc(t, env)
	ctx := context.Background()
	env.now = func() time.Time { return env.base }
	execs, err := s.Executions(ctx, pipelineName, 5, true)
	if err != nil {
		t.Fatalf("Executions: %v", err)
	}
	if !execs[0].Status.InFlight() {
		t.Fatalf("head execution = %s, want in-flight", execs[0].Status)
	}
	env.now = func() time.Time { return env.base.Add(target.CompletesIn.D() + time.Second) }
	execs, err = s.Executions(ctx, pipelineName, 5, true)
	if err != nil {
		t.Fatalf("Executions: %v", err)
	}
	if !execs[0].Status.Terminal() {
		t.Errorf("head execution = %s after completion, want terminal", execs[0].Status)
	}
}

// TestReleaseStartsExecution covers the demo's write path: a Release change
// must produce a run that is genuinely in flight and genuinely finishes, or
// the release recording is a mime act.
func TestReleaseStartsExecution(t *testing.T) {
	env := load(t)
	s := svc(t, env)
	ctx := context.Background()

	name := env.fx.Pipelines[0].Name()
	beforeN := len(env.fx.Pipelines[0].Executions)

	out, err := env.StartAPI().StartPipelineExecution(ctx,
		&codepipeline.StartPipelineExecutionInput{Name: aws.String(name)})
	if err != nil {
		t.Fatalf("StartPipelineExecution: %v", err)
	}
	if out.PipelineExecutionId == nil || *out.PipelineExecutionId == "" {
		t.Fatal("no execution id returned")
	}
	if got := len(env.fx.Pipelines[0].Executions); got != beforeN+1 {
		t.Fatalf("executions = %d, want %d", got, beforeN+1)
	}

	execs, err := s.Executions(ctx, name, 5, true)
	if err != nil {
		t.Fatalf("Executions: %v", err)
	}
	if execs[0].ID != *out.PipelineExecutionId {
		t.Errorf("head execution = %s, want the one just started (%s)",
			execs[0].ID, *out.PipelineExecutionId)
	}
	if !execs[0].Status.InFlight() {
		t.Errorf("released execution = %s, want in-flight", execs[0].Status)
	}

	env.now = func() time.Time { return time.Now().Add(env.fx.Release.Takes.D() + time.Second) }
	execs, err = s.Executions(ctx, name, 5, true)
	if err != nil {
		t.Fatalf("Executions: %v", err)
	}
	if !execs[0].Status.Terminal() {
		t.Errorf("released execution = %s after release.takes, want terminal", execs[0].Status)
	}
}

func TestAccountSourceMatchesPipelines(t *testing.T) {
	env := load(t)
	accounts, err := env.AccountSource().Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	byID := map[string]bool{}
	for _, a := range accounts {
		if a.Name == "" {
			t.Errorf("account %s has no name; the list would show a bare id", a.ID)
		}
		byID[a.ID] = true
	}
	for _, p := range env.fx.Pipelines {
		if !byID[p.AccountID] {
			t.Errorf("pipeline %s has no matching account entry", p.Name())
		}
	}
	if want := "demo(" + filepath.Base(bundledFixture) + ")"; env.AccountSource().Name() != want {
		t.Errorf("source name = %q, want %q", env.AccountSource().Name(), want)
	}
}

func TestLoadRejectsBadFixture(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}

// failedPipeline returns the name of a pipeline whose newest run failed.
func failedPipeline(t *testing.T, env *Env) string {
	t.Helper()
	for _, p := range env.fx.Pipelines {
		if len(p.Executions) > 0 &&
			model.ParseStatus(p.Executions[0].Status) == model.StatusFailed {
			return p.Name()
		}
	}
	t.Fatal("fixture has no failed pipeline; the triage demo has nothing to show")
	return ""
}
