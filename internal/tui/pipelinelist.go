package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

type startMsg struct{ refresh bool }

type fetchedMsg struct {
	items []model.PipelineSummary
	err   error
}

// refreshedMsg carries the result of a targeted (selected-row) refresh: the
// updated summaries are merged into the existing list in place.
type refreshedMsg struct {
	items []model.PipelineSummary
	err   error
}

type progressMsg batch.Progress

// refreshNamesMsg asks the list to refetch the given pipelines' statuses and
// clear their selection — emitted by the release screen on its way back so
// just-released rows update to InProgress.
type refreshNamesMsg struct{ names []string }

// statusCycle is the f-key filter rotation.
var statusCycle = []model.Status{
	"", // all
	model.StatusFailed,
	model.StatusInProgress,
	model.StatusSucceeded,
}

// uiModel is the pipeline list screen: incremental filter, status cycling,
// sort controls, and targeted/full refresh. It talks to the core exclusively
// through the injected Fetch/Refresh/DetailFunc function types.
type uiModel struct {
	ctx          context.Context
	fetch        Fetch
	refresh      Refresh
	detail       DetailFunc
	execsFn      ExecutionsFunc
	actionsFn    ActionsFunc
	logs         LogsFunc
	release      ReleaseFunc
	releaseLimit int

	pollInterval time.Duration
	account      string
	region       string

	table   table.Model
	filter  textinput.Model
	spin    spinner.Model
	loading bool

	items      []model.PipelineSummary
	visible    []model.PipelineSummary // aligned with the table's current rows
	selected   map[string]bool         // pipeline names marked for release
	progress   batch.Progress
	progressCh chan batch.Progress
	pollArmed  bool
	pollGen    int
	// cursor tracks what the cursor row's highlight currently conveys (see
	// syncCursorTint).
	cursor    cursorTint
	statusIdx int
	sortKey   model.SortKey
	sortOrder model.SortOrder
	filtering bool
	err       error
	width     int
	height    int
}

// sortCycle is the s-key rotation over sort keys.
var sortCycle = []model.SortKey{
	model.SortByLastUpdate,
	model.SortByStatus,
	model.SortByAccount,
}

func nextSortKey(k model.SortKey) model.SortKey {
	for i, c := range sortCycle {
		if c == k {
			return sortCycle[(i+1)%len(sortCycle)]
		}
	}
	return sortCycle[0]
}

func newModel(ctx context.Context, d Deps) uiModel {
	ti := textinput.New()
	ti.Placeholder = "filter by account name / id"
	ti.Prompt = "/ "

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return uiModel{
		ctx:   ctx,
		fetch: d.Fetch, refresh: d.Refresh, detail: d.Detail,
		execsFn: d.Executions, actionsFn: d.Actions,
		logs: d.Logs, release: d.Release, releaseLimit: d.ReleaseLimit,
		pollInterval: d.PollInterval, account: d.Account, region: d.Region,
		table: newScreenTable(columns(80)), filter: ti, spin: sp,
		selected: map[string]bool{},
		sortKey:  model.SortByLastUpdate, sortOrder: model.OrderDesc,
	}
}

// pollMsg fires one auto-refresh round of the in-flight rows. gen retires
// ticks armed before the timer chain was restarted (see the WindowSizeMsg
// case), so a re-arm never leaves two timers running.
type pollMsg struct{ gen int }

// schedulePoll arms the next auto-refresh when polling is enabled and at
// least one pipeline is running. Terminal rows never change on their own, so
// an all-idle list stops polling entirely and the tool goes quiet. Only one
// tick is ever in flight (pollArmed), so manual refreshes cannot stack up
// parallel timers.
func (m *uiModel) schedulePoll() tea.Cmd {
	if m.pollArmed || m.pollInterval <= 0 || m.refresh == nil || len(m.inFlightNames()) == 0 {
		return nil
	}
	m.pollArmed = true
	gen := m.pollGen
	return tea.Tick(m.pollInterval, func(time.Time) tea.Msg { return pollMsg{gen: gen} })
}

