package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

func sum(name, id, acct string, st model.Status) model.PipelineSummary {
	return model.PipelineSummary{
		PipelineName: name,
		AccountID:    id,
		AccountName:  acct,
		Latest:       &model.Execution{Status: st},
	}
}

func testModel(t *testing.T, refresh Refresh) uiModel {
	t.Helper()
	m := newModel(context.Background(), nil, refresh)
	m.items = []model.PipelineSummary{
		sum("111111111111-customizations-pipeline", "111111111111", "alpha", model.StatusFailed),
		sum("222222222222-customizations-pipeline", "222222222222", "bravo", model.StatusSucceeded),
	}
	(&m).applyFilter()
	return m
}

// Pressing r with a row selected must enter a targeted refresh (loading).
func TestKeyRRefreshesSelected(t *testing.T) {
	m := testModel(t, func(context.Context, []string, func(batch.Progress)) ([]model.PipelineSummary, error) {
		return nil, nil
	})
	if len(m.visible) != 2 {
		t.Fatalf("applyFilter should expose 2 visible rows, got %d", len(m.visible))
	}
	m.table.SetCursor(1) // select bravo

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	nm := next.(uiModel)
	if !nm.loading {
		t.Error("pressing r on a selected row should start a refresh (loading=true)")
	}
	if cmd == nil {
		t.Error("pressing r should return a command")
	}
}

// r with no rows (empty selection) must be a no-op, not a panic.
func TestKeyRNoSelectionIsNoop(t *testing.T) {
	m := newModel(context.Background(), nil, nil) // no items, empty visible
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if next.(uiModel).loading {
		t.Error("r with no selection should not start a refresh")
	}
}

// s cycles the sort key; o toggles the order; both re-sort the rows.
func TestKeySortControls(t *testing.T) {
	m := testModel(t, nil)
	if m.sortKey != model.SortByLastUpdate || m.sortOrder != model.OrderDesc {
		t.Fatalf("defaults = %s/%s, want last-update/desc", m.sortKey, m.sortOrder)
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = next.(uiModel)
	if m.sortKey != model.SortByStatus {
		t.Errorf("after s, sortKey = %s, want status", m.sortKey)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	m = next.(uiModel)
	if m.sortOrder != model.OrderAsc {
		t.Errorf("after o, sortOrder = %s, want asc", m.sortOrder)
	}
}

// A refreshedMsg updates only the matching row and keeps the others.
func TestRefreshedMsgMergesInPlace(t *testing.T) {
	m := testModel(t, nil)

	updated := sum("222222222222-customizations-pipeline", "222222222222", "bravo", model.StatusInProgress)
	next, _ := m.Update(refreshedMsg{items: []model.PipelineSummary{updated}})
	nm := next.(uiModel)

	if nm.loading {
		t.Error("refreshedMsg should clear loading")
	}
	var got model.Status
	for _, it := range nm.items {
		if it.PipelineName == "222222222222-customizations-pipeline" {
			got = it.Status()
		}
		if it.PipelineName == "111111111111-customizations-pipeline" && it.Status() != model.StatusFailed {
			t.Error("unrelated row must be left unchanged")
		}
	}
	if got != model.StatusInProgress {
		t.Errorf("selected row status = %q, want InProgress after merge", got)
	}
}
