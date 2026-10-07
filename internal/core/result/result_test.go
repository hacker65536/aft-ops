package result

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/aws/smithy-go"

	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/logs"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

// fakeActions serves one execution's action runs and counts calls.
type fakeActions struct {
	actions []model.ActionExecution
	calls   int
}

func (f *fakeActions) ActionExecutions(context.Context, string, string, bool) ([]model.ActionExecution, error) {
	f.calls++
	return f.actions, nil
}

// customGroup is where fakeBuilds says a build logs when the test moves it
// off CodeBuild's default location.
const customGroup = "/custom/aft"

// fakeBuilds locates every build it is asked about as complete, logging to
// customGroup under the build id.
type fakeBuilds struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeBuilds) BatchGetBuilds(_ context.Context, in *codebuild.BatchGetBuildsInput,
	_ ...func(*codebuild.Options)) (*codebuild.BatchGetBuildsOutput, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	out := &codebuild.BatchGetBuildsOutput{}
	for _, id := range in.Ids {
		out.Builds = append(out.Builds, cbtypes.Build{
			Id: aws.String(id), BuildComplete: true,
			Logs: &cbtypes.LogsLocation{GroupName: aws.String(customGroup), StreamName: aws.String(id)},
		})
	}
	return out, nil
}

// fakeLogs serves a log body per build id: at CodeBuild's default location,
// or — for the builds listed in moved — only at customGroup. A build listed
// in fail errors instead. The builds of one execution are read concurrently,
// so it locks.
type fakeLogs struct {
	mu     sync.Mutex
	bodies map[string][]string
	fail   map[string]bool
	moved  map[string]bool
	calls  int
}

func (f *fakeLogs) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput,
	_ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	group, stream := aws.ToString(in.LogGroupName), aws.ToString(in.LogStreamName)
	id := stream // customGroup streams are named by build id
	if group != customGroup {
		id = strings.TrimPrefix(group, "/aws/codebuild/") + ":" + stream
		if f.moved[id] {
			return nil, &smithy.GenericAPIError{Code: "ResourceNotFoundException",
				Message: "The specified log stream does not exist."}
		}
	}
	if f.fail[id] {
		return nil, errors.New("AccessDeniedException")
	}
	out := &cloudwatchlogs.GetLogEventsOutput{NextForwardToken: in.NextToken}
	for _, ln := range f.bodies[id] {
		out.Events = append(out.Events, cwltypes.OutputLogEvent{Message: aws.String(ln)})
	}
	return out, nil
}

const (
	globalBuild  = "aft-global-customizations-terraform:1"
	accountBuild = "aft-account-customizations-terraform:2"
)

func actions(global, account model.Status) []model.ActionExecution {
	out := []model.ActionExecution{{StageName: "Source", ActionName: "aft-global-customizations",
		Status: model.StatusSucceeded}}
	if global != "" {
		out = append(out, model.ActionExecution{StageName: "Global-Customizations",
			ActionName: "Apply", Status: global, CodeBuildID: globalBuild})
	}
	if account != "" {
		out = append(out, model.ActionExecution{StageName: "Account-Customizations",
			ActionName: "Apply", Status: account, CodeBuildID: accountBuild})
	}
	return out
}

type harness struct {
	svc  *Service
	acts *fakeActions
	cb   *fakeBuilds
	cwl  *fakeLogs
	dir  string
}

func newHarness(t *testing.T, acts []model.ActionExecution, bodies map[string][]string) *harness {
	t.Helper()
	h := &harness{
		acts: &fakeActions{actions: acts},
		cb:   &fakeBuilds{},
		cwl:  &fakeLogs{bodies: bodies, fail: map[string]bool{}, moved: map[string]bool{}},
		dir:  t.TempDir(),
	}
	h.svc = h.newService()
	return h
}

// newService is a fresh Service over the same fakes and cache directory —
// what the next run of the tool would see.
func (h *harness) newService() *Service {
	return &Service{
		Actions: h.acts,
		Logs:    &logs.Service{CodeBuild: h.cb, Logs: h.cwl},
		Cache:   cache.New(h.dir, "test", "test"),
	}
}

func (h *harness) apiCalls() int { return h.acts.calls + h.cb.calls + h.cwl.calls }

