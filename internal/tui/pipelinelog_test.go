package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hacker65536/aft-ops/internal/core/logs"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

func failedDetail() *model.PipelineDetail {
	return &model.PipelineDetail{
		PipelineName: "111111111111-customizations-pipeline",
		AccountID:    "111111111111",
		AccountName:  "alpha",
		Stages: []model.StageState{
			{Name: "Apply", Status: model.StatusFailed, Actions: []model.ActionState{
				{Name: "terraform-apply", Status: model.StatusFailed, CodeBuildID: "proj:uuid"},
			}},
		},
	}
}

// logBuildID picks the failed action's build id.
func TestLogBuildIDPicksFailedAction(t *testing.T) {
	id, action := logBuildID(failedDetail())
	if id != "proj:uuid" || action != "terraform-apply" {
		t.Errorf("logBuildID = (%q,%q), want (proj:uuid, terraform-apply)", id, action)
	}
}

// A successful fastLogMsg on the list pushes the resolved log screen; a
// failed one surfaces the error inline.
func TestFastLogMsgOnList(t *testing.T) {
	m := testModel(t, nil)

	lm := newLogModel(context.Background(), nil, "proj:uuid", "terraform-apply", 80, 24)
	next, cmd := m.Update(fastLogMsg{lm: &lm})
	if next.(uiModel).loading {
		t.Error("fastLogMsg should clear loading")
	}
	if cmd == nil {
		t.Fatal("a resolved fast log should return a push command")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("fastLogMsg should emit pushMsg, got %T", cmd())
	}
	if got, ok := push.s.(logModel); !ok || got.buildID != "proj:uuid" {
		t.Errorf("pushed screen should be a logModel for proj:uuid, got %T", push.s)
	}

	next, _ = m.Update(fastLogMsg{err: errors.New("no build")})
	if next.(uiModel).err == nil {
		t.Error("a failed fast log should record the error")
	}
}

// A loaded log renders through the default terraform mode; m cycles modes.
func TestLogModeCycle(t *testing.T) {
	m := newLogModel(context.Background(), nil, "proj:uuid", "terraform-apply", 80, 24)
	if m.mode != logs.ModeTerraform {
		t.Fatalf("default mode = %v, want terraform", m.mode)
	}
	raw := []string{
		"[Container] setup",
		"Terraform will perform the following actions",
		"Plan: 1 to add, 0 to change, 0 to destroy.",
	}
	next, _ := m.Update(logLoadedMsg{lines: raw})
	m = next.(logModel)

	// terraform mode drops the pre-terraform "[Container] setup" line.
	if got := logs.Render(m.raw, m.mode); strings.Contains(strings.Join(got, "\n"), "[Container] setup") {
		t.Error("terraform mode should exclude pre-terraform lines")
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	m = next.(logModel)
	if m.mode != logs.ModeRaw {
		t.Errorf("after m, mode = %v, want raw", m.mode)
	}
	// raw keeps every line.
	if got := logs.Render(m.raw, m.mode); !strings.Contains(strings.Join(got, "\n"), "[Container] setup") {
		t.Error("raw mode should keep every line")
	}
}

// A failed fetch surfaces the error instead of content.
func TestLogLoadError(t *testing.T) {
	m := newLogModel(context.Background(), nil, "proj:uuid", "act", 80, 24)
	next, _ := m.Update(logLoadedMsg{err: errors.New("boom")})
	lm := next.(logModel)
	if lm.err == nil || lm.loading {
		t.Error("a load error should be recorded and clear loading")
	}
}

// q and esc pop the log screen.
func TestLogQuitPops(t *testing.T) {
	m := newLogModel(context.Background(), nil, "proj:uuid", "act", 80, 24)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should return a command")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Errorf("esc should emit popMsg, got %T", cmd())
	}
}

func typeRunes(t *testing.T, m logModel, s string) logModel {
	t.Helper()
	for _, r := range s {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(logModel)
	}
	return m
}

// / opens the search input, enter commits it, and matching is a
// case-insensitive substring test; n/N walk the matches with wraparound.
func TestLogSearch(t *testing.T) {
	// Height 6 → a 3-line viewport, so jumping to a match actually scrolls
	// (a viewport that already shows everything clamps SetYOffset to 0).
	m := newLogModel(context.Background(), nil, "proj:uuid", "act", 80, 6)
	raw := []string{"alpha", "Error: one", "beta", "ERROR: two", "gamma"}
	next, _ := m.Update(logLoadedMsg{lines: raw})
	m = next.(logModel)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = next.(logModel)
	if !m.searching {
		t.Fatal("/ should focus the search input")
	}
	m = typeRunes(t, m, "error")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(logModel)

	if m.searching {
		t.Error("enter should leave search input mode")
	}
	if len(m.matches) != 2 || m.matches[0] != 1 || m.matches[1] != 3 {
		t.Fatalf("matches = %v, want [1 3] (case-insensitive)", m.matches)
	}
	if m.matchIdx != 0 || m.vp.YOffset != 1 {
		t.Errorf("first match: idx=%d yoffset=%d, want 0/1", m.matchIdx, m.vp.YOffset)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = next.(logModel)
	if m.matchIdx != 1 {
		t.Errorf("after n, matchIdx = %d, want 1", m.matchIdx)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = next.(logModel)
	if m.matchIdx != 0 {
		t.Errorf("n should wrap around, matchIdx = %d, want 0", m.matchIdx)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}})
	m = next.(logModel)
	if m.matchIdx != 1 {
		t.Errorf("N should step back with wraparound, matchIdx = %d, want 1", m.matchIdx)
	}
}

// esc clears an active search first and only pops on the next press; h/q
// pop regardless.
func TestLogEscClearsSearchThenPops(t *testing.T) {
	m := newLogModel(context.Background(), nil, "proj:uuid", "act", 80, 24)
	next, _ := m.Update(logLoadedMsg{lines: []string{"alpha", "beta"}})
	m = next.(logModel)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = next.(logModel)
	m = typeRunes(t, m, "beta")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(logModel)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(logModel)
	if cmd != nil {
		t.Error("esc with an active search should clear it, not pop")
	}
	if m.query != "" || m.matches != nil {
		t.Error("esc should clear the query and matches")
	}

	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("second esc should pop")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Errorf("second esc should emit popMsg, got %T", cmd())
	}
}

// Switching modes re-runs the search against the new rendering.
func TestLogSearchSurvivesModeSwitch(t *testing.T) {
	m := newLogModel(context.Background(), nil, "proj:uuid", "act", 80, 24)
	raw := []string{"[Container] setup", "Terraform will perform the following actions", "Error: boom"}
	next, _ := m.Update(logLoadedMsg{lines: raw})
	m = next.(logModel)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = next.(logModel)
	m = typeRunes(t, m, "container")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(logModel)
	// terraform mode drops the pre-terraform line, so no match yet.
	if len(m.matches) != 0 {
		t.Fatalf("terraform mode should have 0 matches, got %v", m.matches)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	m = next.(logModel)
	// raw mode restores the line; the query must re-match line 0.
	if len(m.matches) != 1 || m.matches[0] != 0 {
		t.Errorf("raw mode matches = %v, want [0]", m.matches)
	}
}
