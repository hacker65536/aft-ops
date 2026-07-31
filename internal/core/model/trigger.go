package model

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// TriggerProviderType is the only provider CodePipeline accepts a Git trigger
// for. It is a constant rather than a configuration key because the API itself
// admits no alternative: triggers are documented as supported "only for the
// CodeStarSourceConnection action type".
const TriggerProviderType = "CodeStarSourceConnection"

// CustomizationsNamePlaceholder is what a TriggerPolicy file-path template
// substitutes the account's account_customizations_name for.
const CustomizationsNamePlaceholder = "{customizations_name}"

// TriggerState is one pipeline's verdict in a trigger drift report.
type TriggerState string

const (
	// TriggerOK means the pipeline carries exactly the expected trigger.
	TriggerOK TriggerState = "ok"
	// TriggerMissing means the pipeline has no trigger at all. This is the
	// state an AFT customizations pipeline lands in after aft-create-pipeline
	// runs again, because AFT's own terraform template declares no trigger.
	TriggerMissing TriggerState = "missing"
	// TriggerDrift means a trigger exists but is not the expected one.
	TriggerDrift TriggerState = "drift"
	// TriggerUnknown means no expectation could be derived for this pipeline,
	// so its trigger cannot be judged either way.
	TriggerUnknown TriggerState = "unknown"
	// TriggerFetchError means the pipeline definition could not be read.
	TriggerFetchError TriggerState = "fetch-error"
)

// TriggerStates lists every value --state accepts, in the order the counts
// line presents them.
var TriggerStates = []TriggerState{
	TriggerOK, TriggerMissing, TriggerDrift, TriggerUnknown, TriggerFetchError,
}

// ParseTriggerState validates one user-supplied --state value, ignoring case,
// and returns its canonical spelling. Like ParseStatusFilter, a typo has to
// stop the run: silently selecting nothing would read as "no drift anywhere".
func ParseTriggerState(s string) (TriggerState, error) {
	v := strings.TrimSpace(s)
	for _, st := range TriggerStates {
		if strings.EqualFold(v, string(st)) {
			return st, nil
		}
	}
	names := make([]string, len(TriggerStates))
	for i, st := range TriggerStates {
		names[i] = string(st)
	}
	return "", fmt.Errorf("invalid trigger state %q (want %s)", s, strings.Join(names, "|"))
}

// Reasons a trigger does not match its expectation. They are slugs rather
// than sentences so that a JSON consumer can branch on them; the renderers
// turn them into text.
const (
	ReasonNoTrigger         = "no_trigger"
	ReasonMultipleTriggers  = "multiple_triggers"
	ReasonProviderType      = "provider_type"
	ReasonSourceAction      = "source_action"
	ReasonBranches          = "branches"
	ReasonFilePaths         = "file_paths"
	ReasonPullRequestFilter = "pull_request_filter"
	// ReasonExtraFilters covers every filter the expected shape has no slot
	// for at all: branch/path excludes, tag filters, and push filters beyond
	// the first.
	ReasonExtraFilters = "extra_filters"
)

