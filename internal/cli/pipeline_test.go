package cli

import (
	"bytes"
	"errors"
	"fmt"
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

// A target resolves on any of the three identities, exactly.
func TestResolveOneExactMatches(t *testing.T) {
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
		got, err := resolveOne(items, c.query)
		if err != nil {
			t.Errorf("resolveOne(%q): %v", c.query, err)
			continue
		}
		if got.AccountName != c.want {
			t.Errorf("resolveOne(%q) = %s, want %s", c.query, got.AccountName, c.want)
		}
	}
}

// The point of the whole rule: a substring never resolves, however many
// pipelines it happens to match. "alpha" matching one account today would
// match two after the next one is vended, and a singular argument must not
// change meaning underneath a runbook.
func TestResolveOneRejectsSubstrings(t *testing.T) {
	items := fixture()
	for _, q := range []string{"alpha", "alph", "2222", "brav"} {
		if got, err := resolveOne(items, q); err == nil {
			t.Errorf("resolveOne(%q) resolved to %s, want a refusal", q, got.AccountName)
		}
	}
}

// A refusal has to be actionable: name the near misses, and say which flag
// selects the group on purpose.
func TestResolveOneNamesNearMisses(t *testing.T) {
	_, err := resolveOne(fixture(), "alpha")
	var te *targetError
	if !errors.As(err, &te) {
		t.Fatalf("want *targetError, got %T (%v)", err, err)
	}
	if te.ambiguous {
		t.Error("a substring miss is not an ambiguity: nothing matched exactly")
	}
	if len(te.candidates) != 2 {
		t.Fatalf("candidates = %v, want both alpha rows", pipelineNames(te.candidates))
	}
	msg := te.Error()
	for _, want := range []string{"did you mean", "alpha-prod", "alpha-dev"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q should contain %q", msg, want)
		}
	}
}

// Two accounts can share a name; then the query really is undecidable and the
// message says so rather than offering a "did you mean".
func TestResolveOneAmbiguousExactMatch(t *testing.T) {
	items := append(fixture(), sum("bravo", "444444444444", model.StatusSucceeded))
	_, err := resolveOne(items, "bravo")
	var te *targetError
	if !errors.As(err, &te) {
		t.Fatalf("want *targetError, got %T (%v)", err, err)
	}
	if !te.ambiguous || len(te.candidates) != 2 {
		t.Fatalf("want an ambiguity over 2 rows, got ambiguous=%v %v",
			te.ambiguous, pipelineNames(te.candidates))
	}
	if msg := te.Error(); !strings.Contains(msg, "matches 2 pipelines") {
		t.Errorf("message %q should report the ambiguity", msg)
	}
}

func TestResolveOneUnmatchedAndBlank(t *testing.T) {
	for _, q := range []string{"nothing-matches", "   ", ""} {
		_, err := resolveOne(fixture(), q)
		var te *targetError
		if !errors.As(err, &te) {
			t.Fatalf("resolveOne(%q): want *targetError, got %v", q, err)
		}
		if len(te.candidates) != 0 {
			t.Errorf("resolveOne(%q) offered candidates %v", q, pipelineNames(te.candidates))
		}
		if msg := te.Error(); !strings.Contains(msg, "no pipeline matches") {
			t.Errorf("resolveOne(%q) message = %q", q, msg)
		}
	}
}

// The hint tells the operator that --account selects the whole group, so the
// near misses it counts must be exactly the rows --account would select.
func TestNearMissesMatchTheAccountFilter(t *testing.T) {
	items := fixture()
	for _, q := range []string{"alpha", "2222", "brav"} {
		_, err := resolveOne(items, q)
		var te *targetError
		if !errors.As(err, &te) {
			t.Fatalf("resolveOne(%q): want *targetError, got %v", q, err)
		}
		near := strings.Join(pipelineNames(te.candidates), ",")
		filtered := strings.Join(pipelineNames(filterSummaries(items, nil, q)), ",")
		if near != filtered {
			t.Errorf("query %q: candidates=%v, --account selects=%v", q, near, filtered)
		}
	}
}

