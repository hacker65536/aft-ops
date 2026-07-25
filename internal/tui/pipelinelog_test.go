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

// twoStageDetail is the AFT customizations shape as GetPipelineState reports
// it: a source stage with no build, then the two terraform stages, both
// running an action called "Apply".
func twoStageDetail() *model.PipelineDetail {
	return &model.PipelineDetail{
		PipelineName: "111111111111-customizations-pipeline",
		AccountID:    "111111111111",
		AccountName:  "alpha",
		Stages: []model.StageState{
			{Name: "Source", Status: model.StatusSucceeded, Actions: []model.ActionState{
				{Name: "aft-global-customizations", Status: model.StatusSucceeded},
			}},
			{Name: "AFT-Global-Customizations", Status: model.StatusSucceeded,
				Actions: []model.ActionState{
					{Name: "Apply", Status: model.StatusSucceeded, CodeBuildID: "global:uuid"},
				}},
			{Name: "AFT-Account-Customizations", Status: model.StatusFailed,
				Actions: []model.ActionState{
					{Name: "Apply", Status: model.StatusFailed, CodeBuildID: "account:uuid"},
				}},
		},
	}
}

// testLogModel is a single-build log screen, the shape most of these tests
// exercise.
func testLogModel(action string, w, h int) logModel {
	return newLogModel(context.Background(), nil,
		oneLogTarget("proj:uuid", "", action), action, w, h)
}

// oneBuildLoaded delivers one build's raw lines to a single-build screen.
func oneBuildLoaded(lines []string) logLoadedMsg {
	return logLoadedMsg{raws: [][]string{lines}, errs: []error{nil}}
}

