package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// markSpans swaps the row renderer for one that brackets each styled span, so
// the tests can assert where styling lands without a color profile: <…> is the
// Failed accent and […] a selected row's highlight.
func markSpans(t *testing.T) {
	t.Helper()
	orig := renderSpan
	renderSpan = func(st lipgloss.Style, s string) string {
		switch {
		case st.GetForeground() == failedColor:
			return "<" + s + ">"
		case st.GetBackground() == selectedRowStyle.GetBackground():
			return "[" + s + "]"
		default:
			return s
		}
	}
	t.Cleanup(func() { renderSpan = orig })
}

// plain strips the test markers, so a transformation can be checked against
// the untouched layout.
func plain(s string) string {
	return strings.NewReplacer("<", "", ">", "", "[", "", "]", "").Replace(s)
}

// Only the STATUS cell of a Failed row is styled, and only its word — the
// cell's padding, the other columns, and every other row stay untouched.
func TestStyleTableViewRedensFailedOnly(t *testing.T) {
	markSpans(t)
	cols := execsColumns(80)
	tb := newScreenTable(cols)
	tb.SetHeight(4)
	tb.SetRows([]table.Row{
		{"aaaa1111", "Failed", "2026-07-25 10:00", "4m0s", "fix vpc"},
		{"bbbb2222", "Succeeded", "2026-07-25 10:00", "4m0s", "bump module"},
	})

	out := renderStatusTable(tb, execsStatusCol)
	if !strings.Contains(out, "<Failed>") {
		t.Errorf("the Failed cell should be styled:\n%s", out)
	}
	if strings.Contains(out, "<Succeeded>") || strings.Contains(out, "< ") {
		t.Errorf("only the Failed word should be styled:\n%s", out)
	}
	// Styling must not disturb the layout: dropping the markers restores the
	// table byte for byte.
	if got := plain(out); got != tb.View() {
		t.Errorf("styling changed the laid-out text:\n%q\nwant\n%q", got, tb.View())
	}
}

// Lines the table already styled — the header, its underline, and the cursor
// row — are skipped, since a nested style would reset the row's highlight
// mid-line (the cursor row is marked by its tinted highlight instead).
func TestStyleTableViewSkipsStyledLines(t *testing.T) {
	markSpans(t)
	cols := execsColumns(80)
	view := "\x1b[1mEXECUTION\x1b[0m ... Failed ...\n aaaa1111      Failed"
	out := styleTableView(view, cols, execsStatusCol, -1, nil)
	if strings.Contains(strings.SplitN(out, "\n", 2)[0], "<") {
		t.Errorf("a line carrying escapes must be left alone:\n%q", out)
	}
}

// A row whose earlier columns hold wide (double-width) characters still has
// its STATUS cell located by display width, not by byte offset.
func TestStyleTableViewWideRunes(t *testing.T) {
	markSpans(t)
	cols := columns(80)
	tb := newScreenTable(cols)
	tb.SetHeight(3)
	tb.SetRows([]table.Row{
		{"本番アカウント", "111111111111", "Failed", "2026-07-25 10:00"},
	})

	out := renderStatusTable(tb, listStatusCol)
	if !strings.Contains(out, "<Failed>") {
		t.Errorf("the Failed cell should be styled past wide runes:\n%s", out)
	}
}

