// Command aft-ops is the AFT Operations Toolkit CLI/TUI.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/hacker65536/aft-ops/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if code := cli.Run(ctx); code != cli.ExitOK {
		os.Exit(code)
	}
}
