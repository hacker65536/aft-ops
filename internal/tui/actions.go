package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hacker65536/aft-ops/internal/core/logs"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

// Styles for the add/change/destroy counts in a terraform verdict line
// (terraform's own plan colors: green/yellow/red).
var (
	addStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	changeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	destroyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// verdictNumRe captures the counts in both apply verdicts
// ("1 added, 0 changed, 2 destroyed") and plan verdicts
// ("1 to add, 0 to change, 2 to destroy").
var verdictNumRe = regexp.MustCompile(`(\d+) (added|changed|destroyed|to add|to change|to destroy)`)

// renderSummary renders a (pre-clipped) summary line for the footer: the
// non-zero add/change/destroy counts get terraform's plan colors, the rest
// stays dim. Coloring happens after clipping, so width math is unaffected.
func renderSummary(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range verdictNumRe.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(dimStyle.Render(s[last:loc[0]]))
		num, word := s[loc[2]:loc[3]], s[loc[4]:loc[5]]
		style := dimStyle // a zero count stays dim
		if num != "0" {
			switch {
			case strings.Contains(word, "add"):
				style = addStyle
			case strings.Contains(word, "change"):
				style = changeStyle
			default:
				style = destroyStyle
			}
		}
		b.WriteString(style.Render(num))
		b.WriteString(dimStyle.Render(" " + word))
		last = loc[1]
	}
	b.WriteString(dimStyle.Render(s[last:]))
	return b.String()
}

// actionsLoadedMsg carries the result of the ActionsFunc call.
type actionsLoadedMsg struct {
	actions []model.ActionExecution
	err     error
}

// verdictMsg carries one build's lazily-fetched terraform verdict line
// ("Apply complete! ..." / "Error: ..."), keyed by build id. An empty
// verdict (fetch failed or no terraform ran) leaves the summary as-is.
type verdictMsg struct {
	buildID string
	verdict string
}

// actionsModel is the action list screen: the per-action runs of one
// pipeline execution in chronological order. The selected action's summary
// and error are shown inline below the table (there is no separate action
// detail screen — the rows are few and the detail is thin). l/enter/v opens
// the selected action's CodeBuild log.
type actionsModel struct {
	ctx  context.Context
	load ActionsFunc
	logs LogsFunc
	name string // pipeline name
	acct string // account display name
	exec model.Execution

	table   table.Model
	spin    spinner.Model
	loading bool
	err     error
	actions []model.ActionExecution
	// verdicts maps a build id to its terraform verdict line, lazily
	// fetched from the build log after the action list loads (which also
	// pre-warms the log memo, so opening the log screen is instant).
	verdicts map[string]string
	width    int
	height   int
}

// actionsChrome is the number of non-table lines: header, inline detail
// (summary + error), and the key help footer.
const actionsChrome = 6

// stageColWidth fits the longest stock AFT stage name
// ("AFT-Account-Customizations").
const stageColWidth = 26

func actionsColumns(width int) []table.Column {
	// The table pads every column by 2 (1 each side), so leave 2 per column
	// of slack or the last column falls off the right edge.
	action := max(20, width-stageColWidth-12-18-10-10)
	return []table.Column{
		{Title: "STAGE", Width: stageColWidth},
		{Title: "ACTION", Width: action},
		{Title: "STATUS", Width: 12},
		{Title: "STARTED", Width: 18},
		{Title: "DURATION", Width: 10},
	}
}

func newActionsModel(ctx context.Context, load ActionsFunc, logs LogsFunc,
	name, acct string, exec model.Execution, w, h int) actionsModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	m := actionsModel{
		ctx: ctx, load: load, logs: logs,
		name: name, acct: acct, exec: exec,
		table: newScreenTable(actionsColumns(max(40, w))),
		spin:  sp, loading: true, width: w, height: h,
	}
	m.table.SetHeight(max(3, h-actionsChrome))
	return m
}

func (m actionsModel) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), m.spin.Tick)
}

func (m actionsModel) loadCmd() tea.Cmd {
	ctx, load, name := m.ctx, m.load, m.name
	execID, done := m.exec.ID, m.exec.Status.Terminal()
	return func() tea.Msg {
		actions, err := load(ctx, name, execID, done)
		return actionsLoadedMsg{actions: actions, err: err}
	}
}

