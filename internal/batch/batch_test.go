package batch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunProcessesAllItemsInOrder(t *testing.T) {
	items := make([]int, 100)
	for i := range items {
		items[i] = i
	}
	results := Run(context.Background(), Config{Concurrency: 8}, items,
		func(_ context.Context, v int) (int, error) { return v * 2, nil }, nil)

	if len(results) != 100 {
		t.Fatalf("got %d results, want 100", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("item %d: unexpected error %v", i, r.Err)
		}
		if r.Value != i*2 {
			t.Fatalf("item %d: got %d, want %d (order must be preserved)", i, r.Value, i*2)
		}
	}
}

func TestRunRespectsConcurrencyLimit(t *testing.T) {
	const limit = 3
	var inFlight, peak atomic.Int64
	var mu sync.Mutex

	Run(context.Background(), Config{Concurrency: limit}, make([]int, 50),
		func(_ context.Context, _ int) (struct{}, error) {
			n := inFlight.Add(1)
			mu.Lock()
			if n > peak.Load() {
				peak.Store(n)
			}
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
			return struct{}{}, nil
		}, nil)

	if p := peak.Load(); p > limit {
		t.Fatalf("peak concurrency %d exceeded limit %d", p, limit)
	}
}

func TestRunReportsPartialFailures(t *testing.T) {
	boom := errors.New("boom")
	results := Run(context.Background(), Config{Concurrency: 4}, []int{0, 1, 2, 3},
		func(_ context.Context, v int) (int, error) {
			if v%2 == 1 {
				return 0, boom
			}
			return v, nil
		}, nil)

	for _, r := range results {
		if r.Index%2 == 1 && !errors.Is(r.Err, boom) {
			t.Fatalf("item %d: expected failure, got %v", r.Index, r.Err)
		}
		if r.Index%2 == 0 && r.Err != nil {
			t.Fatalf("item %d: unexpected error %v", r.Index, r.Err)
		}
	}
}

func TestRunCancellationMarksRemaining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int64

	results := Run(ctx, Config{Concurrency: 1}, make([]int, 20),
		func(ctx context.Context, _ int) (struct{}, error) {
			if started.Add(1) == 3 {
				cancel()
			}
			return struct{}{}, ctx.Err()
		}, nil)

	var cancelled int
	for _, r := range results {
		if errors.Is(r.Err, context.Canceled) {
			cancelled++
		}
	}
	if cancelled == 0 {
		t.Fatal("expected some items to be marked cancelled")
	}
	if got := started.Load(); got >= 20 {
		t.Fatalf("all %d items started despite cancellation", got)
	}
}

func TestRunChunksSequentially(t *testing.T) {
	var order []int
	var mu sync.Mutex

	Run(context.Background(), Config{Concurrency: 10, ChunkSize: 5}, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		func(_ context.Context, v int) (struct{}, error) {
			mu.Lock()
			order = append(order, v)
			mu.Unlock()
			return struct{}{}, nil
		}, nil)

	// every item from chunk 1 (0-4) must complete before any from chunk 2 (5-9)
	firstOfSecond := -1
	for i, v := range order {
		if v >= 5 {
			firstOfSecond = i
			break
		}
	}
	if firstOfSecond >= 0 && firstOfSecond < 5 {
		t.Fatalf("chunk 2 started before chunk 1 finished: order=%v", order)
	}
}

// Even a cancelled run must account for every item: a progress display that
// freezes at 3/20 is indistinguishable from a hung tool.
func TestRunProgressReachesTotalWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var last Progress
	var mu sync.Mutex

	results := Run(ctx, Config{Concurrency: 1}, make([]int, 20),
		func(ctx context.Context, _ int) (struct{}, error) {
			cancel()
			return struct{}{}, ctx.Err()
		},
		func(p Progress) {
			mu.Lock()
			if p.Done > last.Done {
				last = p
			}
			mu.Unlock()
		})

	mu.Lock()
	defer mu.Unlock()
	if last.Done != 20 || last.Total != 20 {
		t.Fatalf("final progress %+v, want Done=Total=20 even after cancellation", last)
	}
	for i, r := range results {
		if r.Err == nil {
			t.Fatalf("item %d has no error; every item should be settled after cancellation", i)
		}
	}
}

func TestRunProgressReachesTotal(t *testing.T) {
	var last Progress
	var mu sync.Mutex
	Run(context.Background(), Config{Concurrency: 4}, make([]int, 30),
		func(_ context.Context, _ int) (struct{}, error) { return struct{}{}, nil },
		func(p Progress) {
			mu.Lock()
			if p.Done > last.Done {
				last = p
			}
			mu.Unlock()
		})
	if last.Done != 30 || last.Total != 30 {
		t.Fatalf("final progress %+v, want Done=Total=30", last)
	}
}
