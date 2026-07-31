// Package tui is the Bubble Tea front end. A root model owns a stack of
// screens (list → executions → actions → log view, docs/design.md §9.1);
// each screen is an independent model and navigates by emitting
// pushMsg/popMsg. Movement is vim-flavored: h pops, l (or enter) drills in,
// j/k scroll, and v jumps straight to the most relevant CodeBuild log. The
// TUI talks to the core exclusively through the injected function types
// below — it never touches AWS clients directly.
package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

// Fetch loads all pipeline summaries (the CLI layer wires this to the
// core services).
type Fetch func(ctx context.Context, refresh bool, onProgress func(batch.Progress)) ([]model.PipelineSummary, error)

// Refresh force-refetches the statuses of just the given pipelines and
// returns their updated summaries (wired to Statuses with RefreshOnly).
type Refresh func(ctx context.Context, names []string, onProgress func(batch.Progress)) ([]model.PipelineSummary, error)

// DetailFunc loads one pipeline's stage/action state plus recent history
// (wired to pipeline.Detail). The TUI uses it only as the v shortcut's
// fallback on the list, for rows whose latest execution is unknown: one
// GetPipelineState call resolves the build ids to show.
type DetailFunc func(ctx context.Context, name string) (*model.PipelineDetail, error)

// ExecutionsFunc loads one pipeline's recent executions, newest first
// (wired to pipeline.Executions, which serves its session memo within the
// configured TTL). refresh forces a refetch — the executions screen's r key.
type ExecutionsFunc func(ctx context.Context, name string, refresh bool) ([]model.Execution, error)

// ActionsFunc loads the per-action run details of one pipeline execution in
// chronological order (wired to pipeline.ActionExecutions). done marks the
// execution as terminal so the core may serve/store its immutable action
// list from the session memo.
type ActionsFunc func(ctx context.Context, name, execID string, done bool) ([]model.ActionExecution, error)

// LogsFunc fetches a CodeBuild build's raw log lines by build id (wired to
// logs.Service.Fetch). The log screen applies the terraform/raw/summary
// rendering locally, so switching modes needs no refetch.
type LogsFunc func(ctx context.Context, buildID string) ([]string, error)

// ReleaseFunc triggers Release change on the given targets (wired to
// pipeline.Service.Release with the write client). It reports the per-target
// results; the guard (max targets) and in-progress skipping live in the core,
// exactly as for the CLI.
type ReleaseFunc func(ctx context.Context, targets []model.PipelineSummary, onProgress func(batch.Progress)) ([]model.StartExecutionResult, error)

// Deps bundles the core-service closures the TUI needs. The TUI depends only
// on these function types, never on AWS clients directly.
type Deps struct {
	Fetch        Fetch
	Refresh      Refresh
	Detail       DetailFunc
	Executions   ExecutionsFunc
	Actions      ActionsFunc
	Logs         LogsFunc
	Release      ReleaseFunc
	ReleaseLimit int // guard: max targets per release (0 = no limit)
	// PollInterval re-refetches in-flight pipelines on the list screen while
	// any are running (0 disables it). Terminal rows are left alone: they
	// cannot change on their own.
	PollInterval time.Duration
	// Account and Region name the target the TUI is attached to; they are
	// shown in the list header, since the TUI cannot use the stderr banner.
	Account string
	Region  string
}

// screen is one navigable view in the stack. Both the list and the detail
// screen implement it with value semantics.
type screen interface {
	Init() tea.Cmd
	Update(tea.Msg) (screen, tea.Cmd)
	View() string
}

// pushMsg and popMsg drive the screen stack: a screen requests navigation by
// returning a command that emits one of these; the root model interprets
// them before delegating to the top screen.
type (
	pushMsg struct{ s screen }
	popMsg  struct{}
)

// navDepth is the number of levels in the drill-down path
// (list → executions → actions → log).
const navDepth = 4

// navDots renders the drill-down position indicator shown in every screen
// header: one dot per level, the current one bright, the rest dim. It keeps
// the operator oriented even after a v jump straight to the log.
func navDots(current int) string {
	var b strings.Builder
	for i := 1; i <= navDepth; i++ {
		if i == current {
			b.WriteString(activeDotStyle.Render("•"))
		} else {
			b.WriteString(dimStyle.Render("•"))
		}
	}
	return b.String()
}

// clipToWidth reduces s to its first line and at most w terminal cells,
// appending an ellipsis when it had to cut. Width-aware on purpose: byte
// slicing would split a multi-byte rune (commit messages, error text) and
// paint mojibake into the layout.
func clipToWidth(s string, w int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if w <= 0 || ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// fastLogMsg carries the result of the v shortcut's async resolution: a
// ready-to-push log screen for the picked build, or the error explaining why
// none could be resolved. Emitted on the list and on the executions screen
// (both via ActionsFunc, the list falling back to DetailFunc).
type fastLogMsg struct {
	lm  *logModel
	err error
}

// rootModel owns the screen stack and the current terminal size. It
// delegates every non-navigation message to the top screen.
type rootModel struct {
	stack  []screen
	width  int
	height int
}

func (r rootModel) Init() tea.Cmd {
	if len(r.stack) == 0 {
		return nil
	}
	return r.top().Init()
}

func (r rootModel) top() screen { return r.stack[len(r.stack)-1] }

func (r rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r.width, r.height = msg.Width, msg.Height
		// fall through to deliver the resize to the top screen
	case pushMsg:
		r.stack = append(r.stack, msg.s)
		// Init the new screen and hand it the current size immediately, so it
		// renders correctly before any later WindowSizeMsg arrives.
		return r, tea.Batch(msg.s.Init(), sizeCmd(r.width, r.height))
	case popMsg:
		if len(r.stack) > 1 {
			r.stack = r.stack[:len(r.stack)-1]
		}
		// Re-deliver the size to the newly-exposed screen (it may have been
		// resized while covered).
		return r, sizeCmd(r.width, r.height)
	}

	top, cmd := r.top().Update(msg)
	r.stack[len(r.stack)-1] = top
	return r, cmd
}

func (r rootModel) View() string {
	if len(r.stack) == 0 {
		return ""
	}
	return r.top().View()
}

// sizeCmd re-emits the current terminal size as a WindowSizeMsg (nil until
// the first real resize has been seen).
func sizeCmd(w, h int) tea.Cmd {
	if w == 0 && h == 0 {
		return nil
	}
	return func() tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} }
}

// Run starts the TUI and blocks until exit.
func Run(ctx context.Context, deps Deps) error {
	list := newModel(ctx, deps)
	root := rootModel{stack: []screen{list}}
	_, err := tea.NewProgram(root, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	if err == tea.ErrProgramKilled || err == context.Canceled {
		return nil // normal Ctrl-C path
	}
	return err
}
