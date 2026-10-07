package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// resultsModel is a list of three rows (one failed, listed last) wired to a
// ResultsFunc that records what it was asked for and answers from want, keyed
// by execution id.
func resultsModel(t *testing.T, want map[string]model.ExecutionResults) (*uiModel, *[][]string) {
	t.Helper()
	var asked [][]string
	m := newModel(context.Background(), Deps{
		Results: func(_ context.Context, items []model.PipelineSummary,
			onResult func(string, model.ExecutionResults, error)) {
			var names []string
			for _, it := range items {
				names = append(names, it.PipelineName)
				onResult(it.PipelineName, want[it.Latest.ID], nil)
			}
			asked = append(asked, names)
		},
	})
	m.items = []model.PipelineSummary{
		withExec(sum("a", "111111111111", "alpha", model.StatusSucceeded), "ea"),
		withExec(sum("b", "222222222222", "bravo", model.StatusSucceeded), "eb"),
		withExec(sum("c", "333333333333", "charlie", model.StatusFailed), "ec"),
	}
	m.applyFilter()
	return &m, &asked
}

func withExec(s model.PipelineSummary, id string) model.PipelineSummary {
	s.Latest.ID = id
	return s
}

// drain runs a command and every command it leads to, feeding each message
// back into the model — the event loop, minus the terminal.
func drain(t *testing.T, m uiModel, cmd tea.Cmd) uiModel {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 100 {
			t.Fatal("the result read did not settle")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if msg == nil {
			continue
		}
		next, more := m.Update(msg)
		m = next.(uiModel)
		queue = append(queue, more)
	}
	return m
}

func applied(execID string, add, change, destroy int) model.ExecutionResults {
	return model.ExecutionResults{
		ExecutionID: execID,
		Global:      &model.TerraformResult{Kind: model.ResultNoChanges},
		Account: &model.TerraformResult{Kind: model.ResultApplied,
			Add: add, Change: change, Destroy: destroy},
	}
}

// After the rows arrive the result columns start at "…", fill in row by row,
// and the failed row is read first.
func TestResultColumnsFillInAfterTheRows(t *testing.T) {
	m, asked := resultsModel(t, map[string]model.ExecutionResults{
		"ea": applied("ea", 0, 1, 0),
		"eb": applied("eb", 0, 0, 0),
		"ec": {ExecutionID: "ec",
			Global:  &model.TerraformResult{Kind: model.ResultError},
			Account: &model.TerraformResult{Kind: model.ResultNotRun}},
	})
	if got := m.table.Rows()[0][listGlobalCol]; got != "…" {
		t.Errorf("before the read, GLOBAL = %q, want …", got)
	}

	next, cmd := m.Update(fetchedMsg{items: m.items})
	got := drain(t, next.(uiModel), cmd)

	if len(*asked) != 1 || (*asked)[0][0] != "c" {
		t.Fatalf("read order = %v, want the failed row first", *asked)
	}
	cells := map[string][2]string{}
	for _, r := range got.table.Rows() {
		cells[r[0]] = [2]string{r[listGlobalCol], r[listAccountCol]}
	}
	want := map[string][2]string{
		"alpha":   {"·", "+0 ~1 -0"},
		"bravo":   {"·", "+0 ~0 -0"},
		"charlie": {"✗ error", "—"},
	}
	for name, w := range want {
		if cells[name] != w {
			t.Errorf("%s = %v, want %v", name, cells[name], w)
		}
	}
	if got.resultsBusy {
		t.Error("the read should be over")
	}
}

// Rows whose results are known for their current execution are not read
// again; a new execution, or a status change, makes a row stale.
func TestResultsRereadOnlyWhatChanged(t *testing.T) {
	m, asked := resultsModel(t, map[string]model.ExecutionResults{
		"ea": applied("ea", 1, 0, 0), "eb": applied("eb", 1, 0, 0), "ec": applied("ec", 1, 0, 0),
		"eb2": applied("eb2", 0, 0, 1),
	})
	next, cmd := m.Update(fetchedMsg{items: m.items})
	got := drain(t, next.(uiModel), cmd)

	// bravo started a new run: only it is read again, and until its results
	// arrive it shows "…" rather than the previous run's numbers.
	moved := withExec(sum("b", "222222222222", "bravo", model.StatusSucceeded), "eb2")
	got.items[1] = moved
	got.applyFilter()
	for _, r := range got.table.Rows() {
		if r[0] == "bravo" && r[listAccountCol] != "…" {
			t.Errorf("a new execution should hide the old results, got %q", r[listAccountCol])
		}
	}
	next, cmd = got.Update(refreshedMsg{items: []model.PipelineSummary{moved}})
	got = drain(t, next.(uiModel), cmd)
	if n := len(*asked); n != 2 || len((*asked)[1]) != 1 || (*asked)[1][0] != "b" {
		t.Fatalf("second read = %v, want just bravo", *asked)
	}
}

