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

// Pressing l on a loaded detail with a failed build pushes the log screen.
func TestDetailKeyLPushesLog(t *testing.T) {
	m := newDetailModel(context.Background(), nil,
		func(context.Context, string) ([]string, error) { return nil, nil },
		"111111111111-customizations-pipeline", "alpha", 80, 24)
	loaded, _ := m.Update(detailLoadedMsg{d: failedDetail()})
	m = loaded.(detailModel)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if cmd == nil {
		t.Fatal("l on a detail with a failed build should return a command")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("l should emit pushMsg, got %T", cmd())
	}
	if lm, ok := push.s.(logModel); !ok || lm.buildID != "proj:uuid" {
		t.Errorf("pushed screen should be a logModel for proj:uuid, got %T", push.s)
	}
}

// l is a no-op when no LogsFunc is wired.
func TestDetailKeyLNoLogsIsNoop(t *testing.T) {
	m := newDetailModel(context.Background(), nil, nil, "p", "alpha", 80, 24)
	loaded, _ := m.Update(detailLoadedMsg{d: failedDetail()})
	m = loaded.(detailModel)
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}); cmd != nil {
		t.Error("l without a LogsFunc should be a no-op")
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
