package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// truncate must cut on display width, never mid-rune: commit messages and
// error text are routinely non-ASCII, and a byte-sliced cut emits invalid
// UTF-8 that the terminal renders as garbage.
func TestTruncateIsRuneSafe(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
	}{
		{"ascii", "the quick brown fox jumps over the lazy dog", 20},
		{"japanese", "リリース対応: ネットワーク標準の見直しとアカウント設定の更新", 20},
		{"mixed", "fix(vpc): サブネット追加 for account 123456789012", 24},
		{"emoji", "deploy 🚀 to production", 10},
	}
	for _, c := range cases {
		got := truncate(c.in, c.width)
		if !utf8.ValidString(got) {
			t.Errorf("%s: truncate produced invalid UTF-8: %q", c.name, got)
		}
		if w := ansi.StringWidth(got); w > c.width {
			t.Errorf("%s: truncate returned width %d, want <= %d (%q)", c.name, w, c.width, got)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("%s: a truncated string should end in an ellipsis, got %q", c.name, got)
		}
	}
}

func TestTruncateLeavesShortStringsAlone(t *testing.T) {
	for _, s := range []string{"", "short", "日本語"} {
		if got := truncate(s, 20); got != s {
			t.Errorf("truncate(%q) = %q, want it unchanged", s, got)
		}
	}
	if got := truncate("line one\nline two", 40); strings.Contains(got, "\n") {
		t.Errorf("truncate should flatten newlines, got %q", got)
	}
}

func summary(name, id string, st model.Status, upd *time.Time) model.PipelineSummary {
	return model.PipelineSummary{
		PipelineName: id + "-customizations-pipeline",
		AccountID:    id,
		AccountName:  name,
		Latest:       &model.Execution{ID: "abcdef1234567890", Status: st, LastUpdate: upd},
	}
}

func TestPipelineTableRendersRows(t *testing.T) {
	now := time.Now()
	var b bytes.Buffer
	PipelineTable(&b, []model.PipelineSummary{
		summary("alpha", "111111111111", model.StatusSucceeded, &now),
		{PipelineName: "222222222222-customizations-pipeline", AccountID: "222222222222",
			FetchError: "throttled"},
	}, false)

	out := b.String()
	for _, want := range []string{"ACCOUNT NAME", "alpha", "111111111111", "Succeeded", "abcdef12", "fetch-error"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	// The account-less row must still render, with a placeholder name.
	if !strings.Contains(out, "-  ") && !strings.Contains(out, "- ") {
		t.Errorf("row without an account name should show a placeholder:\n%s", out)
	}
}

func TestPipelineCounts(t *testing.T) {
	now := time.Now()
	var b bytes.Buffer
	PipelineCounts(&b, []model.PipelineSummary{
		summary("a", "111111111111", model.StatusSucceeded, &now),
		summary("b", "222222222222", model.StatusFailed, &now),
		summary("c", "333333333333", model.StatusSucceeded, &now),
		{PipelineName: "444444444444-customizations-pipeline", FetchError: "boom"},
	})
	got := strings.TrimSpace(b.String())
	want := "total=4 Succeeded=2 Failed=1 fetch-error=1"
	if got != want {
		t.Errorf("counts = %q, want %q", got, want)
	}
}

// StatusFreshness reports the core's own numbers, including failed refreshes
// (which keep serving a previous value and would otherwise be invisible).
func TestStatusFreshness(t *testing.T) {
	var b bytes.Buffer
	StatusFreshness(&b, model.StatusStats{Fetched: 5})
	if got := strings.TrimSpace(b.String()); got != "statuses: 5 refetched" {
		t.Errorf("all-fresh line = %q", got)
	}

	b.Reset()
	StatusFreshness(&b, model.StatusStats{
		Fetched:   2,
		FromCache: 180,
		Failed:    1,
		Oldest:    time.Now().Add(-90 * time.Second),
		TTL:       10 * time.Minute,
	})
	got := b.String()
	for _, want := range []string{"2 refetched", "180 from cache", "oldest 1m ago", "ttl 10m", "1 refetch(es) failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("freshness line missing %q: %q", want, got)
		}
	}

	b.Reset()
	StatusFreshness(&b, model.StatusStats{})
	if b.Len() != 0 {
		t.Errorf("empty stats should print nothing, got %q", b.String())
	}
}

func TestReleaseTableCoversEveryOutcome(t *testing.T) {
	var b bytes.Buffer
	ReleaseTable(&b, []model.ReleaseResult{
		{PipelineName: "a", AccountName: "alpha", AccountID: "111111111111", ExecutionID: "exec-abcdef12345"},
		{PipelineName: "b", AccountName: "bravo", AccountID: "222222222222", Skipped: true, SkipReason: "InProgress"},
		{PipelineName: "c", AccountName: "charlie", AccountID: "333333333333", Error: "AccessDenied"},
	}, false)
	out := b.String()
	for _, want := range []string{"started", "skipped", "InProgress", "error", "AccessDenied"} {
		if !strings.Contains(out, want) {
			t.Errorf("release table missing %q:\n%s", want, out)
		}
	}
}

func TestExecutionAndActionTables(t *testing.T) {
	start := time.Now().Add(-5 * time.Minute)
	end := start.Add(3 * time.Minute)

	var b bytes.Buffer
	ExecutionTable(&b, []model.Execution{{
		ID: "exec-1", Status: model.StatusFailed, StartTime: &start, LastUpdate: &end,
		Revisions: []model.Revision{{ActionName: "src", RevisionID: "deadbeef",
			Summary: `{"ProviderType":"GitHub","CommitMessage":"ネットワーク標準の更新"}`}},
	}}, false)
	out := b.String()
	for _, want := range []string{"exec-1", "Failed", "3m0s", "ネットワーク標準の更新"} {
		if !strings.Contains(out, want) {
			t.Errorf("execution table missing %q:\n%s", want, out)
		}
	}

	b.Reset()
	ActionExecutionTable(&b, []model.ActionExecution{
		{StageName: "AFT-Global-Customizations", ActionName: "terraform-apply",
			Status: model.StatusFailed, StartTime: &start, LastUpdate: &end,
			CodeBuildID: "proj:uuid", ErrorMessage: "exit status 1"},
		{StageName: "Source", ActionName: "github", Status: model.StatusSucceeded},
	}, false)
	out = b.String()
	for _, want := range []string{"terraform-apply", "proj:uuid", "exit status 1", "github"} {
		if !strings.Contains(out, want) {
			t.Errorf("action table missing %q:\n%s", want, out)
		}
	}
}

// The JSON envelope is the machine-readable contract: it must carry the
// schema version and wrap the payload under "items".
func TestJSONEnvelope(t *testing.T) {
	var b bytes.Buffer
	if err := JSON(&b, []model.Account{{ID: "111111111111", Name: "alpha"}}); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var doc struct {
		SchemaVersion int             `json:"schema_version"`
		GeneratedAt   time.Time       `json:"generated_at"`
		Items         []model.Account `json:"items"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b.String())
	}
	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	if len(doc.Items) != 1 || doc.Items[0].Name != "alpha" {
		t.Errorf("items = %+v", doc.Items)
	}
	if doc.GeneratedAt.IsZero() {
		t.Error("generated_at should be set")
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m"},
		{2 * time.Hour, "2h"},
		{72 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}
