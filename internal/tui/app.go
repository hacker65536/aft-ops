// Package tui is the Bubble Tea front end. Phase 1 ships the pipeline
// list screen: incremental filter, status cycling, and refresh. It talks
// to the core exclusively through the Fetch function type.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

// Fetch loads all pipeline summaries (the CLI layer wires this to the
// core services).
type Fetch func(ctx context.Context, refresh bool, onProgress func(batch.Progress)) ([]model.PipelineSummary, error)

// Run starts the TUI and blocks until exit.
func Run(ctx context.Context, fetch Fetch) error {
	m := newModel(ctx, fetch)
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	if err == tea.ErrProgramKilled || err == context.Canceled {
		return nil // normal Ctrl-C path
	}
	return err
}

type startMsg struct{ refresh bool }

type fetchedMsg struct {
	items []model.PipelineSummary
	err   error
}

type progressMsg batch.Progress

// statusCycle is the f-key filter rotation.
var statusCycle = []model.Status{
	"", // all
	model.StatusFailed,
	model.StatusInProgress,
	model.StatusSucceeded,
}

type uiModel struct {
	ctx   context.Context
	fetch Fetch

	table   table.Model
	filter  textinput.Model
	spin    spinner.Model
	loading bool

	items      []model.PipelineSummary
	progress   batch.Progress
	progressCh chan batch.Progress
	statusIdx  int
	filtering  bool
	err        error
	width      int
	height     int
}

func newModel(ctx context.Context, fetch Fetch) uiModel {
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

	return uiModel{ctx: ctx, fetch: fetch, table: t, filter: ti, spin: sp}
}

func columns(width int) []table.Column {
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

func waitProgress(ch chan batch.Progress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return nil
		}
		return progressMsg(p)
	}
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		items := msg.items
		sort.Slice(items, func(i, j int) bool {
			if items[i].AccountName != items[j].AccountName {
				return items[i].AccountName < items[j].AccountName
			}
			return items[i].AccountID < items[j].AccountID
		})
		m.items = items
		m.applyFilter()
		return m, nil

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

func (m uiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
	case "r":
		if m.loading {
			return m, nil
		}
		return m.beginFetch(false)
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

// applyFilter recomputes visible rows from items + filter text + status.
func (m *uiModel) applyFilter() {
	q := strings.ToLower(m.filter.Value())
	want := statusCycle[m.statusIdx]

	var rows []table.Row
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
	}
	m.table.SetRows(rows)
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
	b.WriteString(dimStyle.Render("q quit · / filter · f status · r refresh · R hard refresh"))
	return b.String()
}
