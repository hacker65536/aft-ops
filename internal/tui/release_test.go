package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

const alphaPipeline = "111111111111-customizations-pipeline"

// space toggles selection of the current row.
func TestSpaceTogglesSelection(t *testing.T) {
	m := testModel(t, nil)
	m.table.SetCursor(0) // alpha

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(uiModel)
	if !m.selected[alphaPipeline] {
		t.Fatal("space should select the current row")
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(uiModel)
	if m.selected[alphaPipeline] {
		t.Error("space again should deselect the row")
	}
}

// x with a selection pushes the release screen for the selected targets.
func TestKeyXPushesRelease(t *testing.T) {
	m := testModel(t, nil)
	m.release = func(context.Context, []model.PipelineSummary, func(batch.Progress)) ([]model.ReleaseResult, error) {
		return nil, nil
	}
	m.releaseLimit = 50
	m.selected[alphaPipeline] = true

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if cmd == nil {
		t.Fatal("x with a selection should return a command")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("x should emit pushMsg, got %T", cmd())
	}
	rm, ok := push.s.(releaseModel)
	if !ok {
		t.Fatalf("pushed screen should be a releaseModel, got %T", push.s)
	}
	if len(rm.targets) != 1 || rm.targets[0].PipelineName != alphaPipeline {
		t.Errorf("release targets = %v, want just alpha", rm.targets)
	}
}

// x with nothing selected is a no-op.
func TestKeyXNoSelectionIsNoop(t *testing.T) {
	m := testModel(t, nil)
	m.release = func(context.Context, []model.PipelineSummary, func(batch.Progress)) ([]model.ReleaseResult, error) {
		return nil, nil
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}); cmd != nil {
		t.Error("x with no selection should be a no-op")
	}
}

func releaseTargets(n int) []model.PipelineSummary {
	out := make([]model.PipelineSummary, n)
	for i := range out {
		out[i] = model.PipelineSummary{PipelineName: alphaPipeline, AccountID: "111111111111"}
	}
	return out
}

// The guard blocks confirmation when targets exceed the limit.
func TestReleaseGuardBlocksOverLimit(t *testing.T) {
	m := newReleaseModel(context.Background(), nil, nil, releaseTargets(2), 1, 80, 24)
	if !m.overLimit() {
		t.Fatal("2 targets over a limit of 1 should be over limit")
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if next.(releaseModel).phase != phaseConfirm {
		t.Error("y over the limit must not start the release")
	}
	if cmd != nil {
		t.Error("y over the limit should return no command")
	}
}

// n cancels the confirm and pops back.
func TestReleaseCancelPops(t *testing.T) {
	m := newReleaseModel(context.Background(), nil, nil, releaseTargets(1), 50, 80, 24)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd == nil {
		t.Fatal("n should return a command")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Errorf("n should emit popMsg, got %T", cmd())
	}
}

// Confirming runs the batch (→running), the result lands (→done), and any
// key then pops back while asking the list to refresh the started rows.
func TestReleaseConfirmRunDone(t *testing.T) {
	ran := false
	run := func(context.Context, []model.PipelineSummary, func(batch.Progress)) ([]model.ReleaseResult, error) {
		ran = true
		return []model.ReleaseResult{
			{PipelineName: alphaPipeline, ExecutionID: "exec-1"},
		}, nil
	}
	m := newReleaseModel(context.Background(), run, nil, releaseTargets(1), 50, 80, 24)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = next.(releaseModel)
	if m.phase != phaseRunning {
		t.Fatalf("y within the limit should enter running, got phase %d", m.phase)
	}
	if cmd == nil {
		t.Error("running should return a command that executes the release")
	}

	next, _ = m.Update(releaseDoneMsg{results: []model.ReleaseResult{
		{PipelineName: alphaPipeline, ExecutionID: "exec-1"},
	}})
	m = next.(releaseModel)
	if m.phase != phaseDone {
		t.Fatalf("releaseDoneMsg should reach done, got phase %d", m.phase)
	}

	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("a key in done should return a command")
	}
	msgs, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("done key should batch pop + refresh, got %T", cmd())
	}
	var gotPop, gotRefresh bool
	for _, c := range msgs {
		switch v := c().(type) {
		case popMsg:
			gotPop = true
		case refreshNamesMsg:
			gotRefresh = true
			if len(v.names) != 1 || v.names[0] != alphaPipeline {
				t.Errorf("refresh names = %v, want just the started pipeline", v.names)
			}
		}
	}
	if !gotPop || !gotRefresh {
		t.Errorf("done key should emit both popMsg and refreshNamesMsg (pop=%v refresh=%v)", gotPop, gotRefresh)
	}
	_ = ran // begin()'s command runs the batch asynchronously; not awaited here
}

// releaseCounts tallies started/skipped/failed correctly.
func TestReleaseCounts(t *testing.T) {
	results := []model.ReleaseResult{
		{PipelineName: "a", ExecutionID: "e1"},
		{PipelineName: "b", Skipped: true, SkipReason: "InProgress"},
		{PipelineName: "c", Error: "boom"},
		{PipelineName: "d", ExecutionID: "e2"},
	}
	started, skipped, failed := releaseCounts(results)
	if started != 2 || skipped != 1 || failed != 1 {
		t.Errorf("counts = (%d,%d,%d), want (2,1,1)", started, skipped, failed)
	}
	if names := startedNames(results); len(names) != 2 {
		t.Errorf("startedNames = %v, want 2 entries", names)
	}
}
