package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
		newPipelineExecutionsCmd(app),
		newPipelineRefreshCmd(app),
		newPipelineLogsCmd(app),
		newPipelineReleaseCmd(app),
	)
	return cmd
}

// ---- pipeline executions ----

func newPipelineExecutionsCmd(app *App) *cobra.Command {
	var (
		limit   int32
		actions bool
	)
	cmd := &cobra.Command{
		Use:     "executions <target>",
		Aliases: []string{"execs"},
		Short:   "List recent executions of one pipeline",
		Long: `List one pipeline's recent executions, newest first — the CLI counterpart
of the TUI's executions screen.

With --actions, each execution is expanded into its per-action runs (stage,
action, status, duration and CodeBuild id), which is where the ids for
` + "`pipeline logs --execution`" + ` come from.

<target> may be a pipeline name, an account id, or an account name.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name, _, err := resolveTarget(ctx, app, args[0])
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			svc, err := app.PipelineService(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			execs, err := svc.Executions(ctx, name, limit, app.Refresh)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}

			if !actions {
				if app.Format == output.FormatJSON {
					return output.JSON(os.Stdout, execs)
				}
				output.ExecutionTable(os.Stdout, execs, app.Color())
				return nil
			}

			// The embedded execution's fields are promoted into the JSON
			// object, so an entry reads as one execution plus its actions.
			type execWithActions struct {
				model.Execution
				Actions []model.ActionExecution `json:"actions,omitempty"`
			}
			var detailed []execWithActions
			for _, e := range execs {
				acts, err := svc.ActionExecutions(ctx, name, e.ID, e.Status.Terminal())
				if err != nil {
					return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
				}
				detailed = append(detailed, execWithActions{Execution: e, Actions: acts})
			}
			if app.Format == output.FormatJSON {
				return output.JSON(os.Stdout, detailed)
			}
			for _, d := range detailed {
				output.ExecutionTable(os.Stdout, []model.Execution{d.Execution}, app.Color())
				output.ActionExecutionTable(os.Stdout, d.Actions, app.Color())
				fmt.Fprintln(os.Stdout)
			}
			return nil
		},
	}
	cmd.Flags().Int32Var(&limit, "limit", 25, "number of recent executions to list")
	cmd.Flags().BoolVar(&actions, "actions", false,
		"also list each execution's per-action runs (with CodeBuild ids)")
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
			summaries, _ := svc.Statuses(ctx, targetNames, resolver,
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
		watch        bool
		interval     time.Duration
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

			render := func() error {
				summaries, stats, err := fetchSummaries(cmd.Context(), app)
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
					output.StatusFreshness(os.Stderr, stats)
				}

				if failOnError {
					for _, s := range summaries {
						if s.Status() == model.StatusFailed || s.FetchError != "" {
							return domainErr()
						}
					}
				}
				return nil
			}

			if watch {
				return watchLoop(cmd.Context(), app, interval, render)
			}
			return render()
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
	cmd.Flags().BoolVarP(&watch, "watch", "w", false,
		"redraw the list on an interval until interrupted (table output only)")
	cmd.Flags().DurationVar(&interval, "interval", 0,
		"refresh interval for --watch (default: tui.poll_interval, 30s)")
	return cmd
}

// watchLoop redraws render() every interval until the context is cancelled,
// clearing the screen between frames. In-flight pipelines are refetched every
// pass regardless of the status TTL (StatusOptions.RefreshInFlight), which is
// exactly what a watcher is waiting on.
func watchLoop(ctx context.Context, app *App, interval time.Duration, render func() error) error {
	if app.Format != output.FormatTable {
		return &ExitError{Code: ExitToolError,
			Message: "--watch requires table output (drop --output json)"}
	}
	if interval <= 0 {
		interval = app.Cfg.TUI.PollInterval.D()
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if app.StderrIsTTY() {
			fmt.Fprint(os.Stderr, "\033[H\033[2J") // home + clear
		}
		fmt.Fprintf(os.Stderr, "%s · refreshing every %s · ctrl-c to stop\n",
			time.Now().Format("15:04:05"), interval)
		// A domain-level exit (--fail-on-error) is a per-frame verdict, not a
		// reason to stop watching; only tool errors abort the loop.
		if err := render(); err != nil {
			var xe *ExitError
			if !errors.As(err, &xe) || xe.Code != ExitDomainError {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
		execID  string
	)
	cmd := &cobra.Command{
		Use:   "logs <target>",
		Short: "Show the CodeBuild/terraform log of a pipeline action",
		Long: `Fetch the CloudWatch Logs of an AFT customizations build and print the
terraform portion.

By default the failed CodeBuild action of <target>'s current state is used.
Pass --execution <id> to look at a past run instead — an AFT customizations
run applies terraform twice, so that prints both the global and the account
build, separated by a "──── <stage> / <action> ────" rule. Pass --build <id>
to point straight at a single CodeBuild id. Output modes: terraform section
(default), --raw (full log), or --summary (plan verdict and error blocks only
— the machine-readable boundary).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if raw && summary {
				return &ExitError{Code: ExitToolError,
					Message: "--raw and --summary are mutually exclusive"}
			}
			if buildID != "" && execID != "" {
				return &ExitError{Code: ExitToolError,
					Message: "--build and --execution are mutually exclusive"}
			}

			targets := []buildTarget{{BuildID: buildID}}
			if buildID == "" {
				var err error
				if targets, err = resolveBuildTargets(ctx, app, args[0], execID); err != nil {
					return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
				}
			}

			svc, err := app.LogsService(ctx)
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

			// Fetch sequentially: these calls sit outside the batch rate
			// limiter, and an execution has two builds at most.
			sections := make([]logSection, 0, len(targets))
			var failed int
			for _, t := range targets {
				s := logSection{
					Stage:   t.Stage,
					Action:  t.Action,
					BuildID: t.BuildID,
				}
				bl, err := svc.Fetch(ctx, t.BuildID)
				if err != nil {
					// One unreadable build must not hide the other: record the
					// gap where the log would have been and keep going. Only a
					// clean sweep of failures is fatal.
					failed++
					s.Error = err.Error()
					s.Lines = []string{"(log unavailable: " + err.Error() + ")"}
					fmt.Fprintf(os.Stderr, "warning: could not fetch build %s: %v\n", t.BuildID, err)
				} else {
					s.BuildID, s.Group, s.Stream = bl.BuildID, bl.Group, bl.Stream
					s.Lines = logs.Render(bl.Lines, mode)
				}
				sections = append(sections, s)
			}
			if failed == len(sections) {
				return &ExitError{Code: ExitToolError,
					Message: fmt.Sprintf("could not fetch any log of %s", args[0])}
			}

			if app.Format == output.FormatJSON {
				return output.JSON(os.Stdout, sections)
			}
			writeLogSections(os.Stdout, sections)
			return nil
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "print the full build log unmodified")
	cmd.Flags().BoolVar(&summary, "summary", false,
		"print only the plan verdict and terraform error blocks")
	cmd.Flags().StringVar(&buildID, "build", "",
		"CodeBuild id to fetch instead of the auto-detected action")
	cmd.Flags().StringVar(&execID, "execution", "",
		"pipeline execution id to take the log from (default: the pipeline's current state)")
	return cmd
}

