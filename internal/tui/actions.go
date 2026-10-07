package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

// verdictsMsg carries the lazily-fetched terraform results keyed by build
// id. Builds whose fetch failed, or that ran no terraform, carry no verdict
// line and keep the API's own summary.
type verdictsMsg struct {
	verdicts map[string]model.TerraformResult
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
	// results reads the terraform result of each build (wired to
	// result.Service.ForActions — the same path the list's result columns
	// take). nil disables the verdict fetch.
	results BuildResultsFunc
	name    string // pipeline name
	acct    string // account display name
	exec    model.Execution

	table   table.Model
	spin    spinner.Model
	loading bool
	err     error
	actions []model.ActionExecution
	// verdicts maps a build id to its terraform result, lazily fetched
	// after the action list loads. A result the list already read is served
	// from the shared results cache without a request.
	verdicts map[string]model.TerraformResult
	// cursor tracks what the cursor row's highlight currently conveys (see
	// syncCursorTint).
	cursor cursorTint
	width  int
	height int
}

// actionsChrome is the number of non-table lines: header, inline detail
// (summary + error), and the key help footer.
const actionsChrome = 6

// stageColWidth fits the longest stock AFT stage name
// ("AFT-Account-Customizations").
const stageColWidth = 26

// actionsStatusCol is the STATUS column's index in actionsColumns.
const actionsStatusCol = 2

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
		return m, m.verdictCmd()

	case verdictsMsg:
		if len(msg.verdicts) > 0 {
			if m.verdicts == nil {
				m.verdicts = map[string]model.TerraformResult{}
			}
			for id, v := range msg.verdicts {
				m.verdicts[id] = v
			}
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
	m.syncCursor()
	return m, cmd
}

// syncCursor keeps the cursor row's highlight in step with the status of the
// action run under it.
func (m *actionsModel) syncCursor() {
	var want cursorTint
	if a := m.selectedAction(); a != nil {
		want.failed = a.Status == model.StatusFailed
	}
	m.cursor = syncCursorTint(&m.table, want, m.cursor)
}

// verdictCmd reads the terraform result of each terminal CodeBuild action
// in the background. It goes through the same core path as the list's
// result columns (result.Service.ForActions): served from the results cache
// when the list — or an earlier visit — already read it, and otherwise from
// the tail of the build log, under the run's shared API rate limit. Fetch
// errors are swallowed here: the summary just stays as the API's.
func (m actionsModel) verdictCmd() tea.Cmd {
	if m.results == nil {
		return nil
	}
	var builds []model.ActionExecution
	for _, a := range m.actions {
		if a.CodeBuildID != "" && a.Status.Terminal() {
			builds = append(builds, a)
		}
	}
	if len(builds) == 0 {
		return nil
	}
	ctx, read := m.ctx, m.results
	return func() tea.Msg {
		out, _ := read(ctx, builds)
		return verdictsMsg{verdicts: out}
	}
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
	lm := newLogModel(m.ctx, m.logs, oneLogTarget(a.CodeBuildID, a.StageName, a.ActionName),
		"execution "+shortExecID(m.exec.ID), m.width, m.height)
	return func() tea.Msg { return pushMsg{s: lm} }
}

func (m *actionsModel) setRows() {
	rows := make([]table.Row, 0, len(m.actions))
	now := time.Now()
	for _, a := range m.actions {
		rows = append(rows, table.Row{
			a.StageName,
			a.ActionName,
			string(a.Status),
			fmtTimePtr(a.StartTime),
			fmtElapsed(a.Elapsed(now)),
		})
	}
	m.table.SetRows(rows)
	m.syncCursor()
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
	summary = a.Summary
	if v := m.verdicts[a.CodeBuildID].Line; v != "" {
		summary = v
	}
	return clipToWidth(summary, m.width), clipToWidth(a.ErrorMessage, m.width)
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
	b.WriteString(renderStatusTable(m.table, actionsStatusCol) + "\n")

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
