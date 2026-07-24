package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/model"
	"github.com/hacker65536/aft-ops/internal/core/pipeline"
	"github.com/hacker65536/aft-ops/internal/tui"
)

func newTUICmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Launch the interactive TUI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTUI(cmd.Context(), app)
		},
	}
}

// runTUI wires core services into the TUI. The TUI depends only on the
// fetch/refresh function types, never on AWS clients directly. Note: no
// stderr cache notes here — stray writes would corrupt the TUI screen.
func runTUI(ctx context.Context, app *App) error {
	// loadResolver is shared by both closures; accounts come from cache
	// (refresh=false) so a per-row refresh stays cheap.
	loadResolver := func(ctx context.Context, refresh bool) (*account.Resolver, error) {
		src, err := app.AccountSource(ctx)
		if err != nil {
			return nil, err
		}
		return account.Load(ctx, src, app.CacheStore(), app.Cfg.Cache.AccountTTL.D(), refresh)
	}

	fetch := func(ctx context.Context, refresh bool, onProgress func(batch.Progress)) ([]model.PipelineSummary, error) {
		svc, err := app.PipelineService(ctx)
		if err != nil {
			return nil, err
		}
		names, _, err := svc.Inventory(ctx, refresh)
		if err != nil {
			return nil, err
		}
		resolver, err := loadResolver(ctx, refresh)
		if err != nil {
			return nil, err
		}
		opts := pipeline.StatusOptions{
			TTL:             app.Cfg.Cache.StatusTTL.D(),
			RefreshAll:      refresh,
			RefreshInFlight: true,
		}
		summaries := svc.Statuses(ctx, names, resolver, opts, onProgress)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return summaries, nil
	}

	// refresh force-refetches just the named pipelines (RefreshOnly), merging
	// them back into the full status cache — the selected-row refresh.
	refresh := func(ctx context.Context, names []string, onProgress func(batch.Progress)) ([]model.PipelineSummary, error) {
		svc, err := app.PipelineService(ctx)
		if err != nil {
			return nil, err
		}
		resolver, err := loadResolver(ctx, false)
		if err != nil {
			return nil, err
		}
		only := make(map[string]bool, len(names))
		for _, n := range names {
			only[n] = true
		}
		summaries := svc.Statuses(ctx, names, resolver,
			pipeline.StatusOptions{RefreshOnly: only}, onProgress)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return summaries, nil
	}

	// detail resolves one pipeline's current stage/action state — the v
	// shortcut's single-call path to the most relevant build id (no history;
	// the executions screen owns history now).
	detail := func(ctx context.Context, name string) (*model.PipelineDetail, error) {
		svc, err := app.PipelineService(ctx)
		if err != nil {
			return nil, err
		}
		return svc.Detail(ctx, name, 0, nil)
	}

	// executions loads one pipeline's recent runs for the executions screen
	// (session-memoized within cache.executions_ttl; r forces a refetch).
	executions := func(ctx context.Context, name string, refresh bool) ([]model.Execution, error) {
		svc, err := app.PipelineService(ctx)
		if err != nil {
			return nil, err
		}
		return svc.Executions(ctx, name, 25, refresh)
	}

	// actions loads the per-action run details of one execution for the
	// actions screen (chronological order, CodeBuild ids included; terminal
	// executions' immutable action lists are session-memoized).
	actions := func(ctx context.Context, name, execID string, done bool) ([]model.ActionExecution, error) {
		svc, err := app.PipelineService(ctx)
		if err != nil {
			return nil, err
		}
		return svc.ActionExecutions(ctx, name, execID, done)
	}

	// logsFn fetches a CodeBuild build's raw log lines for the log screen —
	// the same core path as `pipeline logs`. The screen applies the
	// terraform/raw/summary rendering locally.
	logsFn := func(ctx context.Context, buildID string) ([]string, error) {
		svc, err := app.LogsService(ctx)
		if err != nil {
			return nil, err
		}
		bl, err := svc.Fetch(ctx, buildID)
		if err != nil {
			return nil, err
		}
		return bl.Lines, nil
	}

	// release triggers Release change on the selected targets — the same core
	// path and guard as `pipeline release`. In-progress skipping comes from
	// config. Cache invalidation is best-effort (the screen refreshes the
	// started rows afterward regardless); stderr is off-limits in the TUI.
	release := func(ctx context.Context, targets []model.PipelineSummary, onProgress func(batch.Progress)) ([]model.ReleaseResult, error) {
		svc, err := app.PipelineService(ctx)
		if err != nil {
			return nil, err
		}
		start, err := app.StartClient(ctx)
		if err != nil {
			return nil, err
		}
		results := svc.Release(ctx, start, pipeline.ReleaseRequest{
			Targets:        targets,
			SkipInProgress: app.Cfg.Release.SkipInProgress,
		}, onProgress)

		var started []string
		for _, r := range results {
			if r.ExecutionID != "" && r.Error == "" {
				started = append(started, r.PipelineName)
			}
		}
		_ = svc.InvalidateStatuses(started)
		if err := ctx.Err(); err != nil {
			return results, err
		}
		return results, nil
	}

	deps := tui.Deps{
		Fetch:        fetch,
		Refresh:      refresh,
		Detail:       detail,
		Executions:   executions,
		Actions:      actions,
		Logs:         logsFn,
		Release:      release,
		ReleaseLimit: app.Cfg.Release.MaxTargets,
	}
	if err := tui.Run(ctx, deps); err != nil {
		return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
	}
	return nil
}
