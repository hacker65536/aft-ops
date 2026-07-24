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

	sums := svc.Statuses(context.Background(), []string{p1, p2}, nil, opts, nil)
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
	sums := svc.Statuses(context.Background(), []string{p1}, nil,
		StatusOptions{TTL: time.Hour, RefreshOnly: map[string]bool{p1: true}}, nil)
	if sums[0].Latest == nil {
		t.Error("stale cached value should be retained on fetch error")
	}
	if sums[0].FetchError != "" {
		t.Errorf("no FetchError expected when a cached value stands in; got %q", sums[0].FetchError)
	}

	// A name that was never cached and errors must surface FetchError.
	api.errNames[p3] = true
	sums = svc.Statuses(context.Background(), []string{p3}, nil, StatusOptions{TTL: time.Hour}, nil)
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
