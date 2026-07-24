// Package cli defines the cobra command tree. Commands orchestrate core
// services and render through internal/output; they contain no AWS logic.
package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/hacker65536/aft-ops/internal/config"
	"github.com/hacker65536/aft-ops/internal/output"
)

// Execute runs the root command. Returned errors are mapped to exit codes
// in main.
func Execute(ctx context.Context) error {
	app := &App{}

	var (
		cfgPath     string
		profile     string
		region      string
		outFormat   string
		concurrency int
		rps         float64
	)

	root := &cobra.Command{
		Use:           "aft-ops",
		Short:         "AFT Operations Toolkit — operate AFT-vended CodePipelines at scale",
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			// flags > env > file > defaults
			if cmd.Flags().Changed("profile") {
				cfg.Profile = profile
			}
			if cmd.Flags().Changed("region") {
				cfg.Region = region
			}
			if cmd.Flags().Changed("concurrency") {
				cfg.Batch.Concurrency = concurrency
			}
			if cmd.Flags().Changed("rps") {
				cfg.Batch.RPS = rps
			}
			app.Cfg = cfg

			f, err := output.ParseFormat(outFormat)
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			app.Format = f
			app.initMetrics()
			return nil
		},
		PersistentPostRun: func(*cobra.Command, []string) {
			app.closeMetrics()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// bare `aft-ops` launches the TUI
			return runTUI(cmd.Context(), app)
		},
	}

	// Print just the banner for `--version`/`-v`, matching the `version`
	// subcommand instead of cobra's default "aft-ops version ..." line.
	root.SetVersionTemplate("{{.Version}}\n")

	pf := root.PersistentFlags()
	pf.StringVar(&cfgPath, "config", "", "config file (default ~/.config/aft-ops/config.yaml)")
	pf.StringVar(&profile, "profile", "", "AWS profile")
	pf.StringVar(&region, "region", "", "AWS region")
	pf.StringVarP(&outFormat, "output", "o", string(output.FormatTable), "output format: table|json")
	pf.BoolVar(&app.NoColor, "no-color", false, "disable colored output")
	pf.BoolVar(&app.Refresh, "refresh", false, "bypass caches and refetch")
	pf.IntVar(&concurrency, "concurrency", 0, "batch concurrency (overrides config)")
	pf.Float64Var(&rps, "rps", 0, "API requests per second limit (overrides config)")

	root.AddCommand(
		newPipelineCmd(app),
		newAccountCmd(app),
		newCacheCmd(app),
		newMetricsCmd(app),
		newTUICmd(app),
		newVersionCmd(),
	)

	return root.ExecuteContext(ctx)
}
