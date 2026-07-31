package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	cptypes "github.com/aws/aws-sdk-go-v2/service/codepipeline/types"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

var triggerPolicy = model.TriggerPolicy{
	SourceAction:     "aft-account-customizations",
	Branch:           "main",
	FilePathTemplate: "{customizations_name}/terraform/*.tf",
}

// triggerAPI serves pipeline definitions and records GetPipeline calls, so a
// test can assert both the verdict and how many requests produced it.
type triggerAPI struct {
	// Embedded by pointer: countingAPI carries a mutex, and it supplies the
	// rest of the read-side API this test does not exercise.
	*countingAPI

	tmu      sync.Mutex
	getCalls map[string]int
	// filePath overrides the file path a pipeline's trigger filters on;
	// noTrigger makes it declare none; errNames makes GetPipeline fail.
	filePath  map[string]string
	noTrigger map[string]bool
	getErrs   map[string]bool
	// maxParallel records the highest number of concurrent GetPipeline calls.
	inFlight, maxParallel int
}

func newTriggerAPI() *triggerAPI {
	return &triggerAPI{
		countingAPI: newCountingAPI(),
		getCalls:    map[string]int{},
		filePath:    map[string]string{},
		noTrigger:   map[string]bool{},
		getErrs:     map[string]bool{},
	}
}

func (a *triggerAPI) GetPipeline(_ context.Context, in *codepipeline.GetPipelineInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.GetPipelineOutput, error) {
	name := aws.ToString(in.Name)

	a.tmu.Lock()
	a.getCalls[name]++
	a.inFlight++
	if a.inFlight > a.maxParallel {
		a.maxParallel = a.inFlight
	}
	fail, none := a.getErrs[name], a.noTrigger[name]
	path, ok := a.filePath[name]
	a.tmu.Unlock()

	// Hold the slot long enough that overlapping calls really overlap.
	time.Sleep(2 * time.Millisecond)
	defer func() {
		a.tmu.Lock()
		a.inFlight--
		a.tmu.Unlock()
	}()

	if fail {
		return nil, errors.New("ThrottlingException: Rate exceeded")
	}
	decl := &cptypes.PipelineDeclaration{Name: aws.String(name)}
	if !none {
		if !ok {
			path = "acct/terraform/*.tf"
		}
		decl.Triggers = []cptypes.PipelineTriggerDeclaration{{
			ProviderType: cptypes.PipelineTriggerProviderType(model.TriggerProviderType),
			GitConfiguration: &cptypes.GitConfiguration{
				SourceActionName: aws.String("aft-account-customizations"),
				Push: []cptypes.GitPushFilter{{
					Branches:  &cptypes.GitBranchFilterCriteria{Includes: []string{"main"}},
					FilePaths: &cptypes.GitFilePathFilterCriteria{Includes: []string{path}},
				}},
			},
		}}
	}
	return &codepipeline.GetPipelineOutput{Pipeline: decl}, nil
}

func (a *triggerAPI) getCallCount(name string) int {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	return a.getCalls[name]
}

func newTriggerService(api *triggerAPI, t *testing.T, cfg batch.Config) *Service {
	return &Service{Read: api, Batch: cfg, Cache: cache.New(t.TempDir(), "", "")}
}

// staticAccounts is an account.Source over a fixed list.
type staticAccounts struct{ accounts []model.Account }

func (s staticAccounts) Name() string { return "test" }

func (s staticAccounts) Fetch(context.Context) ([]model.Account, error) { return s.accounts, nil }

// triggerResolver builds a resolver whose accounts carry customizations
// names, keyed by account id. An empty name stands for an account AFT
// recorded without one, which is what makes its pipeline unjudgeable.
func triggerResolver(t *testing.T, names map[string]string) *account.Resolver {
	t.Helper()
	accounts := make([]model.Account, 0, len(names))
	for id, name := range names {
		accounts = append(accounts, model.Account{ID: id, Name: name, CustomizationsName: name})
	}
	r, err := account.Load(context.Background(), staticAccounts{accounts},
		cache.New(t.TempDir(), "", ""), time.Hour, true)
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	return r
}

