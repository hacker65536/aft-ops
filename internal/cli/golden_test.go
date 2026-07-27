package cli

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Golden tests for the command surface: run the real command tree against the
// demo fixture and compare stdout, stderr and the exit code against a
// recorded transcript (docs/design.md §11).
//
// The fixture is what makes this possible — no AWS, no credentials, the same
// 42 pipelines every time — so these cover the parts of internal/cli that
// unit tests reach around: flag validation, target resolution, the guards,
// and the exit-code contract.
//
// Run `go test ./internal/cli -update` to re-record after an intentional
// change, then read the diff: a golden file is only worth as much as the
// review of the change to it.

var updateGolden = flag.Bool("update", false, "rewrite the golden transcripts")

// demoFixture is the same fixture the README recordings use.
const demoFixture = "../../docs/demo/fixture.json"

var goldenCases = []struct {
	name string
	args []string
}{
	{"pipeline-list", []string{"pipeline", "list"}},
	{"pipeline-list-sort-account", []string{"pipeline", "list", "--sort", "account", "--order", "asc"}},
	{"pipeline-list-status-failed", []string{"pipeline", "list", "--status", "Failed"}},
	{"pipeline-list-status-mixed-case", []string{"pipeline", "list", "--status", "failed,STOPPED"}},
	{"pipeline-list-status-unknown", []string{"pipeline", "list", "--status", "Unknown"}},
	{"pipeline-list-status-typo", []string{"pipeline", "list", "--status", "Bogus"}},
	{"pipeline-list-account-filter", []string{"pipeline", "list", "--account", "payments"}},
	{"pipeline-list-json", []string{"pipeline", "list", "--account", "payments", "-o", "json"}},
	{"pipeline-list-fail-on-error", []string{"pipeline", "list", "--account", "payments", "--fail-on-error"}},
	{"pipeline-list-bad-sort", []string{"pipeline", "list", "--sort", "bogus"}},
	{"pipeline-list-bad-output", []string{"pipeline", "list", "-o", "yaml"}},
	// Flags are merged after config.Load, so this only fails if the config
	// is re-validated afterwards. It used to pass validation and get
	// silently restored to the default of 10.
	{"pipeline-list-bad-concurrency", []string{"pipeline", "list", "--concurrency", "0"}},
	{"pipeline-show", []string{"pipeline", "show", "payments-stg"}},
	{"pipeline-show-ambiguous", []string{"pipeline", "show", "payments"}},
	{"pipeline-show-unknown", []string{"pipeline", "show", "no-such-account"}},
	{"pipeline-executions", []string{"pipeline", "executions", "payments-stg", "--limit", "3", "--actions"}},
	{"pipeline-logs", []string{"pipeline", "logs", "payments-stg"}},
	{"pipeline-release-dry-run", []string{"pipeline", "release", "payments-stg", "--dry-run"}},
	{"pipeline-release-status-typo", []string{"pipeline", "release", "--status", "Faild", "--dry-run"}},
	{"pipeline-release-no-targets", []string{"pipeline", "release"}},
	{"pipeline-release-unknown-target", []string{"pipeline", "release", "ghost", "--dry-run"}},
	{"account-list", []string{"account", "list"}},
}

func TestGolden(t *testing.T) {
	for _, c := range goldenCases {
		t.Run(c.name, func(t *testing.T) {
			got := transcript(c.args, runCLI(t, c.args...))
			path := filepath.Join("testdata", "golden", c.name+".txt")

			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run `go test ./internal/cli -update` to record)", err)
			}
			if got != string(want) {
				t.Errorf("transcript differs from %s\n--- got ---\n%s\n--- want ---\n%s",
					path, got, want)
			}
		})
	}
}

// result is one command run's observable output.
type result struct {
	stdout string
	stderr string
	code   int
}

// runCLI executes the command tree in-process with stdout, stderr and the
// exit code captured.
//
// In-process rather than by exec: Run is the same entry point main uses, so
// the exit codes here are the real ones, and no built binary has to exist for
// `go test ./...` to work.
func runCLI(t *testing.T, args ...string) result {
	t.Helper()

	// Each run gets its own XDG root. The caches are exactly what makes two
	// runs of the same command differ ("42 refetched" the first time, "40
	// from cache" the second), and a test must not write to the developer's
	// real ~/.cache either way.
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "AFT_OPS_") {
			t.Setenv(k, "")
		}
	}
	// The fixture's own 40ms per call, and the 8 rps admission rate meant for
	// a real account's throttling limits, would otherwise make a suite of
	// full fan-outs take a minute. Neither changes what is printed.
	t.Setenv("AFT_OPS_DEMO_LATENCY", "0s")
	t.Setenv("AFT_OPS_RPS", "0")

	stdout := filepath.Join(root, "stdout")
	stderr := filepath.Join(root, "stderr")
	fo, err := os.Create(stdout)
	if err != nil {
		t.Fatal(err)
	}
	defer fo.Close()
	fe, err := os.Create(stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer fe.Close()

	// Files, not pipes: a pipe with no reader blocks once its buffer fills,
	// and `pipeline list` alone outruns it.
	oldOut, oldErr, oldArgs := os.Stdout, os.Stderr, os.Args
	defer func() { os.Stdout, os.Stderr, os.Args = oldOut, oldErr, oldArgs }()
	os.Stdout, os.Stderr = fo, fe
	os.Args = append([]string{"aft-ops", "--demo", demoFixture}, args...)

	code := Run(context.Background())

	return result{stdout: readFile(t, stdout), stderr: readFile(t, stderr), code: code}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return normalize(string(b))
}

// transcript is the recorded form: the command, its exit code, and each
// stream labelled. Keeping all three in one file means a change to any of
// them shows up in the same diff.
func transcript(args []string, r result) string {
	var b strings.Builder
	b.WriteString("$ aft-ops " + strings.Join(args, " ") + "\n")
	b.WriteString("exit " + strconv.Itoa(r.code) + "\n")
	b.WriteString("--- stdout ---\n")
	b.WriteString(r.stdout)
	b.WriteString("--- stderr ---\n")
	b.WriteString(r.stderr)
	return b.String()
}

var (
	// RFC 3339, as JSON output carries it.
	rfc3339Re = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	// The table columns' "2026-07-27 09:41" half of "2026-07-27 09:41 (2m ago)".
	wallClockRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}`)
)

// normalize replaces the wall clock, and only the wall clock.
//
// The fixture places every execution relative to the moment it is loaded, so
// the ages ("2m ago") and the durations are the same on every run and stay in
// the transcript — they are half of what these tests are checking. The
// absolute timestamps those ages are printed beside are not: they move with
// the calendar. The replacement is deliberately the same width as what it
// replaces, so the recorded table stays column-aligned and readable.
func normalize(s string) string {
	s = rfc3339Re.ReplaceAllString(s, "<TIMESTAMP>")
	return wallClockRe.ReplaceAllString(s, "YYYY-MM-DD HH:MM")
}
