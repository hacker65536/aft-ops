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
