package model

import (
	"slices"
	"testing"
)

// A fix writes only what the expectation fully describes. Any difference it
// has no slot for is a trigger someone may have meant, so the pipeline is
// refused rather than overwritten — and the refusal names exactly those
// differences, not the fixable ones riding along.
func TestPlanTriggerFix(t *testing.T) {
	want, _ := testPolicy.Expect("payments-prod")
	other := want
	other.SourceAction = "aft-global-customizations"
	withPR := want
	withPR.PullRequest = true
	withPR.FilePaths = []string{"payments-prod/terraform/*.tf"}
	narrow := want
	narrow.FilePaths = []string{"payments-prod/terraform/*.tf"}
	narrow.FilePathExcludes = nil

	tests := []struct {
		name        string
		expected    *PushTrigger
		actual      []PushTrigger
		state       TriggerState // overrides ClassifyTrigger when set
		wantAction  TriggerFixAction
		wantReasons []string
	}{
		{name: "ok", expected: &want, actual: []PushTrigger{want}, wantAction: FixNone},
		{name: "missing", expected: &want, wantAction: FixUpdate},
		{name: "narrow pattern", expected: &want, actual: []PushTrigger{narrow}, wantAction: FixUpdate},
		{name: "other source action", expected: &want, actual: []PushTrigger{other},
			wantAction: FixRefuse, wantReasons: []string{ReasonSourceAction}},
		{name: "two triggers", expected: &want, actual: []PushTrigger{want, other},
			wantAction: FixRefuse, wantReasons: []string{ReasonMultipleTriggers}},
		{name: "pull request filter", expected: &want, actual: []PushTrigger{withPR},
			wantAction: FixRefuse, wantReasons: []string{ReasonPullRequestFilter}},
		{name: "no expectation", wantAction: FixRefuse, wantReasons: []string{ReasonNoExpectation}},
		{name: "fetch error", state: TriggerFetchError, wantAction: FixRefuse,
			wantReasons: []string{ReasonFetchError}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := TriggerSummary{Expected: tc.expected, Actual: tc.actual}
			s.State, s.Reasons = ClassifyTrigger(tc.expected, tc.actual)
			if tc.state != "" {
				s.State, s.Reasons = tc.state, nil
			}
			action, reasons := PlanTriggerFix(s)
			if action != tc.wantAction {
				t.Fatalf("action = %q, want %q", action, tc.wantAction)
			}
			if !slices.Equal(reasons, tc.wantReasons) {
				t.Errorf("reasons = %v, want %v", reasons, tc.wantReasons)
			}
		})
	}
}

// The plan-versus-now check must not call a reordered pattern list a change:
// CodePipeline does not evaluate order, and neither does ClassifyTrigger.
func TestSameTriggers(t *testing.T) {
	a := PushTrigger{SourceAction: "s", Branches: []string{"main"},
		FilePaths: []string{"x/**", "y/**"}, FilePathExcludes: []string{"**/*.md"}}
	b := a
	b.FilePaths = []string{"y/**", "x/**"}
	if !SameTriggers([]PushTrigger{a}, []PushTrigger{b}) {
		t.Error("reordered file paths should compare equal")
	}
	if !SameTriggers(nil, []PushTrigger{}) {
		t.Error("nil and empty should compare equal")
	}
	c := a
	c.FilePathExcludes = nil
	if SameTriggers([]PushTrigger{a}, []PushTrigger{c}) {
		t.Error("dropped excludes should compare different")
	}
	if SameTriggers([]PushTrigger{a}, nil) {
		t.Error("a trigger and no trigger should compare different")
	}
}
