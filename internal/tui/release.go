package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/core/model"
	"github.com/hacker65536/aft-ops/internal/output"
)

// releasePhase is the release screen's state machine: confirm the targets,
// run the batch, then show the results.
type releasePhase int

const (
	phaseConfirm releasePhase = iota
	phaseRunning
	phaseDone
)

// releaseDoneMsg carries the batch release outcome.
type releaseDoneMsg struct {
	results []model.ReleaseResult
	err     error
}

// releaseModel is the multi-select release screen. It shows the selected
// targets and the guard verdict (confirm), runs the batch with progress
// (running), then the per-target results (done). Release goes through the
// injected ReleaseFunc — the same core path and guard as `pipeline release`.
type releaseModel struct {
	ctx     context.Context
	run     ReleaseFunc
	targets []model.PipelineSummary
	limit   int

	phase      releasePhase
	vp         viewport.Model
	spin       spinner.Model
	progress   batch.Progress
	progressCh chan batch.Progress
	results    []model.ReleaseResult
	err        error
	width      int
	height     int
}

// releaseChrome is the header + status + footer line budget; the viewport
// gets the rest.
const releaseChrome = 4

func newReleaseModel(ctx context.Context, run ReleaseFunc, targets []model.PipelineSummary, limit, w, h int) releaseModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	m := releaseModel{
		ctx: ctx, run: run, targets: targets, limit: limit,
		phase: phaseConfirm, spin: sp, width: w, height: h,
	}
	m.vp = viewport.New(max(1, w), max(1, h-releaseChrome))
	m.vp.SetContent(renderSummaries(targets))
	return m
}

// overLimit reports whether the target count trips the guard.
func (m releaseModel) overLimit() bool {
	return m.limit > 0 && len(m.targets) > m.limit
}

func (m releaseModel) Init() tea.Cmd { return nil }

func (m releaseModel) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width
		m.vp.Height = max(1, msg.Height-releaseChrome)
		return m, nil

	case progressMsg:
		m.progress = batch.Progress(msg)
		return m, waitProgress(m.progressCh)

	case releaseDoneMsg:
		m.phase = phaseDone
		m.results = msg.results
		m.err = msg.err
		m.vp.SetContent(renderResults(msg.results))
		m.vp.GotoTop()
		return m, nil

	case spinner.TickMsg:
		if m.phase != phaseRunning {
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

func (m releaseModel) handleKey(msg tea.KeyMsg) (screen, tea.Cmd) {
	switch m.phase {
	case phaseConfirm:
		switch msg.String() {
		case "y", "enter":
			if m.overLimit() {
				return m, nil // guard blocks confirmation
			}
			return m.begin()
		case "n", "q", "esc":
			return m, func() tea.Msg { return popMsg{} }
		case "ctrl+c":
			return m, tea.Quit
		}
	case phaseRunning:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		// ignore other keys while the batch runs
	case phaseDone:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		// any other key: back to the list, refreshing the started rows so
		// they update to InProgress.
		started := startedNames(m.results)
		return m, tea.Batch(
			func() tea.Msg { return popMsg{} },
			func() tea.Msg { return refreshNamesMsg{names: started} },
		)
	}
	return m, nil
}

// begin transitions to the running phase and kicks off the batch release.
func (m releaseModel) begin() (screen, tea.Cmd) {
	ch := make(chan batch.Progress, 64)
	m.phase = phaseRunning
	m.progress = batch.Progress{}
	m.progressCh = ch

	ctx, run, targets := m.ctx, m.run, m.targets
	runCmd := func() tea.Msg {
		results, err := run(ctx, targets, func(p batch.Progress) {
			select {
			case ch <- p:
			default:
			}
		})
		close(ch)
		return releaseDoneMsg{results: results, err: err}
	}
	return m, tea.Batch(runCmd, waitProgress(ch), m.spin.Tick)
}

func (m releaseModel) View() string {
	var b strings.Builder

	switch m.phase {
	case phaseConfirm:
		b.WriteString(titleStyle.Render(fmt.Sprintf("release %d pipeline(s)?", len(m.targets))) + "\n")
		if m.overLimit() {
			b.WriteString(errStyle.Render(fmt.Sprintf(
				"exceeds guard limit of %d — deselect %d to proceed",
				m.limit, len(m.targets)-m.limit)) + "\n")
		} else {
			b.WriteString(dimStyle.Render("this triggers StartPipelineExecution on each target") + "\n")
		}
		b.WriteString(m.vp.View() + "\n")
		if m.overLimit() {
			b.WriteString(dimStyle.Render("over limit · n/esc cancel"))
		} else {
			b.WriteString(dimStyle.Render("y/enter release · n/esc cancel"))
		}

	case phaseRunning:
		b.WriteString(titleStyle.Render("releasing…") + "  " + m.spin.View() +
			fmt.Sprintf(" %d/%d (failed %d)", m.progress.Done, m.progress.Total, m.progress.Failed) + "\n")
		b.WriteString(dimStyle.Render("do not close the terminal") + "\n")
		b.WriteString(m.vp.View() + "\n")
		b.WriteString(dimStyle.Render("working…"))

	case phaseDone:
		started, skipped, failed := releaseCounts(m.results)
		head := titleStyle.Render("release complete")
		head += dimStyle.Render(fmt.Sprintf("  started %d · skipped %d · failed %d", started, skipped, failed))
		b.WriteString(head + "\n")
		if m.err != nil {
			b.WriteString(errStyle.Render("error: "+m.err.Error()) + "\n")
		} else {
			b.WriteString(dimStyle.Render("released rows will refresh to InProgress") + "\n")
		}
		b.WriteString(m.vp.View() + "\n")
		b.WriteString(dimStyle.Render("any key: back to list"))
	}
	return b.String()
}

// renderSummaries and renderResults reuse the CLI table renderers so the
// confirm list and results match `pipeline release` output.
func renderSummaries(items []model.PipelineSummary) string {
	var b strings.Builder
	output.PipelineTable(&b, items, false)
	return strings.TrimRight(b.String(), "\n")
}

func renderResults(items []model.ReleaseResult) string {
	var b strings.Builder
	output.ReleaseTable(&b, items, false)
	return strings.TrimRight(b.String(), "\n")
}

func startedNames(results []model.ReleaseResult) []string {
	var out []string
	for _, r := range results {
		if r.ExecutionID != "" && r.Error == "" {
			out = append(out, r.PipelineName)
		}
	}
	return out
}

func releaseCounts(results []model.ReleaseResult) (started, skipped, failed int) {
	for _, r := range results {
		switch {
		case r.Error != "":
			failed++
		case r.Skipped:
			skipped++
		case r.ExecutionID != "":
			started++
		}
	}
	return started, skipped, failed
}
