package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Set via -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Printf("aft-ops %s (commit %s, built %s)\n", version, commit, date)
		},
	}
}