var (
	applied   = []string{"Apply complete! Resources: 1 added, 2 changed, 0 destroyed."}
	noChanges = []string{"No changes. Your infrastructure matches the configuration."}
)

// A finished execution reads both layers' logs straight from CodeBuild's
// default location — no BatchGetBuilds — and the next run of the tool reads
// the same execution from disk with no API call.
func TestForExecutionCachesTerminalExecution(t *testing.T) {
	h := newHarness(t, actions(model.StatusSucceeded, model.StatusSucceeded),
		map[string][]string{globalBuild: noChanges, accountBuild: applied})
	exec := model.Execution{ID: "e1", Status: model.StatusSucceeded}

	got, err := h.svc.ForExecution(context.Background(), "p", exec)
	if err != nil {
		t.Fatalf("ForExecution: %v", err)
	}
	if got.Global.Kind != model.ResultNoChanges {
		t.Errorf("global = %+v, want no_changes", got.Global)
	}
	if a := got.Account; a.Kind != model.ResultApplied || a.Add != 1 || a.Change != 2 {
		t.Errorf("account = %+v, want applied +1 ~2", a)
	}
	if h.cb.calls != 0 || h.cwl.calls != 2 {
		t.Errorf("BatchGetBuilds=%d GetLogEvents=%d, want 0 and 2", h.cb.calls, h.cwl.calls)
	}

	before := h.apiCalls()
	again, err := h.newService().ForExecution(context.Background(), "p", exec)
	if err != nil {
		t.Fatalf("ForExecution (cached): %v", err)
	}
	if h.apiCalls() != before {
		t.Errorf("a cached terminal execution made %d API calls", h.apiCalls()-before)
	}
	if again.Account.Short() != "+1 ~2 -0" {
		t.Errorf("cached account = %q", again.Account.Short())
	}
}

// A running execution: the finished global layer is read, the running
// account build is not (its log is still growing), and nothing about the
// execution is stored.
func TestForExecutionInFlight(t *testing.T) {
	h := newHarness(t, actions(model.StatusSucceeded, model.StatusInProgress),
		map[string][]string{globalBuild: applied})
	exec := model.Execution{ID: "e1", Status: model.StatusInProgress}

	got, err := h.svc.ForExecution(context.Background(), "p", exec)
	if err != nil {
		t.Fatalf("ForExecution: %v", err)
	}
	if got.Global.Kind != model.ResultApplied || got.Account.Kind != model.ResultRunning {
		t.Errorf("got global=%s account=%s, want applied/running", got.Global.Kind, got.Account.Kind)
	}
	if h.cwl.calls != 1 {
		t.Errorf("GetLogEvents calls = %d, want 1 (the running build is not read)", h.cwl.calls)
	}

	// A stage the run has not reached yet is running too, not "not run".
	h.acts.actions = actions(model.StatusInProgress, "")
	got, _ = h.svc.ForExecution(context.Background(), "p", exec)
	if got.Account.Kind != model.ResultRunning {
		t.Errorf("unreached stage of a running execution = %s, want running", got.Account.Kind)
	}
}

// A failed global stage: the account stage never ran, and the global result
// is the terraform error.
func TestForExecutionFailedGlobal(t *testing.T) {
	h := newHarness(t, actions(model.StatusFailed, ""),
		map[string][]string{globalBuild: {"│ Error: creating S3 Bucket: BucketAlreadyExists"}})
	got, err := h.svc.ForExecution(context.Background(), "p",
		model.Execution{ID: "e1", Status: model.StatusFailed})
	if err != nil {
		t.Fatalf("ForExecution: %v", err)
	}
	if got.Global.Short() != "✗ error" || got.Account.Short() != "—" {
		t.Errorf("got %q / %q, want ✗ error / —", got.Global.Short(), got.Account.Short())
	}
}

