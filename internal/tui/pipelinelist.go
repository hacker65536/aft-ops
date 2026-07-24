package tui

import (
	"context"
	"fmt"
	"strings"

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
	logs         LogsFunc
	release      ReleaseFunc
	releaseLimit int

	table   table.Model
	filter  textinput.Model
	spin    spinner.Model
	loading bool

	items      []model.PipelineSummary
	visible    []model.PipelineSummary // aligned with the table's current rows
	selected   map[string]bool         // pipeline names marked for release
	progress   batch.Progress
	progressCh chan batch.Progress
	statusIdx  int
	sortKey    model.SortKey
	sortOrder  model.SortOrder
	filtering  bool
	err        error
	width      int
	height     int
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

	t := table.New(
		table.WithColumns(columns(80)),
		table.WithFocused(true),
	)
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).BorderForeground(lipgloss.Color("8"))
	st.Selected = st.Selected.Foreground(lipgloss.Color("0")).
		Background(lipgloss.Color("6"))
	t.SetStyles(st)

	return uiModel{
		ctx: ctx,
		fetch: d.Fetch, refresh: d.Refresh, detail: d.Detail,
		logs: d.Logs, release: d.Release, releaseLimit: d.ReleaseLimit,
		table: t, filter: ti, spin: sp,
		selected: map[string]bool{},
		sortKey:  model.SortByLastUpdate, sortOrder: model.OrderDesc,
	}
}

// selMark is the first column's width; it shows a check for selected rows.
const selMark = 2

func columns(width int) []table.Column {
	name := max(20, width-selMark-12-12-18-10)
	return []table.Column{
		{Title: "", Width: selMark},
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
		return m, nil

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
		return m, nil

	case refreshedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
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
		return m, nil

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
	case "enter":
		return m, m.openDetail()
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
		m.applyFilter()
		m.table.SetCursor(cur)
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
	return m, cmd
}

// openDetail pushes the detail screen for the selected row. It is a no-op
// when nothing is selected or no DetailFunc was wired (e.g. unit tests).
func (m uiModel) openDetail() tea.Cmd {
	if m.detail == nil {
		return nil
	}
	cur := m.table.Cursor()
	if cur < 0 || cur >= len(m.visible) {
		return nil
	}
	sel := m.visible[cur]
	d := newDetailModel(m.ctx, m.detail, m.logs, sel.PipelineName, sel.AccountName, m.width, m.height)
	return func() tea.Msg { return pushMsg{s: d} }
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
	rm := newReleaseModel(m.ctx, m.release, targets, m.releaseLimit, m.width, m.height)
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
		mark := ""
		if m.selected[it.PipelineName] {
			mark = "✓"
		}
		rows = append(rows, table.Row{mark, name, it.AccountID, status, last})
		visible = append(visible, it)
	}
	m.table.SetRows(rows)
	m.visible = visible
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

func (m uiModel) View() string {
	var b strings.Builder

	header := titleStyle.Render("aft-ops — account pipelines")
	if want := statusCycle[m.statusIdx]; want != "" {
		header += dimStyle.Render("  [status: " + string(want) + "]")
	}
	header += dimStyle.Render("  [sort: " + string(m.sortKey) + " " + orderArrow(m.sortOrder) + "]")
	if n := len(m.selected); n > 0 {
		header += titleStyle.Render(fmt.Sprintf("  [%d selected]", n))
	}
	if m.loading {
		header += "  " + m.spin.View() +
			fmt.Sprintf(" fetching %d/%d", m.progress.Done, m.progress.Total)
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
	b.WriteString(m.table.View() + "\n")
	b.WriteString(dimStyle.Render("q quit · / filter · f status · s sort · o order · enter details · space select · x release · r/R refresh"))
	return b.String()
}

func orderArrow(o model.SortOrder) string {
	if o == model.OrderAsc {
		return "↑"
	}
	return "↓"
}
