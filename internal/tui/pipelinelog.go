package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hacker65536/aft-ops/internal/core/logs"
)

// logLoadedMsg carries the raw log lines fetched for a build.
type logLoadedMsg struct {
	lines []string
	err   error
}

// logModel is the log view screen: it fetches a CodeBuild build's log once
// (raw lines) and renders it through the terraform/raw/summary modes locally,
// so cycling modes never refetches. Rendering reuses core/logs so the CLI
// (`pipeline logs`) and TUI produce identical output.
type logModel struct {
	ctx     context.Context
	load    LogsFunc
	buildID string
	action  string

	vp      viewport.Model
	spin    spinner.Model
	loading bool
	err     error
	raw     []string
	mode    logs.Mode
	width   int
	height  int
}

func newLogModel(ctx context.Context, load LogsFunc, buildID, action string, w, h int) logModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	m := logModel{
		ctx: ctx, load: load, buildID: buildID, action: action,
		spin: sp, loading: true, mode: logs.ModeTerraform, width: w, height: h,
	}
	m.vp = viewport.New(max(1, w), max(1, h-chromeHeight))
	return m
}

func (m logModel) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), m.spin.Tick)
}

func (m logModel) loadCmd() tea.Cmd {
	ctx, load, id := m.ctx, m.load, m.buildID
	return func() tea.Msg {
		lines, err := load(ctx, id)
		return logLoadedMsg{lines: lines, err: err}
	}
}

// logModeCycle is the m-key rotation over render modes.
var logModeCycle = []logs.Mode{logs.ModeTerraform, logs.ModeRaw, logs.ModeSummary}

func nextLogMode(mode logs.Mode) logs.Mode {
	for i, c := range logModeCycle {
		if c == mode {
			return logModeCycle[(i+1)%len(logModeCycle)]
		}
	}
	return logModeCycle[0]
}

func logModeName(mode logs.Mode) string {
	switch mode {
	case logs.ModeRaw:
		return "raw"
	case logs.ModeSummary:
		return "summary"
	default:
		return "terraform"
	}
}

func (m logModel) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width
		m.vp.Height = max(1, msg.Height-chromeHeight)
		return m, nil

	case logLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.raw = msg.lines
		m.applyMode()
		return m, nil

	case spinner.TickMsg:
		if !m.loading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc":
			return m, func() tea.Msg { return popMsg{} }
		case "ctrl+c":
			return m, tea.Quit
		case "m":
			m.mode = nextLogMode(m.mode)
			m.applyMode()
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// applyMode re-renders the raw log through the current mode and scrolls to
// the top (the new view's line 1 is what the operator wants to see).
func (m *logModel) applyMode() {
	lines := logs.Render(m.raw, m.mode)
	m.vp.SetContent(strings.Join(lines, "\n"))
	m.vp.GotoTop()
}

func (m logModel) View() string {
	var b strings.Builder

	header := titleStyle.Render("log: " + m.action)
	header += dimStyle.Render("  [mode: " + logModeName(m.mode) + "]")
	if m.loading {
		header += "  " + m.spin.View() + dimStyle.Render(" loading…")
	}
	b.WriteString(header + "\n")

	if m.err != nil {
		b.WriteString(errStyle.Render("error: "+m.err.Error()) + "\n")
		b.WriteString(dimStyle.Render("q/esc back"))
		return b.String()
	}

	b.WriteString(m.vp.View() + "\n")
	b.WriteString(dimStyle.Render("↑/↓ scroll · m mode (terraform/raw/summary) · q/esc back"))
	return b.String()
}