// A short query can match most of a large fleet; the message stays readable.
func TestTargetErrorCapsCandidates(t *testing.T) {
	var items []model.PipelineSummary
	for i := range maxCandidates + 5 {
		items = append(items, sum(fmt.Sprintf("team-%02d", i),
			fmt.Sprintf("%012d", i), model.StatusSucceeded))
	}
	_, err := resolveOne(items, "team")
	msg := err.Error()
	if lines := strings.Count(msg, "\n  "); lines != maxCandidates+1 {
		t.Errorf("message lists %d indented lines, want %d candidates plus the elision:\n%s",
			lines, maxCandidates+1, msg)
	}
	if !strings.Contains(msg, "and 5 more") {
		t.Errorf("message should say how many were elided:\n%s", msg)
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
	got, err := selectTargets(items, targetQuery{
		args: []string{"alpha-dev", "222222222222",
			"222222222222-customizations-pipeline"},
		statuses: []string{"Failed"},
	})
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
	_, err := selectTargets(fixture(), targetQuery{args: []string{"alpha-dev", "ghost"}})
	if err == nil {
		t.Fatal("an unmatched target should be an error")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the offending query, got %v", err)
	}
}

// A substring given as an argument is refused, and the refusal names the flag
// that would have selected the group deliberately.
func TestSelectTargetsSubstringSuggestsTheAccountFlag(t *testing.T) {
	_, err := selectTargets(fixture(), targetQuery{args: []string{"alpha"}, verb: "release"})
	if err == nil {
		t.Fatal("a substring argument should not resolve to two pipelines")
	}
	if want := "to release all 2, pass --account alpha"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q should contain %q", err, want)
	}
}

// --account is the set-shaped form: it selects the group the bare argument
// was refused for.
func TestSelectTargetsAccountSelectsTheGroup(t *testing.T) {
	got, err := selectTargets(fixture(), targetQuery{account: "alpha"})
	if err != nil {
		t.Fatalf("selectTargets: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("targets = %v, want both alpha rows", pipelineNames(got))
	}
}

// --account and --status intersect, matching `pipeline list`; named targets
// union in on top of that.
func TestSelectTargetsAccountAndStatusIntersect(t *testing.T) {
	items := fixture()
	got, err := selectTargets(items, targetQuery{account: "alpha", statuses: []string{"Failed"}})
	if err != nil {
		t.Fatalf("selectTargets: %v", err)
	}
	if len(got) != 1 || got[0].AccountName != "alpha-dev" {
		t.Fatalf("targets = %v, want only the failed alpha row", pipelineNames(got))
	}

	got, err = selectTargets(items, targetQuery{
		args: []string{"bravo"}, account: "alpha", statuses: []string{"Failed"}})
	if err != nil {
		t.Fatalf("selectTargets: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("targets = %v, want the named row plus the filtered one", pipelineNames(got))
	}
}

// An unselective query is a caller bug, not an empty result: every command
// checks it before touching AWS.
func TestTargetQueryEmpty(t *testing.T) {
	if !(targetQuery{}).empty() {
		t.Error("a query with no selector should be empty")
	}
	for _, q := range []targetQuery{
		{args: []string{"alpha-dev"}},
		{file: "targets.txt"},
		{account: "alpha"},
		{statuses: []string{"Failed"}},
	} {
		if q.empty() {
			t.Errorf("%+v should not be empty", q)
		}
	}
}

func TestSelectTargetsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.txt")
	content := "# accounts to re-release\nalpha-dev\n\n  bravo  \n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := selectTargets(fixture(), targetQuery{file: path})
	if err != nil {
		t.Fatalf("selectTargets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("targets = %v, want alpha-dev and bravo (comments/blanks dropped)", pipelineNames(got))
	}
}

// A file is an explicit list, so its lines resolve by the same exact rule as
// arguments — one line must not expand into three pipelines.
func TestSelectTargetsFileLinesResolveExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := selectTargets(fixture(), targetQuery{file: path}); err == nil {
		t.Fatal("a substring line should be refused just like an argument")
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

func TestParseStatusFiltersCanonicalizes(t *testing.T) {
	got, err := parseStatusFilters([]string{"failed", "  INPROGRESS "})
	if err != nil {
		t.Fatalf("parseStatusFilters: %v", err)
	}
	if len(got) != 2 || got[0] != "Failed" || got[1] != "InProgress" {
		t.Errorf("parseStatusFilters = %v, want [Failed InProgress]", got)
	}
	if _, err := parseStatusFilters([]string{"Failed", "Faild"}); err == nil {
		t.Error("a typo among valid values must still be reported")
	} else if !strings.Contains(err.Error(), "Faild") {
		t.Errorf("error should name the offending value, got %v", err)
	}
}

// A pipeline whose status could not be fetched is listed as "fetch-error",
// so that is the value that has to select it — not the Unknown it degrades
// to internally.
func TestFilterSummariesMatchesFetchErrorRows(t *testing.T) {
	items := fixture()
	items = append(items, model.PipelineSummary{
		PipelineName: "444444444444-customizations-pipeline",
		AccountID:    "444444444444",
		AccountName:  "charlie",
		FetchError:   "ThrottlingException",
	})

	got := filterSummaries(items, []string{"fetch-error"}, "")
	if len(got) != 1 || got[0].AccountName != "charlie" {
		t.Errorf("fetch-error filter = %v, want [charlie]", pipelineNames(got))
	}
	if got := filterSummaries(items, []string{"Unknown"}, ""); len(got) != 0 {
		t.Errorf("Unknown must not pick up fetch-error rows, got %v", pipelineNames(got))
	}
}
