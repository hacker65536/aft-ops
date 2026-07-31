package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	cptypes "github.com/aws/aws-sdk-go-v2/service/codepipeline/types"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
)

const (
	p1 = "111111111111-customizations-pipeline"
	p2 = "222222222222-customizations-pipeline"
	p3 = "333333333333-customizations-pipeline"
)

// countingAPI records ListPipelineExecutions calls per pipeline and lets each
// pipeline's returned status (or a forced error) be controlled per test.
type countingAPI struct {
	mu       sync.Mutex
	calls    map[string]int
	status   map[string]cptypes.PipelineExecutionStatus
	errNames map[string]bool
}

func newCountingAPI() *countingAPI {
	return &countingAPI{
		calls:    map[string]int{},
		status:   map[string]cptypes.PipelineExecutionStatus{},
		errNames: map[string]bool{},
	}
}

func (c *countingAPI) ListPipelines(context.Context, *codepipeline.ListPipelinesInput,
	...func(*codepipeline.Options)) (*codepipeline.ListPipelinesOutput, error) {
	return &codepipeline.ListPipelinesOutput{}, nil
}

func (c *countingAPI) GetPipelineState(context.Context, *codepipeline.GetPipelineStateInput,
	...func(*codepipeline.Options)) (*codepipeline.GetPipelineStateOutput, error) {
	return &codepipeline.GetPipelineStateOutput{}, nil
}

func (c *countingAPI) ListActionExecutions(context.Context, *codepipeline.ListActionExecutionsInput,
	...func(*codepipeline.Options)) (*codepipeline.ListActionExecutionsOutput, error) {
	return &codepipeline.ListActionExecutionsOutput{}, nil
}

func (c *countingAPI) GetPipeline(context.Context, *codepipeline.GetPipelineInput,
	...func(*codepipeline.Options)) (*codepipeline.GetPipelineOutput, error) {
	return &codepipeline.GetPipelineOutput{}, nil
}

func (c *countingAPI) ListPipelineExecutions(_ context.Context, in *codepipeline.ListPipelineExecutionsInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.ListPipelineExecutionsOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := aws.ToString(in.PipelineName)
	c.calls[name]++
	if c.errNames[name] {
		return nil, errors.New("throttled")
	}
	st := c.status[name]
	if st == "" {
		st = cptypes.PipelineExecutionStatusSucceeded
	}
	return &codepipeline.ListPipelineExecutionsOutput{
		PipelineExecutionSummaries: []cptypes.PipelineExecutionSummary{
			{PipelineExecutionId: aws.String("exec-" + name), Status: st},
		},
	}, nil
}

func (c *countingAPI) callCount(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[name]
}

func newStatusService(api *countingAPI, t *testing.T) *Service {
	return &Service{
		Read:  api,
		Batch: batch.Config{Concurrency: 4},
		Cache: cache.New(t.TempDir(), "", ""),
	}
}

func TestStatusesServesFreshFromCache(t *testing.T) {
	api := newCountingAPI()
	svc := newStatusService(api, t)
	opts := StatusOptions{TTL: time.Hour, RefreshInFlight: true}

	sums, _ := svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)
	if api.callCount(p1) != 1 || api.callCount(p2) != 1 {
		t.Fatalf("first call should fetch both; got p1=%d p2=%d", api.callCount(p1), api.callCount(p2))
	}
	if sums[0].Latest == nil || sums[0].StatusFetchedAt == nil {
		t.Fatal("summary missing Latest/StatusFetchedAt after fetch")
	}

	// Within the TTL the second call must hit the API zero more times.
	svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)
	if api.callCount(p1) != 1 || api.callCount(p2) != 1 {
		t.Fatalf("fresh cache should serve without refetch; got p1=%d p2=%d", api.callCount(p1), api.callCount(p2))
	}
}

func TestStatusesRefetchesPastTTL(t *testing.T) {
	api := newCountingAPI()
	svc := newStatusService(api, t)
	// TTL=0 makes every cached entry immediately stale.
	opts := StatusOptions{TTL: 0}

	svc.Statuses(context.Background(), []string{p1}, nil, opts, nil)
	svc.Statuses(context.Background(), []string{p1}, nil, opts, nil)
	if api.callCount(p1) != 2 {
		t.Fatalf("stale entry should refetch each call; got %d", api.callCount(p1))
	}
}