// inFlightNames lists the pipelines that are currently running.
func (m uiModel) inFlightNames() []string {
	var names []string
	for _, it := range m.items {
		if it.Status().InFlight() {
			names = append(names, it.PipelineName)
		}
	}
	return names
}

// Column indices in columns: the STATUS cell is colored by status and the
// ACCOUNT ID cell identifies a rendered row (it is unique per pipeline and
// never truncated), which is how selected rows are found again in the
// laid-out view.
const (
	listStatusCol = 2
	listKeyCol    = 1
)

func columns(width int) []table.Column {
	// The table pads every column by 2 (1 each side), so leave 2 per column of
	// slack or the last column falls off the right edge.
	name := max(20, width-12-12-18-8)
	return []table.Column{
		{Title: "ACCOUNT NAME", Width: name},
		{Title: "ACCOUNT ID", Width: 12},
		{Title: "STATUS", Width: 12},
		{Title: "LAST UPDATE", Width: 18},
	}
}

func (m uiModel) Init() tea.Cmd {
	return func() tea.Msg { return startMsg{refresh: false} }
}

// beginFetch kicks off a fetch goroutine plus a progress listener and
// returns the updated model (value semantics, Bubble Tea style).
func (m uiModel) beginFetch(refresh bool) (uiModel, tea.Cmd) {
	ch := make(chan batch.Progress, 64)
	m.loading = true
	m.err = nil
	m.progress = batch.Progress{}
	m.progressCh = ch

	fetch, ctx := m.fetch, m.ctx
	fetchCmd := func() tea.Msg {
		items, err := fetch(ctx, refresh, func(p batch.Progress) {
			select {
			case ch <- p:
			default: // drop rather than block workers
			}
		})
		close(ch)
		return fetchedMsg{items: items, err: err}
	}
	return m, tea.Batch(fetchCmd, waitProgress(ch), m.spin.Tick)
}

// beginRefreshOne force-refetches a single pipeline's status and returns a
// refreshedMsg to merge it back into the list.
func (m uiModel) beginRefreshOne(name string) (uiModel, tea.Cmd) {
	ch := make(chan batch.Progress, 8)
	m.loading = true
	m.err = nil
	m.progress = batch.Progress{}
	m.progressCh = ch

	refresh, ctx := m.refresh, m.ctx
	refreshCmd := func() tea.Msg {
		items, err := refresh(ctx, []string{name}, func(p batch.Progress) {
			select {
			case ch <- p:
			default:
			}
		})
		close(ch)
		return refreshedMsg{items: items, err: err}
	}
	return m, tea.Batch(refreshCmd, waitProgress(ch), m.spin.Tick)
}

func waitProgress(ch chan batch.Progress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return nil
		}
		return progressMsg(p)
	}
}

