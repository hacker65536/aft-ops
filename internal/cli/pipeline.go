package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/config"
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
		newPipelineTriggersCmd(app),
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

// ---- pipeline triggers ----

func newPipelineTriggersCmd(app *App) *cobra.Command {
	var (
		accountQuery string
		stateFilter  []string
		failOnDrift  bool
	)
	cmd := &cobra.Command{
		Use:     "triggers",
		Aliases: []string{"trig"},
		Short:   "Report whether each pipeline carries its expected push trigger",
		Long: `Compare every account pipeline's push trigger against the one its account
calls for, and report the difference.

The expectation is derived, not configured per account: AFT's metadata table
records each account's account_customizations_name, and the file-path filter
follows from it (trigger.file_path_template). A fleet of several hundred
pipelines therefore needs no per-account setting, and the expectation cannot
drift away from what AFT itself recorded.

This matters because AFT's own terraform template declares no trigger at all,
so any trigger is out-of-band: re-running aft-create-pipeline — which an AFT
upgrade or a rebuilt CodeConnections connection does across the fleet —
removes it. This command is how that becomes visible.

Read-only: it never writes a pipeline definition.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// Before any fetching, for the same reason --status is: an unknown
			// value must not cost a fleet-wide sweep to reach "nothing
			// selected", which reads exactly like "no drift anywhere".
			states, err := parseTriggerStates(stateFilter)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}

			svc, err := app.PipelineService(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			names, cachedAt, err := svc.Inventory(ctx, app.Refresh)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			if !cachedAt.IsZero() {
				output.CacheNote(os.Stderr, "pipeline inventory", cachedAt)
			}
			resolver, err := app.Resolver(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			warnNoCustomizationsNames(app, resolver)

			opts := pipeline.TriggerOptions{
				Policy:     app.triggerPolicy(),
				TTL:        app.Cfg.Cache.TriggerTTL.D(),
				RefreshAll: app.Refresh,
				Prune:      true,
			}
			// The core caps this fan-out well below batch.concurrency, so
			// forward the configured value only when the operator asked for a
			// number on purpose. --concurrency is a persistent flag, hence
			// Changed on the subcommand's own flag set.
			if cmd.Flags().Changed("concurrency") {
				opts.Concurrency = app.Cfg.Batch.Concurrency
			}
			summaries, stats := svc.Triggers(ctx, names, resolver, opts, progressPrinter(app))
			clearProgress(app)
			if err := ctx.Err(); err != nil {
				return err
			}

			summaries = filterTriggers(summaries, states, accountQuery)
			model.SortTriggers(summaries)

			if app.Format == output.FormatJSON {
				if err := output.JSON(os.Stdout, summaries); err != nil {
					return err
				}
			} else {
				output.TriggerTable(os.Stdout, summaries, app.Color())
				output.TriggerCounts(os.Stderr, summaries)
				output.Freshness(os.Stderr, "triggers", stats)
			}

			if failOnDrift {
				for _, t := range summaries {
					if t.State != model.TriggerOK {
						return domainErr()
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&accountQuery, "account", "a", "",
		"filter by account id or name substring")
	cmd.Flags().StringSliceVarP(&stateFilter, "state", "s", nil,
		"filter by trigger state, comma-separated:\n"+triggerStateValues())
	cmd.Flags().BoolVar(&failOnDrift, "fail-on-drift", false,
		"exit 1 unless every listed pipeline carries its expected trigger")
	return cmd
}

// triggerPolicy is the expectation the trigger report judges against.
func (a *App) triggerPolicy() model.TriggerPolicy {
	return model.TriggerPolicy{
		SourceAction:     a.Cfg.Trigger.SourceAction,
		Branch:           a.Cfg.Trigger.Branch,
		FilePathTemplate: a.Cfg.Trigger.FilePathTemplate,
	}
}

// warnNoCustomizationsNames explains a report that can only say "unknown".
//
// Every expectation comes from account_customizations_name, so when no account
// carries one the whole fleet reads as unjudged — and the two causes need
// different answers: an account source that has no such field, or an account
// cache written before the field was carried.
func warnNoCustomizationsNames(app *App, resolver *account.Resolver) {
	if resolver == nil || resolver.HasCustomizationsNames() {
		return
	}
	if app.Cfg.AccountSource != config.SourceAFTDynamoDB && app.Demo == nil {
		fmt.Fprintf(os.Stderr,
			"warning: account_source %q carries no account_customizations_name, "+
				"so no expected trigger can be derived; set account_source: %s\n",
			app.Cfg.AccountSource, config.SourceAFTDynamoDB)
		return
	}
	fmt.Fprintln(os.Stderr,
		"warning: no account carries an account_customizations_name; "+
			"if the account cache predates this field, --refresh refills it")
}

// triggerStateValues lists the accepted --state values for the flag help.
func triggerStateValues() string {
	names := make([]string, len(model.TriggerStates))
	for i, s := range model.TriggerStates {
		names[i] = string(s)
	}
	return strings.Join(names, "|")
}

// parseTriggerStates validates a --state list and returns the canonical
// spellings. See parseStatusFilters for why this runs before any fetching.
func parseTriggerStates(values []string) ([]model.TriggerState, error) {
	out := make([]model.TriggerState, 0, len(values))
	for _, v := range values {
		s, err := model.ParseTriggerState(v)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func filterTriggers(items []model.TriggerSummary, states []model.TriggerState, accountQuery string) []model.TriggerSummary {
	stateSet := map[model.TriggerState]bool{}
	for _, s := range states {
		stateSet[s] = true
	}
	q := strings.ToLower(strings.TrimSpace(accountQuery))

	var out []model.TriggerSummary
	for _, it := range items {
		if len(stateSet) > 0 && !stateSet[it.State] {
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

// ---- pipeline refresh ----

func newPipelineRefreshCmd(app *App) *cobra.Command {
	var accountQuery string
	cmd := &cobra.Command{
		Use:   "refresh [target...]",
		Short: "Refetch the status of specific pipelines into the cache",
		Long: `Refetch just the given pipelines' latest execution status and update the
status cache, without fanning out over every pipeline.

Each target names one pipeline exactly, by pipeline name, account id, or
account name. To refresh a group, select it with --account.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			q := targetQuery{args: args, account: accountQuery, verb: "refresh"}
			if q.empty() {
				return &ExitError{Code: ExitToolError,
					Message: "no targets: give arguments or --account"}
			}
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

			targets, err := selectTargets(summariesFromNames(names, resolver), q)
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
	cmd.Flags().StringVarP(&accountQuery, "account", "a", "",
		"refresh every pipeline whose account name or id contains this substring")
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
			statuses, err := parseStatusFilters(statusFilter)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}

			render := func() error {
				summaries, stats, err := fetchSummaries(cmd.Context(), app)
				if err != nil {
					return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
				}
				summaries = filterSummaries(summaries, statuses, accountQuery)
				model.SortSummaries(summaries, key, order)

				if app.Format == output.FormatJSON {
					if err := output.JSON(os.Stdout, summaries); err != nil {
						return err
					}
				} else {
					output.PipelineTable(os.Stdout, summaries, app.Color())
					output.PipelineCounts(os.Stderr, summaries)
					output.Freshness(os.Stderr, "statuses", stats)
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
		"filter by status, comma-separated:\n"+statusFlagValues())
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
// fetch statuses. Resolution is exact (see resolveOne), so a single-target
// command can never act on a pipeline the operator did not name.
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

	matched, err := resolveOne(summariesFromNames(names, resolver), target)
	if err != nil {
		return "", nil, err
	}
	return matched.PipelineName, resolver, nil
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
func fetchSummaries(ctx context.Context, app *App) ([]model.PipelineSummary, model.FetchStats, error) {
	svc, err := app.PipelineService(ctx)
	if err != nil {
		return nil, model.FetchStats{}, err
	}
	names, cachedAt, err := svc.Inventory(ctx, app.Refresh)
	if err != nil {
		return nil, model.FetchStats{}, err
	}
	if !cachedAt.IsZero() {
		output.CacheNote(os.Stderr, "pipeline inventory", cachedAt)
	}
	resolver, err := app.Resolver(ctx)
	if err != nil {
		return nil, model.FetchStats{}, err
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

// statusFlagValues lists the accepted --status values for the flag help, so
// the set a typo is rejected against is the set the help shows. It wraps:
// all ten on one line runs off the side of any terminal.
func statusFlagValues() string {
	const perLine = 5
	var lines []string
	for chunk := range slices.Chunk(model.FilterableStatuses, perLine) {
		names := make([]string, len(chunk))
		for i, s := range chunk {
			names[i] = string(s)
		}
		lines = append(lines, strings.Join(names, "|"))
	}
	return strings.Join(lines, "|\n")
}

// parseStatusFilters validates a --status list and returns the canonical
// spellings. Callers must do this before resolving targets or fetching
// anything: an unknown value has to stop the run while it is still free,
// not after a full fan-out has been paid for.
func parseStatusFilters(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, v := range values {
		s, err := model.ParseStatusFilter(v)
		if err != nil {
			return nil, err
		}
		out = append(out, string(s))
	}
	return out, nil
}

func filterSummaries(items []model.PipelineSummary, statuses []string, accountQuery string) []model.PipelineSummary {
	statusSet := map[string]bool{}
	for _, s := range statuses {
		statusSet[strings.ToLower(strings.TrimSpace(s))] = true
	}
	q := strings.ToLower(strings.TrimSpace(accountQuery))

	var out []model.PipelineSummary
	for _, it := range items {
		// A row whose status could not be fetched is filtered as what the
		// table shows it as, not as the Unknown it degrades to.
		status := it.Status()
		if it.FetchError != "" {
			status = model.StatusFetchError
		}
		if len(statusSet) > 0 && !statusSet[strings.ToLower(string(status))] {
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
		accountQuery      string
		fromFile          string
		dryRun            bool
		yes               bool
		maxTargets        int
		expect            int
		includeInProgress bool
	)
	cmd := &cobra.Command{
		Use:   "release [target...]",
		Short: "Trigger Release change (StartPipelineExecution) on selected pipelines",
		Long: `Trigger Release change on selected pipelines.

Each target names one pipeline exactly, by pipeline name, account id, or
account name, given as arguments or via --file (one per line, "-" for
stdin). Releasing a group is asked for explicitly: --account selects every
pipeline whose account matches a substring, and --status selects by current
state (e.g. --status Failed for a retry sweep).

--expect N fails the run unless the selection resolves to exactly N
pipelines, so an unattended --yes cannot quietly widen as the fleet grows.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Before releaseTargets: a --status selection makes that call
			// refetch every pipeline's status, so a typo caught afterwards
			// would have cost a full fan-out to reach "no matching targets".
			statuses, err := parseStatusFilters(statusFilter)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			q := targetQuery{
				args: args, file: fromFile, account: accountQuery,
				statuses: statuses, verb: "release",
			}
			if q.empty() {
				return &ExitError{Code: ExitToolError,
					Message: "no targets: give arguments, --account, --file, or --status"}
			}

			targets, err := releaseTargets(ctx, app, q)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			// Before the empty check: "--expect 3 selected nothing" is a
			// failed assertion, not a quiet success.
			if cmd.Flags().Changed("expect") && len(targets) != expect {
				return &ExitError{Code: ExitToolError, Message: fmt.Sprintf(
					"--expect %d, but the selection resolved to %d pipeline(s)",
					expect, len(targets))}
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
				// A dry run stops before any write credentials are resolved,
				// so it cannot check which account they land in. Name the
				// profile a real run would use, marked as unchecked, rather
				// than let the rehearsal imply the write side was looked at.
				if wp := app.Cfg.EffectiveWriteProfile(); wp != app.Cfg.Profile {
					fmt.Fprintf(os.Stderr,
						"dry-run: a real run would write with profile %s (not verified here)\n", wp)
				}
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

			results := svc.StartExecution(ctx, start, pipeline.StartExecutionRequest{
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
		"select targets by current status, comma-separated:\n"+statusFlagValues())
	cmd.Flags().StringVarP(&accountQuery, "account", "a", "",
		"release every pipeline whose account name or id contains this substring")
	cmd.Flags().StringVarP(&fromFile, "file", "f", "",
		"read targets from file, one per line (\"-\" for stdin)")
	cmd.Flags().IntVar(&expect, "expect", 0,
		"fail unless the selection resolves to exactly N pipelines")
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
//   - named targets and --account select by name, not by state, so only the
//     selected rows are refetched (cheap, and enough for the in-progress
//     guard).
func releaseTargets(
	ctx context.Context,
	app *App,
	q targetQuery,
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
	if len(q.statuses) > 0 {
		fmt.Fprintln(os.Stderr, "refetching statuses so --status selects on current state…")
		var stats model.FetchStats
		base, stats = svc.Statuses(ctx, names, resolver,
			pipeline.StatusOptions{RefreshAll: true, Prune: true}, progressPrinter(app))
		clearProgress(app)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		output.Freshness(os.Stderr, "statuses", stats)
	}

	targets, err := selectTargets(base, q)
	if err != nil || len(targets) == 0 || len(q.statuses) > 0 {
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

// targetQuery is a command's complete description of what to act on: targets
// named one by one (arguments, or a file of them), plus the set-shaped
// selectors. verb only ever appears in an error hint.
type targetQuery struct {
	args     []string
	file     string
	account  string
	statuses []string
	verb     string
}

func (q targetQuery) empty() bool {
	return len(q.args) == 0 && q.file == "" && q.account == "" && len(q.statuses) == 0
}

// selectTargets resolves a targetQuery into a deduped target list.
//
// The two halves have deliberately different failure modes. A named target
// must resolve to exactly one pipeline, and anything else is an error: the
// operator said "this one" and was wrong about it. --account and --status
// are sets by construction, so selecting nothing is a legitimate answer and
// the caller reports it as "no matching targets". Where they overlap the two
// selectors intersect, matching `pipeline list`; named targets union in on
// top.
func selectTargets(summaries []model.PipelineSummary, q targetQuery) ([]model.PipelineSummary, error) {
	queries := append([]string(nil), q.args...)
	if q.file != "" {
		lines, err := readLines(q.file)
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

	for _, name := range queries {
		m, err := resolveOne(summaries, name)
		if err != nil {
			var te *targetError
			if q.verb != "" && errors.As(err, &te) && len(te.candidates) > 0 {
				te.hint = fmt.Sprintf("to %s all %d, pass --account %s",
					q.verb, len(te.candidates), te.query)
			}
			return nil, err
		}
		add(m)
	}
	if q.account != "" || len(q.statuses) > 0 {
		for _, s := range filterSummaries(summaries, q.statuses, q.account) {
			add(s)
		}
	}
	return targets, nil
}

// maxCandidates caps the list of near misses in an error. A short query can
// match most of a several-hundred-account fleet, and a wall of names is not
// a suggestion.
const maxCandidates = 10

// targetError reports an argument that did not name exactly one pipeline. It
// carries the near misses because that is what the operator's next move
// depends on: a typo needs to see the real names, and someone who meant a
// whole group needs to be told which flag asks for a group on purpose.
type targetError struct {
	query      string
	candidates []model.PipelineSummary
	ambiguous  bool   // the candidates all matched exactly — genuinely undecidable
	hint       string // set by commands that have a set-shaped flag to offer
}

func (e *targetError) Error() string {
	var b strings.Builder
	switch {
	case len(e.candidates) == 0:
		return fmt.Sprintf("no pipeline matches %q", e.query)
	case e.ambiguous:
		fmt.Fprintf(&b, "%q matches %d pipelines; name one of them:", e.query, len(e.candidates))
	default:
		fmt.Fprintf(&b, "no pipeline is named %q; did you mean:", e.query)
	}
	shown := e.candidates
	if len(shown) > maxCandidates {
		shown = shown[:maxCandidates]
	}
	for _, c := range shown {
		name := c.AccountName
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(&b, "\n  %s  %s (%s)", c.PipelineName, name, c.AccountID)
	}
	if n := len(e.candidates) - len(shown); n > 0 {
		fmt.Fprintf(&b, "\n  … and %d more", n)
	}
	if e.hint != "" {
		fmt.Fprintf(&b, "\n%s", e.hint)
	}
	return b.String()
}

// resolveOne maps one target argument to exactly one pipeline: a pipeline
// name, an account id, or an account name, matched exactly and
// case-insensitively.
//
// Substrings deliberately do not resolve. A fragment that identifies one
// pipeline today identifies three after the next account is vended, so a
// singular argument is never allowed to quietly become a set — and a command
// line kept in a runbook or a CI job then no longer says how many pipelines
// it acts on. Selecting a group is what --account is for. Near misses are
// still collected, but only to name them in the error.
func resolveOne(summaries []model.PipelineSummary, query string) (model.PipelineSummary, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return model.PipelineSummary{}, &targetError{query: query}
	}
	var exact, near []model.PipelineSummary
	for _, s := range summaries {
		switch {
		case strings.ToLower(s.PipelineName) == q,
			s.AccountID == q,
			strings.ToLower(s.AccountName) == q:
			exact = append(exact, s)
		case strings.Contains(strings.ToLower(s.AccountName), q),
			strings.Contains(s.AccountID, q):
			near = append(near, s)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(exact) > 1:
		return model.PipelineSummary{}, &targetError{
			query: query, candidates: exact, ambiguous: true}
	default:
		return model.PipelineSummary{}, &targetError{query: query, candidates: near}
	}
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
