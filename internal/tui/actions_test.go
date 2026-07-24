package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

func testActions() []model.ActionExecution {
	t0 := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Minute)
	t2 := t0.Add(5 * time.Minute)
	return []model.ActionExecution{
		{StageName: "Source", ActionName: "Source", Status: model.StatusSucceeded,
			StartTime: &t0, LastUpdate: &t1},
		{StageName: "Apply", ActionName: "terraform-apply", Status: model.StatusFailed,
			StartTime: &t1, LastUpdate: &t2,
			CodeBuildID: "proj:uuid", Summary: "build failed", ErrorMessage: "exit status 1"},
	}
}

func loadedActionsModel(t *testing.T, logs LogsFunc) actionsModel {
	t.Helper()
	m := newActionsModel(context.Background(), nil, logs,
		"111111111111-customizations-pipeline", "alpha",
		model.Execution{ID: "aaaa1111-2222", Status: model.StatusFailed}, 80, 24)
	next, _ := m.Update(actionsLoadedMsg{actions: testActions()})
	return next.(actionsModel)
}

// A successful actionsLoadedMsg clears loading and fills the table.
func TestActionsLoadedPopulatesTable(t *testing.T) {
	m := loadedActionsModel(t, nil)
	if m.loading {
		t.Error("actionsLoadedMsg should clear loading")
	}
	if got := len(m.table.Rows()); got != 2 {
		t.Fatalf("got %d rows, want 2", got)
	}
	row := m.table.Rows()[1]
	if row[0] != "Apply" || row[1] != "terraform-apply" {
		t.Errorf("row = %v, want Apply/terraform-apply", row)
	}
	if row[4] != "4m0s" {
		t.Errorf("duration column = %q, want 4m0s", row[4])
	}
}

// The selected action's summary and error render inline below the table.
func TestActionsInlineDetail(t *testing.T) {
	m := loadedActionsModel(t, nil)
	m.table.SetCursor(1) // terraform-apply
	summary, errMsg := m.detailLines()
	if summary != "build failed" {
		t.Errorf("summary = %q, want build failed", summary)
	}
	if errMsg != "exit status 1" {
		t.Errorf("error = %q, want exit status 1", errMsg)
	}
}

// l, enter, and v on an action with a build id push its log screen.
func TestActionsKeyLPushesLog(t *testing.T) {
	m := loadedActionsModel(t, func(context.Context, string) ([]string, error) { return nil, nil })
	m.table.SetCursor(1) // terraform-apply (has a build id)

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'l'}},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune{'v'}},
	} {
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("%v on a build action should return a command", key)
		}
		push, ok := cmd().(pushMsg)
		if !ok {
			t.Fatalf("%v should emit pushMsg, got %T", key, cmd())
		}
		if lm, ok := push.s.(logModel); !ok || lm.buildID != "proj:uuid" {
			t.Errorf("pushed screen should be a logModel for proj:uuid, got %T", push.s)
		}
	}
}

// l on an action without a build id (e.g. Source) is a no-op.
func TestActionsKeyLNoBuildIsNoop(t *testing.T) {
	m := loadedActionsModel(t, func(context.Context, string) ([]string, error) { return nil, nil })
	m.table.SetCursor(0) // Source (no build id)
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}); cmd != nil {
		t.Error("l on an action without a build id should be a no-op")
	}
}

// h, q, and esc pop the actions screen.
func TestActionsBackPops(t *testing.T) {
	m := loadedActionsModel(t, nil)
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'h'}},
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyEsc},
	} {
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("%v should return a command", key)
		}
		if _, ok := cmd().(popMsg); !ok {
			t.Errorf("%v should emit popMsg, got %T", key, cmd())
		}
	}
}

// Loading the action list triggers a background verdict fetch for the
// terminal CodeBuild action, and the resulting verdictMsg replaces the
// selected action's summary line.
func TestActionsVerdictFlow(t *testing.T) {
	logsFn := func(context.Context, string) ([]string, error) {
		return []string{"Apply complete! Resources: 0 added, 0 changed, 0 destroyed."}, nil
	}
	m := newActionsModel(context.Background(), nil, logsFn,
		"111111111111-customizations-pipeline", "alpha",
		model.Execution{ID: "aaaa1111-2222", Status: model.StatusSucceeded}, 80, 24)

	next, cmd := m.Update(actionsLoadedMsg{actions: testActions()})
	m = next.(actionsModel)
	if cmd == nil {
		t.Fatal("loaded actions with a LogsFunc should trigger a verdict fetch")
	}
	vm, ok := cmd().(verdictMsg)
	if !ok {
		t.Fatalf("verdict fetch should emit verdictMsg, got %T", cmd())
	}
	if vm.buildID != "proj:uuid" || !strings.HasPrefix(vm.verdict, "Apply complete!") {
		t.Fatalf("verdictMsg = %+v", vm)
	}

	next, _ = m.Update(vm)
	m = next.(actionsModel)
	m.table.SetCursor(1) // terraform-apply
	summary, _ := m.detailLines()
	if summary != "Apply complete! Resources: 0 added, 0 changed, 0 destroyed." {
		t.Errorf("summary = %q, want the terraform verdict", summary)
	}

	// The source action (no build id) keeps its plain summary.
	m.table.SetCursor(0)
	if summary, _ := m.detailLines(); summary != "" {
		t.Errorf("source summary = %q, want empty", summary)
	}
}

// renderSummary must colorize without altering the text content, for both
// apply and plan verdict shapes (indices-based slicing is easy to get wrong).
func TestRenderSummaryPreservesText(t *testing.T) {
	for _, s := range []string{
		"Apply complete! Resources: 0 added, 1 changed, 0 destroyed.",
		"Plan: 3 to add, 0 to change, 2 to destroy.",
		"Error: creating S3 Bucket: BucketAlreadyExists",
		"-",
	} {
		if got := ansi.Strip(renderSummary(s)); got != s {
			t.Errorf("renderSummary mangled text: %q -> %q", s, got)
		}
	}
}