func TestTriggersClassifiesEachState(t *testing.T) {
	api := newTriggerAPI()
	api.noTrigger[p2] = true
	api.filePath[p1] = "alpha/terraform/*.tf"
	api.filePath[p3] = "wrong/terraform/*.tf"
	svc := newTriggerService(api, t, batch.Config{Concurrency: 2})

	resolver := triggerResolver(t, map[string]string{
		"111111111111": "alpha",
		"222222222222": "bravo",
		"333333333333": "charlie",
		// 444444444444 exists but has no customizations name.
		"444444444444": "",
	})
	p4 := "444444444444-customizations-pipeline"

	sums, stats := svc.Triggers(context.Background(), []string{p1, p2, p3, p4},
		resolver, TriggerOptions{Policy: triggerPolicy, TTL: time.Hour}, nil)

	want := map[string]model.TriggerState{
		p1: model.TriggerOK,
		p2: model.TriggerMissing,
		p3: model.TriggerDrift,
		p4: model.TriggerUnknown,
	}
	for _, s := range sums {
		if s.State != want[s.PipelineName] {
			t.Errorf("%s: state = %q, want %q (reasons %v)",
				s.PipelineName, s.State, want[s.PipelineName], s.Reasons)
		}
	}
	if stats.Fetched != 4 {
		t.Errorf("fetched = %d, want 4", stats.Fetched)
	}
	// An unjudgeable pipeline still had its definition read, so the report can
	// show what it does have.
	if len(sums[3].Actual) == 0 || sums[3].Expected != nil {
		t.Errorf("unknown row = %+v, want actual set and no expectation", sums[3])
	}
}

func TestTriggersServesFreshFromCache(t *testing.T) {
	api := newTriggerAPI()
	svc := newTriggerService(api, t, batch.Config{Concurrency: 2})
	opts := TriggerOptions{Policy: triggerPolicy, TTL: time.Hour}

	svc.Triggers(context.Background(), []string{p1}, nil, opts, nil)
	_, stats := svc.Triggers(context.Background(), []string{p1}, nil, opts, nil)
	if api.getCallCount(p1) != 1 {
		t.Fatalf("fresh cache should serve without refetch; got %d calls", api.getCallCount(p1))
	}
	if stats.FromCache != 1 || stats.Fetched != 0 {
		t.Errorf("stats = %+v, want 1 from cache", stats)
	}

	// --refresh ignores the cache entirely.
	refresh := opts
	refresh.RefreshAll = true
	svc.Triggers(context.Background(), []string{p1}, nil, refresh, nil)
	if api.getCallCount(p1) != 2 {
		t.Fatalf("RefreshAll should refetch; got %d calls", api.getCallCount(p1))
	}
}

// The verdict is recomputed every run even when the definition comes from
// cache, so an account rename is reflected without refetching a pipeline.
func TestTriggersRejudgesCachedDefinitions(t *testing.T) {
	api := newTriggerAPI()
	api.filePath[p1] = "alpha/terraform/*.tf"
	svc := newTriggerService(api, t, batch.Config{Concurrency: 1})
	opts := TriggerOptions{Policy: triggerPolicy, TTL: time.Hour}

	r1 := triggerResolver(t, map[string]string{"111111111111": "alpha"})
	sums, _ := svc.Triggers(context.Background(), []string{p1}, r1, opts, nil)
	if sums[0].State != model.TriggerOK {
		t.Fatalf("first pass state = %q, want ok", sums[0].State)
	}

	r2 := triggerResolver(t, map[string]string{"111111111111": "renamed"})
	sums, stats := svc.Triggers(context.Background(), []string{p1}, r2, opts, nil)
	if sums[0].State != model.TriggerDrift {
		t.Errorf("after rename state = %q, want drift", sums[0].State)
	}
	if stats.FromCache != 1 || api.getCallCount(p1) != 1 {
		t.Errorf("re-judging must not refetch; calls=%d stats=%+v", api.getCallCount(p1), stats)
	}
}