// buildTarget is one CodeBuild run `pipeline logs` prints, named by the stage
// and action that produced it.
type buildTarget struct {
	Stage   string
	Action  string
	BuildID string
}

// logSection is one build's rendered log, and the JSON element `pipeline logs`
// emits. The command returns a list of these because an AFT customizations
// execution runs terraform twice.
type logSection struct {
	Stage   string   `json:"stage,omitempty"`
	Action  string   `json:"action,omitempty"`
	BuildID string   `json:"build_id"`
	Group   string   `json:"log_group,omitempty"`
	Stream  string   `json:"log_stream,omitempty"`
	Error   string   `json:"error,omitempty"`
	Lines   []string `json:"lines"`
}

// writeLogSections prints the fetched logs, separating them by a
// "──── <stage> / <action> ────" rule when there is more than one.
//
// A single build gets no rule: the stderr line already named it, so the rule
// would only add a line that scripts reading this output did not have before.
func writeLogSections(w io.Writer, sections []logSection) {
	for i, s := range sections {
		if len(sections) > 1 {
			if i > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "──── %s ────\n", model.ActionLabel(s.Stage, s.Action))
		}
		for _, ln := range s.Lines {
			fmt.Fprintln(w, ln)
		}
	}
}

// resolveBuildTargets picks which CodeBuild logs to show.
//
// With --execution it returns every CodeBuild-backed action of that run, in
// pipeline order. A customizations execution applies terraform twice — global
// customizations, then account customizations — and either half may be the one
// that changed something, so showing a single build answers "what did this run
// do?" with half the truth. This matches what the TUI's v shortcut shows.
//
// Without --execution the subject is a failure in the pipeline's current
// state, which is naturally one build: AFT never reaches the account stage
// when the global stage fails.
func resolveBuildTargets(ctx context.Context, app *App, target, execID string) ([]buildTarget, error) {
	name, resolver, err := resolveTarget(ctx, app, target)
	if err != nil {
		return nil, err
	}
	svc, err := app.PipelineService(ctx)
	if err != nil {
		return nil, err
	}

	if execID != "" {
		// done=false: a single-shot CLI run has no session memo to populate,
		// and the execution may still be running.
		actions, err := svc.ActionExecutions(ctx, name, execID, false)
		if err != nil {
			return nil, err
		}
		builds := model.LogActions(actions)
		if len(builds) == 0 {
			return nil, fmt.Errorf("execution %s of %s has no CodeBuild action", execID, name)
		}
		targets := make([]buildTarget, 0, len(builds))
		for _, a := range builds {
			// Name the stage, not just the action: an AFT customizations run
			// has an "Apply" action in both the global and the account stage,
			// so the action name alone does not say which log this is.
			fmt.Fprintf(os.Stderr, "using action %q of stage %q, execution %s (build %s)\n",
				a.ActionName, a.StageName, execID, a.CodeBuildID)
			targets = append(targets, buildTarget{
				Stage: a.StageName, Action: a.ActionName, BuildID: a.CodeBuildID,
			})
		}
		return targets, nil
	}

	detail, err := svc.Detail(ctx, name, 0, resolver)
	if err != nil {
		return nil, err
	}
	for _, b := range detail.BuildActions() {
		if b.Action.Status != model.StatusFailed {
			continue
		}
		fmt.Fprintf(os.Stderr, "using failed action %q of stage %q (build %s)\n",
			b.Action.Name, b.Stage, b.Action.CodeBuildID)
		return []buildTarget{{
			Stage: b.Stage, Action: b.Action.Name, BuildID: b.Action.CodeBuildID,
		}}, nil
	}
	return nil, fmt.Errorf(
		"%s has no failed CodeBuild action; pass --execution <id> or --build <id> to pick one", name)
}