func TestStatusesRefreshOnly(t *testing.T) {
	api := newCountingAPI()
	svc := newStatusService(api, t)
	opts := StatusOptions{TTL: time.Hour}

	svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)

	only := StatusOptions{TTL: time.Hour, RefreshOnly: map[string]bool{p1: true}}
	svc.Statuses(context.Background(), []string{p1, p2}, nil, only, nil)
	if api.callCount(p1) != 2 {
		t.Errorf("p1 should be force-refetched; got %d", api.callCount(p1))
	}
	if api.callCount(p2) != 1 {
		t.Errorf("p2 should stay cached; got %d", api.callCount(p2))
	}
}

func TestStatusesAlwaysRefetchesInFlight(t *testing.T) {
	api := newCountingAPI()
	api.status[p1] = cptypes.PipelineExecutionStatusInProgress
	api.status[p2] = cptypes.PipelineExecutionStatusSucceeded
	svc := newStatusService(api, t)
	opts := StatusOptions{TTL: time.Hour, RefreshInFlight: true}

	svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)
	svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)
	if api.callCount(p1) != 2 {
		t.Errorf("in-flight p1 should refetch despite fresh TTL; got %d", api.callCount(p1))
	}
	if api.callCount(p2) != 1 {
		t.Errorf("terminal p2 should stay cached; got %d", api.callCount(p2))
	}
}

func TestStatusesErrorRetainsStaleValue(t *testing.T) {
	api := newCountingAPI()
	svc := newStatusService(api, t)

	// Seed a good cached value for p1.
	svc.Statuses(context.Background(), []string{p1}, nil, StatusOptions{TTL: time.Hour}, nil)

	// Now p1 errors; force a refetch. The stale-but-known value must survive
	// and no FetchError is surfaced.
	api.errNames[p1] = true
	sums, _ := svc.Statuses(context.Background(), []string{p1}, nil,
		StatusOptions{TTL: time.Hour, RefreshOnly: map[string]bool{p1: true}}, nil)
	if sums[0].Latest == nil {
		t.Error("stale cached value should be retained on fetch error")
	}
	if sums[0].FetchError != "" {
		t.Errorf("no FetchError expected when a cached value stands in; got %q", sums[0].FetchError)
	}

	// A name that was never cached and errors must surface FetchError.
	api.errNames[p3] = true
	sums, _ = svc.Statuses(context.Background(), []string{p3}, nil, StatusOptions{TTL: time.Hour}, nil)
	if sums[0].FetchError == "" {
		t.Error("uncached fetch error should surface FetchError")
	}
}

// TestStatusesSubsetRefreshPreservesOthers guards the `pl refresh <one>`
// path: refetching a subset with RefreshOnly must merge into — not clobber —
// the cache entries for pipelines outside the subset.
func TestStatusesSubsetRefreshPreservesOthers(t *testing.T) {
	api := newCountingAPI()
	svc := newStatusService(api, t)

	// Seed both p1 and p2.
	svc.Statuses(context.Background(), []string{p1, p2}, nil, StatusOptions{TTL: time.Hour}, nil)

	// Refresh only p1, passing only p1 as names (as `pl refresh p1` does).
	svc.Statuses(context.Background(), []string{p1}, nil,
		StatusOptions{RefreshOnly: map[string]bool{p1: true}}, nil)
	if api.callCount(p1) != 2 {
		t.Errorf("p1 should be refetched; got %d", api.callCount(p1))
	}

	// p2 must still be cached: a later fresh list serves it without refetch.
	svc.Statuses(context.Background(), []string{p1, p2}, nil, StatusOptions{TTL: time.Hour}, nil)
	if api.callCount(p2) != 1 {
		t.Errorf("p2 cache entry was clobbered by the subset refresh; got %d calls", api.callCount(p2))
	}
}