func (m uiModel) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case startMsg:
		return m.beginFetch(msg.refresh)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table.SetColumns(columns(msg.Width))
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(3, msg.Height-4))
		// Re-arm the poll. Every message goes to the screen on top of the
		// stack, so a tick that fired while a drill-down was open was
		// delivered there and dropped — leaving pollArmed set and the timer
		// chain broken for the rest of the session. The root re-sends the
		// window size whenever this screen is re-exposed, which is the one
		// signal we get that the chain may need restarting. The generation
		// counter retires any tick that is still pending.
		m.pollGen++
		m.pollArmed = false
		return m, m.schedulePoll()

	case progressMsg:
		m.progress = batch.Progress(msg)
		return m, waitProgress(m.progressCh)

	case fetchedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.items = msg.items
		m.resort()
		return m, m.schedulePoll()

	case refreshedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, m.schedulePoll()
		}
		cur := m.table.Cursor()
		byName := make(map[string]int, len(m.items))
		for i, it := range m.items {
			byName[it.PipelineName] = i
		}
		for _, u := range msg.items {
			if i, ok := byName[u.PipelineName]; ok {
				m.items[i] = u
			}
		}
		m.applyFilter()
		m.table.SetCursor(cur) // keep the operator's place
		return m, m.schedulePoll()

	case pollMsg:
		if msg.gen != m.pollGen {
			return m, nil // retired by a re-arm; a newer tick is pending
		}
		m.pollArmed = false
		if m.loading {
			return m, m.schedulePoll() // busy; try again next interval
		}
		names := m.inFlightNames()
		if len(names) == 0 {
			return m, nil
		}
		return m.beginRefreshMany(names)

	case fastLogMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		lm := *msg.lm
		return m, func() tea.Msg { return pushMsg{s: lm} }

	case refreshNamesMsg:
		for _, n := range msg.names {
			delete(m.selected, n)
		}
		m.applyFilter()
		return m.beginRefreshMany(msg.names)

	case spinner.TickMsg:
		if !m.loading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m uiModel) handleKey(msg tea.KeyMsg) (screen, tea.Cmd) {
	if m.filtering {
		switch msg.String() {
		case "ctrl+c":
			// Not just a shortcut: bubbletea puts the terminal in raw mode,
			// so no SIGINT arrives and this is the only way out short of
			// killing the process from another shell. The log screen's
			// search mode already binds it; only this one was missing.
			return m, tea.Quit
		case "esc":
			m.filtering = false
			m.filter.Blur()
			m.filter.SetValue("")
			m.applyFilter()
			return m, nil
		case "enter":
			m.filtering = false
			m.filter.Blur()
			return m, nil
		default:
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.applyFilter()
			return m, cmd
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "/":
		m.filtering = true
		m.filter.Focus()
		return m, textinput.Blink
	case "f":
		m.statusIdx = (m.statusIdx + 1) % len(statusCycle)
		m.applyFilter()
		return m, nil
	case "s":
		cur := m.table.Cursor()
		m.sortKey = nextSortKey(m.sortKey)
		m.resort()
		m.table.SetCursor(cur)
		return m, nil
	case "o":
		cur := m.table.Cursor()
		if m.sortOrder == model.OrderAsc {
			m.sortOrder = model.OrderDesc
		} else {
			m.sortOrder = model.OrderAsc
		}
		m.resort()
		m.table.SetCursor(cur)
		return m, nil
	case "l", "enter":
		return m, m.openExecutions()
	case "v":
		if m.loading {
			return m, nil
		}
		if cmd := m.openFastLog(); cmd != nil {
			m.loading = true
			return m, tea.Batch(cmd, m.spin.Tick)
		}
		return m, nil
	case " ", "space":
		cur := m.table.Cursor()
		if cur < 0 || cur >= len(m.visible) {
			return m, nil
		}
		name := m.visible[cur].PipelineName
		if m.selected[name] {
			delete(m.selected, name)
		} else {
			m.selected[name] = true
		}
		// Selection is drawn from m.selected at render time, not baked into the
		// rows, so toggling needs no row rebuild — only the cursor row's
		// highlight has to be told (it carries selection as an underline).
		m.syncCursor()
		return m, nil
	case "x":
		return m, m.openRelease()
	case "r":
		if m.loading {
			return m, nil
		}
		cur := m.table.Cursor()
		if cur < 0 || cur >= len(m.visible) {
			return m, nil
		}
		return m.beginRefreshOne(m.visible[cur].PipelineName)
	case "R":
		if m.loading {
			return m, nil
		}
		return m.beginFetch(true)
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	m.syncCursor()
	return m, cmd
}

// syncCursor keeps the cursor row's highlight in step with the pipeline under
// it: its status, and whether it is picked for release (which a plain
// background cannot show while the cursor sits on it).
func (m *uiModel) syncCursor() {
	var want cursorTint
	if cur := m.table.Cursor(); cur >= 0 && cur < len(m.visible) {
		it := m.visible[cur]
		want = cursorTint{failed: it.Status() == model.StatusFailed, selected: m.selected[it.PipelineName]}
	}
	m.cursor = syncCursorTint(&m.table, want, m.cursor)
}

// renderTable draws the list with the rows picked for release on a highlight
// background and Failed statuses in red. Rows are matched by their ACCOUNT ID
// cell, the one column that is unique per row and never truncated.
func (m uiModel) renderTable() string {
	sel := make(map[string]bool, len(m.selected))
	for _, it := range m.items {
		if m.selected[it.PipelineName] && it.AccountID != "" {
			sel[it.AccountID] = true
		}
	}
	return renderSelectableTable(m.table, listStatusCol, listKeyCol,
		func(key string) (lipgloss.Style, bool) { return selectedRowStyle, sel[key] })
}

// openExecutions pushes the execution history screen for the selected row.
// It is a no-op when nothing is selected or no ExecutionsFunc was wired
// (e.g. unit tests).
func (m uiModel) openExecutions() tea.Cmd {
	if m.execsFn == nil {
		return nil
	}
	cur := m.table.Cursor()
	if cur < 0 || cur >= len(m.visible) {
		return nil
	}
	sel := m.visible[cur]
	em := newExecsModel(m.ctx, m.execsFn, m.actionsFn, m.logs,
		sel.PipelineName, sel.AccountName, m.width, m.height)
	return func() tea.Msg { return pushMsg{s: em} }
}

// openFastLog resolves the selected pipeline's terraform logs and pushes the
// log screen directly — the v shortcut for the everyday case ("it failed,
// show me the terraform log") without walking executions → actions. All of
// its builds, not just one: an AFT customizations run is two terraform builds
// (global, then account customizations), and which one holds the answer is
// exactly what the operator is looking for.
//
// The row already knows its latest execution, so the builds are taken from
// that execution's action runs. That is one ListActionExecutions call and,
// unlike the pipeline's current state, self-consistent: GetPipelineState
// reports each action's own latest run, so a stage that did not run in the
// latest execution would contribute a log from an older one. Rows whose
// latest execution is unknown (a status fetch that failed) fall back to the
// state call, which is all there is to go on.
func (m uiModel) openFastLog() tea.Cmd {
	if m.logs == nil {
		return nil
	}
	cur := m.table.Cursor()
	if cur < 0 || cur >= len(m.visible) {
		return nil
	}
	sel := m.visible[cur]
	ctx, logs := m.ctx, m.logs
	w, h := m.width, m.height

	if m.actionsFn != nil && sel.Latest != nil && sel.Latest.ID != "" {
		actions := m.actionsFn
		name, execID, done := sel.PipelineName, sel.Latest.ID, sel.Latest.Status.Terminal()
		return func() tea.Msg {
			acts, err := actions(ctx, name, execID, done)
			if err != nil {
				return fastLogMsg{err: err}
			}
			targets := execLogTargets(acts)
			if len(targets) == 0 {
				return fastLogMsg{err: fmt.Errorf("no CodeBuild log in execution %s of %s",
					shortExecID(execID), name)}
			}
			lm := newLogModel(ctx, logs, targets, rowLabel(sel), w, h)
			return fastLogMsg{lm: &lm}
		}
	}

	if m.detail == nil {
		return nil
	}
	detail := m.detail
	return func() tea.Msg {
		d, err := detail(ctx, sel.PipelineName)
		if err != nil {
			return fastLogMsg{err: err}
		}
		targets := stateLogTargets(d)
		if len(targets) == 0 {
			return fastLogMsg{err: fmt.Errorf("no CodeBuild log found for %s", sel.PipelineName)}
		}
		lm := newLogModel(ctx, logs, targets, rowLabel(sel), w, h)
		return fastLogMsg{lm: &lm}
	}
}

// rowLabel names a list row for a screen title: its account name, else the
// pipeline name (an account the resolver could not name).
func rowLabel(p model.PipelineSummary) string {
	if p.AccountName != "" {
		return p.AccountName
	}
	return p.PipelineName
}

// openRelease pushes the release confirm/run screen for the selected rows.
// It is a no-op when nothing is selected or no ReleaseFunc was wired.
func (m uiModel) openRelease() tea.Cmd {
	if m.release == nil {
		return nil
	}
	var targets []model.PipelineSummary
	for _, it := range m.items {
		if m.selected[it.PipelineName] {
			targets = append(targets, it)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	model.SortSummaries(targets, model.SortByAccount, model.OrderAsc)
	rm := newReleaseModel(m.ctx, m.release, m.refresh, targets, m.releaseLimit, m.width, m.height)
	return func() tea.Msg { return pushMsg{s: rm} }
}

// beginRefreshMany force-refetches several pipelines' statuses (RefreshOnly)
// and merges them back in place via refreshedMsg. Used after a release.
func (m uiModel) beginRefreshMany(names []string) (uiModel, tea.Cmd) {
	if len(names) == 0 || m.refresh == nil {
		return m, nil
	}
	ch := make(chan batch.Progress, 64)
	m.loading = true
	m.err = nil
	m.progress = batch.Progress{}
	m.progressCh = ch

	refresh, ctx := m.refresh, m.ctx
	refreshCmd := func() tea.Msg {
		items, err := refresh(ctx, names, func(p batch.Progress) {
			select {
			case ch <- p:
			default:
			}
		})
		close(ch)
		return refreshedMsg{items: items, err: err}
	}
	return m, tea.Batch(refreshCmd, waitProgress(ch), m.spin.Tick)
}

// resort re-sorts items by the current key/order and rebuilds the rows.
func (m *uiModel) resort() {
	model.SortSummaries(m.items, m.sortKey, m.sortOrder)
	m.applyFilter()
}

// applyFilter recomputes visible rows from items + filter text + status.
func (m *uiModel) applyFilter() {
	q := strings.ToLower(m.filter.Value())
	want := statusCycle[m.statusIdx]

	var rows []table.Row
	var visible []model.PipelineSummary
	for _, it := range m.items {
		if want != "" && it.Status() != want {
			continue
		}
		if q != "" &&
			!strings.Contains(strings.ToLower(it.AccountName), q) &&
			!strings.Contains(it.AccountID, q) {
			continue
		}
		last := "-"
		if it.Latest != nil && it.Latest.LastUpdate != nil {
			last = it.Latest.LastUpdate.Local().Format("2006-01-02 15:04")
		}
		status := string(it.Status())
		if it.FetchError != "" {
			status = "fetch-error"
		}
		name := it.AccountName
		if name == "" {
			name = "-"
		}
		rows = append(rows, table.Row{name, it.AccountID, status, last})
		visible = append(visible, it)
	}
	m.table.SetRows(rows)
	m.visible = visible
	m.syncCursor()
}

var (
	titleStyle     = lipgloss.NewStyle().Bold(true)
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	activeDotStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
)

func (m uiModel) View() string {
	var b strings.Builder

	header := navDots(1) + " " + titleStyle.Render("aft-ops — account pipelines")
	if m.account != "" {
		header += dimStyle.Render("  [" + m.account + " " + m.region + "]")
	}
	if want := statusCycle[m.statusIdx]; want != "" {
		header += dimStyle.Render("  [status: " + string(want) + "]")
	}
	header += dimStyle.Render("  [sort: " + string(m.sortKey) + " " + orderArrow(m.sortOrder) + "]")
	if n := len(m.selected); n > 0 {
		header += titleStyle.Render(fmt.Sprintf("  [%d selected]", n))
	}
	if m.loading {
		header += "  " + m.spin.View()
		if m.progress.Total > 0 {
			header += fmt.Sprintf(" fetching %d/%d", m.progress.Done, m.progress.Total)
		} else {
			header += dimStyle.Render(" loading…")
		}
	} else {
		header += dimStyle.Render(fmt.Sprintf("  %d shown / %d total", len(m.table.Rows()), len(m.items)))
	}
	b.WriteString(header + "\n")

	if m.filtering || m.filter.Value() != "" {
		b.WriteString(m.filter.View() + "\n")
	}
	if m.err != nil {
		b.WriteString(errStyle.Render("error: "+m.err.Error()) + "\n")
	}
	b.WriteString(m.renderTable() + "\n")
	b.WriteString(dimStyle.Render("q quit · / filter · f status · s sort · o order · l/enter executions · v log · space select · x release · r/R refresh"))
	return b.String()
}

func orderArrow(o model.SortOrder) string {
	if o == model.OrderAsc {
		return "↑"
	}
	return "↓"
}
