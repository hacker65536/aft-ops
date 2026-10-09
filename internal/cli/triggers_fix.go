package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/hacker65536/aft-ops/internal/config"
	"github.com/hacker65536/aft-ops/internal/core/model"
	"github.com/hacker65536/aft-ops/internal/core/pipeline"
	"github.com/hacker65536/aft-ops/internal/output"
)

// ---- pipeline triggers fix ----

// defaultFixStates is what `triggers fix` selects when --state is not given:
// the two verdicts a write can correct.
var defaultFixStates = []model.TriggerState{model.TriggerMissing, model.TriggerDrift}

func newPipelineTriggersFixCmd(app *App) *cobra.Command {
	var (
		accountQuery string
		fromFile     string
		stateFilter  []string
		dryRun       bool
		yes          bool
		maxTargets   int
		expect       int
	)
	cmd := &cobra.Command{
		Use:   "fix [target...]",
		Short: "Put the expected push trigger back on pipelines that lost it (UpdatePipeline)",
		Long: `Replace each selected pipeline's push trigger with the one "pipeline
triggers" expects, judged by the same expectation.

With no targets, every pipeline whose trigger is missing or drifted is
selected; arguments, --file and --account narrow that the same way they do
for release, and --state widens or narrows the verdicts considered.

Only the trigger changes. Each pipeline's definition is read again right
before the write and only its triggers are replaced; UpdatePipeline does not
start the pipeline. The trigger as it was is saved under
~/.local/state/aft-ops/trigger-backups/ before each write.

Some pipelines are never written:
  - a trigger the expectation cannot describe (another source action's
    trigger, several triggers, pull-request or tag filters, branch excludes)
    is refused and reported, because someone may have put it there on purpose
  - a running pipeline is skipped, because UpdatePipeline stops the running
    execution
  - a pipeline whose trigger changed after the plan was read is skipped

Refused or failed pipelines make the run exit 1.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			states := defaultFixStates
			if cmd.Flags().Changed("state") {
				var err error
				if states, err = parseTriggerStates(stateFilter); err != nil {
					return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
				}
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

			q := targetQuery{args: args, file: fromFile, account: accountQuery, verb: "fix"}
			if !q.empty() {
				sel, err := selectTargets(summariesFromNames(names, resolver), q)
				if err != nil {
					return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
				}
				names = names[:0:0]
				for _, s := range sel {
					names = append(names, s.PipelineName)
				}
			}
			if len(names) == 0 {
				fmt.Fprintln(os.Stderr, "no matching targets")
				return nil
			}

			// A write decides from the trigger, so the plan is read fresh
			// rather than from the trigger cache (TTL 0 refetches every
			// selected name; the rest of the cache is left as it was).
			opts := pipeline.TriggerOptions{Policy: app.triggerPolicy()}
			if cmd.Flags().Changed("concurrency") {
				opts.Concurrency = app.Cfg.Batch.Concurrency
			}
			fmt.Fprintln(os.Stderr, "reading the current triggers of the selected pipelines…")
			sums, stats := svc.Triggers(ctx, names, resolver, opts, progressPrinter(app))
			clearProgress(app)
			if err := ctx.Err(); err != nil {
				return err
			}
			output.Freshness(os.Stderr, "triggers", stats)

			updates, refused, alreadyOK := planTriggerFix(filterTriggers(sums, states, ""))
			if alreadyOK > 0 {
				fmt.Fprintf(os.Stderr, "%d selected pipeline(s) already carry the expected trigger\n", alreadyOK)
			}
			// Before the empty check: "--expect 3 selected nothing" is a
			// failed assertion, not a quiet success.
			if cmd.Flags().Changed("expect") && len(updates) != expect {
				return &ExitError{Code: ExitToolError, Message: fmt.Sprintf(
					"--expect %d, but %d pipeline(s) would be updated", expect, len(updates))}
			}
			if len(updates) == 0 && len(refused) == 0 {
				fmt.Fprintln(os.Stderr, "nothing to fix")
				return nil
			}
			limit := app.Cfg.Trigger.MaxTargets
			if maxTargets > 0 {
				limit = maxTargets
			}
			if len(updates) > limit {
				return &ExitError{Code: ExitToolError, Message: fmt.Sprintf(
					"%d targets exceed the limit of %d; pass --max-targets %d to proceed",
					len(updates), limit, len(updates))}
			}

			color := app.StderrIsTTY() && !app.NoColor
			if len(updates) > 0 {
				fmt.Fprintf(os.Stderr, "%d pipeline(s) to update:\n", len(updates))
				output.TriggerTable(os.Stderr, updates, color)
			}
			if len(refused) > 0 {
				fmt.Fprintf(os.Stderr,
					"%d pipeline(s) refused (a trigger the expectation cannot describe, or no expectation); left untouched:\n",
					len(refused))
				output.TriggerTable(os.Stderr, refused, color)
			}
			refusedResults := refusedFixResults(refused)

			if dryRun {
				if wp := app.Cfg.EffectiveWriteProfile(); wp != app.Cfg.Profile {
					fmt.Fprintf(os.Stderr,
						"dry-run: a real run would write with profile %s (not verified here)\n", wp)
				}
				fmt.Fprintln(os.Stderr, "dry-run: nothing written")
				if len(refused) > 0 {
					return domainErr()
				}
				return nil
			}
			if len(updates) == 0 {
				// Nothing to write, but the refusals are this run's result.
				return writeFixResults(app, refusedResults)
			}

			// Build the write client before prompting: it verifies the write
			// credentials and prints the account they land in, so that line
			// sits directly above the confirmation (as for release).
			upd, err := app.UpdateClient(ctx)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			if !yes {
				ok, err := confirm(fmt.Sprintf("Update the trigger on %d pipeline(s)?", len(updates)))
				if err != nil {
					return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
				}
				if !ok {
					fmt.Fprintln(os.Stderr, "aborted")
					return nil
				}
			}

			req := pipeline.UpdateTriggersRequest{
				Targets:   updates,
				BackupDir: triggerBackupDir(app.Identity(ctx), time.Now()),
			}
			if cmd.Flags().Changed("concurrency") {
				req.Concurrency = app.Cfg.Batch.Concurrency
			}
			results := svc.UpdateTriggers(ctx, upd, req, progressPrinter(app))
			clearProgress(app)
			for _, r := range results {
				if r.BackupPath != "" {
					fmt.Fprintf(os.Stderr, "previous triggers saved under %s\n", req.BackupDir)
					break
				}
			}
			return writeFixResults(app, append(results, refusedResults...))
		},
	}
	cmd.Flags().StringVarP(&accountQuery, "account", "a", "",
		"fix every pipeline whose account name or id contains this substring")
	cmd.Flags().StringVarP(&fromFile, "file", "f", "",
		"read targets from file, one per line (\"-\" for stdin)")
	cmd.Flags().StringSliceVarP(&stateFilter, "state", "s", nil,
		"trigger states to consider, comma-separated (default missing,drift):\n"+triggerStateValues())
	cmd.Flags().IntVar(&expect, "expect", 0,
		"fail unless exactly N pipelines would be updated")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the plan without writing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().IntVar(&maxTargets, "max-targets", 0,
		"override trigger.max_targets for this run")
	return cmd
}

// writeFixResults renders a fix run's rows and turns a refused or failed row
// into exit 1.
func writeFixResults(app *App, results []model.TriggerFixResult) error {
	if app.Format == output.FormatJSON {
		if err := output.JSON(os.Stdout, results); err != nil {
			return err
		}
	} else {
		output.TriggerFixTable(os.Stdout, results, app.Color())
		output.TriggerFixCounts(os.Stderr, results)
	}
	for _, r := range results {
		if r.Outcome == model.FixFailed || r.Outcome == model.FixRefused {
			return domainErr()
		}
	}
	return nil
}

// planTriggerFix splits a trigger report into what a fix writes, what it
// refuses, and how many rows already need nothing. Both lists come back in
// the report's triage order.
func planTriggerFix(items []model.TriggerSummary) (updates, refused []model.TriggerSummary, alreadyOK int) {
	for _, t := range items {
		switch action, _ := model.PlanTriggerFix(t); action {
		case model.FixUpdate:
			updates = append(updates, t)
		case model.FixRefuse:
			refused = append(refused, t)
		default:
			alreadyOK++
		}
	}
	model.SortTriggers(updates)
	model.SortTriggers(refused)
	return updates, refused, alreadyOK
}

// refusedFixResults turns refused plan rows into result rows, so the report
// accounts for every selected pipeline and not only the ones written.
func refusedFixResults(refused []model.TriggerSummary) []model.TriggerFixResult {
	out := make([]model.TriggerFixResult, len(refused))
	for i, t := range refused {
		_, reasons := model.PlanTriggerFix(t)
		out[i] = model.TriggerFixResult{
			PipelineName: t.PipelineName,
			AccountID:    t.AccountID,
			AccountName:  t.AccountName,
			Outcome:      model.FixRefused,
			Reasons:      reasons,
			Before:       t.Actual,
		}
	}
	return out
}

// triggerBackupDir names one run's backup directory. The AFT management
// account is part of the path because one operator may fix two
// organizations' fleets, and pipeline names alone do not say which.
func triggerBackupDir(account string, at time.Time) string {
	if account == "" {
		account = "unknown-account"
	}
	return filepath.Join(config.DefaultTriggerBackupDir(), account, at.Format("20060102T150405"))
}
