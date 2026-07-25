package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

func sum(name, id string, st model.Status) model.PipelineSummary {
	return model.PipelineSummary{
		PipelineName: id + "-customizations-pipeline",
		AccountID:    id,
		AccountName:  name,
		Latest:       &model.Execution{Status: st},
	}
}

func fixture() []model.PipelineSummary {
	return []model.PipelineSummary{
		sum("alpha-prod", "111111111111", model.StatusSucceeded),
		sum("alpha-dev", "222222222222", model.StatusFailed),
		sum("bravo", "333333333333", model.StatusInProgress),
	}
}

func pipelineNames(items []model.PipelineSummary) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.AccountName
	}
	return out
}

// An exact match on any of the three identities wins outright — a query that
// exactly names one account must never also drag in its substring neighbours.
func TestMatchSummariesExactWins(t *testing.T) {
	items := fixture()
	cases := []struct {
		query string
		want  string
	}{
		{"alpha-dev", "alpha-dev"},                        // exact account name
		{"ALPHA-DEV", "alpha-dev"},                        // case-insensitive
		{"  alpha-dev  ", "alpha-dev"},                    // trimmed
		{"111111111111", "alpha-prod"},                    // exact account id
		{"333333333333-customizations-pipeline", "bravo"}, // exact pipeline name
		{"333333333333-CUSTOMIZATIONS-PIPELINE", "bravo"}, // case-insensitive
	}
	for _, c := range cases {
		got := matchSummaries(items, c.query)
		if len(got) != 1 || got[0].AccountName != c.want {
			t.Errorf("matchSummaries(%q) = %v, want exactly [%s]", c.query, pipelineNames(got), c.want)
		}
	}
}

// Substring matching covers both the account name and the account id, so a
// target query selects the same rows as `list --account`.
func TestMatchSummariesSubstring(t *testing.T) {
	items := fixture()

	got := matchSummaries(items, "alpha")
	if len(got) != 2 {
		t.Errorf("substring on name = %v, want both alpha rows", pipelineNames(got))
	}

	got = matchSummaries(items, "2222")
	if len(got) != 1 || got[0].AccountID != "222222222222" {
		t.Errorf("substring on account id = %v, want the 2222… row", pipelineNames(got))
	}

	if got := matchSummaries(items, "nothing-matches"); len(got) != 0 {
		t.Errorf("unmatched query returned %v", pipelineNames(got))
	}
	if got := matchSummaries(items, "   "); len(got) != 0 {
		t.Errorf("blank query returned %v", pipelineNames(got))
	}
}

// matchSummaries and filterSummaries must agree: the same query selects the
// same rows whether it is a release target or a list filter.
func TestMatchAndFilterAgreeOnSubstrings(t *testing.T) {
	items := fixture()
	for _, q := range []string{"alpha", "2222", "bravo"} {
		matched := pipelineNames(matchSummaries(items, q))
		filtered := pipelineNames(filterSummaries(items, nil, q))
		if strings.Join(matched, ",") != strings.Join(filtered, ",") {
			t.Errorf("query %q: match=%v filter=%v", q, matched, filtered)
		}
	}
}

func TestFilterSummariesByStatus(t *testing.T) {
	items := fixture()
	got := filterSummaries(items, []string{"failed"}, "") // case-insensitive
	if len(got) != 1 || got[0].AccountName != "alpha-dev" {
		t.Errorf("status filter = %v, want [alpha-dev]", pipelineNames(got))
	}
	got = filterSummaries(items, []string{"Failed", "InProgress"}, "")
	if len(got) != 2 {
		t.Errorf("multi-status filter = %v, want 2 rows", pipelineNames(got))
	}
	got = filterSummaries(items, []string{"Failed"}, "bravo") // both must hold
	if len(got) != 0 {
		t.Errorf("status+account filter = %v, want none", pipelineNames(got))
	}
}

