package cli

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hacker65536/aft-ops/internal/output"
)

func newCacheCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and manage the local cache",
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show cache entries for the current profile/region",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			store := app.CacheStore()
			entries, err := store.Entries()
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			if app.Format == output.FormatJSON {
				return output.JSON(os.Stdout, entries)
			}
			fmt.Fprintln(os.Stderr, "cache dir:", store.Dir())
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tFETCHED\tAGE\tSIZE")
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%dB\n",
					e.Key,
					e.FetchedAt.Local().Format("2006-01-02 15:04:05"),
					time.Since(e.FetchedAt).Round(time.Second),
					e.SizeBytes)
			}
			return tw.Flush()
		},
	}

	clear := &cobra.Command{
		Use:   "clear",
		Short: "Remove all cache entries for the current profile/region",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := app.CacheStore().Clear(); err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			fmt.Fprintln(os.Stderr, "cache cleared")
			return nil
		},
	}

	refresh := &cobra.Command{
		Use:   "refresh",
		Short: "Refetch accounts and pipeline inventory into the cache",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			app.Refresh = true
			if _, err := app.Resolver(ctx); err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			svc, err := app.PipelineService(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			names, _, err := svc.Inventory(ctx, true)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			fmt.Fprintf(os.Stderr, "refreshed: accounts and %d pipelines\n", len(names))
			return nil
		},
	}

	cmd.AddCommand(status, clear, refresh)
	return cmd
}
