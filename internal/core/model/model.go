// Package model defines the UI-agnostic domain types shared by the CLI,
// the TUI, and the core services. AWS SDK types must not leak out of the
// adapter layer; everything is normalized into these types.
package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
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
	StatusAbandoned  Status = "Abandoned" // action-level only
	StatusUnknown    Status = "Unknown"   // no execution yet, or fetch failed
)

// InFlight reports whether the execution is still running.
func (s Status) InFlight() bool {
	return s == StatusInProgress || s == StatusStopping
}

// ParseStatus normalizes an AWS status string.
func ParseStatus(s string) Status {
	switch Status(s) {
	case StatusSucceeded, StatusFailed, StatusInProgress,
		StatusStopped, StatusStopping, StatusSuperseded, StatusCancelled,
		StatusAbandoned:
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
	Revisions  []Revision `json:"revisions,omitempty"`
}

// Revision is a source revision that triggered an execution (e.g. the
// git commit fed into the pipeline's source stage).
type Revision struct {
	ActionName string `json:"action_name,omitempty"`
	RevisionID string `json:"revision_id,omitempty"`
	Summary    string `json:"summary,omitempty"`
	URL        string `json:"url,omitempty"`
}

// ActionState is one action within a stage.
type ActionState struct {
	Name         string     `json:"name"`
	Status       Status     `json:"status"`
	Summary      string     `json:"summary,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	LastChange   *time.Time `json:"last_change,omitempty"`
	// CodeBuildID is the action's external execution id. For CodeBuild
	// actions this is the build id ("<project>:<uuid>") — the handle that
	// `pipeline logs` follows to the terraform output. Empty for other
	// action types.
	CodeBuildID string `json:"codebuild_id,omitempty"`
	// LogStreamARN, when present, points directly at the action compute's
	// CloudWatch Logs stream (a shortcut for log retrieval).
	LogStreamARN string `json:"log_stream_arn,omitempty"`
	ExternalURL  string `json:"external_url,omitempty"`
}

// StageState is one stage's status and its actions.
type StageState struct {
	Name    string        `json:"name"`
	Status  Status        `json:"status"`
	Actions []ActionState `json:"actions,omitempty"`
}

// FailedAction returns the first non-succeeded action carrying a CodeBuild
// id, or nil. This is what `pipeline logs` targets by default.
func (s StageState) FailedAction() *ActionState {
	for i := range s.Actions {
		a := s.Actions[i]
		if a.Status == StatusFailed && a.CodeBuildID != "" {
			return &s.Actions[i]
		}
	}
	return nil
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
	// StatusFetchedAt is when this status was last fetched from AWS (as
	// opposed to Latest.LastUpdate, the execution's own time). It surfaces
	// staleness when the status is served from cache.
	StatusFetchedAt *time.Time `json:"status_fetched_at,omitempty"`
}

// Status returns the latest execution status (StatusUnknown when absent).
func (p PipelineSummary) Status() Status {
	if p.Latest == nil {
		return StatusUnknown
	}
	return p.Latest.Status
}

// SortKey selects the primary ordering of a pipeline list.
type SortKey string

const (
	SortByLastUpdate SortKey = "last-update"
	SortByStatus     SortKey = "status"
	SortByAccount    SortKey = "account"
)

// SortOrder is the direction of a sort.
type SortOrder string

const (
	OrderAsc  SortOrder = "asc"
	OrderDesc SortOrder = "desc"
)

// ParseSortKey validates a --sort value.
func ParseSortKey(s string) (SortKey, error) {
	switch SortKey(s) {
	case SortByLastUpdate, SortByStatus, SortByAccount:
		return SortKey(s), nil
	default:
		return "", fmt.Errorf("invalid sort key %q (want last-update|status|account)", s)
	}
}

// ParseSortOrder validates an --order value.
func ParseSortOrder(s string) (SortOrder, error) {
	switch SortOrder(s) {
	case OrderAsc, OrderDesc:
		return SortOrder(s), nil
	default:
		return "", fmt.Errorf("invalid sort order %q (want asc|desc)", s)
	}
}

// SortSummaries sorts items in place by key/order. Pipelines with no last
// execution time always sink to the bottom (regardless of order), so a
// recency view is never headed by never-run rows. Ties break by account
// name then id for a stable, deterministic result.
func SortSummaries(items []PipelineSummary, key SortKey, order SortOrder) {
	desc := order == OrderDesc
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch key {
		case SortByStatus:
			as, bs := string(a.Status()), string(b.Status())
			if as != bs {
				return xorDesc(as < bs, desc)
			}
		case SortByAccount:
			if !accountEqual(a, b) {
				return xorDesc(accountLess(a, b), desc)
			}
		default: // SortByLastUpdate
			at, bt := lastUpdateOf(a), lastUpdateOf(b)
			if at == nil || bt == nil {
				if at == nil && bt == nil {
					break // fall through to account tiebreak
				}
				return bt == nil // the non-nil row is "less" so nil sinks last
			}
			if !at.Equal(*bt) {
				return xorDesc(at.Before(*bt), desc)
			}
		}
		return accountLess(a, b) // stable tiebreak, always ascending
	})
}

// xorDesc flips an ascending comparison when the order is descending.
func xorDesc(asc, desc bool) bool {
	if desc {
		return !asc
	}
	return asc
}

func lastUpdateOf(p PipelineSummary) *time.Time {
	if p.Latest != nil {
		return p.Latest.LastUpdate
	}
	return nil
}

func accountLess(a, b PipelineSummary) bool {
	an, bn := strings.ToLower(a.AccountName), strings.ToLower(b.AccountName)
	if an != bn {
		return an < bn
	}
	return a.AccountID < b.AccountID
}

func accountEqual(a, b PipelineSummary) bool {
	return strings.EqualFold(a.AccountName, b.AccountName) && a.AccountID == b.AccountID
}

// PipelineDetail is the full `pipeline show` view: current stage/action
// state plus recent execution history.
type PipelineDetail struct {
	PipelineName string       `json:"pipeline_name"`
	AccountID    string       `json:"account_id"`
	AccountName  string       `json:"account_name,omitempty"`
	Stages       []StageState `json:"stages"`
	History      []Execution  `json:"history,omitempty"`
}

// FailedActions returns every failed CodeBuild action across all stages,
// in stage order — the candidates for `pipeline logs`.
func (d PipelineDetail) FailedActions() []ActionState {
	var out []ActionState
	for _, st := range d.Stages {
		if a := st.FailedAction(); a != nil {
			out = append(out, *a)
		}
	}
	return out
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
