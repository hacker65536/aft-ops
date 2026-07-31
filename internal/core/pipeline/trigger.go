package pipeline

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	cptypes "github.com/aws/aws-sdk-go-v2/service/codepipeline/types"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

// The trigger cache envelope is read with cache.Forever for the same reason
// the status one is: each entry carries its own FetchedAt.
const triggerCacheKey = "triggers"

// triggerConcurrencyCap bounds the GetPipeline fan-out.
//
// GetPipeline is a heavier call than the rest of the read side, and the
// throttling limit is correspondingly lower: sweeping a 195-pipeline
// management account at 6 concurrent calls left 11 of them failing with
// ThrottlingException (SDK-internal retries included), while 3 concurrent
// calls swept the same account with none. The cap applies on top of whatever
// batch.concurrency is configured, because that value was tuned against
// ListPipelineExecutions and would otherwise quietly break this command.
//
// TriggerOptions.Concurrency lifts it — an operator who passes --concurrency
// is asking for a specific number on purpose.
const triggerConcurrencyCap = 3

// triggerEntry is one pipeline's cached trigger declaration.
//
// Only the observed triggers are cached, never the verdict: the expectation
// comes from the account metadata and the configured policy, both of which
// can change without any pipeline changing. Recomputing the verdict on every
// run means editing the policy or refreshing the accounts re-judges the fleet
// without refetching a single pipeline definition.
type triggerEntry struct {
	Actual    []model.PushTrigger `json:"actual"`
	FetchedAt time.Time           `json:"fetched_at"`
}

// TriggerOptions controls how Triggers uses the trigger cache and what it
// judges against.
type TriggerOptions struct {
	Policy     model.TriggerPolicy
	TTL        time.Duration // serve a cached entry younger than this
	RefreshAll bool          // ignore the cache; refetch every name
	// Prune drops cached entries for pipelines outside names. Like
	// StatusOptions.Prune, set it only when names is the full inventory.
	Prune bool
	// Concurrency overrides triggerConcurrencyCap when > 0. Reserved for an
	// explicitly requested value; leave it zero to get the safe default.
	Concurrency int
}