func TestSelectTargetsDedupes(t *testing.T) {
	items := fixture()
	// The same pipeline reached three ways plus a status sweep that includes
	// it again must yield one entry.
	got, err := selectTargets(items,
		[]string{"alpha-dev", "222222222222", "222222222222-customizations-pipeline"},
		"", []string{"Failed"})
	if err != nil {
		t.Fatalf("selectTargets: %v", err)
	}
	if len(got) != 1 || got[0].AccountName != "alpha-dev" {
		t.Errorf("targets = %v, want exactly [alpha-dev]", pipelineNames(got))
	}
}

// An unmatched query is an error rather than a silent skip: releasing fewer
// pipelines than asked for is worse than refusing.
func TestSelectTargetsUnknownIsAnError(t *testing.T) {
	if _, err := selectTargets(fixture(), []string{"alpha-dev", "ghost"}, "", nil); err == nil {
		t.Fatal("an unmatched target should be an error")
	} else if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the offending query, got %v", err)
	}
}

func TestSelectTargetsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.txt")
	content := "# accounts to re-release\nalpha-dev\n\n  bravo  \n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := selectTargets(fixture(), nil, path, nil)
	if err != nil {
		t.Fatalf("selectTargets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("targets = %v, want alpha-dev and bravo (comments/blanks dropped)", pipelineNames(got))
	}
}

func TestReadLinesSkipsCommentsAndBlanks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(path, []byte("a\n\n# comment\n  b  \n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readLines(path)
	if err != nil {
		t.Fatalf("readLines: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("readLines = %v, want [a b]", got)
	}
}

func TestSummariesFromNames(t *testing.T) {
	got := summariesFromNames([]string{"111111111111-customizations-pipeline"}, nil)
	if len(got) != 1 || got[0].AccountID != "111111111111" {
		t.Fatalf("summariesFromNames = %+v", got)
	}
	if got[0].Latest != nil {
		t.Error("status-free summaries must not carry an execution")
	}
}

func TestWriteLogSectionsSingleBuildHasNoRule(t *testing.T) {
	var b bytes.Buffer
	writeLogSections(&b, []logSection{{
		Stage:  "AFT-Account-Customizations",
		Action: "Apply",
		Lines:  []string{"Apply complete!"},
	}})

	if got, want := b.String(), "Apply complete!\n"; got != want {
		t.Errorf("single build output = %q, want the log alone %q", got, want)
	}
}

// An AFT customizations execution applies terraform twice, and either half may
// be the one that changed something, so both are printed and each is labelled
// by its stage — both actions are called "Apply".
func TestWriteLogSectionsLabelsEachBuildByStage(t *testing.T) {
	var b bytes.Buffer
	writeLogSections(&b, []logSection{
		{Stage: "AFT-Global-Customizations", Action: "Apply",
			Lines: []string{"Apply complete! Resources: 0 added, 1 changed, 0 destroyed."}},
		{Stage: "AFT-Account-Customizations", Action: "Apply",
			Lines: []string{"No changes."}},
	})

	want := "──── AFT-Global-Customizations / Apply ────\n" +
		"Apply complete! Resources: 0 added, 1 changed, 0 destroyed.\n" +
		"\n" +
		"──── AFT-Account-Customizations / Apply ────\n" +
		"No changes.\n"
	if got := b.String(); got != want {
		t.Errorf("two-build output =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteLogSectionsKeepsUnreadableBuildVisible(t *testing.T) {
	var b bytes.Buffer
	writeLogSections(&b, []logSection{
		{Stage: "AFT-Global-Customizations", Action: "Apply",
			Error: "boom", Lines: []string{"(log unavailable: boom)"}},
		{Stage: "AFT-Account-Customizations", Action: "Apply",
			Lines: []string{"No changes."}},
	})

	out := b.String()
	if !strings.Contains(out, "AFT-Global-Customizations / Apply") ||
		!strings.Contains(out, "(log unavailable: boom)") {
		t.Errorf("a failed fetch must leave a labelled gap, got\n%s", out)
	}
	if !strings.Contains(out, "No changes.") {
		t.Errorf("one unreadable build hid the other, got\n%s", out)
	}
}