// A failed fetch is reported on its own row and counted; the rest of the
// sweep still lands.
func TestTriggersReportsPerItemFailure(t *testing.T) {
	api := newTriggerAPI()
	api.getErrs[p2] = true
	svc := newTriggerService(api, t, batch.Config{Concurrency: 2})

	sums, stats := svc.Triggers(context.Background(), []string{p1, p2}, nil,
		TriggerOptions{Policy: triggerPolicy, TTL: time.Hour}, nil)
	if sums[1].State != model.TriggerFetchError || sums[1].FetchError == "" {
		t.Errorf("failed row = %+v, want fetch-error with a message", sums[1])
	}
	if stats.Failed != 1 {
		t.Errorf("failed = %d, want 1", stats.Failed)
	}
	if sums[0].State == model.TriggerFetchError {
		t.Error("one failure must not poison the other rows")
	}
}

// Prune drops pipelines that no longer exist, so a stale name cannot linger
// in the cache forever.
func TestTriggersPrunesRemovedPipelines(t *testing.T) {
	api := newTriggerAPI()
	svc := newTriggerService(api, t, batch.Config{Concurrency: 2})
	opts := TriggerOptions{Policy: triggerPolicy, TTL: time.Hour, Prune: true}

	svc.Triggers(context.Background(), []string{p1, p2}, nil, opts, nil)
	svc.Triggers(context.Background(), []string{p1}, nil, opts, nil)
	// p2 is gone from the cache, so listing it again has to fetch it.
	svc.Triggers(context.Background(), []string{p1, p2}, nil, opts, nil)
	if api.getCallCount(p2) != 2 {
		t.Errorf("pruned entry should refetch; got %d calls", api.getCallCount(p2))
	}
	if api.getCallCount(p1) != 1 {
		t.Errorf("kept entry should stay cached; got %d calls", api.getCallCount(p1))
	}
}

// GetPipeline throttles at a lower concurrency than the rest of the read
// side, so the configured batch concurrency is capped here regardless.
func TestTriggersCapsConcurrency(t *testing.T) {
	names := make([]string, 0, 12)
	for i := range 12 {
		names = append(names, fmt.Sprintf("%012d-customizations-pipeline", i+1))
	}

	api := newTriggerAPI()
	svc := newTriggerService(api, t, batch.Config{Concurrency: 10})
	svc.Triggers(context.Background(), names, nil,
		TriggerOptions{Policy: triggerPolicy, TTL: time.Hour}, nil)
	if api.maxParallel > triggerConcurrencyCap {
		t.Errorf("ran %d calls in parallel, cap is %d", api.maxParallel, triggerConcurrencyCap)
	}

	// An explicitly requested number lifts the cap.
	api2 := newTriggerAPI()
	svc2 := newTriggerService(api2, t, batch.Config{Concurrency: 10})
	svc2.Triggers(context.Background(), names, nil,
		TriggerOptions{Policy: triggerPolicy, TTL: time.Hour, Concurrency: 8}, nil)
	if api2.maxParallel <= triggerConcurrencyCap {
		t.Errorf("explicit concurrency was ignored: maxParallel = %d", api2.maxParallel)
	}
}

func TestPushTriggerFromDeclaration(t *testing.T) {
	got := pushTriggerFromDeclaration(cptypes.PipelineTriggerDeclaration{
		ProviderType: cptypes.PipelineTriggerProviderType(model.TriggerProviderType),
		GitConfiguration: &cptypes.GitConfiguration{
			SourceActionName: aws.String("aft-account-customizations"),
			PullRequest:      []cptypes.GitPullRequestFilter{{}},
			Push: []cptypes.GitPushFilter{
				{
					Branches: &cptypes.GitBranchFilterCriteria{
						Includes: []string{"main"}, Excludes: []string{"tmp/*"},
					},
					FilePaths: &cptypes.GitFilePathFilterCriteria{Includes: []string{"a/terraform/*.tf"}},
					Tags:      &cptypes.GitTagFilterCriteria{Includes: []string{"v*"}},
				},
				{}, // a second push filter the flat shape cannot hold
			},
		},
	})
	if got.SourceAction != "aft-account-customizations" || !got.PullRequest {
		t.Errorf("normalized = %+v", got)
	}
	// Everything the flat shape drops must still be recorded, or it would
	// silently pass as matching.
	if got.ExtraPushFilters != 1 || len(got.BranchExcludes) != 1 || len(got.Tags) != 1 {
		t.Errorf("dropped filters not recorded: %+v", got)
	}
}
