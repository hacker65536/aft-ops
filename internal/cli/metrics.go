package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hacker65536/aft-ops/internal/metrics"
	"github.com/hacker65536/aft-ops/internal/output"
)

func newMetricsCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Analyze recorded AWS API call metrics",
	}

	var last int
	show := &cobra.Command{
		Use:   "show",
		Short: "Aggregate call counts, latency, and throttle rates per operation",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			files, err := metrics.LatestFiles(app.Cfg.Metrics.Dir, last)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			if len(files) == 0 {
				fmt.Fprintln(os.Stderr, "no metrics recorded yet in", app.Cfg.Metrics.Dir)
				return nil
			}
			var entries []metrics.Entry
			for _, f := range files {
				es, err := metrics.ReadFile(f)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: skip %s: %v\n", f, err)
					continue
				}
				entries = append(entries, es...)
			}
			stats := metrics.Summarize(entries)

			if app.Format == output.FormatJSON {
				return output.JSON(os.Stdout, stats)
			}
			fmt.Fprintf(os.Stderr, "%d run(s), %d call attempts\n", len(files), len(entries))
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tOPERATION\tCALLS\tERRORS\tTHROTTLES\tTHROTTLE%\tAVG_MS\tMAX_MS")
			for _, s := range stats {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%.1f\t%d\t%d\n",
					s.Service, s.Operation, s.Calls, s.Errors, s.Throttles,
					s.ThrottlePc, s.AvgMs, s.MaxMs)
			}
			return tw.Flush()
		},
	}
	show.Flags().IntVar(&last, "last", 1, "aggregate the last N runs (0 = all)")

	cmd.AddCommand(show)
	return cmd
}
