// Package batch is the sequential-batch engine: items are processed in
// sequential chunks, with bounded concurrency inside a chunk and a shared
// requests-per-second token bucket across all workers. Concurrency
// (simultaneous in-flight calls) and RPS (call admission rate) are
// controlled independently — see docs/design.md §5.
package batch

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Config controls one batch run. Zero values fall back to safe defaults.
type Config struct {
	Concurrency int           // workers per chunk (default 10)
	RPS         float64       // token-bucket admission rate; 0 = unlimited
	ChunkSize   int           // 0 = single chunk (pure streaming)
	ChunkPause  time.Duration // pause between chunks
}

func (c Config) withDefaults() Config {
	if c.Concurrency < 1 {
		c.Concurrency = 10
	}
	return c
}

// Result is the per-item outcome. Err is never swallowed: callers decide
// how to surface partial failures.
type Result[R any] struct {
	Index int
	Value R
	Err   error
}

// Progress is a snapshot emitted after every completed item.
type Progress struct {
	Done   int
	Failed int
	Total  int
}

// Run processes all items and returns per-item results in input order.
// onProgress (optional) is called after each item completes; it must be
// fast and is invoked from worker goroutines (serialized internally).
// On context cancellation, unstarted items get ctx.Err() as their Err and
// in-flight items run to completion.
func Run[T, R any](
	ctx context.Context,
	cfg Config,
	items []T,
	fn func(context.Context, T) (R, error),
	onProgress func(Progress),
) []Result[R] {
	cfg = cfg.withDefaults()
	results := make([]Result[R], len(items))
	for i := range results {
		results[i].Index = i
	}

	var limiter *rate.Limiter
	if cfg.RPS > 0 {
		limiter = rate.NewLimiter(rate.Limit(cfg.RPS), 1)
	}

	var done, failed atomic.Int64
	var progressMu sync.Mutex
	notify := func() {
		if onProgress == nil {
			return
		}
		progressMu.Lock()
		defer progressMu.Unlock()
		onProgress(Progress{
			Done:   int(done.Load()),
			Failed: int(failed.Load()),
			Total:  len(items),
		})
	}

	chunks := chunkIndexes(len(items), cfg.ChunkSize)
	for ci, chunk := range chunks {
		if ctx.Err() != nil {
			markCancelled(ctx, results, chunk[0], &done, &failed, notify)
			return results
		}
		runChunk(ctx, cfg, limiter, items, results, chunk, fn, &done, &failed, notify)

		last := ci == len(chunks)-1
		if !last && cfg.ChunkPause > 0 {
			select {
			case <-ctx.Done():
				markCancelled(ctx, results, chunks[ci+1][0], &done, &failed, notify)
				return results
			case <-time.After(cfg.ChunkPause):
			}
		}
	}
	return results
}

func runChunk[T, R any](
	ctx context.Context,
	cfg Config,
	limiter *rate.Limiter,
	items []T,
	results []Result[R],
	chunk []int,
	fn func(context.Context, T) (R, error),
	done, failed *atomic.Int64,
	notify func(),
) {
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	// Every item accounts for itself exactly once — including the ones that
	// never run. A progress display that stops short of Total looks hung;
	// a cancelled batch should visibly finish instead.
	settle := func(i int, err error) {
		results[i].Err = err
		done.Add(1)
		if err != nil {
			failed.Add(1)
		}
		notify()
	}
	for _, idx := range chunk {
		if ctx.Err() != nil {
			settle(idx, ctx.Err())
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			if limiter != nil {
				if err := limiter.Wait(ctx); err != nil {
					settle(i, err)
					return
				}
			}
			v, err := fn(ctx, items[i])
			results[i].Value = v
			settle(i, err)
		}(idx)
	}
	wg.Wait()
}

// markCancelled settles every not-yet-run item so the caller sees a complete
// result set (and a progress count that reaches Total) after a cancellation.
func markCancelled[R any](ctx context.Context, results []Result[R], from int,
	done, failed *atomic.Int64, notify func()) {
	for i := from; i < len(results); i++ {
		if results[i].Err == nil {
			results[i].Err = ctx.Err()
			done.Add(1)
			failed.Add(1)
		}
	}
	notify()
}

// chunkIndexes splits [0,n) into chunks of size (0 = one chunk).
func chunkIndexes(n, size int) [][]int {
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	if n == 0 {
		return nil
	}
	if size <= 0 || size >= n {
		return [][]int{all}
	}
	var chunks [][]int
	for start := 0; start < n; start += size {
		end := min(start+size, n)
		chunks = append(chunks, all[start:end])
	}
	return chunks
}
