package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// execsLoadedMsg carries the result of the ExecutionsFunc call.
type execsLoadedMsg struct {
	execs []model.Execution
	err   error
}

// execsModel is the execution history screen: one pipeline's recent
// executions, newest first. l/enter drills into the selected execution's
// actions; v jumps straight to its most relevant CodeBuild log.
type execsModel struct {
	ctx     context.Context
	load    ExecutionsFunc
	actions ActionsFunc
	logs    LogsFunc
	name    string // pipeline name
	acct    string // account display name

	table   table.Model
	spin    spinner.Model
	loading bool
	err     error
	execs   []model.Execution
	// cursor tracks what the cursor row's highlight currently conveys (see
	// syncCursorTint).
	cursor cursorTint
	width  int
	height int
}

// newScreenTable builds a focused table with the shared list styling.
func newScreenTable(cols []table.Column) table.Model {
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	t.SetStyles(screenTableStyles(cursorTint{}))
	return t
}

// execsStatusCol is the STATUS column's index in execsColumns (the column
// whose cells are colored by status).
const execsStatusCol = 1

func execsColumns(width int) []table.Column {
	// The table pads every column by 2 (1 each side), so leave 2 per column
	// of slack or the last column falls off the right edge.
	rev := max(20, width-12-12-18-10-10)
	return []table.Column{
		{Title: "EXECUTION", Width: 12},
		{Title: "STATUS", Width: 12},
		{Title: "STARTED", Width: 18},
		{Title: "DURATION", Width: 10},
		{Title: "REVISION", Width: rev},
	}
}

// execsRevLines is how many source-revision detail lines are reserved below
// the table (AFT customizations pipelines have two source repos: global and
// account customizations).
const execsRevLines = 2

// execsChrome is the number of non-table lines: header, revision detail
// lines, and the key help footer.
const execsChrome = 4 + execsRevLines

func newExecsModel(ctx context.Context, load ExecutionsFunc, actions ActionsFunc,
	logs LogsFunc, name, acct string, w, h int) execsModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	m := execsModel{
		ctx: ctx, load: load, actions: actions, logs: logs,
		name: name, acct: acct,
		table: newScreenTable(execsColumns(max(40, w))),
		spin:  sp, loading: true, width: w, height: h,
	}
	m.table.SetHeight(max(3, h-execsChrome))
	return m
}

func (m execsModel) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(false), m.spin.Tick)
}

func (m execsModel) loadCmd(refresh bool) tea.Cmd {
	ctx, load, name := m.ctx, m.load, m.name
	return func() tea.Msg {
		execs, err := load(ctx, name, refresh)
		return execsLoadedMsg{execs: execs, err: err}
	}
}

func (m execsModel) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table.SetColumns(execsColumns(msg.Width))
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(3, msg.Height-execsChrome))
		return m, nil

	case execsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.execs = msg.execs
		m.setRows()
		return m, nil

	case fastLogMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		lm := *msg.lm
		return m, func() tea.Msg { return pushMsg{s: lm} }

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
		case "l", "enter":
			return m, m.openActions()
		case "v":
			if m.loading {
				return m, nil
			}
			if cmd := m.openFastLog(); cmd != nil {
				m.loading = true
				return m, tea.Batch(cmd, m.spin.Tick)
			}
			return m, nil
		case "r":
			if m.loading {
				return m, nil
			}
			m.loading = true
			return m, tea.Batch(m.loadCmd(true), m.spin.Tick)
		}
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	m.syncCursor()
	return m, cmd
}

// syncCursor keeps the cursor row's highlight in step with the status of the
// execution under it.
func (m *execsModel) syncCursor() {
	var want cursorTint
	if e := m.selectedExec(); e != nil {
		want.failed = e.Status == model.StatusFailed
	}
	m.cursor = syncCursorTint(&m.table, want, m.cursor)
}

// selectedExec returns the execution under the cursor, or nil.
func (m execsModel) selectedExec() *model.Execution {
	cur := m.table.Cursor()
	if cur < 0 || cur >= len(m.execs) {
		return nil
	}
	return &m.execs[cur]
}

// openActions pushes the action list screen for the selected execution. It
// is a no-op when nothing is selected or no ActionsFunc is wired.
func (m execsModel) openActions() tea.Cmd {
	e := m.selectedExec()
	if e == nil || m.actions == nil {
		return nil
	}
	am := newActionsModel(m.ctx, m.actions, m.logs, m.name, m.acct, *e, m.width, m.height)
	return func() tea.Msg { return pushMsg{s: am} }
}