// Triggers reports, for each pipeline, whether it carries the push trigger
// its account's customizations directory calls for.
//
// The shape mirrors Statuses: per-entry caching so a short TTL works at
// pipeline granularity, per-item fetch failures surfaced on the row rather
// than aborting the sweep, and the accompanying FetchStats counting where
// every row came from.
func (s *Service) Triggers(
	ctx context.Context,
	names []string,
	resolver *account.Resolver,
	opts TriggerOptions,
	onProgress func(batch.Progress),
) ([]model.TriggerSummary, model.FetchStats) {
	cached := map[string]triggerEntry{}
	if !opts.RefreshAll {
		if m, _, ok := cache.Get[map[string]triggerEntry](s.Cache, triggerCacheKey, cache.Forever); ok {
			cached = m
		}
	}

	pruned := 0
	if opts.Prune {
		keep := make(map[string]bool, len(names))
		for _, n := range names {
			keep[n] = true
		}
		for n := range cached {
			if !keep[n] {
				delete(cached, n)
				pruned++
			}
		}
	}

	now := time.Now()
	var toFetch []string
	for _, name := range names {
		e, ok := cached[name]
		if opts.RefreshAll || !ok || now.Sub(e.FetchedAt) > opts.TTL {
			toFetch = append(toFetch, name)
		}
	}

	stats := model.FetchStats{TTL: opts.TTL}
	fetched := map[string]bool{}
	fetchErrs := map[string]string{}
	if len(toFetch) > 0 {
		cfg := s.Batch
		if opts.Concurrency > 0 {
			cfg.Concurrency = opts.Concurrency
		} else if cfg.Concurrency > triggerConcurrencyCap || cfg.Concurrency == 0 {
			cfg.Concurrency = triggerConcurrencyCap
		}
		results := batch.Run(ctx, cfg, toFetch,
			func(ctx context.Context, name string) ([]model.PushTrigger, error) {
				return s.pipelineTriggers(ctx, name)
			}, onProgress)
		fetchedAt := time.Now()
		for i, res := range results {
			name := toFetch[i]
			if res.Err != nil {
				stats.Failed++
				// Keep any prior cached value; only surface an error on a row
				// that has nothing to fall back on.
				if _, ok := cached[name]; !ok {
					fetchErrs[name] = res.Err.Error()
				}
				continue
			}
			cached[name] = triggerEntry{Actual: res.Value, FetchedAt: fetchedAt}
			fetched[name] = true
		}
	}
	if len(fetched) > 0 || pruned > 0 {
		if err := cache.Put(s.Cache, triggerCacheKey, cached); err != nil {
			fmt.Fprintln(os.Stderr, "warning: failed to write trigger cache:", err)
		}
	}

	summaries := make([]model.TriggerSummary, len(names))
	for i, name := range names {
		sum := model.TriggerSummary{
			PipelineName: name,
			AccountID:    model.AccountIDFromPipeline(name),
		}
		var customizations string
		if resolver != nil {
			if a := resolver.ByID(sum.AccountID); a != nil {
				sum.AccountName = a.Name
				customizations = a.CustomizationsName
			}
		}
		if want, ok := opts.Policy.Expect(customizations); ok {
			sum.Expected = &want
		}

		if msg, ok := fetchErrs[name]; ok {
			sum.State = model.TriggerFetchError
			sum.FetchError = msg
			summaries[i] = sum
			continue
		}
		e, ok := cached[name]
		if !ok {
			// No cached value and no error recorded: the sweep was cancelled
			// before this row was reached.
			sum.State = model.TriggerFetchError
			sum.FetchError = "not fetched"
			summaries[i] = sum
			continue
		}
		sum.Actual = e.Actual
		at := e.FetchedAt
		sum.FetchedAt = &at
		sum.State, sum.Reasons = model.ClassifyTrigger(sum.Expected, e.Actual)
		if fetched[name] {
			stats.Fetched++
		} else {
			stats.FromCache++
			if stats.Oldest.IsZero() || at.Before(stats.Oldest) {
				stats.Oldest = at
			}
		}
		summaries[i] = sum
	}
	return summaries, stats
}

// pipelineTriggers reads one pipeline's definition and normalizes its
// triggers.
func (s *Service) pipelineTriggers(ctx context.Context, name string) ([]model.PushTrigger, error) {
	out, err := s.Read.GetPipeline(ctx, &codepipeline.GetPipelineInput{Name: aws.String(name)})
	if err != nil {
		return nil, err
	}
	if out.Pipeline == nil {
		return nil, nil
	}
	triggers := make([]model.PushTrigger, 0, len(out.Pipeline.Triggers))
	for _, t := range out.Pipeline.Triggers {
		triggers = append(triggers, pushTriggerFromDeclaration(t))
	}
	return triggers, nil
}

// pushTriggerFromDeclaration flattens an SDK trigger declaration into the
// domain type (SDK types must not leak past this package). Everything the
// flat shape cannot hold is still counted, so it shows up as drift instead of
// disappearing — see model.PushTrigger.
func pushTriggerFromDeclaration(t cptypes.PipelineTriggerDeclaration) model.PushTrigger {
	pt := model.PushTrigger{ProviderType: string(t.ProviderType)}
	git := t.GitConfiguration
	if git == nil {
		return pt
	}
	pt.SourceAction = aws.ToString(git.SourceActionName)
	pt.PullRequest = len(git.PullRequest) > 0
	if len(git.Push) == 0 {
		return pt
	}
	pt.ExtraPushFilters = len(git.Push) - 1
	push := git.Push[0]
	if b := push.Branches; b != nil {
		pt.Branches = b.Includes
		pt.BranchExcludes = b.Excludes
	}
	if f := push.FilePaths; f != nil {
		pt.FilePaths = f.Includes
		pt.FilePathExcludes = f.Excludes
	}
	if tags := push.Tags; tags != nil {
		pt.Tags = append(append([]string{}, tags.Includes...), tags.Excludes...)
	}
	return pt
}
