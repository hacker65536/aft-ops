package cli

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
)

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
