// Package model defines the UI-agnostic domain types shared by the CLI,
// the TUI, and the core services. AWS SDK types must not leak out of the
// adapter layer; everything is normalized into these types.
package model

import (
	"regexp"
	"time"
)

// Status is a normalized pipeline execution status.
type Status string

const (
	StatusSucceeded  Status = "Succeeded"
	StatusFailed     Status = "Failed"
	StatusInProgress Status = "InProgress"
	StatusStopped    Status = "Stopped"
	StatusStopping   Status = "Stopping"
	StatusSuperseded Status = "Superseded"
	StatusCancelled  Status = "Cancelled"
	StatusUnknown    Status = "Unknown" // no execution yet, or fetch failed
)

// InFlight reports whether the execution is still running.
func (s Status) InFlight() bool {
	return s == StatusInProgress || s == StatusStopping
}

// ParseStatus normalizes an AWS status string.
func ParseStatus(s string) Status {
	switch Status(s) {
	case StatusSucceeded, StatusFailed, StatusInProgress,
		StatusStopped, StatusStopping, StatusSuperseded, StatusCancelled:
		return Status(s)
	default:
		return StatusUnknown
	}
}

// Account is an AWS account vended by AFT.
type Account struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// Execution is a single pipeline execution.
type Execution struct {
	ID         string     `json:"id"`
	Status     Status     `json:"status"`
	StartTime  *time.Time `json:"start_time,omitempty"`
	LastUpdate *time.Time `json:"last_update,omitempty"`
}

// AccountPipelineRe matches AFT per-account customizations pipelines and
// captures the account id.
var AccountPipelineRe = regexp.MustCompile(`^(\d{12})-customizations-pipeline$`)

// AccountIDFromPipeline extracts the account id from an AFT per-account
// pipeline name, or "" if the name is not an account pipeline.
func AccountIDFromPipeline(name string) string {
	m := AccountPipelineRe.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	return m[1]
}

// PipelineSummary is one row of `pipeline list`: an account pipeline with
// its resolved account and latest execution.
type PipelineSummary struct {
	PipelineName string     `json:"pipeline_name"`
	AccountID    string     `json:"account_id"`
	AccountName  string     `json:"account_name,omitempty"`
	Latest       *Execution `json:"latest_execution,omitempty"`
	FetchError   string     `json:"fetch_error,omitempty"` // per-item failure, never silent
}

// Status returns the latest execution status (StatusUnknown when absent).
func (p PipelineSummary) Status() Status {
	if p.Latest == nil {
		return StatusUnknown
	}
	return p.Latest.Status
}

// ReleaseResult is the outcome of one StartPipelineExecution.
type ReleaseResult struct {
	PipelineName string `json:"pipeline_name"`
	AccountID    string `json:"account_id,omitempty"`
	AccountName  string `json:"account_name,omitempty"`
	ExecutionID  string `json:"execution_id,omitempty"`
	Skipped      bool   `json:"skipped,omitempty"`
	SkipReason   string `json:"skip_reason,omitempty"`
	Error        string `json:"error,omitempty"`
}
