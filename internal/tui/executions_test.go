package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

func testExecs() []model.Execution {
	t0 := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(4 * time.Minute)
	return []model.Execution{
		// Revisions arrive global-first here; the detail lines must reorder
		// them by action name (the API order varies between executions).
		{ID: "aaaa1111-2222", Status: model.StatusFailed, StartTime: &t0, LastUpdate: &t1,
			Revisions: []model.Revision{
				{ActionName: "aft-global-customizations", RevisionID: "5368c27bdeadbeef",
					Summary: `{"ProviderType":"GitHub","CommitMessage":"fix vpc"}`},
				{ActionName: "aft-account-customizations", RevisionID: "9752f0addeadbeef",
					Summary: `{"ProviderType":"GitHub","CommitMessage":"Merge pull request #801"}`},
			}},
		{ID: "bbbb1111-2222", Status: model.StatusSucceeded, StartTime: &t0, LastUpdate: &t1},
	}
}

// Pressing l or enter on a list row with an ExecutionsFunc wired must push
// the executions screen for that pipeline.
func TestKeyLPushesExecutions(t *testing.T) {
	m := testModel(t, nil)
	m.execsFn = func(context.Context, string, bool) ([]model.Execution, error) { return nil, nil }
	m.table.SetCursor(0) // select alpha (111111111111)

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'l'}},
		{Type: tea.KeyEnter},
	} {
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("%v on a selected row should return a command", key)
		}
		push, ok := cmd().(pushMsg)
		if !ok {
			t.Fatalf("%v should emit pushMsg, got %T", key, cmd())
		}
		em, ok := push.s.(execsModel)
		if !ok {
			t.Fatalf("pushed screen should be an execsModel, got %T", push.s)
		}
		if em.name != "111111111111-customizations-pipeline" {
			t.Errorf("executions targets %q, want the selected pipeline", em.name)
		}
	}
}

// enter with no ExecutionsFunc wired (unit-test default) must be a no-op.
func TestKeyEnterNoExecutionsIsNoop(t *testing.T) {
	m := testModel(t, nil) // execsFn is nil
	m.table.SetCursor(0)
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Error("enter without an ExecutionsFunc should not push anything")
	}
}

// A successful execsLoadedMsg clears loading and fills the table.
func TestExecsLoadedPopulatesTable(t *testing.T) {
	m := newExecsModel(context.Background(), nil, nil, nil,
		"111111111111-customizations-pipeline", "alpha", 80, 24)

	next, _ := m.Update(execsLoadedMsg{execs: testExecs()})
	em := next.(execsModel)
	if em.loading {
		t.Error("execsLoadedMsg should clear loading")
	}
	if got := len(em.table.Rows()); got != 2 {
		t.Fatalf("got %d rows, want 2", got)
	}
	row := em.table.Rows()[0]
	if row[0] != "aaaa1111" {
		t.Errorf("execution id column = %q, want short id aaaa1111", row[0])
	}
	if row[3] != "4m0s" {
		t.Errorf("duration column = %q, want 4m0s", row[3])
	}
	// The REVISION column shows the first-by-action-name revision's commit
	// message unwrapped from the CodeConnections JSON, not the raw JSON.
	if row[4] != "Merge pull request #801" {
		t.Errorf("revision column = %q, want Merge pull request #801", row[4])
	}
}

// The selected execution's source revisions render inline below the table
// as "action – hash: message", padded to a fixed number of lines.
func TestExecsRevisionLines(t *testing.T) {
	m := newExecsModel(context.Background(), nil, nil, nil,
		"111111111111-customizations-pipeline", "alpha", 120, 24)
	next, _ := m.Update(execsLoadedMsg{execs: testExecs()})
	em := next.(execsModel)

	em.table.SetCursor(0)
	lines := em.revisionLines()
	if len(lines) != execsRevLines {
		t.Fatalf("got %d lines, want %d", len(lines), execsRevLines)
	}
	// Ordered by action name (account before global) and padded so the hash
	// and message columns line up.
	if lines[0] != "aft-account-customizations  9752f0ad  Merge pull request #801" {
		t.Errorf("lines[0] = %q", lines[0])
	}
	if lines[1] != "aft-global-customizations   5368c27b  fix vpc" {
		t.Errorf("lines[1] = %q", lines[1])
	}

	// An execution without revisions pads with placeholders.
	em.table.SetCursor(1)
	for i, l := range em.revisionLines() {
		if l != "-" {
			t.Errorf("lines[%d] = %q, want -", i, l)
		}
	}
}

// A failed load surfaces the error instead of content.
func TestExecsLoadError(t *testing.T) {
	m := newExecsModel(context.Background(), nil, nil, nil, "p", "alpha", 80, 24)
	next, _ := m.Update(execsLoadedMsg{err: errors.New("boom")})
	em := next.(execsModel)
	if em.err == nil || em.loading {
		t.Error("a load error should be recorded and clear loading")
	}
}

// l/enter on a selected execution pushes the actions screen for it.
func TestExecsKeyLPushesActions(t *testing.T) {
	m := newExecsModel(context.Background(), nil,
		func(context.Context, string, string, bool) ([]model.ActionExecution, error) { return nil, nil },
		nil, "111111111111-customizations-pipeline", "alpha", 80, 24)
	loaded, _ := m.Update(execsLoadedMsg{execs: testExecs()})
	m = loaded.(execsModel)
	m.table.SetCursor(0)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if cmd == nil {
		t.Fatal("l on a selected execution should return a command")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("l should emit pushMsg, got %T", cmd())
	}
	am, ok := push.s.(actionsModel)
	if !ok {
		t.Fatalf("pushed screen should be an actionsModel, got %T", push.s)
	}
	if am.exec.ID != "aaaa1111-2222" {
		t.Errorf("actions targets execution %q, want aaaa1111-2222", am.exec.ID)
	}
}

// h, q, and esc pop the executions screen back to the list.
func TestExecsBackPops(t *testing.T) {
	m := newExecsModel(context.Background(), nil, nil, nil, "p", "alpha", 80, 24)
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