func (m actionsModel) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table.SetColumns(actionsColumns(msg.Width))
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(3, msg.Height-actionsChrome))
		return m, nil

	case actionsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.actions = msg.actions
		m.setRows()
		return m, m.verdictCmds()

	case verdictMsg:
		if msg.verdict != "" {
			if m.verdicts == nil {
				m.verdicts = map[string]string{}
			}
			m.verdicts[msg.buildID] = msg.verdict
		}
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
		case "h", "q", "esc":
			return m, func() tea.Msg { return popMsg{} }
		case "ctrl+c":
			return m, tea.Quit
		case "l", "enter", "v":
			return m, m.openLog()
		case "r":
			if m.loading {
				return m, nil
			}
			m.loading = true
			return m, tea.Batch(m.loadCmd(), m.spin.Tick)
		}
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// verdictCmds spawns one background fetch per terminal CodeBuild action to
// pull its log and extract the terraform verdict line. Fetch errors are
// swallowed (the summary just stays as the API's) and completed builds land
// in the log memo, doubling as a prefetch for the log screen.
func (m actionsModel) verdictCmds() tea.Cmd {
	if m.logs == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, a := range m.actions {
		if a.CodeBuildID == "" || !a.Status.Terminal() {
			continue
		}
		ctx, load, id := m.ctx, m.logs, a.CodeBuildID
		cmds = append(cmds, func() tea.Msg {
			lines, err := load(ctx, id)
			if err != nil {
				return verdictMsg{buildID: id}
			}
			return verdictMsg{buildID: id, verdict: logs.Verdict(lines)}
		})
	}
	return tea.Batch(cmds...)
}

// selectedAction returns the action under the cursor, or nil.
func (m actionsModel) selectedAction() *model.ActionExecution {
	cur := m.table.Cursor()
	if cur < 0 || cur >= len(m.actions) {
		return nil
	}
	return &m.actions[cur]
}

// openLog pushes the log screen for the selected action. It is a no-op when
// the action carries no CodeBuild id (e.g. a source action) or no LogsFunc
// is wired.
func (m actionsModel) openLog() tea.Cmd {
	a := m.selectedAction()
	if a == nil || m.logs == nil || a.CodeBuildID == "" {
		return nil
	}
	lm := newLogModel(m.ctx, m.logs, a.CodeBuildID, a.ActionName, m.width, m.height)
	return func() tea.Msg { return pushMsg{s: lm} }
}

func (m *actionsModel) setRows() {
	rows := make([]table.Row, 0, len(m.actions))
	for _, a := range m.actions {
		rows = append(rows, table.Row{
			a.StageName,
			a.ActionName,
			string(a.Status),
			fmtTimePtr(a.StartTime),
			fmtDurationPtr(a.StartTime, a.LastUpdate),
		})
	}
	m.table.SetRows(rows)
}

// detailLines renders the selected action's summary and error inline (each
// clipped to one line so the layout stays stable). The lazily-fetched
// terraform verdict ("Apply complete! ..." / "Error: ...") takes precedence
// over the API's own summary — it says what actually happened.
func (m actionsModel) detailLines() (summary, errMsg string) {
	a := m.selectedAction()
	if a == nil {
		return "", ""
	}
	clip := func(s string) string {
		s = strings.SplitN(s, "\n", 2)[0]
		if m.width > 3 && len(s) > m.width {
			return s[:m.width-1] + "…"
		}
		return s
	}
	summary = a.Summary
	if v := m.verdicts[a.CodeBuildID]; v != "" {
		summary = v
	}
	return clip(summary), clip(a.ErrorMessage)
}

func (m actionsModel) View() string {
	var b strings.Builder

	title := m.acct
	if title == "" {
		title = "-"
	}
	header := navDots(3) + " " + titleStyle.Render("actions: "+title)
	header += dimStyle.Render(fmt.Sprintf("  [execution: %s %s]", shortExecID(m.exec.ID), m.exec.Status))
	if m.loading {
		header += "  " + m.spin.View() + dimStyle.Render(" loading…")
	}
	b.WriteString(header + "\n")

	if m.err != nil {
		b.WriteString(errStyle.Render("error: "+m.err.Error()) + "\n")
	}
	b.WriteString(m.table.View() + "\n")

	summary, errMsg := m.detailLines()
	if summary == "" {
		summary = "-"
	}
	b.WriteString(dimStyle.Render("summary: ") + renderSummary(summary) + "\n")
	if errMsg != "" {
		b.WriteString(errStyle.Render("error: "+errMsg) + "\n")
	}
	b.WriteString(dimStyle.Render("j/k move · l/enter/v log · r refresh · h/q/esc back"))
	return b.String()
}
