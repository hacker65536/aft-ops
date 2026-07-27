package cli

import (
	"fmt"
	"os"
	"slices"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hacker65536/aft-ops/internal/metrics"
	"github.com/hacker65536/aft-ops/internal/output"
)

// reportFiles picks which recorded runs a report covers, given every run
// newest-first, the file this process is recording to, and --last.
//
// The reporting process is recording too, and its file is the newest of all,
// so it has to come out before --last is applied. Otherwise `metrics show`
// with its default of one run reports on the invocation doing the reporting,
// which has made no API calls yet and prints an empty table — the numbers
// asked for are in the run before it. Dropping it after truncation instead
// would silently return one run fewer than requested.
func reportFiles(all []string, own string, last int) []string {
	files := slices.DeleteFunc(slices.Clone(all), func(p string) bool {
		return own != "" && p == own
	})
	if last > 0 && len(files) > last {
		files = files[:last]
	}
	return files
}

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
			all, err := metrics.LatestFiles(app.Cfg.Metrics.Dir, 0)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			files := reportFiles(all, app.MetricsPath(), last)
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
			fmt.Fprintln(tw, "SERVICE\tOPERATION\tCALLS\tERRORS\tTHROTTLES\tTHROTTLE%\tP50_MS\tP99_MS\tMAX_MS")
			for _, s := range stats {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%.1f\t%d\t%d\t%d\n",
					s.Service, s.Operation, s.Calls, s.Errors, s.Throttles,
					s.ThrottlePc, s.P50Ms, s.P99Ms, s.MaxMs)
			}
			return tw.Flush()
		},
	}
	show.Flags().IntVar(&last, "last", 1, "aggregate the last N runs (0 = all)")

	cmd.AddCommand(show)
	return cmd
}
