package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/model"
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
// fetch function type, never on AWS clients directly. Note: no stderr
// cache notes here — stray writes would corrupt the TUI screen.
func runTUI(ctx context.Context, app *App) error {
	fetch := func(ctx context.Context, refresh bool, onProgress func(batch.Progress)) ([]model.PipelineSummary, error) {
		svc, err := app.PipelineService(ctx)
		if err != nil {
			return nil, err
		}
		names, _, err := svc.Inventory(ctx, refresh)
		if err != nil {
			return nil, err
		}
		src, err := app.AccountSource(ctx)
		if err != nil {
			return nil, err
		}
		resolver, err := account.Load(ctx, src, app.CacheStore(),
			app.Cfg.Cache.AccountTTL.D(), refresh)
		if err != nil {
			return nil, err
		}
		summaries := svc.Statuses(ctx, names, resolver, onProgress)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return summaries, nil
	}
	if err := tui.Run(ctx, fetch); err != nil {
		return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
	}
	return nil
}
