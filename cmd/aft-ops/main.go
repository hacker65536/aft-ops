// Command aft-ops is the AFT Operations Toolkit CLI/TUI.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hacker65536/aft-ops/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.Execute(ctx)
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "interrupted")
		os.Exit(130)
	}
	var xe *cli.ExitError
	if errors.As(err, &xe) {
		if xe.Message != "" {
			fmt.Fprintln(os.Stderr, "Error:", xe.Message)
		}
		os.Exit(xe.Code)
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(2)
}
