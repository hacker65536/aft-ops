// Package cli defines the cobra command tree. Commands orchestrate core
// services and render through internal/output; they contain no AWS logic.
package cli

import (
	"context"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hacker65536/aft-ops/internal/config"
	"github.com/hacker65536/aft-ops/internal/demo"
	"github.com/hacker65536/aft-ops/internal/output"
)

// Execute runs the root command. Returned errors are mapped to exit codes
// in main.
func Execute(ctx context.Context) error {
	app := &App{}
	// Close from here rather than PersistentPostRun only: cobra skips the
	// Post hooks when a command returns an error.
	defer app.closeMetrics()

	var (
		cfgPath       string
		profile       string
		region        string
		awsConfigFile string
		outFormat     string
		demoPath      string
		concurrency   int
		rps           float64
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
			if cmd.Flags().Changed("aws-config-file") {
				cfg.AWSConfigFile = awsConfigFile
			}
			if cmd.Flags().Changed("concurrency") {
				cfg.Batch.Concurrency = concurrency
			}
			if cmd.Flags().Changed("rps") {
				cfg.Batch.RPS = rps
			}
			if err := applyDemo(app, &cfg, demoPath); err != nil {
				return err
			}
			// Re-validate: config.Load only saw the file and the environment,
			// so without this pass a flag can install a value the same check
			// would have rejected a moment earlier. After applyDemo, so a
			// fixture run is never held up by the real credential setup.
			if err := cfg.Validate(); err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
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
	pf.StringVar(&awsConfigFile, "aws-config-file", "",
		"AWS shared config file the profile is looked up in (default: $AWS_CONFIG_FILE, else ~/.aws/config)")
	pf.StringVarP(&outFormat, "output", "o", string(output.FormatTable), "output format: table|json")
	pf.StringVar(&demoPath, "demo", "",
		"run against a local fixture instead of AWS (offline demo; see docs/demo)")
	pf.BoolVar(&app.NoColor, "no-color", false, "disable colored output")
	pf.BoolVar(&app.Refresh, "refresh", false, "bypass caches and refetch")
	pf.IntVar(&concurrency, "concurrency", 0, "batch concurrency (overrides config)")
	pf.Float64Var(&rps, "rps", 0, "API requests per second limit, 0 = unlimited (overrides config)")

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

// applyDemo switches the app into offline demo mode when --demo (or
// AFT_OPS_DEMO) names a fixture.
//
// The fixture's identity replaces the configured profile and region rather
// than merely being displayed: the cache is scoped by profile+region, so
// borrowing the operator's real profile name would file fake pipelines under
// the scope their real ones live in. Metrics go off for the same reason the
// fakes exist at all — nothing here goes through the SDK middleware, so
// recording a run would only produce a file of zeroes.
func applyDemo(app *App, cfg *config.Config, path string) error {
	if path == "" {
		path = os.Getenv("AFT_OPS_DEMO")
	}
	if path == "" {
		return nil
	}
	env, err := demo.Load(path)
	if err != nil {
		return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
	}
	if v := os.Getenv("AFT_OPS_DEMO_LATENCY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return &ExitError{Code: ExitToolError, Err: err,
				Message: "invalid AFT_OPS_DEMO_LATENCY: " + err.Error()}
		}
		env.SetLatency(d)
	}
	app.Demo = env

	id := env.Identity()
	cfg.Profile = id.Profile
	if cfg.Profile == "" {
		cfg.Profile = "demo"
	}
	cfg.WriteProfile = ""
	cfg.AWSConfigFile = "" // the fixture replaces the credential source entirely
	cfg.Region = id.Region
	cfg.Metrics.Enabled = false
	return nil
}