// A failed action whose log has no terraform error died outside terraform;
// even a clean apply in its log must not read as success.
func TestForActionsFailedOutsideTerraform(t *testing.T) {
	h := newHarness(t, nil, map[string][]string{
		globalBuild:  {"[Container] Phase complete: PRE_BUILD State: FAILED"},
		accountBuild: applied,
	})
	acts := actions(model.StatusFailed, model.StatusFailed)
	got, err := h.svc.ForActions(context.Background(), acts)
	if err != nil {
		t.Fatalf("ForActions: %v", err)
	}
	if g := got[globalBuild]; g.Kind != model.ResultFailed {
		t.Errorf("no-terraform failure = %+v, want failed", g)
	}
	if a := got[accountBuild]; a.Kind != model.ResultFailed || a.Add != 1 {
		t.Errorf("failure after apply = %+v, want failed with the counts kept", a)
	}
}

// An unreadable log is reported, comes back unknown, and is not stored —
// the next attempt reads it again.
func TestForExecutionUnreadableLogIsNotStored(t *testing.T) {
	h := newHarness(t, actions(model.StatusSucceeded, model.StatusSucceeded),
		map[string][]string{globalBuild: applied, accountBuild: applied})
	h.cwl.fail[accountBuild] = true
	exec := model.Execution{ID: "e1", Status: model.StatusSucceeded}

	got, err := h.svc.ForExecution(context.Background(), "p", exec)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("err = %v, want the log fetch error", err)
	}
	if got.Global.Kind != model.ResultApplied || got.Account.Kind != model.ResultUnknown {
		t.Errorf("got global=%s account=%s, want applied/unknown", got.Global.Kind, got.Account.Kind)
	}

	h.cwl.fail[accountBuild] = false
	got, err = h.newService().ForExecution(context.Background(), "p", exec)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got.Account.Kind != model.ResultApplied {
		t.Errorf("retry account = %s, want applied", got.Account.Kind)
	}
}

// Stored results older than MaxAge are dropped when the cache is written.
func TestMaxAgePrunes(t *testing.T) {
	h := newHarness(t, actions(model.StatusSucceeded, model.StatusSucceeded),
		map[string][]string{globalBuild: applied, accountBuild: applied})
	h.svc.MaxAge = 1 // 1ns: entries are pruned on the following write
	ctx := context.Background()
	if _, err := h.svc.ForExecution(ctx, "p", model.Execution{ID: "e1", Status: model.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	h.acts.actions = actions(model.StatusSucceeded, "")
	if _, err := h.svc.ForExecution(ctx, "p", model.Execution{ID: "e2", Status: model.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.newService().cachedExec("e1"); ok {
		t.Error("an expired execution survived the next write")
	}
}

// A project that logs somewhere other than CodeBuild's default is found
// through BatchGetBuilds, once the default location turns out not to exist.
func TestReadBuildFallsBackToLocate(t *testing.T) {
	h := newHarness(t, nil, map[string][]string{accountBuild: applied})
	h.cwl.moved[accountBuild] = true
	got, err := h.svc.ForActions(context.Background(), actions("", model.StatusSucceeded))
	if err != nil {
		t.Fatalf("ForActions: %v", err)
	}
	if r := got[accountBuild]; r.Kind != model.ResultApplied {
		t.Errorf("result = %+v, want applied", r)
	}
	if h.cb.calls != 1 {
		t.Errorf("BatchGetBuilds calls = %d, want 1 (the fallback)", h.cb.calls)
	}
}

// A finished build whose log shows no verdict is reported unknown but not
// stored: it may only be missing lines that had not reached CloudWatch yet.
func TestUnknownIsNotStored(t *testing.T) {
	h := newHarness(t, actions(model.StatusSucceeded, model.StatusSucceeded),
		map[string][]string{globalBuild: applied, accountBuild: {"[Container] still flushing"}})
	exec := model.Execution{ID: "e1", Status: model.StatusSucceeded}
	got, err := h.svc.ForExecution(context.Background(), "p", exec)
	if err != nil {
		t.Fatalf("ForExecution: %v", err)
	}
	if got.Account.Kind != model.ResultUnknown {
		t.Fatalf("account = %s, want unknown", got.Account.Kind)
	}
	if _, ok := h.newService().cachedExec("e1"); ok {
		t.Error("an execution with an unknown layer was stored")
	}
	if _, ok := h.newService().cachedBuild(accountBuild); ok {
		t.Error("an unknown build result was stored")
	}
	if _, ok := h.newService().cachedBuild(globalBuild); !ok {
		t.Error("the readable layer should still be stored")
	}
}