// A pipeline's current state yields every CodeBuild-backed action as a
// target, stage-qualified and in pipeline order — source actions excluded.
func TestStateLogTargetsCoverEveryBuild(t *testing.T) {
	got := stateLogTargets(twoStageDetail())
	want := []logTarget{
		{stage: "AFT-Global-Customizations", action: "Apply", buildID: "global:uuid"},
		{stage: "AFT-Account-Customizations", action: "Apply", buildID: "account:uuid"},
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %+v, want both builds %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("targets[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Two builds of the same run share an action name, so a label must name the
// stage; a target that knows only one of the two falls back to it.
func TestLogTargetLabel(t *testing.T) {
	for _, tc := range []struct {
		t    logTarget
		want string
	}{
		{logTarget{stage: "AFT-Global-Customizations", action: "Apply"},
			"AFT-Global-Customizations / Apply"},
		{logTarget{action: "Apply"}, "Apply"},
		{logTarget{stage: "AFT-Global-Customizations"}, "AFT-Global-Customizations"},
	} {
		if got := tc.t.label(); got != tc.want {
			t.Errorf("%+v.label() = %q, want %q", tc.t, got, tc.want)
		}
	}
}

// A single-build screen carries the build's label in its title (it renders no
// section header); a multi-build screen keeps the title to its origin.
func TestLogScreenTitle(t *testing.T) {
	one := oneLogTarget("proj:uuid", "AFT-Account-Customizations", "Apply")
	if got, want := logScreenTitle("alpha", one),
		"alpha · AFT-Account-Customizations / Apply"; got != want {
		t.Errorf("single-build title = %q, want %q", got, want)
	}
	if got, want := logScreenTitle("", one), "AFT-Account-Customizations / Apply"; got != want {
		t.Errorf("originless title = %q, want %q", got, want)
	}
	if got, want := logScreenTitle("alpha", twoBuilds()), "alpha"; got != want {
		t.Errorf("multi-build title = %q, want %q", got, want)
	}
}

// v on the list resolves every terraform build of the row's latest execution
// into one log screen, via its action runs — not the pipeline state, whose
// per-action latest runs may come from different executions.
func TestListFastLogCoversEveryBuild(t *testing.T) {
	acts := []model.ActionExecution{
		{StageName: "Source", ActionName: "aft-global-customizations"}, // source: no build id
		{StageName: "AFT-Global-Customizations", ActionName: "Apply",
			Status: model.StatusSucceeded, CodeBuildID: "global:uuid"},
		{StageName: "AFT-Account-Customizations", ActionName: "Apply",
			Status: model.StatusFailed, CodeBuildID: "account:uuid"},
	}
	var gotExec string
	m := testModel(t, nil)
	m.logs = func(context.Context, string) ([]string, error) { return nil, nil }
	m.actionsFn = func(_ context.Context, _, execID string, _ bool) ([]model.ActionExecution, error) {
		gotExec = execID
		return acts, nil
	}
	m.detail = func(context.Context, string) (*model.PipelineDetail, error) {
		t.Error("a row with a known execution should not need the state call")
		return nil, nil
	}
	m.items[0].Latest = &model.Execution{ID: "aaaa1111-2222", Status: model.StatusFailed}
	(&m).applyFilter()
	m.table.SetCursor(0)

	cmd := m.openFastLog()
	if cmd == nil {
		t.Fatal("v on a selected row should resolve a log")
	}
	msg, ok := cmd().(fastLogMsg)
	if !ok || msg.err != nil {
		t.Fatalf("openFastLog = %+v, want a resolved log screen", msg)
	}
	if gotExec != "aaaa1111-2222" {
		t.Errorf("actions fetched for execution %q, want the row's latest", gotExec)
	}
	want := []logTarget{
		{stage: "AFT-Global-Customizations", action: "Apply", buildID: "global:uuid"},
		{stage: "AFT-Account-Customizations", action: "Apply", buildID: "account:uuid"},
	}
	got := msg.lm.targets
	if len(got) != len(want) {
		t.Fatalf("targets = %+v, want both builds %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("targets[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A row whose latest execution is unknown (a status fetch that failed) still
// resolves its logs, from the pipeline's current state.
func TestListFastLogFallsBackToState(t *testing.T) {
	m := testModel(t, nil)
	m.logs = func(context.Context, string) ([]string, error) { return nil, nil }
	m.actionsFn = func(context.Context, string, string, bool) ([]model.ActionExecution, error) {
		t.Error("a row with no known execution has no action runs to ask for")
		return nil, nil
	}
	m.detail = func(context.Context, string) (*model.PipelineDetail, error) {
		return twoStageDetail(), nil
	}
	m.items[0].Latest = nil
	(&m).applyFilter()
	m.table.SetCursor(0)

	cmd := m.openFastLog()
	if cmd == nil {
		t.Fatal("v should fall back to the state call")
	}
	msg, ok := cmd().(fastLogMsg)
	if !ok || msg.err != nil {
		t.Fatalf("openFastLog = %+v, want a resolved log screen", msg)
	}
	if got := msg.lm.targets; len(got) != 2 {
		t.Errorf("targets = %+v, want both builds", got)
	}
}

// A successful fastLogMsg on the list pushes the resolved log screen; a
// failed one surfaces the error inline.
func TestFastLogMsgOnList(t *testing.T) {
	m := testModel(t, nil)

	lm := testLogModel("terraform-apply", 80, 24)
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
	if got, ok := push.s.(logModel); !ok || len(got.targets) != 1 || got.targets[0].buildID != "proj:uuid" {
		t.Errorf("pushed screen should be a logModel for proj:uuid, got %T", push.s)
	}

	next, _ = m.Update(fastLogMsg{err: errors.New("no build")})
	if next.(uiModel).err == nil {
		t.Error("a failed fast log should record the error")
	}
}

// A loaded log renders through the default terraform mode; m cycles modes.
func TestLogModeCycle(t *testing.T) {
	m := testLogModel("terraform-apply", 80, 24)
	if m.mode != logs.ModeTerraform {
		t.Fatalf("default mode = %v, want terraform", m.mode)
	}
	raw := []string{
		"[Container] setup",
		"Terraform will perform the following actions",
		"Plan: 1 to add, 0 to change, 0 to destroy.",
	}
	next, _ := m.Update(oneBuildLoaded(raw))
	m = next.(logModel)

	// terraform mode drops the pre-terraform "[Container] setup" line.
	if got := logs.Render(m.raws[0], m.mode); strings.Contains(strings.Join(got, "\n"), "[Container] setup") {
		t.Error("terraform mode should exclude pre-terraform lines")
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	m = next.(logModel)
	if m.mode != logs.ModeRaw {
		t.Errorf("after m, mode = %v, want raw", m.mode)
	}
	// raw keeps every line.
	if got := logs.Render(m.raws[0], m.mode); !strings.Contains(strings.Join(got, "\n"), "[Container] setup") {
		t.Error("raw mode should keep every line")
	}
}

// A failed fetch surfaces the error instead of content.
func TestLogLoadError(t *testing.T) {
	m := testLogModel("act", 80, 24)
	next, _ := m.Update(logLoadedMsg{err: errors.New("boom")})
	lm := next.(logModel)
	if lm.err == nil || lm.loading {
		t.Error("a load error should be recorded and clear loading")
	}
}

// q and esc pop the log screen.
func TestLogQuitPops(t *testing.T) {
	m := testLogModel("act", 80, 24)
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
	m := testLogModel("act", 80, 6)
	raw := []string{"alpha", "Error: one", "beta", "ERROR: two", "gamma"}
	next, _ := m.Update(oneBuildLoaded(raw))
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
	m := testLogModel("act", 80, 24)
	next, _ := m.Update(oneBuildLoaded([]string{"alpha", "beta"}))
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
	m := testLogModel("act", 80, 24)
	raw := []string{"[Container] setup", "Terraform will perform the following actions", "Error: boom"}
	next, _ := m.Update(oneBuildLoaded(raw))
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

// twoBuilds is the AFT customizations shape: one execution, two terraform
// builds (global, then account customizations).
func twoBuilds() []logTarget {
	return []logTarget{
		{stage: "AFT-Global-Customizations", action: "Apply", buildID: "global:uuid"},
		{stage: "AFT-Account-Customizations", action: "Apply", buildID: "account:uuid"},
	}
}

// A multi-build screen concatenates every build under a header line each, in
// pipeline order, and one search covers all of them.
func TestLogMultipleBuildsConcatenated(t *testing.T) {
	m := newLogModel(context.Background(), nil, twoBuilds(), "execution aaaa1111", 80, 24)
	next, _ := m.Update(logLoadedMsg{
		raws: [][]string{
			{"Terraform will perform the following actions", "Plan: 1 to add, 0 to change, 0 to destroy."},
			{"Terraform will perform the following actions", "Error: account boom"},
		},
		errs: []error{nil, nil},
	})
	m = next.(logModel)

	if len(m.secLine) != 2 {
		t.Fatalf("got %d sections, want 2", len(m.secLine))
	}
	// Both builds run an action called "Apply", so the section headers have
	// to name the stage for the two logs to be tellable apart.
	body := strings.Join(m.lines, "\n")
	for _, want := range []string{
		"AFT-Global-Customizations / Apply", "Plan: 1 to add",
		"AFT-Account-Customizations / Apply", "Error: account boom",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the concatenated log should contain %q:\n%s", want, body)
		}
	}
	if got, want := strings.Index(body, "AFT-Global"), strings.Index(body, "AFT-Account"); got > want {
		t.Error("builds should stay in pipeline order (global before account)")
	}

	// A search spans both builds.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = typeRunes(t, next.(logModel), "terraform will perform")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := len(next.(logModel).matches); got != 2 {
		t.Errorf("search matched %d lines, want one per build", got)
	}
}

// ] and [ jump to the next/previous build's header, without wrapping.
func TestLogSectionJump(t *testing.T) {
	// A 3-line viewport so SetYOffset is not clamped back to 0.
	m := newLogModel(context.Background(), nil, twoBuilds(), "execution aaaa1111", 80, 6)
	next, _ := m.Update(logLoadedMsg{
		raws: [][]string{
			{"g1", "g2", "g3", "g4"},
			{"a1", "a2", "a3", "a4"},
		},
		errs: []error{nil, nil},
	})
	m = next.(logModel)

	if m.vp.YOffset != 0 || m.currentSection() != 0 {
		t.Fatalf("a fresh screen starts at the first build, got offset %d section %d",
			m.vp.YOffset, m.currentSection())
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}})
	m = next.(logModel)
	if m.vp.YOffset != m.secLine[1] || m.currentSection() != 1 {
		t.Fatalf("] should land on the second build's header (line %d), got %d",
			m.secLine[1], m.vp.YOffset)
	}

	// No wraparound past the last build.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}})
	if got := next.(logModel).vp.YOffset; got != m.secLine[1] {
		t.Errorf("] on the last build should stay put, got offset %d", got)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}})
	m = next.(logModel)
	if m.vp.YOffset != m.secLine[0] || m.currentSection() != 0 {
		t.Errorf("[ should return to the first build, got offset %d", m.vp.YOffset)
	}
}

// One build failing to load must not hide the other: its section carries the
// error and the rest of the log still renders.
func TestLogPartialBuildError(t *testing.T) {
	m := newLogModel(context.Background(), nil, twoBuilds(), "execution aaaa1111", 80, 24)
	next, _ := m.Update(logLoadedMsg{
		raws: [][]string{nil, {"account log line"}},
		errs: []error{errors.New("global boom"), nil},
	})
	m = next.(logModel)

	if m.err != nil {
		t.Errorf("a partial failure should not blank the screen: %v", m.err)
	}
	body := strings.Join(m.lines, "\n")
	if !strings.Contains(body, "global boom") || !strings.Contains(body, "account log line") {
		t.Errorf("both the error and the surviving log should show:\n%s", body)
	}
}

// A single-build screen gets no section headers, and the section keys are
// inert there.
func TestLogSingleBuildHasNoSections(t *testing.T) {
	m := testLogModel("terraform-apply", 80, 6)
	next, _ := m.Update(oneBuildLoaded([]string{"l1", "l2", "l3", "l4", "l5"}))
	m = next.(logModel)

	if len(m.secLine) != 0 {
		t.Errorf("a single build should have no section headers, got %v", m.secLine)
	}
	if got := strings.Join(m.lines, "\n"); strings.Contains(got, "────") {
		t.Errorf("a single build should render no separator:\n%s", got)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}})
	if got := next.(logModel).vp.YOffset; got != 0 {
		t.Errorf("] with one build should not scroll, got offset %d", got)
	}
}
