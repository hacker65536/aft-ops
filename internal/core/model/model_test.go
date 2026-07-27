package model

import (
	"testing"
	"time"
)

func at(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

func sumT(name string, upd *time.Time, st Status) PipelineSummary {
	var e *Execution
	if upd != nil || st != "" {
		e = &Execution{Status: st, LastUpdate: upd}
	}
	return PipelineSummary{PipelineName: name, AccountID: name, AccountName: name, Latest: e}
}

func names(items []PipelineSummary) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.AccountName
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSortByLastUpdateDescNilSinks(t *testing.T) {
	items := []PipelineSummary{
		sumT("old", at("2026-01-01T00:00:00Z"), StatusSucceeded),
		sumT("none", nil, StatusUnknown),
		sumT("new", at("2026-07-01T00:00:00Z"), StatusFailed),
	}
	SortSummaries(items, SortByLastUpdate, OrderDesc)
	eq(t, names(items), []string{"new", "old", "none"})
}

func TestSortByLastUpdateAscNilStillSinks(t *testing.T) {
	items := []PipelineSummary{
		sumT("new", at("2026-07-01T00:00:00Z"), StatusFailed),
		sumT("none", nil, StatusUnknown),
		sumT("old", at("2026-01-01T00:00:00Z"), StatusSucceeded),
	}
	SortSummaries(items, SortByLastUpdate, OrderAsc)
	// ascending by time, but the never-run row still goes last.
	eq(t, names(items), []string{"old", "new", "none"})
}

func TestSortByAccount(t *testing.T) {
	items := []PipelineSummary{
		sumT("charlie", at("2026-01-01T00:00:00Z"), StatusSucceeded),
		sumT("alpha", at("2026-01-01T00:00:00Z"), StatusSucceeded),
		sumT("bravo", at("2026-01-01T00:00:00Z"), StatusSucceeded),
	}
	SortSummaries(items, SortByAccount, OrderAsc)
	eq(t, names(items), []string{"alpha", "bravo", "charlie"})
	SortSummaries(items, SortByAccount, OrderDesc)
	eq(t, names(items), []string{"charlie", "bravo", "alpha"})
}

// Sorting by status ranks by urgency, not alphabetically: the point of the
// view is triage, so Failed leads and Succeeded trails.
func TestSortByStatus(t *testing.T) {
	items := []PipelineSummary{
		sumT("s", at("2026-01-01T00:00:00Z"), StatusSucceeded),
		sumT("c", at("2026-01-01T00:00:00Z"), StatusCancelled),
		sumT("f", at("2026-01-01T00:00:00Z"), StatusFailed),
		sumT("i", at("2026-01-01T00:00:00Z"), StatusInProgress),
	}
	SortSummaries(items, SortByStatus, OrderAsc)
	eq(t, names(items), []string{"f", "i", "c", "s"})

	SortSummaries(items, SortByStatus, OrderDesc)
	eq(t, names(items), []string{"s", "c", "i", "f"})
}

// Message unwraps the CodeConnections JSON RevisionSummary and falls back
// to plain text (or empty) otherwise.
func TestRevisionMessage(t *testing.T) {
	cases := []struct {
		name, summary, want string
	}{
		{"codeconnections json",
			`{"ProviderType":"GitHub","CommitMessage":"Merge pull request #801 from C-FO/x"}`,
			"Merge pull request #801 from C-FO/x"},
		{"plain text", "fix vpc", "fix vpc"},
		{"json without CommitMessage stays raw", `{"ProviderType":"GitHub"}`, `{"ProviderType":"GitHub"}`},
		{"invalid json stays raw", "{not json", "{not json"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := (Revision{Summary: c.summary}).Message(); got != c.want {
			t.Errorf("%s: Message() = %q, want %q", c.name, got, c.want)
		}
	}
}

// LogActions keeps every build of an execution, in pipeline order, and drops
// the actions that have no log (source actions).
func TestLogActions(t *testing.T) {
	acts := []ActionExecution{
		{StageName: "Source", ActionName: "aft-global-customizations", Status: StatusSucceeded},
		{StageName: "AFT-Global-Customizations", ActionName: "aft-global-customizations",
			Status: StatusSucceeded, CodeBuildID: "global:uuid"},
		{StageName: "AFT-Account-Customizations", ActionName: "aft-account-customizations",
			Status: StatusFailed, CodeBuildID: "account:uuid"},
	}

	got := LogActions(acts)
	if len(got) != 2 {
		t.Fatalf("LogActions returned %d builds, want 2", len(got))
	}
	if got[0].CodeBuildID != "global:uuid" || got[1].CodeBuildID != "account:uuid" {
		t.Errorf("LogActions = %q/%q, want global then account",
			got[0].CodeBuildID, got[1].CodeBuildID)
	}
	if got := LogActions(acts[:1]); len(got) != 0 {
		t.Errorf("LogActions with no build ids = %+v, want none", got)
	}
}

// BuildActions flattens a pipeline's state to its logs, keeping each build's
// stage — the two customizations stages run identically named actions, so the
// stage is the only thing that tells their logs apart.
func TestBuildActions(t *testing.T) {
	d := PipelineDetail{Stages: []StageState{
		{Name: "Source", Actions: []ActionState{{Name: "aft-global-customizations"}}},
		{Name: "AFT-Global-Customizations", Actions: []ActionState{
			{Name: "Apply", Status: StatusSucceeded, CodeBuildID: "global:uuid"},
		}},
		{Name: "AFT-Account-Customizations", Actions: []ActionState{
			{Name: "Apply", Status: StatusFailed, CodeBuildID: "account:uuid"},
		}},
	}}

	got := d.BuildActions()
	want := []StageAction{
		{Stage: "AFT-Global-Customizations", Action: d.Stages[1].Actions[0]},
		{Stage: "AFT-Account-Customizations", Action: d.Stages[2].Actions[0]},
	}
	if len(got) != len(want) {
		t.Fatalf("BuildActions = %+v, want both builds %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("BuildActions[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if got := (PipelineDetail{Stages: d.Stages[:1]}).BuildActions(); len(got) != 0 {
		t.Errorf("a source-only pipeline has no logs, got %+v", got)
	}
}

// CodePipeline leaves lastUpdateTime equal to startTime while an execution is
// in flight, so the recorded span is 0s for a run that may have been going for
// minutes. Elapsed counts up to now until the run reaches a terminal status.
func TestElapsedCountsUpWhileInFlight(t *testing.T) {
	start := at("2026-07-26T00:58:35Z")
	now := start.Add(3*time.Minute + 25*time.Second)

	inFlight := Execution{Status: StatusInProgress, StartTime: start, LastUpdate: start}
	if got, want := inFlight.Elapsed(now), 3*time.Minute+25*time.Second; got != want {
		t.Errorf("in-flight Elapsed = %v, want %v", got, want)
	}

	done := Execution{
		Status:     StatusSucceeded,
		StartTime:  start,
		LastUpdate: at("2026-07-26T01:03:06Z"),
	}
	if got, want := done.Elapsed(now), 4*time.Minute+31*time.Second; got != want {
		t.Errorf("terminal Elapsed = %v, want the recorded span %v", got, want)
	}

	act := ActionExecution{Status: StatusInProgress, StartTime: start, LastUpdate: start}
	if got, want := act.Elapsed(now), 3*time.Minute+25*time.Second; got != want {
		t.Errorf("in-flight action Elapsed = %v, want %v", got, want)
	}
}

func TestElapsedNeverGoesNegative(t *testing.T) {
	start := at("2026-07-26T00:58:35Z")
	past := start.Add(-time.Minute) // clock skew: start is in the future

	if got := (Execution{Status: StatusInProgress, StartTime: start}).Elapsed(past); got != 0 {
		t.Errorf("Elapsed with start in the future = %v, want 0", got)
	}
	if got := (Execution{Status: StatusInProgress}).Elapsed(past); got != 0 {
		t.Errorf("Elapsed without a start time = %v, want 0", got)
	}
	if got := (Execution{Status: StatusSucceeded, StartTime: start}).Elapsed(past); got != 0 {
		t.Errorf("terminal Elapsed without an end time = %v, want 0", got)
	}
}

func TestParseStatusFilterAccepts(t *testing.T) {
	cases := map[string]Status{
		"Failed":      StatusFailed,
		"failed":      StatusFailed, // case-insensitive
		"  Failed  ":  StatusFailed, // trimmed
		"INPROGRESS":  StatusInProgress,
		"Unknown":     StatusUnknown,    // selects never-run pipelines
		"fetch-error": StatusFetchError, // what the STATUS column prints
	}
	for in, want := range cases {
		got, err := ParseStatusFilter(in)
		if err != nil {
			t.Errorf("ParseStatusFilter(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseStatusFilter(%q) = %q, want %q", in, got, want)
		}
	}
}

// The whole point of the filter parser: a misspelling must be reported, not
// quietly turned into Unknown the way ParseStatus does for AWS input.
func TestParseStatusFilterRejectsTypos(t *testing.T) {
	for _, in := range []string{"Faild", "Bogus", "", "succeed"} {
		if got, err := ParseStatusFilter(in); err == nil {
			t.Errorf("ParseStatusFilter(%q) = %q, want an error", in, got)
		}
	}
	if got := ParseStatus("Faild"); got != StatusUnknown {
		t.Errorf("ParseStatus still normalizes AWS input: got %q", got)
	}
}

// Every filterable value must have a spelling the parser accepts, so the
// list in the flag help can never drift from what validation allows.
func TestFilterableStatusesRoundTrip(t *testing.T) {
	for _, s := range FilterableStatuses {
		got, err := ParseStatusFilter(string(s))
		if err != nil || got != s {
			t.Errorf("ParseStatusFilter(%q) = %q, %v", s, got, err)
		}
	}
}
