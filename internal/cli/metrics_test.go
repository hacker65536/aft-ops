package cli

import (
	"strings"
	"testing"
)

// `metrics show` must not report on itself. Its own recording is the newest
// file in the directory, so the default of one run used to aggregate the
// invocation doing the aggregating: "1 run(s), 0 call attempts" and an empty
// table, with the numbers the operator wanted sitting in the run just below.
func TestReportFilesExcludesTheReportingRun(t *testing.T) {
	// Newest first, as LatestFiles returns them.
	all := []string{"03_own.jsonl", "02.jsonl", "01.jsonl"}

	got := reportFiles(all, "03_own.jsonl", 1)
	if len(got) != 1 || got[0] != "02.jsonl" {
		t.Errorf("--last 1 = %v, want the newest run that is not ours", got)
	}

	// Dropping ours before truncating is what makes --last mean what it says:
	// filtering afterwards would have returned one run here.
	got = reportFiles(all, "03_own.jsonl", 2)
	if strings.Join(got, ",") != "02.jsonl,01.jsonl" {
		t.Errorf("--last 2 = %v, want two real runs", got)
	}

	got = reportFiles(all, "03_own.jsonl", 0)
	if strings.Join(got, ",") != "02.jsonl,01.jsonl" {
		t.Errorf("--last 0 = %v, want every real run", got)
	}
}

// With metrics disabled there is no run of our own to exclude, and nothing
// should be dropped on the strength of an empty path.
func TestReportFilesKeepsEverythingWhenNotRecording(t *testing.T) {
	all := []string{"02.jsonl", "01.jsonl"}
	if got := reportFiles(all, "", 0); strings.Join(got, ",") != "02.jsonl,01.jsonl" {
		t.Errorf("got %v, want both runs", got)
	}
}

// A first-ever run has nothing but itself to report on, and says so rather
// than printing a table of zeroes.
func TestReportFilesEmptyWhenOnlyOurOwnRunExists(t *testing.T) {
	if got := reportFiles([]string{"01_own.jsonl"}, "01_own.jsonl", 1); len(got) != 0 {
		t.Errorf("got %v, want no runs to report", got)
	}
}

// The input is not the caller's to keep: LatestFiles' slice must survive.
func TestReportFilesDoesNotMutateItsInput(t *testing.T) {
	all := []string{"02_own.jsonl", "01.jsonl"}
	reportFiles(all, "02_own.jsonl", 0)
	if strings.Join(all, ",") != "02_own.jsonl,01.jsonl" {
		t.Errorf("input was modified: %v", all)
	}
}