// The stats are what the UI prints instead of guessing from timestamps, so
// they have to match what actually happened: fetched vs served, plus the
// refreshes that failed behind a retained value.
func TestStatusesReportStats(t *testing.T) {
	api := newCountingAPI()
	svc := newStatusService(api, t)
	opts := StatusOptions{TTL: time.Hour}

	_, stats := svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)
	if stats.Fetched != 2 || stats.FromCache != 0 || stats.Failed != 0 {
		t.Errorf("cold call stats = %+v, want 2 fetched", stats)
	}

	_, stats = svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)
	if stats.Fetched != 0 || stats.FromCache != 2 {
		t.Errorf("warm call stats = %+v, want 2 from cache", stats)
	}
	if stats.Oldest.IsZero() {
		t.Error("cached stats should carry the oldest fetch time")
	}

	// A failed refresh keeps serving the previous value, but must still be
	// counted — otherwise the failure is invisible.
	api.errNames[p1] = true
	_, stats = svc.Statuses(context.Background(), []string{p1, p2}, nil,
		StatusOptions{TTL: time.Hour, RefreshOnly: map[string]bool{p1: true}}, nil)
	if stats.Failed != 1 {
		t.Errorf("stats = %+v, want Failed=1", stats)
	}
}

// A full-inventory pass drops cached entries for pipelines that no longer
// exist; a subset refresh must never do that (it would wipe the cache).
func TestStatusesPruneOnlyOnFullPass(t *testing.T) {
	api := newCountingAPI()
	svc := newStatusService(api, t)
	opts := StatusOptions{TTL: time.Hour}

	svc.Statuses(context.Background(), []string{p1, p2, p3}, nil, opts, nil)

	// p3 is gone from the inventory: a full pass prunes it.
	svc.Statuses(context.Background(), []string{p1, p2}, nil,
		StatusOptions{TTL: time.Hour, Prune: true}, nil)

	cached, _, ok := cache.Get[map[string]StatusEntry](svc.Cache, statusCacheKey, cache.Forever)
	if !ok {
		t.Fatal("status cache missing after prune")
	}
	if _, stale := cached[p3]; stale {
		t.Error("a full pass should prune pipelines that are no longer in the inventory")
	}
	if len(cached) != 2 {
		t.Errorf("cache has %d entries, want p1 and p2", len(cached))
	}

	// A targeted refresh of p1 alone must leave p2 in place.
	svc.Statuses(context.Background(), []string{p1}, nil,
		StatusOptions{RefreshOnly: map[string]bool{p1: true}}, nil)
	cached, _, _ = cache.Get[map[string]StatusEntry](svc.Cache, statusCacheKey, cache.Forever)
	if _, ok := cached[p2]; !ok {
		t.Error("a subset refresh must not prune the pipelines it did not cover")
	}
}

// After a release the memoized history for those pipelines has to go: the
// new run postdates it, and the memo TTL would otherwise hide it.
func TestInvalidateExecutionsDropsMemo(t *testing.T) {
	api := newCountingAPI()
	svc := newStatusService(api, t)
	svc.ExecutionsTTL = time.Hour
	ctx := context.Background()

	if _, err := svc.Executions(ctx, p1, 5, false); err != nil {
		t.Fatalf("Executions: %v", err)
	}
	if _, err := svc.Executions(ctx, p1, 5, false); err != nil {
		t.Fatalf("Executions: %v", err)
	}
	if api.callCount(p1) != 1 {
		t.Fatalf("second call should hit the memo; got %d calls", api.callCount(p1))
	}

	svc.InvalidateExecutions([]string{p1})
	if _, err := svc.Executions(ctx, p1, 5, false); err != nil {
		t.Fatalf("Executions: %v", err)
	}
	if api.callCount(p1) != 2 {
		t.Errorf("invalidated history should refetch; got %d calls", api.callCount(p1))
	}
}

func TestInvalidateStatusesForcesRefetch(t *testing.T) {
	api := newCountingAPI()
	svc := newStatusService(api, t)
	opts := StatusOptions{TTL: time.Hour}

	svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)
	if err := svc.InvalidateStatuses([]string{p1}); err != nil {
		t.Fatalf("InvalidateStatuses: %v", err)
	}
	svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)
	if api.callCount(p1) != 2 {
		t.Errorf("invalidated p1 should refetch; got %d", api.callCount(p1))
	}
	if api.callCount(p2) != 1 {
		t.Errorf("p2 should stay cached; got %d", api.callCount(p2))
	}
}
