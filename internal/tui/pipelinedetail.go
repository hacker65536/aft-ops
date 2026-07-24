package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hacker65536/aft-ops/internal/core/model"
	"github.com/hacker65536/aft-ops/internal/output"
)

// detailLoadedMsg carries the result of the pipeline.Detail call.
type detailLoadedMsg struct {
	d   *model.PipelineDetail
	err error
}

// detailModel is the pipeline detail screen: it fetches one pipeline's
// stage/action state plus recent history and shows it in a scrollable
// viewport. Rendering reuses output.PipelineDetailText so the CLI and TUI
// present identical detail text.
type detailModel struct {
	ctx     context.Context
	load    DetailFunc
	logs    LogsFunc
	name    string
	acct    string

	vp      viewport.Model
	spin    spinner.Model
	loading bool
	err     error
	d       *model.PipelineDetail
	width   int
	height  int
}

func newDetailModel(ctx context.Context, load DetailFunc, logs LogsFunc, name, acct string, w, h int) detailModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	m := detailModel{
		ctx: ctx, load: load, logs: logs, name: name, acct: acct,
		spin: sp, loading: true, width: w, height: h,
	}
	m.vp = viewport.New(max(1, w), max(1, h-chromeHeight))
	return m
}

// chromeHeight is the number of lines the header and footer consume; the
// viewport gets the rest.
const chromeHeight = 3

func (m detailModel) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), m.spin.Tick)
}

func (m detailModel) loadCmd() tea.Cmd {
	ctx, load, name := m.ctx, m.load, m.name
	return func() tea.Msg {
		d, err := load(ctx, name)
		return detailLoadedMsg{d: d, err: err}
	}
}

func (m detailModel) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width
		m.vp.Height = max(1, msg.Height-chromeHeight)
		return m, nil

	case detailLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.d = msg.d
		m.vp.SetContent(m.render())
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
		case "l":
			if cmd := m.openLog(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// openLog pushes the log screen for the pipeline's most relevant CodeBuild
// action: the failed one if any, else the last action carrying a build id
// (so a successful apply's log is still viewable). It is a no-op when no
// LogsFunc is wired or no action has a build id.
func (m detailModel) openLog() tea.Cmd {
	if m.logs == nil || m.d == nil {
		return nil
	}
	id, action := logBuildID(m.d)
	if id == "" {
		return nil
	}
	lm := newLogModel(m.ctx, m.logs, id, action, m.width, m.height)
	return func() tea.Msg { return pushMsg{s: lm} }
}

// logBuildID picks the CodeBuild action to show logs for: the first failed
// action, otherwise the last action across stages that has a build id.
func logBuildID(d *model.PipelineDetail) (id, action string) {
	if fa := d.FailedActions(); len(fa) > 0 {
		return fa[0].CodeBuildID, fa[0].Name
	}
	for _, st := range d.Stages {
		for _, a := range st.Actions {
			if a.CodeBuildID != "" {
				id, action = a.CodeBuildID, a.Name
			}
		}
	}
	return id, action
}

// render formats the loaded detail into the viewport body, reusing the CLI's
// text renderer (with color, which renders fine inside the alt screen).
func (m detailModel) render() string {
	if m.d == nil {
		return ""
	}
	var b strings.Builder
	output.PipelineDetailText(&b, *m.d, true)
	return strings.TrimRight(b.String(), "\n")
}

func (m detailModel) View() string {
	var b strings.Builder

	title := m.acct
	if title == "" {
		title = "-"
	}
	header := titleStyle.Render("detail: " + title)
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
	b.WriteString(dimStyle.Render("↑/↓ scroll · l logs · q/esc back"))
	return b.String()
}
