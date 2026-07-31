package model

import (
	"slices"
	"testing"
)

var testPolicy = TriggerPolicy{
	SourceAction:     "aft-account-customizations",
	Branch:           "main",
	FilePathTemplate: "{customizations_name}/terraform/*.tf",
}

// The expectation is derived from the account's own customizations name, so
// the file path a pipeline is judged against can never drift from what AFT
// recorded.
func TestTriggerPolicyExpect(t *testing.T) {
	want, ok := testPolicy.Expect("payments-prod")
	if !ok {
		t.Fatal("Expect(payments-prod) returned ok=false")
	}
	if want.ProviderType != TriggerProviderType {
		t.Errorf("provider = %q, want %q", want.ProviderType, TriggerProviderType)
	}
	if !slices.Equal(want.FilePaths, []string{"payments-prod/terraform/*.tf"}) {
		t.Errorf("file paths = %v", want.FilePaths)
	}
	if !slices.Equal(want.Branches, []string{"main"}) {
		t.Errorf("branches = %v", want.Branches)
	}
}

// An account with no customizations name has no file path to filter on, so
// there is nothing to judge its pipeline against.
func TestTriggerPolicyExpectWithoutName(t *testing.T) {
	for _, name := range []string{"", "   "} {
		if _, ok := testPolicy.Expect(name); ok {
			t.Errorf("Expect(%q) = ok, want not ok", name)
		}
	}
	empty := TriggerPolicy{}
	if _, ok := empty.Expect("payments-prod"); ok {
		t.Error("an empty policy must not produce an expectation")
	}
}

// expected is the trigger a correctly configured payments-prod carries.
func expected(t *testing.T) PushTrigger {
	t.Helper()
	e, ok := testPolicy.Expect("payments-prod")
	if !ok {
		t.Fatal("policy produced no expectation")
	}
	return e
}

func TestClassifyTrigger(t *testing.T) {
	want := expected(t)
	// mutate returns the expected trigger with one field changed.
	mutate := func(f func(*PushTrigger)) []PushTrigger {
		got := want
		got.Branches = slices.Clone(want.Branches)
		got.FilePaths = slices.Clone(want.FilePaths)
		f(&got)
		return []PushTrigger{got}
	}

	cases := []struct {
		name    string
		actual  []PushTrigger
		state   TriggerState
		reasons []string
	}{
		{"match", []PushTrigger{want}, TriggerOK, nil},
		{"no trigger", nil, TriggerMissing, []string{ReasonNoTrigger}},
		{"two triggers", []PushTrigger{want, want}, TriggerDrift,
			[]string{ReasonMultipleTriggers}},
		{"wrong provider", mutate(func(p *PushTrigger) { p.ProviderType = "GitHub" }),
			TriggerDrift, []string{ReasonProviderType}},
		{"wrong source action", mutate(func(p *PushTrigger) { p.SourceAction = "aft-global-customizations" }),
			TriggerDrift, []string{ReasonSourceAction}},
		{"wrong branch", mutate(func(p *PushTrigger) { p.Branches = []string{"master"} }),
			TriggerDrift, []string{ReasonBranches}},
		{"wrong file path", mutate(func(p *PushTrigger) { p.FilePaths = []string{"payments/terraform/*.tf"} }),
			TriggerDrift, []string{ReasonFilePaths}},
		{"pull request filter", mutate(func(p *PushTrigger) { p.PullRequest = true }),
			TriggerDrift, []string{ReasonPullRequestFilter}},
		{"branch excludes", mutate(func(p *PushTrigger) { p.BranchExcludes = []string{"tmp/*"} }),
			TriggerDrift, []string{ReasonExtraFilters}},
		{"tag filter", mutate(func(p *PushTrigger) { p.Tags = []string{"v*"} }),
			TriggerDrift, []string{ReasonExtraFilters}},
		{"second push filter", mutate(func(p *PushTrigger) { p.ExtraPushFilters = 1 }),
			TriggerDrift, []string{ReasonExtraFilters}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, reasons := ClassifyTrigger(&want, c.actual)
			if state != c.state {
				t.Errorf("state = %q, want %q", state, c.state)
			}
			if !slices.Equal(reasons, c.reasons) {
				t.Errorf("reasons = %v, want %v", reasons, c.reasons)
			}
		})
	}
}

// A pipeline we could not derive an expectation for is Unknown, never OK: a
// report that called it OK would present a gap in its own inputs as a clean
// bill of health.
func TestClassifyTriggerWithoutExpectation(t *testing.T) {
	for _, actual := range [][]PushTrigger{nil, {{SourceAction: "whatever"}}} {
		state, reasons := ClassifyTrigger(nil, actual)
		if state != TriggerUnknown || reasons != nil {
			t.Errorf("no expectation: state=%q reasons=%v, want unknown/nil", state, reasons)
		}
	}
}

func TestParseTriggerState(t *testing.T) {
	got, err := ParseTriggerState("  FETCH-ERROR ")
	if err != nil || got != TriggerFetchError {
		t.Errorf("ParseTriggerState(FETCH-ERROR) = %q, %v", got, err)
	}
	if _, err := ParseTriggerState("drifted"); err == nil {
		t.Error("a typo must be rejected, not silently selected as nothing")
	}
}

// Triage order: what needs attention first, ties broken deterministically.
func TestSortTriggers(t *testing.T) {
	items := []TriggerSummary{
		{AccountName: "zulu", State: TriggerOK},
		{AccountName: "bravo", State: TriggerUnknown},
		{AccountName: "delta", State: TriggerDrift},
		{AccountName: "alpha", State: TriggerOK},
		{AccountName: "echo", State: TriggerMissing},
		{AccountName: "charlie", State: TriggerFetchError},
	}
	SortTriggers(items)
	var got []string
	for _, it := range items {
		got = append(got, it.AccountName)
	}
	want := []string{"echo", "delta", "charlie", "bravo", "alpha", "zulu"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}
