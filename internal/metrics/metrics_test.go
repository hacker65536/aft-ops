package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two recorders opened in the same second must not share a file.
//
// They did, when the name was second-granular: the first Close then removed
// the file for having recorded nothing, unlinking it out from under the
// other process, whose remaining entries went nowhere.
func TestRecorderFilesAreUniquePerProcess(t *testing.T) {
	dir := t.TempDir()
	a, err := NewRecorder(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// The name must still start with a fixed-width timestamp, since
	// LatestFiles orders runs by name.
	base := filepath.Base(a.Path())
	if !strings.HasSuffix(base, ".jsonl") || len(base) < len("20060102_150405") {
		t.Fatalf("unexpected metrics file name %q", base)
	}
	if _, err := os.Stat(a.Path()); err != nil {
		t.Fatalf("the file should exist: %v", err)
	}

	// The pid is what makes it unique; assert it is in there rather than
	// relying on the clock ticking between the two opens.
	if !strings.Contains(base, "_") {
		t.Errorf("name %q carries no per-process component", base)
	}
	if got := strings.Count(base, "_"); got < 2 {
		t.Errorf("name %q should be <date>_<time>_<pid>.jsonl", base)
	}
}

// LatestFiles orders by name, so the name must sort newest-first even with
// the pid appended.
func TestLatestFilesOrdersByTimestamp(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"20260725_100000_9.jsonl",
		"20260725_100001_10.jsonl",
		"20260726_090000_1.jsonl",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LatestFiles(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"20260726_090000_1.jsonl", "20260725_100001_10.jsonl", "20260725_100000_9.jsonl"}
	for i, p := range got {
		if filepath.Base(p) != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// Close removes a run that recorded nothing (every invocation opens a file,
// so keeping the empty ones would bury the useful runs) but keeps one that did.
func TestCloseRemovesOnlyEmptyRuns(t *testing.T) {
	dir := t.TempDir()

	empty, err := NewRecorder(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := empty.Path()
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("an empty run should be removed, stat err = %v", err)
	}

	used, err := NewRecorder(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	used.Record(Entry{Service: "codepipeline", Operation: "ListPipelines"})
	path = used.Path()
	if err := used.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a run with entries must be kept: %v", err)
	}
}

// A nil Recorder is the "metrics disabled" case and must be inert, not a
// panic: it is stored and called unconditionally by the CLI layer.
func TestNilRecorderIsInert(t *testing.T) {
	var r *Recorder
	r.Record(Entry{Service: "sts"})
	if r.Path() != "" {
		t.Error("a nil recorder has no path")
	}
	if err := r.Close(); err != nil {
		t.Errorf("closing a nil recorder: %v", err)
	}
}

// Nearest rank: the result is always a sample that was measured, and small
// runs report their maximum rather than an interpolation between values that
// never occurred.
func TestPercentileNearestRank(t *testing.T) {
	cases := []struct {
		name   string
		sorted []int64
		p      int
		want   int64
	}{
		{"empty", nil, 50, 0},
		{"single sample is every percentile", []int64{10}, 50, 10},
		{"single sample p99", []int64{10}, 99, 10},
		{"pair takes the lower half", []int64{10, 20}, 50, 10},
		{"pair p99 is the max", []int64{10, 20}, 99, 20},
		{"ten samples p50", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 50, 5},
		{"ten samples p99 rounds up to the max", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 99, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := percentile(c.sorted, c.p); got != c.want {
				t.Errorf("percentile(%v, %d) = %d, want %d", c.sorted, c.p, got, c.want)
			}
		})
	}
}

func TestPercentileOverAHundredSamples(t *testing.T) {
	var sorted []int64
	for i := int64(1); i <= 100; i++ {
		sorted = append(sorted, i)
	}
	if got := percentile(sorted, 50); got != 50 {
		t.Errorf("p50 = %d, want 50", got)
	}
	if got := percentile(sorted, 99); got != 99 {
		t.Errorf("p99 = %d, want 99", got)
	}
}

// The tail is the point: one slow call among fast ones must be visible at p99
// while p50 keeps reporting what a typical call cost.
func TestSummarizeReportsTailLatencyAndRates(t *testing.T) {
	var entries []Entry
	for _, ms := range []int64{10, 10, 10, 10, 10, 10, 10, 10, 10, 900} {
		entries = append(entries, Entry{
			Service: "codepipeline", Operation: "ListPipelineExecutions", DurationMs: ms,
		})
	}
	entries = append(entries,
		Entry{Service: "codepipeline", Operation: "ListPipelineExecutions",
			DurationMs: 20, Throttled: true},
		Entry{Service: "sts", Operation: "GetCallerIdentity",
			DurationMs: 5, Error: "boom"},
	)

	stats := Summarize(entries)
	if len(stats) != 2 {
		t.Fatalf("got %d operations, want 2", len(stats))
	}
	// Sorted by service then operation.
	got := stats[0]
	if got.Service != "codepipeline" || got.Calls != 11 {
		t.Fatalf("first stat = %+v", got)
	}
	if got.P50Ms != 10 {
		t.Errorf("p50 = %d, want 10 (the typical call)", got.P50Ms)
	}
	if got.P99Ms != 900 || got.MaxMs != 900 {
		t.Errorf("p99 = %d, max = %d, want the 900ms outlier in both", got.P99Ms, got.MaxMs)
	}
	if got.Throttles != 1 {
		t.Errorf("throttles = %d, want 1", got.Throttles)
	}
	if want := 100.0 / 11; got.ThrottlePc < want-0.01 || got.ThrottlePc > want+0.01 {
		t.Errorf("throttle%% = %v, want %v", got.ThrottlePc, want)
	}

	if sts := stats[1]; sts.Errors != 1 || sts.P50Ms != 5 || sts.MaxMs != 5 {
		t.Errorf("sts stat = %+v", sts)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	if got := Summarize(nil); len(got) != 0 {
		t.Errorf("Summarize(nil) = %v, want no rows", got)
	}
}