// A row whose results could not be read shows "?" and is counted in the
// header, so the gap is never silent.
func TestResultsErrorIsVisible(t *testing.T) {
	m := newModel(context.Background(), Deps{
		Results: func(_ context.Context, items []model.PipelineSummary,
			onResult func(string, model.ExecutionResults, error)) {
			for _, it := range items {
				onResult(it.PipelineName, model.ExecutionResults{ExecutionID: it.Latest.ID},
					errors.New("AccessDeniedException"))
			}
		},
	})
	m.items = []model.PipelineSummary{withExec(sum("a", "111111111111", "alpha", model.StatusSucceeded), "ea")}
	next, cmd := m.Update(fetchedMsg{items: m.items})
	got := drain(t, next.(uiModel), cmd)

	if c := got.table.Rows()[0][listGlobalCol]; c != "?" {
		t.Errorf("GLOBAL = %q, want ?", c)
	}
	if !strings.Contains(got.View(), "1 unreadable") {
		t.Errorf("header should count the unreadable row:\n%s", got.View())
	}
}

// Non-zero counts take terraform's colors, zero counts and placeholders stay
// dim, and the cell's text is unchanged.
func TestStyleResultCell(t *testing.T) {
	orig := renderSpan
	t.Cleanup(func() { renderSpan = orig })
	renderSpan = func(st lipgloss.Style, s string) string {
		switch st.GetForeground() {
		case resultAddColor:
			return "<add:" + s + ">"
		case resultChangeColor:
			return "<chg:" + s + ">"
		case resultDestroyColor:
			return "<red:" + s + ">"
		case resultDimColor:
			return "<dim:" + s + ">"
		}
		return s
	}
	cases := map[string]string{
		"+1 ~0 -2  ": "<add:+1> <dim:~0> <red:-2>  ",
		"·         ": "<dim:·>         ",
		"✗ error   ": "<red:✗> <red:error>   ",
		"running   ": "<chg:running>   ",
	}
	for in, want := range cases {
		got, ok := styleResultCell(lipgloss.NewStyle(), in)
		if !ok || got != want {
			t.Errorf("styleResultCell(%q) = %q, want %q", in, got, want)
		}
	}
}

// The header says whether the result columns are complete: a running read
// shows progress without a check mark, and a finished one keeps "✓ n/n" on
// screen rather than vanishing (which would look like never having started).
func TestResultsIndicator(t *testing.T) {
	m, _ := resultsModel(t, map[string]model.ExecutionResults{
		"ea": applied("ea", 1, 0, 0), "eb": applied("eb", 0, 0, 0), "ec": applied("ec", 0, 0, 0),
	})
	m.resultsBusy = true
	m.results["a"] = rowResults{execID: "ea", r: applied("ea", 1, 0, 0)}
	if got := m.resultsIndicator(); !strings.Contains(got, "1/3") || strings.Contains(got, "✓") {
		t.Errorf("running indicator = %q, want progress 1/3 without a check mark", got)
	}

	m.resultsBusy = false
	next, cmd := m.Update(fetchedMsg{items: m.items})
	got := drain(t, next.(uiModel), cmd)
	if ind := got.resultsIndicator(); !strings.Contains(ind, "✓ 3/3") {
		t.Errorf("finished indicator = %q, want ✓ 3/3", ind)
	}
}

// The header puts what changes ahead of the fixed context, so a narrow
// terminal cuts the sort order before the results indicator.
func TestHeaderOrderPutsResultsFirst(t *testing.T) {
	m, _ := resultsModel(t, map[string]model.ExecutionResults{})
	m.account, m.region = "123456789012", "ap-northeast-1"
	m.statusIdx = 1 // Failed
	head := strings.SplitN(m.View(), "\n", 2)[0]
	order := []string{"results", "[status:", "shown /", "[123456789012", "[sort:"}
	last := -1
	for _, w := range order {
		i := strings.Index(head, w)
		if i < 0 || i < last {
			t.Fatalf("header %q: %q missing or out of order (want %v)", head, w, order)
		}
		last = i
	}
}
