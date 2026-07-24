package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/logs"
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
	cmd.AddCommand(
		newPipelineListCmd(app),
		newPipelineShowCmd(app),
		newPipelineRefreshCmd(app),
		newPipelineLogsCmd(app),
		newPipelineReleaseCmd(app),
	)
	return cmd
}

// ---- pipeline refresh ----

func newPipelineRefreshCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh <target...>",
		Short: "Refetch the status of specific pipelines into the cache",
		Long: `Refetch just the given pipelines' latest execution status and update the
status cache, without fanning out over every pipeline.

Each target may be a pipeline name, an account id, or an account name.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			svc, err := app.PipelineService(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			names, _, err := svc.Inventory(ctx, app.Refresh)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			resolver, err := app.Resolver(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}

			targets, err := selectTargets(summariesFromNames(names, resolver), args, "", nil)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			if len(targets) == 0 {
				fmt.Fprintln(os.Stderr, "no matching targets")
				return nil
			}
			targetNames := make([]string, len(targets))
			refreshSet := make(map[string]bool, len(targets))
			for i, t := range targets {
				targetNames[i] = t.PipelineName
				refreshSet[t.PipelineName] = true
			}

			// RefreshOnly (not RefreshAll) force-refetches exactly these names
			// while still loading the existing cache, so the other entries are
			// merged back intact rather than clobbered.
			summaries := svc.Statuses(ctx, targetNames, resolver,
				pipeline.StatusOptions{RefreshOnly: refreshSet}, progressPrinter(app))
			clearProgress(app)
			if err := ctx.Err(); err != nil {
				return err
			}
			model.SortSummaries(summaries, model.SortByAccount, model.OrderAsc)

			if app.Format == output.FormatJSON {
				return output.JSON(os.Stdout, summaries)
			}
			output.PipelineTable(os.Stdout, summaries, app.Color())
			output.PipelineCounts(os.Stderr, summaries)
			return nil
		},
	}
	return cmd
}

// ---- pipeline list ----

func newPipelineListCmd(app *App) *cobra.Command {
	var (
		statusFilter []string
		accountQuery string
		failOnError  bool
		sortKey      string
		sortOrder    string
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List account pipelines with their latest execution status",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, err := model.ParseSortKey(sortKey)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			order, err := model.ParseSortOrder(sortOrder)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}

			summaries, err := fetchSummaries(cmd.Context(), app)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			summaries = filterSummaries(summaries, statusFilter, accountQuery)
			model.SortSummaries(summaries, key, order)

			if app.Format == output.FormatJSON {
				if err := output.JSON(os.Stdout, summaries); err != nil {
					return err
				}
			} else {
				output.PipelineTable(os.Stdout, summaries, app.Color())
				output.PipelineCounts(os.Stderr, summaries)
				output.StatusFreshness(os.Stderr, summaries, app.Cfg.Cache.StatusTTL.D())
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
	cmd.Flags().StringVar(&sortKey, "sort", string(model.SortByLastUpdate),
		"sort by: last-update|status|account")
	cmd.Flags().StringVar(&sortOrder, "order", string(model.OrderDesc),
		"sort order: asc|desc")
	return cmd
}

// ---- pipeline show ----

func newPipelineShowCmd(app *App) *cobra.Command {
	var history int32
	cmd := &cobra.Command{
		Use:   "show <target>",
		Short: "Show stage/action state and recent history for one pipeline",
		Long: `Show the current stage/action state of a single pipeline plus its
recent execution history.

<target> may be a pipeline name, an account id, or an account name; it must
resolve to exactly one pipeline.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name, resolver, err := resolveTarget(ctx, app, args[0])
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			svc, err := app.PipelineService(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			detail, err := svc.Detail(ctx, name, history, resolver)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}

			if app.Format == output.FormatJSON {
				return output.JSON(os.Stdout, detail)
			}
			output.PipelineDetailText(os.Stdout, *detail, app.Color())
			return nil
		},
	}
	cmd.Flags().Int32Var(&history, "history", 5,
		"number of recent executions to include (0 = none)")
	return cmd
}

// resolveTarget maps a single target string to exactly one pipeline name,
// using the (cache-aware) inventory and account resolver only — it does not
// fetch statuses. Ambiguous matches are an error so single-target commands
// never act on the wrong pipeline.
func resolveTarget(ctx context.Context, app *App, target string) (string, *account.Resolver, error) {
	svc, err := app.PipelineService(ctx)
	if err != nil {
		return "", nil, err
	}
	names, cachedAt, err := svc.Inventory(ctx, app.Refresh)
	if err != nil {
		return "", nil, err
	}
	if !cachedAt.IsZero() {
		output.CacheNote(os.Stderr, "pipeline inventory", cachedAt)
	}
	resolver, err := app.Resolver(ctx)
	if err != nil {
		return "", nil, err
	}

	matched := matchSummaries(summariesFromNames(names, resolver), target)
	switch len(matched) {
	case 0:
		return "", nil, fmt.Errorf("no pipeline matches %q", target)
	case 1:
		return matched[0].PipelineName, resolver, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d pipelines; be more specific:", target, len(matched))
		for _, m := range matched {
			name := m.AccountName
			if name == "" {
				name = "-"
			}
			fmt.Fprintf(&b, "\n  %s  %s (%s)", m.PipelineName, name, m.AccountID)
		}
		return "", nil, fmt.Errorf("%s", b.String())
	}
}

