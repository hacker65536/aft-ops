package tui

import (
	"context"
	"strings"
	"testing"
	"time"

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
	m := newModel(context.Background(), Deps{Refresh: refresh})
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
	m := newModel(context.Background(), Deps{}) // no items, empty visible
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

// Auto-polling only arms while something is actually running: a list of
// terminal rows cannot change on its own, so the tool must go quiet.
func TestPollArmsOnlyWithInFlightRows(t *testing.T) {
	refresh := func(context.Context, []string, func(batch.Progress)) ([]model.PipelineSummary, error) {
		return nil, nil
	}
	m := newModel(context.Background(), Deps{Refresh: refresh, PollInterval: time.Minute})
	m.items = []model.PipelineSummary{
		sum("111111111111-customizations-pipeline", "111111111111", "alpha", model.StatusSucceeded),
	}
	if cmd := (&m).schedulePoll(); cmd != nil {
		t.Error("an all-terminal list should not arm a poll")
	}

	m.items = append(m.items,
		sum("222222222222-customizations-pipeline", "222222222222", "bravo", model.StatusInProgress))
	cmd := (&m).schedulePoll()
	if cmd == nil {
		t.Fatal("an in-flight row should arm a poll")
	}
	if !m.pollArmed {
		t.Error("arming a poll should set pollArmed")
	}
	if second := (&m).schedulePoll(); second != nil {
		t.Error("only one poll tick may be in flight at a time")
	}
}

// A poll tick refreshes just the running pipelines.
func TestPollMsgRefreshesInFlightOnly(t *testing.T) {
	called := make(chan []string, 1)
	refresh := func(_ context.Context, names []string, _ func(batch.Progress)) ([]model.PipelineSummary, error) {
		called <- names
		return nil, nil
	}
	m := newModel(context.Background(), Deps{Refresh: refresh, PollInterval: time.Minute})
	m.items = []model.PipelineSummary{
		sum("111111111111-customizations-pipeline", "111111111111", "alpha", model.StatusSucceeded),
		sum("222222222222-customizations-pipeline", "222222222222", "bravo", model.StatusInProgress),
	}
	(&m).applyFilter()
	m.pollArmed = true

	next, cmd := m.Update(pollMsg{})
	nm := next.(uiModel)
	if nm.pollArmed {
		t.Error("handling the tick should clear pollArmed")
	}
	if !nm.loading || cmd == nil {
		t.Fatal("a poll tick with in-flight rows should start a refresh")
	}

	// The refresh arrives as a tea.Batch; its members are run concurrently by
	// the runtime (one of them blocks on the progress channel until the fetch
	// closes it), so drive them the same way here.
	msgs, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected a batched command, got %T", cmd())
	}
	for _, c := range msgs {
		go c()
	}
	select {
	case names := <-called:
		if len(names) != 1 || names[0] != "222222222222-customizations-pipeline" {
			t.Errorf("polled names = %v, want only the in-flight pipeline", names)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll tick never called Refresh")
	}
}

// A tick that fires while a drill-down screen is on top is delivered to that
// screen and dropped: the root hands every message to the top of the stack.
// If the list did not restart the chain when it is re-exposed, pollArmed
// would stay set and auto-refresh would be dead for the rest of the session —
// silently, since nothing about a stale list says it stopped updating.
func TestPollReArmsWhenListIsReExposed(t *testing.T) {
	refresh := func(context.Context, []string, func(batch.Progress)) ([]model.PipelineSummary, error) {
		return nil, nil
	}
	m := newModel(context.Background(), Deps{Refresh: refresh, PollInterval: time.Minute})
	m.items = []model.PipelineSummary{
		sum("222222222222-customizations-pipeline", "222222222222", "bravo", model.StatusInProgress),
	}
	(&m).applyFilter()

	if cmd := (&m).schedulePoll(); cmd == nil {
		t.Fatal("an in-flight row should arm a poll")
	}
	staleGen := m.pollGen

	// Drill in and back: the root re-delivers the window size to whichever
	// screen it just uncovered.
	next, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(uiModel)
	if cmd == nil {
		t.Fatal("re-exposing the list with a running pipeline must re-arm the poll")
	}
	if !m.pollArmed {
		t.Error("the re-armed poll should be marked in flight")
	}

	// The tick armed before the re-arm must not also fire, or two timer
	// chains run in parallel from here on.
	next, cmd = m.Update(pollMsg{gen: staleGen})
	if cmd != nil {
		t.Error("a retired tick must not start a refresh")
	}
	if !next.(uiModel).pollArmed {
		t.Error("a retired tick must not clear the current tick's arming")
	}
}

// Polling is off unless an interval was configured.
func TestPollDisabledWithoutInterval(t *testing.T) {
	m := testModel(t, func(context.Context, []string, func(batch.Progress)) ([]model.PipelineSummary, error) {
		return nil, nil
	})
	m.items[0].Latest.Status = model.StatusInProgress
	if cmd := (&m).schedulePoll(); cmd != nil {
		t.Error("PollInterval 0 must disable auto-refresh")
	}
}

// navDots renders one dot per drill-down level regardless of the active one.
func TestNavDots(t *testing.T) {
	for depth := 1; depth <= navDepth; depth++ {
		if got := strings.Count(navDots(depth), "•"); got != navDepth {
			t.Errorf("navDots(%d) has %d dots, want %d", depth, got, navDepth)
		}
	}
}

// ctrl+c must quit from inside the filter input too.
//
// bubbletea runs the terminal in raw mode, so no SIGINT is delivered: if the
// key is not bound here, the only exits are esc (which also discards the
// filter) or killing the process from another shell. The log screen's search
// mode has always bound it; the list filter had not.
func TestCtrlCQuitsWhileFiltering(t *testing.T) {
	m := testModel(t, nil)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = next.(uiModel)
	if !m.filtering {
		t.Fatal("/ should enter filter input mode")
	}
	// Type something first: the bug only bites once the textinput has focus
	// and is swallowing every key that is not esc or enter.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = next.(uiModel)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c while filtering should return a command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c while filtering should quit, got %T", cmd())
	}
}

// ctrl+c still quits from the normal (non-filtering) list.
func TestCtrlCQuitsFromList(t *testing.T) {
	m := testModel(t, nil)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should return a command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c should quit, got %T", cmd())
	}
}
