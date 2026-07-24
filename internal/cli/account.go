package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/hacker65536/aft-ops/internal/core/model"
	"github.com/hacker65536/aft-ops/internal/output"
)

func newAccountCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "account",
		Aliases: []string{"acc"},
		Short:   "List AFT-vended accounts",
	}
	cmd.AddCommand(newAccountListCmd(app))
	return cmd
}

func newAccountListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list [query]",
		Aliases: []string{"ls"},
		Short:   "List accounts (optionally filtered by id or name)",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolver, err := app.Resolver(cmd.Context())
			if err != nil {
				return &ExitError{Code: ExitToolError, Err: err, Message: err.Error()}
			}
			var accounts []model.Account
			if len(args) == 1 {
				accounts = resolver.Match(args[0])
			} else {
				accounts = resolver.All()
			}
			if app.Format == output.FormatJSON {
				return output.JSON(os.Stdout, accounts)
			}
			output.AccountTable(os.Stdout, accounts)
			return nil
		},
	}
}
