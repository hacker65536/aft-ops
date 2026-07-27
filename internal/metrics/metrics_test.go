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
