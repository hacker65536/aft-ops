package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Exit code contract (docs/design.md §8.3):
//
//	0   success
//	1   domain-level failure (failed pipelines exist / partial release failure)
//	2   tool error (bad config, auth failure, API error)
//	130 interrupted
const (
	ExitOK          = 0
	ExitDomainError = 1
	ExitToolError   = 2
	ExitInterrupted = 130
)

// Run executes the root command and turns its error into the process exit
// code, reporting it on stderr. main is a one-liner over this so that the
// exit contract lives — and is tested — in the same package as the commands
// that produce it.
func Run(ctx context.Context) int {
	err := Execute(ctx)
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "interrupted")
		return ExitInterrupted
	}
	var xe *ExitError
	if errors.As(err, &xe) {
		// A domain error carries no message: the results it refers to were
		// already rendered, and "Error:" over a printed table reads as if
		// something else went wrong.
		if xe.Message != "" {
			fmt.Fprintln(os.Stderr, "Error:", xe.Message)
		}
		return xe.Code
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	return ExitToolError
}

// ExitError carries an exit code through cobra's error return.
type ExitError struct {
	Code    int
	Message string
	Err     error
}

func (e *ExitError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "exit error"
}

func (e *ExitError) Unwrap() error { return e.Err }

// domainErr signals exit 1 with no extra stderr message (results were
// already rendered).
func domainErr() error { return &ExitError{Code: ExitDomainError} }