// openFastLog resolves every CodeBuild log of the selected execution via
// ActionsFunc and pushes them into one log screen, in pipeline order — the v
// shortcut, skipping the action list. All of them, not just the failed one:
// an AFT customizations run is two terraform builds (global, then account),
// and which one holds the answer is exactly what the operator is looking for.
func (m execsModel) openFastLog() tea.Cmd {
	e := m.selectedExec()
	if e == nil || m.actions == nil || m.logs == nil {
		return nil
	}
	ctx, actions, logs := m.ctx, m.actions, m.logs
	name, execID, done := m.name, e.ID, e.Status.Terminal()
	w, h := m.width, m.height
	return func() tea.Msg {
		acts, err := actions(ctx, name, execID, done)
		if err != nil {
			return fastLogMsg{err: err}
		}
		builds := model.LogActions(acts)
		if len(builds) == 0 {
			return fastLogMsg{err: fmt.Errorf("no CodeBuild log in execution %s", shortExecID(execID))}
		}
		targets := make([]logTarget, 0, len(builds))
		for _, a := range builds {
			targets = append(targets, logTarget{title: a.ActionName, buildID: a.CodeBuildID})
		}
		lm := newLogModel(ctx, logs, targets, "execution "+shortExecID(execID), w, h)
		return fastLogMsg{lm: &lm}
	}
}

func (m *execsModel) setRows() {
	rows := make([]table.Row, 0, len(m.execs))
	for _, e := range m.execs {
		rows = append(rows, table.Row{
			shortExecID(e.ID),
			string(e.Status),
			fmtTimePtr(e.StartTime),
			fmtDurationPtr(e.StartTime, e.LastUpdate),
			revisionOf(e),
		})
	}
	m.table.SetRows(rows)
	m.syncCursor()
}

// shortExecID abbreviates a pipeline execution id (a UUID) to its first
// segment for table display.
func shortExecID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}

// fmtTimePtr renders an optional time in the local zone, "-" when unknown.
func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// fmtDurationPtr renders the elapsed time between two optional times,
// truncated to seconds; "-" when either end is unknown.
func fmtDurationPtr(from, to *time.Time) string {
	if from == nil || to == nil {
		return "-"
	}
	return to.Sub(*from).Truncate(time.Second).String()
}

// revisionOf summarizes the source revision that triggered an execution:
// the first-by-action-name revision's commit message (unwrapped from the
// CodeConnections JSON where needed), first line only. Ordering by action
// name keeps the column deterministic — the API's revision order varies
// between executions.
func revisionOf(e model.Execution) string {
	best := -1
	for i, r := range e.Revisions {
		if best < 0 || r.ActionName < e.Revisions[best].ActionName {
			best = i
		}
	}
	if best < 0 {
		return "-"
	}
	r := e.Revisions[best]
	if msg := r.Message(); msg != "" {
		return strings.SplitN(msg, "\n", 2)[0]
	}
	if r.RevisionID != "" {
		return r.RevisionID
	}
	return "-"
}

// shortHash abbreviates a commit hash for display.
func shortHash(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// revisionLines renders the selected execution's source revisions —
// "action  hash  message" per repo, console-style — padded/clipped to
// exactly execsRevLines lines so the layout stays stable. Revisions are
// ordered by action name (the API order varies between executions) and the
// action column is padded so hashes and messages line up.
func (m execsModel) revisionLines() []string {
	lines := make([]string, 0, execsRevLines)
	if e := m.selectedExec(); e != nil {
		revs := make([]model.Revision, len(e.Revisions))
		copy(revs, e.Revisions)
		sort.Slice(revs, func(i, j int) bool { return revs[i].ActionName < revs[j].ActionName })

		nameW := 0
		for i, r := range revs {
			if i == execsRevLines {
				break
			}
			nameW = max(nameW, ansi.StringWidth(r.ActionName))
		}
		for _, r := range revs {
			if len(lines) == execsRevLines {
				break
			}
			msg := strings.SplitN(r.Message(), "\n", 2)[0]
			s := fmt.Sprintf("%-*s  %s  %s", nameW, r.ActionName, shortHash(r.RevisionID), msg)
			lines = append(lines, clipToWidth(s, m.width))
		}
	}
	for len(lines) < execsRevLines {
		lines = append(lines, "-")
	}
	return lines
}

func (m execsModel) View() string {
	var b strings.Builder

	title := m.acct
	if title == "" {
		title = "-"
	}
	header := navDots(2) + " " + titleStyle.Render("executions: "+title)
	if m.loading {
		header += "  " + m.spin.View() + dimStyle.Render(" loading…")
	} else {
		header += dimStyle.Render(fmt.Sprintf("  %d executions", len(m.execs)))
	}
	b.WriteString(header + "\n")

	if m.err != nil {
		b.WriteString(errStyle.Render("error: "+m.err.Error()) + "\n")
	}
	b.WriteString(renderStatusTable(m.table, execsStatusCol) + "\n")
	for _, line := range m.revisionLines() {
		b.WriteString(dimStyle.Render(line) + "\n")
	}
	b.WriteString(dimStyle.Render("j/k move · l/enter actions · v log · r refresh · h/q/esc back"))
	return b.String()
}
