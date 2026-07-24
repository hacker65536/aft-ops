// Package pipeline implements the core services around AFT per-account
// customizations pipelines: inventory, latest-status fan-out, and release
// (StartPipelineExecution). Both the CLI and the TUI call this package —
// guards and instrumentation live here, not in the UIs.
package pipeline

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

const inventoryCacheKey = "pipelines"

// API is the subset of the CodePipeline client used by the read side.
type API interface {
	codepipeline.ListPipelinesAPIClient
	ListPipelineExecutions(ctx context.Context, in *codepipeline.ListPipelineExecutionsInput,
		opts ...func(*codepipeline.Options)) (*codepipeline.ListPipelineExecutionsOutput, error)
}

// StartAPI is the subset used by the write side (may be a client built on
// a different, admin-capable profile).
type StartAPI interface {
	StartPipelineExecution(ctx context.Context, in *codepipeline.StartPipelineExecutionInput,
		opts ...func(*codepipeline.Options)) (*codepipeline.StartPipelineExecutionOutput, error)
}

// Service bundles the dependencies of the pipeline operations.
type Service struct {
	Read        API
	Batch       batch.Config
	Cache       cache.Store
	PipelineTTL time.Duration
}

// Inventory returns the names of all AFT account pipelines, served from
// cache when fresh (existence changes rarely; statuses are never cached).
func (s *Service) Inventory(ctx context.Context, refresh bool) (names []string, cachedAt time.Time, err error) {
	if !refresh {
		if names, at, ok := cache.Get[[]string](s.Cache, inventoryCacheKey, s.PipelineTTL); ok {
			return names, at, nil
		}
	}
	p := codepipeline.NewListPipelinesPaginator(s.Read, &codepipeline.ListPipelinesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("ListPipelines: %w", err)
		}
		for _, pl := range page.Pipelines {
			name := aws.ToString(pl.Name)
			if model.AccountIDFromPipeline(name) != "" {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	if err := cache.Put(s.Cache, inventoryCacheKey, names); err != nil {
		fmt.Fprintln(os.Stderr, "warning: failed to write pipeline cache:", err)
	}
	return names, time.Time{}, nil
}

// Statuses fans out ListPipelineExecutions(max=1) over the given pipelines
// through the batch engine and joins account names in. Per-item fetch
// failures are reported in PipelineSummary.FetchError, never dropped.
func (s *Service) Statuses(
	ctx context.Context,
	names []string,
	resolver *account.Resolver,
	onProgress func(batch.Progress),
) []model.PipelineSummary {
	results := batch.Run(ctx, s.Batch, names,
		func(ctx context.Context, name string) (*model.Execution, error) {
			return s.latestExecution(ctx, name)
		}, onProgress)

	summaries := make([]model.PipelineSummary, len(names))
	for i, res := range results {
		sum := model.PipelineSummary{
			PipelineName: names[i],
			AccountID:    model.AccountIDFromPipeline(names[i]),
		}
		if resolver != nil {
			if a := resolver.ByID(sum.AccountID); a != nil {
				sum.AccountName = a.Name
			}
		}
		if res.Err != nil {
			sum.FetchError = res.Err.Error()
		} else {
			sum.Latest = res.Value
		}
		summaries[i] = sum
	}
	return summaries
}

func (s *Service) latestExecution(ctx context.Context, name string) (*model.Execution, error) {
	out, err := s.Read.ListPipelineExecutions(ctx, &codepipeline.ListPipelineExecutionsInput{
		PipelineName: aws.String(name),
		MaxResults:   aws.Int32(1),
	})
	if err != nil {
		return nil, err
	}
	if len(out.PipelineExecutionSummaries) == 0 {
		return nil, nil // pipeline exists but never ran
	}
	e := out.PipelineExecutionSummaries[0]
	exec := &model.Execution{
		ID:         aws.ToString(e.PipelineExecutionId),
		Status:     model.ParseStatus(string(e.Status)),
		StartTime:  e.StartTime,
		LastUpdate: e.LastUpdateTime,
	}
	return exec, nil
}

// ReleaseRequest is one guarded batch of StartPipelineExecution calls.
type ReleaseRequest struct {
	Targets        []model.PipelineSummary
	SkipInProgress bool
}

// Release triggers the targets through the batch engine. In-progress
// pipelines are skipped when SkipInProgress (re-releasing them supersedes
// the running execution, which is rarely what an operator wants).
func (s *Service) Release(
	ctx context.Context,
	start StartAPI,
	req ReleaseRequest,
	onProgress func(batch.Progress),
) []model.ReleaseResult {
	results := batch.Run(ctx, s.Batch, req.Targets,
		func(ctx context.Context, t model.PipelineSummary) (model.ReleaseResult, error) {
			r := model.ReleaseResult{
				PipelineName: t.PipelineName,
				AccountID:    t.AccountID,
				AccountName:  t.AccountName,
			}
			if req.SkipInProgress && t.Status().InFlight() {
				r.Skipped = true
				r.SkipReason = string(t.Status())
				return r, nil
			}
			out, err := start.StartPipelineExecution(ctx, &codepipeline.StartPipelineExecutionInput{
				Name: aws.String(t.PipelineName),
			})
			if err != nil {
				r.Error = err.Error()
				return r, err
			}
			r.ExecutionID = aws.ToString(out.PipelineExecutionId)
			return r, nil
		}, onProgress)

	out := make([]model.ReleaseResult, len(results))
	for i, res := range results {
		if res.Err != nil && res.Value.PipelineName == "" {
			// batch-level failure (e.g. cancelled before start)
			out[i] = model.ReleaseResult{
				PipelineName: req.Targets[i].PipelineName,
				AccountID:    req.Targets[i].AccountID,
				AccountName:  req.Targets[i].AccountName,
				Error:        res.Err.Error(),
			}
			continue
		}
		out[i] = res.Value
	}
	return out
}
