package tui

import (
	"context"
	"fmt"
	"testing"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// benchTable is a full-size list: 187 account rows (the production inventory)
// on a wide terminal, with a 40-row viewport.
func benchTable(rows int) (table.Model, map[string]bool) {
	cols := columns(180)
	tb := newScreenTable(cols)
	tb.SetWidth(180)
	tb.SetHeight(42)

	sel := map[string]bool{}
	trs := make([]table.Row, 0, rows)
	for i := range rows {
		id := fmt.Sprintf("%012d", i)
		status := "Succeeded"
		if i%10 == 0 {
			status = "Failed"
		}
		trs = append(trs, table.Row{fmt.Sprintf("account-name-%03d-prod", i), id, status, "2026-07-25 10:00"})
		if i%20 == 0 { // ~10 selected rows
			sel[id] = true
		}
	}
	tb.SetRows(trs)
	return tb, sel
}

// BenchmarkTableViewPlain is the baseline: what bubbles' own rendering costs.
func BenchmarkTableViewPlain(b *testing.B) {
	tb, _ := benchTable(187)
	for b.Loop() {
		_ = tb.View()
	}
}

// BenchmarkTableViewStatus adds the STATUS coloring pass.
func BenchmarkTableViewStatus(b *testing.B) {
	tb, _ := benchTable(187)
	for b.Loop() {
		_ = renderStatusTable(tb, listStatusCol)
	}
}

// BenchmarkTableViewSelectable adds the STATUS coloring plus the per-row
// selection highlight (keyed on the ACCOUNT ID cell).
func BenchmarkTableViewSelectable(b *testing.B) {
	tb, sel := benchTable(187)
	for b.Loop() {
		_ = renderSelectableTable(tb, listStatusCol, listKeyCol,
			func(key string) (lipgloss.Style, bool) { return selectedRowStyle, sel[key] })
	}
}

// BenchmarkSpaceToggle is one space keypress on a full list: with selection
// drawn at render time the row data is untouched, so this is the cost of the
// toggle plus the cursor row's restyle — no 187-row rebuild.
func BenchmarkSpaceToggle(b *testing.B) {
	m := newModel(context.Background(), Deps{})
	items := make([]model.PipelineSummary, 0, 187)
	for i := range 187 {
		id := fmt.Sprintf("%012d", i)
		items = append(items, model.PipelineSummary{
			PipelineName: id + "-customizations-pipeline", AccountID: id,
			AccountName: fmt.Sprintf("account-name-%03d-prod", i),
			Latest:      &model.Execution{Status: model.StatusSucceeded},
		})
	}
	m.items = items
	m.width, m.height = 180, 42
	m.table.SetColumns(columns(180))
	m.table.SetWidth(180)
	m.table.SetHeight(40)
	(&m).applyFilter()
	m.table.SetCursor(20)

	key := tea.KeyMsg{Type: tea.KeySpace}
	for b.Loop() {
		next, _ := m.Update(key)
		m = next.(uiModel)
	}
}

// BenchmarkApplyFilter is the row rebuild that a marker column would need on
// every toggle (it is also what a filter keystroke costs).
func BenchmarkApplyFilter(b *testing.B) {
	m := newModel(context.Background(), Deps{})
	items := make([]model.PipelineSummary, 0, 187)
	for i := range 187 {
		id := fmt.Sprintf("%012d", i)
		items = append(items, model.PipelineSummary{
			PipelineName: id + "-customizations-pipeline", AccountID: id,
			AccountName: fmt.Sprintf("account-name-%03d-prod", i),
			Latest:      &model.Execution{Status: model.StatusSucceeded},
		})
	}
	m.items = items
	m.table.SetColumns(columns(180))
	m.table.SetWidth(180)
	m.table.SetHeight(40)

	for b.Loop() {
		(&m).applyFilter()
	}
}