// PushTrigger is one pipeline trigger, normalized to the push configuration
// AFT customizations pipelines use.
//
// CodePipeline's own shape is a tree (trigger → gitConfiguration → push[] →
// branches/filePaths/tags), and a faithful mirror of it would push the "is
// there exactly one of everything?" question onto every consumer. Flattening
// it here means the comparison is a field-by-field one, and the cases the
// flattening cannot represent — more than one push filter, a pullRequest
// filter, tag filters — are recorded as their own fields precisely so they
// still show up as drift rather than being silently dropped.
type PushTrigger struct {
	ProviderType     string   `json:"provider_type,omitempty"`
	SourceAction     string   `json:"source_action,omitempty"`
	Branches         []string `json:"branches,omitempty"`
	BranchExcludes   []string `json:"branch_excludes,omitempty"`
	FilePaths        []string `json:"file_paths,omitempty"`
	FilePathExcludes []string `json:"file_path_excludes,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	// PullRequest reports that the trigger also fires on pull-request events.
	PullRequest bool `json:"pull_request,omitempty"`
	// ExtraPushFilters counts push filters beyond the first, which this type
	// does not carry. Anything above zero is drift by itself.
	ExtraPushFilters int `json:"extra_push_filters,omitempty"`
}

// TriggerPolicy derives the trigger an account's customizations pipeline
// should carry.
//
// The file path is a template over the account's account_customizations_name
// rather than a per-account setting: a fleet of several hundred pipelines
// would otherwise need several hundred lines of configuration, all of them
// duplicating what AFT's own metadata table already says.
type TriggerPolicy struct {
	SourceAction     string
	Branch           string
	FilePathTemplate string
}

// Expect returns the trigger a pipeline whose account has the given
// account_customizations_name should carry. ok is false when the expectation
// cannot be derived — an account with no customizations name has no file path
// to filter on, so its pipeline is judged as Unknown rather than as drifted.
func (p TriggerPolicy) Expect(customizationsName string) (PushTrigger, bool) {
	name := strings.TrimSpace(customizationsName)
	if name == "" || p.SourceAction == "" || p.Branch == "" || p.FilePathTemplate == "" {
		return PushTrigger{}, false
	}
	return PushTrigger{
		ProviderType: TriggerProviderType,
		SourceAction: p.SourceAction,
		Branches:     []string{p.Branch},
		FilePaths: []string{
			strings.ReplaceAll(p.FilePathTemplate, CustomizationsNamePlaceholder, name),
		},
	}, true
}

// TriggerSummary is one row of `pipeline triggers`: an account pipeline, the
// trigger it carries, the one it should carry, and the verdict.
type TriggerSummary struct {
	PipelineName string       `json:"pipeline_name"`
	AccountID    string       `json:"account_id"`
	AccountName  string       `json:"account_name,omitempty"`
	State        TriggerState `json:"state"`
	Reasons      []string     `json:"reasons,omitempty"`
	Expected     *PushTrigger `json:"expected,omitempty"`
	// Actual holds every trigger the pipeline declares. More than one is
	// itself drift, and dropping the extras would hide what is really there.
	Actual     []PushTrigger `json:"actual,omitempty"`
	FetchError string        `json:"fetch_error,omitempty"`
	// FetchedAt is when this pipeline's definition was last read from AWS.
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
}

// ClassifyTrigger judges one pipeline's triggers against its expectation.
//
// A nil expectation is Unknown, not OK: "we could not work out what this
// pipeline should have" and "this pipeline has what it should" are opposite
// answers, and a drift report that conflates them reports a gap in its own
// inputs as a clean bill of health.
func ClassifyTrigger(expected *PushTrigger, actual []PushTrigger) (TriggerState, []string) {
	switch {
	case expected == nil:
		return TriggerUnknown, nil
	case len(actual) == 0:
		return TriggerMissing, []string{ReasonNoTrigger}
	}

	var reasons []string
	if len(actual) > 1 {
		reasons = append(reasons, ReasonMultipleTriggers)
	}
	got := actual[0]
	if got.ProviderType != expected.ProviderType {
		reasons = append(reasons, ReasonProviderType)
	}
	if got.SourceAction != expected.SourceAction {
		reasons = append(reasons, ReasonSourceAction)
	}
	if !slices.Equal(got.Branches, expected.Branches) {
		reasons = append(reasons, ReasonBranches)
	}
	if !slices.Equal(got.FilePaths, expected.FilePaths) {
		reasons = append(reasons, ReasonFilePaths)
	}
	if got.PullRequest {
		reasons = append(reasons, ReasonPullRequestFilter)
	}
	// Excludes, tag filters and further push filters have no place in the
	// expected shape, so any value at all is a difference. They share one
	// reason: whichever it is, the operator has to go read the actual trigger.
	if len(got.BranchExcludes) > 0 || len(got.FilePathExcludes) > 0 ||
		len(got.Tags) > 0 || got.ExtraPushFilters > 0 {
		reasons = append(reasons, ReasonExtraFilters)
	}
	if len(reasons) == 0 {
		return TriggerOK, nil
	}
	return TriggerDrift, reasons
}

// SortTriggers orders a trigger report for triage: the pipelines that need
// attention first, ties broken by account so the result is deterministic.
//
// There is no --sort flag here, unlike `pipeline list`. That list is a view
// of a fleet an operator browses; this is a report with a single question
// behind it ("what is not as it should be?"), and an ordering that can be
// turned off would only ever hide the answer below the fold.
func SortTriggers(items []TriggerSummary) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		ar, br := triggerRank(a.State), triggerRank(b.State)
		if ar != br {
			return ar < br
		}
		an, bn := strings.ToLower(a.AccountName), strings.ToLower(b.AccountName)
		if an != bn {
			return an < bn
		}
		return a.AccountID < b.AccountID
	})
}

// triggerRank orders states by how much they want an operator's attention. A
// missing trigger outranks a drifted one: a pipeline nothing starts is a
// worse position than one started on the wrong paths.
func triggerRank(s TriggerState) int {
	switch s {
	case TriggerMissing:
		return 0
	case TriggerDrift:
		return 1
	case TriggerFetchError:
		return 2
	case TriggerUnknown:
		return 3
	case TriggerOK:
		return 4
	default:
		return 5
	}
}

// TriggerCounts tallies states in the order TriggerStates lists them.
func TriggerCounts(items []TriggerSummary) map[TriggerState]int {
	counts := make(map[TriggerState]int, len(TriggerStates))
	for _, t := range items {
		counts[t.State]++
	}
	return counts
}
