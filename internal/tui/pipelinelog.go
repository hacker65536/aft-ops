package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/hacker65536/aft-ops/internal/core/logs"
)

// matchLineStyle marks the current search match's line (reverse video, like
// less's highlight).
var matchLineStyle = lipgloss.NewStyle().Reverse(true)

// chromeHeight is the number of lines the log screen's header and footer
// consume; the viewport gets the rest.
const chromeHeight = 3

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

	// less-style search state: / opens the input, enter commits the query,
	// n/N walk the matches, esc clears. Matching is a case-insensitive
	// substring test against the current mode's rendered lines.
	search    textinput.Model
	searching bool // the / input is focused
	query     string
	lines     []string // current mode's rendered lines (highlight source)
	matches   []int    // indices into lines
	matchIdx  int
}

func newLogModel(ctx context.Context, load LogsFunc, buildID, action string, w, h int) logModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	ti := textinput.New()
	ti.Prompt = "/"
	m := logModel{
		ctx: ctx, load: load, buildID: buildID, action: action,
		spin: sp, loading: true, mode: logs.ModeTerraform, width: w, height: h,
		search: ti,
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
		if m.searching {
			switch msg.String() {
			case "esc", "ctrl+c":
				m.searching = false
				m.search.Blur()
				return m, nil
			case "enter":
				m.searching = false
				m.search.Blur()
				m.query = m.search.Value()
				m.findMatches()
				m.jumpToMatchFrom(m.vp.YOffset)
				m.setContent()
				return m, nil
			default:
				var cmd tea.Cmd
				m.search, cmd = m.search.Update(msg)
				return m, cmd
			}
		}
		switch msg.String() {
		case "h", "q":
			return m, func() tea.Msg { return popMsg{} }
		case "esc":
			// esc first clears an active search (like less's highlight
			// reset), then pops on the next press.
			if m.query != "" {
				m.query = ""
				m.matches = nil
				m.setContent()
				return m, nil
			}
			return m, func() tea.Msg { return popMsg{} }
		case "ctrl+c":
			return m, tea.Quit
		case "m":
			m.mode = nextLogMode(m.mode)
			m.applyMode()
			return m, nil
		case "/":
			m.searching = true
			m.search.SetValue("")
			m.search.Focus()
			return m, textinput.Blink
		case "n":
			m.stepMatch(1)
			return m, nil
		case "N":
			m.stepMatch(-1)
			return m, nil
		// The viewport's default keymap has no top/bottom jumps; match the
		// table screens' g/G (and home/end) so navigation feels uniform.
		case "g", "home":
			m.vp.GotoTop()
			return m, nil
		case "G", "end":
			m.vp.GotoBottom()
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// applyMode re-renders the raw log through the current mode and scrolls to
// the top (the new view's line 1 is what the operator wants to see). An
// active search is re-run against the new rendering, since line content and
// indices both change with the mode.
func (m *logModel) applyMode() {
	m.lines = logs.Render(m.raw, m.mode)
	m.findMatches()
	m.setContent()
	m.vp.GotoTop()
}

// findMatches recomputes the match line indices for the current query
// (case-insensitive substring over ANSI-stripped lines).
func (m *logModel) findMatches() {
	m.matches = nil
	m.matchIdx = 0
	if m.query == "" {
		return
	}
	q := strings.ToLower(m.query)
	for i, l := range m.lines {
		if strings.Contains(strings.ToLower(ansi.Strip(l)), q) {
			m.matches = append(m.matches, i)
		}
	}
}

// jumpToMatchFrom selects the first match at or below the given line
// (wrapping to the first match overall), less-style: a fresh search starts
// from the current scroll position.
func (m *logModel) jumpToMatchFrom(top int) {
	if len(m.matches) == 0 {
		return
	}
	m.matchIdx = 0
	for i, line := range m.matches {
		if line >= top {
			m.matchIdx = i
			break
		}
	}
	m.vp.SetYOffset(m.matches[m.matchIdx])
}

// stepMatch moves to the next/previous match with wraparound and scrolls it
// to the top of the viewport.
func (m *logModel) stepMatch(delta int) {
	if len(m.matches) == 0 {
		return
	}
	n := len(m.matches)
	m.matchIdx = (m.matchIdx + delta + n) % n
	m.vp.SetYOffset(m.matches[m.matchIdx])
	m.setContent()
}

// setContent refreshes the viewport body, marking the current match's line
// in reverse video (the marked line is ANSI-stripped first so the reverse
// span covers the whole line instead of fighting the log's own colors).
func (m *logModel) setContent() {
	if len(m.matches) == 0 {
		m.vp.SetContent(strings.Join(m.lines, "\n"))
		return
	}
	out := make([]string, len(m.lines))
	copy(out, m.lines)
	cur := m.matches[m.matchIdx]
	out[cur] = matchLineStyle.Render(ansi.Strip(m.lines[cur]))
	m.vp.SetContent(strings.Join(out, "\n"))
}

func (m logModel) View() string {
	var b strings.Builder

	header := navDots(4) + " " + titleStyle.Render("log: "+m.action)
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
	switch {
	case m.searching:
		b.WriteString(m.search.View())
	case m.query != "":
		status := "no match"
		if len(m.matches) > 0 {
			status = fmt.Sprintf("%d/%d", m.matchIdx+1, len(m.matches))
		}
		b.WriteString(dimStyle.Render("/" + m.query + "  (" + status + ") · n/N next/prev · esc clear · h/q back"))
	default:
		b.WriteString(dimStyle.Render("j/k scroll · g/G top/bottom · / search · m mode (terraform/raw/summary) · h/q/esc back"))
	}
	return b.String()
}