// summariesFromNames builds status-free summaries (name + resolved account)
// for target matching without a status fan-out.
func summariesFromNames(names []string, resolver *account.Resolver) []model.PipelineSummary {
	out := make([]model.PipelineSummary, len(names))
	for i, n := range names {
		s := model.PipelineSummary{PipelineName: n, AccountID: model.AccountIDFromPipeline(n)}
		if resolver != nil {
			if a := resolver.ByID(s.AccountID); a != nil {
				s.AccountName = a.Name
			}
		}
		out[i] = s
	}
	return out
}

// ---- pipeline logs ----

func newPipelineLogsCmd(app *App) *cobra.Command {
	var (
		raw     bool
		summary bool
		buildID string
	)
	cmd := &cobra.Command{
		Use:   "logs <target>",
		Short: "Show the CodeBuild/terraform log of a pipeline's failed action",
		Long: `Fetch the CloudWatch Logs of an AFT customizations build and print the
terraform portion.

By default the failed CodeBuild action of <target>'s current state is used;
pass --build to point at a specific CodeBuild id instead. Output modes:
terraform section (default), --raw (full log), or --summary (plan verdict
and error blocks only — the machine-readable boundary).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if raw && summary {
				return &ExitError{Code: ExitToolError,
					Message: "--raw and --summary are mutually exclusive"}
			}

			if buildID == "" {
				id, err := failedBuildID(ctx, app, args[0])
				if err != nil {
					return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
				}
				buildID = id
			}

			svc, err := app.LogsService(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			bl, err := svc.Fetch(ctx, buildID)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}

			mode := logs.ModeTerraform
			switch {
			case raw:
				mode = logs.ModeRaw
			case summary:
				mode = logs.ModeSummary
			}
			lines := logs.Render(bl.Lines, mode)

			if app.Format == output.FormatJSON {
				return output.JSON(os.Stdout, struct {
					BuildID string   `json:"build_id"`
					Group   string   `json:"log_group"`
					Stream  string   `json:"log_stream"`
					Lines   []string `json:"lines"`
				}{bl.BuildID, bl.Group, bl.Stream, lines})
			}
			for _, ln := range lines {
				fmt.Fprintln(os.Stdout, ln)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "print the full build log unmodified")
	cmd.Flags().BoolVar(&summary, "summary", false,
		"print only the plan verdict and terraform error blocks")
	cmd.Flags().StringVar(&buildID, "build", "",
		"CodeBuild id to fetch instead of the auto-detected failed action")
	return cmd
}

// failedBuildID resolves a target to its current failed CodeBuild action's
// build id, erroring clearly when there is nothing failed to look at.
func failedBuildID(ctx context.Context, app *App, target string) (string, error) {
	name, resolver, err := resolveTarget(ctx, app, target)
	if err != nil {
		return "", err
	}
	svc, err := app.PipelineService(ctx)
	if err != nil {
		return "", err
	}
	detail, err := svc.Detail(ctx, name, 0, resolver)
	if err != nil {
		return "", err
	}
	failed := detail.FailedActions()
	if len(failed) == 0 {
		return "", fmt.Errorf(
			"%s has no failed CodeBuild action; pass --build <id> to fetch a specific build", name)
	}
	a := failed[0]
	fmt.Fprintf(os.Stderr, "using failed action %q (build %s)\n", a.Name, a.CodeBuildID)
	return a.CodeBuildID, nil
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
	summaries := svc.Statuses(ctx, names, resolver, app.statusOptions(), progress)
	clearProgress(app)

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Callers choose the display order (pl list --sort/--order); return in
	// inventory order (account-id) here.
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
			model.SortSummaries(targets, model.SortByAccount, model.OrderAsc)

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

			// Drop just-triggered pipelines from the status cache: their
			// cached terminal status is now stale (they are InProgress).
			var started []string
			for _, r := range results {
				if r.ExecutionID != "" && r.Error == "" {
					started = append(started, r.PipelineName)
				}
			}
			if err := svc.InvalidateStatuses(started); err != nil {
				fmt.Fprintln(os.Stderr, "warning: failed to invalidate status cache:", err)
			}

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
