package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Stamped at release time by GoReleaser via -ldflags:
//
//	-X github.com/hacker65536/aft-ops/internal/cli.version={{ .Version }}
//	-X github.com/hacker65536/aft-ops/internal/cli.commit={{ .Commit }}
//	-X github.com/hacker65536/aft-ops/internal/cli.date={{ .Date }}
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// resolveVersion returns build metadata, filling any unset fields from the
// build info the Go toolchain embeds. This keeps `go install ...@v1.2.3`
// and `go build` builds informative even without GoReleaser's ldflags.
func resolveVersion() (v, c, d string) {
	v, c, d = version, commit, date
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v, c, d
	}
	if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if c == "none" && s.Value != "" {
				c = s.Value
			}
		case "vcs.time":
			if d == "unknown" && s.Value != "" {
				d = s.Value
			}
		}
	}
	return v, c, d
}

// versionString is the one-line banner shared by the `version` subcommand and
// the root `--version` flag.
func versionString() string {
	v, c, d := resolveVersion()
	return fmt.Sprintf("aft-ops %s (commit %s, built %s, %s)", v, c, d, runtime.Version())
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), versionString())
		},
	}
}
