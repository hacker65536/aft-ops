package tui

import (
	"context"
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// Pressing enter on a selected row with a DetailFunc wired must push a
// detail screen for that pipeline.
func TestKeyEnterPushesDetail(t *testing.T) {
	m := testModel(t, nil)
	m.detail = func(context.Context, string) (*model.PipelineDetail, error) { return nil, nil }
	m.table.SetCursor(0) // select alpha (111111111111)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on a selected row should return a command")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("enter should emit pushMsg, got %T", cmd())
	}
	d, ok := push.s.(detailModel)
	if !ok {
		t.Fatalf("pushed screen should be a detailModel, got %T", push.s)
	}
	if d.name != "111111111111-customizations-pipeline" {
		t.Errorf("detail targets %q, want the selected pipeline", d.name)
	}
}

// enter with no DetailFunc wired (unit-test default) must be a no-op.
func TestKeyEnterNoDetailIsNoop(t *testing.T) {
	m := testModel(t, nil) // detail is nil
	m.table.SetCursor(0)
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Error("enter without a DetailFunc should not push anything")
	}
}

// A successful detailLoadedMsg clears loading and fills the viewport.
func TestDetailLoadedPopulatesViewport(t *testing.T) {
	m := newDetailModel(context.Background(), nil, nil,
		"111111111111-customizations-pipeline", "alpha", 80, 24)

	detail := &model.PipelineDetail{
		PipelineName: "111111111111-customizations-pipeline",
		AccountID:    "111111111111",
		AccountName:  "alpha",
		Stages: []model.StageState{
			{Name: "Apply", Status: model.StatusFailed, Actions: []model.ActionState{
				{Name: "terraform-apply", Status: model.StatusFailed, CodeBuildID: "proj:uuid"},
			}},
		},
	}
	next, _ := m.Update(detailLoadedMsg{d: detail})
	dm := next.(detailModel)
	if dm.loading {
		t.Error("detailLoadedMsg should clear loading")
	}
	if dm.d == nil {
		t.Fatal("detail should be stored")
	}
	if got := dm.render(); got == "" {
		t.Error("render should produce non-empty detail text")
	}
}

// A failed load surfaces the error instead of content.
func TestDetailLoadError(t *testing.T) {
	m := newDetailModel(context.Background(), nil, nil, "p", "alpha", 80, 24)
	next, _ := m.Update(detailLoadedMsg{err: errors.New("boom")})
	dm := next.(detailModel)
	if dm.err == nil {
		t.Error("a load error should be recorded")
	}
	if dm.loading {
		t.Error("a load error should clear loading")
	}
}

// q and esc pop the detail screen back to the list.
func TestDetailQuitPops(t *testing.T) {
	m := newDetailModel(context.Background(), nil, nil, "p", "alpha", 80, 24)
	for _, key := range []tea.KeyMsg{
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