// Rows claimed by rowStyle get a whole-line highlight, and a Failed status
// inside one is still reddened as its own span (so the highlight survives the
// color instead of being reset mid-line).
func TestStyleTableViewSelectedRows(t *testing.T) {
	markSpans(t)
	cols := columns(80)
	tb := newScreenTable(cols)
	// The table's height counts its two header lines, so a 3-row body needs 5.
	tb.SetHeight(5)
	tb.SetRows([]table.Row{
		{"alpha", "111111111111", "Failed", "2026-07-25 10:00"},
		{"beta", "222222222222", "Succeeded", "2026-07-25 10:00"},
		{"gamma", "333333333333", "Succeeded", "2026-07-25 10:00"},
	})
	sel := map[string]bool{"111111111111": true, "333333333333": true}

	out := renderSelectableTable(tb, listStatusCol, listKeyCol,
		func(key string) (lipgloss.Style, bool) { return selectedRowStyle, sel[key] })

	lines := strings.Split(out, "\n")
	if len(lines) != 5 { // header, underline, 3 rows
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	// The selected failed row: highlight, with the status as a separate span.
	if !strings.Contains(lines[2], "<Failed>") || !strings.Contains(lines[2], "[") {
		t.Errorf("a selected failed row needs both spans:\n%q", lines[2])
	}
	// The unselected row keeps no highlight; the other selected one gets one.
	if strings.Contains(lines[3], "[") {
		t.Errorf("an unselected row should stay plain:\n%q", lines[3])
	}
	if !strings.HasPrefix(lines[4], "[") {
		t.Errorf("a selected row should be highlighted for its full width:\n%q", lines[4])
	}
	if got := plain(out); got != tb.View() {
		t.Errorf("styling changed the laid-out text:\n%q\nwant\n%q", got, tb.View())
	}
}

func TestCutAtWidth(t *testing.T) {
	for _, tc := range []struct {
		s          string
		w          int
		head, tail string
		ok         bool
	}{
		{"abcdef", 3, "abc", "def", true},
		{"abc", 3, "abc", "", true},
		{"abc", 5, "", "abc", false}, // shorter than the cut
		{"日本語", 4, "日本", "語", true},
		{"日本語", 3, "", "日本語", false}, // a wide rune straddles the cut
		{"abc", 0, "", "abc", true},
	} {
		head, tail, ok := cutAtWidth(tc.s, tc.w)
		if head != tc.head || tail != tc.tail || ok != tc.ok {
			t.Errorf("cutAtWidth(%q, %d) = (%q, %q, %v), want (%q, %q, %v)",
				tc.s, tc.w, head, tail, ok, tc.head, tc.tail, tc.ok)
		}
	}
}

func TestCellStartAndText(t *testing.T) {
	// execsColumns: EXECUTION(12) then STATUS — one space of padding each
	// side, so STATUS' text starts at 12+2+1.
	if got, ok := cellStart(execsColumns(80), execsStatusCol); !ok || got != 15 {
		t.Errorf("cellStart = (%d, %v), want (15, true)", got, ok)
	}
	// Zero-width columns are dropped by the table and must not shift the offset.
	cols := []table.Column{{Title: "A", Width: 0}, {Title: "B", Width: 4}, {Title: "S", Width: 12}}
	if got, ok := cellStart(cols, 2); !ok || got != 7 {
		t.Errorf("cellStart with a hidden column = (%d, %v), want (7, true)", got, ok)
	}
	if _, ok := cellStart(cols, 0); ok {
		t.Error("a zero-width column has no rendered cell")
	}
	if _, ok := cellStart(cols, 9); ok {
		t.Error("an out-of-range column has no rendered cell")
	}

	// cellText reads a cell out of a plain rendered row, trimmed.
	tb := newScreenTable(columns(80))
	tb.SetHeight(3)
	tb.SetRows([]table.Row{{"alpha", "111111111111", "Failed", "2026-07-25 10:00"}})
	row := strings.Split(tb.View(), "\n")[2]
	if got := cellText(row, columns(80), listKeyCol); got != "111111111111" {
		t.Errorf("cellText(key) = %q, want the account id", got)
	}
	if got := cellText("short", columns(80), listKeyCol); got != "" {
		t.Errorf("cellText on a short line = %q, want empty", got)
	}
}

// The cursor row's highlight follows what it has to convey, and is only
// restyled when that changes (restyling re-renders the visible rows).
func TestSyncCursorTint(t *testing.T) {
	tb := newScreenTable(execsColumns(80))
	have := cursorTint{}
	if got := syncCursorTint(&tb, cursorTint{}, have); got != have {
		t.Error("an unchanged cursor state should stay put")
	}
	have = syncCursorTint(&tb, cursorTint{failed: true}, have)
	if !have.failed {
		t.Error("a failed row should tint the highlight")
	}

	neutral := screenTableStyles(cursorTint{})
	failed := screenTableStyles(cursorTint{failed: true})
	selected := screenTableStyles(cursorTint{selected: true})
	if ansi.Strip(neutral.Selected.Render("x")) != "x" {
		t.Error("the selected style should only add styling, not text")
	}
	if neutral.Selected.GetBackground() == failed.Selected.GetBackground() {
		t.Error("the failed tint should differ from the neutral highlight")
	}
	// Selection cannot use the background (the cursor owns it), so it shows as
	// an underline on top.
	if !selected.Selected.GetUnderline() || neutral.Selected.GetUnderline() {
		t.Error("a selected cursor row should be underlined and a plain one not")
	}
	if selected.Selected.GetBackground() != neutral.Selected.GetBackground() {
		t.Error("selection should not change the cursor background")
	}
}
