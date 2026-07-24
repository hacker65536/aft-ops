package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/model"
	"github.com/hacker65536/aft-ops/internal/core/pipeline"
	"github.com/hacker65536/aft-ops/internal/output"
)

func newPipelineCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pipeline",
		Aliases: []string{"pl"},
		Short:   "Operate AFT per-account customizations pipelines",
	}
	cmd.AddCommand(newPipelineListCmd(app), newPipelineReleaseCmd(app))
	return cmd
}

// ---- pipeline list ----

func newPipelineListCmd(app *App) *cobra.Command {
	var (
		statusFilter []string
		accountQuery string
		failOnError  bool
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List account pipelines with their latest execution status",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			summaries, err := fetchSummaries(cmd.Context(), app)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			summaries = filterSummaries(summaries, statusFilter, accountQuery)

			if app.Format == output.FormatJSON {
				if err := output.JSON(os.Stdout, summaries); err != nil {
					return err
				}
			} else {
				output.PipelineTable(os.Stdout, summaries, app.Color())
				output.PipelineCounts(os.Stderr, summaries)
			}

			if failOnError {
				for _, s := range summaries {
					if s.Status() == model.StatusFailed || s.FetchError != "" {
						return domainErr()
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVarP(&statusFilter, "status", "s", nil,
		"filter by status (comma-separated: Succeeded,Failed,InProgress,...)")
	cmd.Flags().StringVarP(&accountQuery, "account", "a", "",
		"filter by account id or name substring")
	cmd.Flags().BoolVar(&failOnError, "fail-on-error", false,
		"exit 1 when any pipeline is Failed or could not be fetched")
	return cmd
}

// fetchSummaries is the shared inventory→statuses→join flow.
func fetchSummaries(ctx context.Context, app *App) ([]model.PipelineSummary, error) {
	svc, err := app.PipelineService(ctx)
	if err != nil {
		return nil, err
	}
	names, cachedAt, err := svc.Inventory(ctx, app.Refresh)
	if err != nil {
		return nil, err
	}
	if !cachedAt.IsZero() {
		output.CacheNote(os.Stderr, "pipeline inventory", cachedAt)
	}
	resolver, err := app.Resolver(ctx)
	if err != nil {
		return nil, err
	}

	progress := progressPrinter(app)
	summaries := svc.Statuses(ctx, names, resolver, progress)
	clearProgress(app)

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].AccountName != summaries[j].AccountName {
			return summaries[i].AccountName < summaries[j].AccountName
		}
		return summaries[i].AccountID < summaries[j].AccountID
	})
	return summaries, nil
}

func progressPrinter(app *App) func(batch.Progress) {
	if !app.StderrIsTTY() {
		return nil
	}
	return func(p batch.Progress) {
		fmt.Fprintf(os.Stderr, "\rfetching %d/%d (failed: %d)", p.Done, p.Total, p.Failed)
	}
}

func clearProgress(app *App) {
	if app.StderrIsTTY() {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

func filterSummaries(items []model.PipelineSummary, statuses []string, accountQuery string) []model.PipelineSummary {
	statusSet := map[string]bool{}
	for _, s := range statuses {
		statusSet[strings.ToLower(strings.TrimSpace(s))] = true
	}
	q := strings.ToLower(accountQuery)

	var out []model.PipelineSummary
	for _, it := range items {
		if len(statusSet) > 0 && !statusSet[strings.ToLower(string(it.Status()))] {
			continue
		}
		if q != "" &&
			!strings.Contains(strings.ToLower(it.AccountName), q) &&
			!strings.Contains(it.AccountID, accountQuery) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// ---- pipeline release ----

func newPipelineReleaseCmd(app *App) *cobra.Command {
	var (
		statusFilter      []string
		fromFile          string
		dryRun            bool
		yes               bool
		maxTargets        int
		includeInProgress bool
	)
	cmd := &cobra.Command{
		Use:   "release [target...]",
		Short: "Trigger Release change (StartPipelineExecution) on selected pipelines",
		Long: `Trigger Release change on selected pipelines.

Targets may be pipeline names, account ids, or account names, given as
arguments, via --file (one per line, "-" for stdin), or selected by
--status (e.g. --status Failed for a retry sweep).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if len(args) == 0 && fromFile == "" && len(statusFilter) == 0 {
				return &ExitError{Code: ExitToolError,
					Message: "no targets: give arguments, --file, or --status"}
			}

			summaries, err := fetchSummaries(ctx, app)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}

			targets, err := selectTargets(summaries, args, fromFile, statusFilter)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			if len(targets) == 0 {
				fmt.Fprintln(os.Stderr, "no matching targets")
				return nil
			}

			// Safety guards (docs/design.md §4.3).
			limit := app.Cfg.Release.MaxTargets
			if maxTargets > 0 {
				limit = maxTargets
			}
			if len(targets) > limit {
				return &ExitError{Code: ExitToolError, Message: fmt.Sprintf(
					"%d targets exceed the limit of %d; pass --max-targets %d to proceed",
					len(targets), limit, len(targets))}
			}

			fmt.Fprintf(os.Stderr, "%d target(s):\n", len(targets))
			output.PipelineTable(os.Stderr, targets, app.StderrIsTTY() && !app.NoColor)
			if dryRun {
				fmt.Fprintln(os.Stderr, "dry-run: nothing released")
				return nil
			}
			if !yes {
				ok, err := confirm(fmt.Sprintf("Release %d pipeline(s)?", len(targets)))
				if err != nil {
					return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
				}
				if !ok {
					fmt.Fprintln(os.Stderr, "aborted")
					return nil
				}
			}

			svc, err := app.PipelineService(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			start, err := app.StartClient(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}

			results := svc.Release(ctx, start, pipeline.ReleaseRequest{
				Targets:        targets,
				SkipInProgress: app.Cfg.Release.SkipInProgress && !includeInProgress,
			}, progressPrinter(app))
			clearProgress(app)

			if app.Format == output.FormatJSON {
				if err := output.JSON(os.Stdout, results); err != nil {
					return err
				}
			} else {
				output.ReleaseTable(os.Stdout, results, app.Color())
			}

			for _, r := range results {
				if r.Error != "" {
					return domainErr()
				}
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVarP(&statusFilter, "status", "s", nil,
		"select targets by current status (e.g. Failed)")
	cmd.Flags().StringVarP(&fromFile, "file", "f", "",
		"read targets from file, one per line (\"-\" for stdin)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show targets without releasing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().IntVar(&maxTargets, "max-targets", 0,
		"override the release target limit for this run")
	cmd.Flags().BoolVar(&includeInProgress, "include-in-progress", false,
		"also release pipelines that are currently running")
	return cmd
}

// selectTargets resolves args/file/status filters into a deduped target list.
func selectTargets(
	summaries []model.PipelineSummary,
	args []string,
	fromFile string,
	statuses []string,
) ([]model.PipelineSummary, error) {
	queries := append([]string(nil), args...)
	if fromFile != "" {
		lines, err := readLines(fromFile)
		if err != nil {
			return nil, err
		}
		queries = append(queries, lines...)
	}

	seen := map[string]bool{}
	var targets []model.PipelineSummary
	add := func(s model.PipelineSummary) {
		if !seen[s.PipelineName] {
			seen[s.PipelineName] = true
			targets = append(targets, s)
		}
	}

	for _, q := range queries {
		matched := matchSummaries(summaries, q)
		if len(matched) == 0 {
			return nil, fmt.Errorf("no pipeline matches %q", q)
		}
		for _, m := range matched {
			add(m)
		}
	}
	if len(statuses) > 0 {
		for _, s := range filterSummaries(summaries, statuses, "") {
			add(s)
		}
	}
	return targets, nil
}

// matchSummaries resolves one query: exact pipeline name, exact account
// id, exact account name (case-insensitive), then name substring.
func matchSummaries(summaries []model.PipelineSummary, query string) []model.PipelineSummary {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var exact, partial []model.PipelineSummary
	for _, s := range summaries {
		switch {
		case s.PipelineName == query || s.AccountID == query,
			strings.ToLower(s.AccountName) == q:
			exact = append(exact, s)
		case strings.Contains(strings.ToLower(s.AccountName), q):
			partial = append(partial, s)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

func readLines(path string) ([]string, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var lines []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines, sc.Err()
}

// confirm prompts on the terminal. Non-interactive runs must pass --yes.
func confirm(prompt string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("stdin is not a terminal; pass --yes to confirm non-interactively")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false, sc.Err()
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes", nil
}