// fetchSummaries is the shared inventory→statuses→join flow. It always
// covers the full inventory, so the status cache is pruned of pipelines that
// no longer exist.
func fetchSummaries(ctx context.Context, app *App) ([]model.PipelineSummary, model.StatusStats, error) {
	svc, err := app.PipelineService(ctx)
	if err != nil {
		return nil, model.StatusStats{}, err
	}
	names, cachedAt, err := svc.Inventory(ctx, app.Refresh)
	if err != nil {
		return nil, model.StatusStats{}, err
	}
	if !cachedAt.IsZero() {
		output.CacheNote(os.Stderr, "pipeline inventory", cachedAt)
	}
	resolver, err := app.Resolver(ctx)
	if err != nil {
		return nil, model.StatusStats{}, err
	}

	opts := app.statusOptions()
	opts.Prune = true
	summaries, stats := svc.Statuses(ctx, names, resolver, opts, progressPrinter(app))
	clearProgress(app)

	if err := ctx.Err(); err != nil {
		return nil, stats, err
	}
	// Callers choose the display order (pl list --sort/--order); return in
	// inventory order (account-id) here.
	return summaries, stats, nil
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
	q := strings.ToLower(strings.TrimSpace(accountQuery))

	var out []model.PipelineSummary
	for _, it := range items {
		if len(statusSet) > 0 && !statusSet[strings.ToLower(string(it.Status()))] {
			continue
		}
		if q != "" &&
			!strings.Contains(strings.ToLower(it.AccountName), q) &&
			!strings.Contains(it.AccountID, q) {
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

			targets, err := releaseTargets(ctx, app, args, fromFile, statusFilter)
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

			svc, err := app.PipelineService(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			// Build the write client before prompting, not after: it verifies
			// the write credentials against STS and prints the account they
			// land in, so that line sits directly above the confirmation.
			start, err := app.StartClient(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
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
			svc.InvalidateExecutions(started)

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

// releaseTargets resolves the requested targets with statuses fresh enough
// to act on. A release decides two things from status — which pipelines a
// --status filter selects, and which ones the in-progress guard skips — and
// the status cache is minutes old by design, so this path refetches rather
// than trusting it:
//
//   - --status selects the target set itself → refetch the whole inventory
//     (a pipeline that has since succeeded must not be re-released).
//   - explicit targets → refetch only those (cheap, and enough for the
//     in-progress guard).
func releaseTargets(
	ctx context.Context,
	app *App,
	args []string,
	fromFile string,
	statuses []string,
) ([]model.PipelineSummary, error) {
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

	base := summariesFromNames(names, resolver)
	if len(statuses) > 0 {
		fmt.Fprintln(os.Stderr, "refetching statuses so --status selects on current state…")
		var stats model.StatusStats
		base, stats = svc.Statuses(ctx, names, resolver,
			pipeline.StatusOptions{RefreshAll: true, Prune: true}, progressPrinter(app))
		clearProgress(app)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		output.StatusFreshness(os.Stderr, stats)
	}

	targets, err := selectTargets(base, args, fromFile, statuses)
	if err != nil || len(targets) == 0 || len(statuses) > 0 {
		return targets, err
	}

	only := make(map[string]bool, len(targets))
	targetNames := make([]string, len(targets))
	for i, t := range targets {
		targetNames[i] = t.PipelineName
		only[t.PipelineName] = true
	}
	fresh, _ := svc.Statuses(ctx, targetNames, resolver,
		pipeline.StatusOptions{RefreshOnly: only}, progressPrinter(app))
	clearProgress(app)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return fresh, nil
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

// matchSummaries resolves one query against the pipeline list. Exact matches
// (pipeline name, account id, account name — all case-insensitive) win
// outright; only when there are none does it fall back to substring matches
// on the account name or id. The substring rule mirrors `list --account`, so
// the same query selects the same rows in both places.
func matchSummaries(summaries []model.PipelineSummary, query string) []model.PipelineSummary {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var exact, partial []model.PipelineSummary
	for _, s := range summaries {
		switch {
		case strings.ToLower(s.PipelineName) == q,
			s.AccountID == q,
			strings.ToLower(s.AccountName) == q:
			exact = append(exact, s)
		case strings.Contains(strings.ToLower(s.AccountName), q),
			strings.Contains(s.AccountID, q):
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
