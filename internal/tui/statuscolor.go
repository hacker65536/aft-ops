package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// Row styling for the table screens. Everything here is applied to the
// laid-out view rather than to the row values: bubbles' table truncates each
// cell with a width helper that counts escape bytes as visible width, so a
// pre-styled value comes back mangled ("\x1b[31mFailed\x1b[…").
//
// The cost of that choice is a dependency on how bubbles lays a row out —
// each cell padded by one space on each side, zero-width columns dropped —
// which cellStart below reimplements. Nothing enforces the agreement, so a
// bubbles upgrade can shift the offsets and quietly color the wrong column.
// go.mod pins bubbles for that reason; if you raise it, suspect
// TestCellStartRendersWhereBubblesPuts (which measures a real rendered row
// rather than trusting the constant), TestCellStartAndText, and
// TestSyncCursorTint before anything else.
var (
	// failedColor reddens an alarming STATUS cell — the thing an operator
	// scans a list for.
	failedColor = lipgloss.Color("1")
	// selectedRowStyle marks a row picked for release (the list's space key).
	// A whole-row highlight rather than a marker column: it reads at a glance
	// and costs no width.
	selectedRowStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("0")).Background(lipgloss.Color("7"))
)

// alarming reports whether a STATUS cell should be reddened.
//
// fetch-error is not a CodePipeline status; it is what the tables print for a
// row whose status could not be retrieved. The CLI table has always reddened
// it, and a row the tool could not read is exactly as worth noticing as one
// that failed — dimming it in the TUI only made the two views disagree.
func alarming(s model.Status) bool {
	return s == model.StatusFailed || s == model.StatusFetchError
}

// cursorTint is what the cursor row's highlight has to convey beyond "the
// cursor is here": the run failed, and (on the list) the row is picked for
// release. The cursor row cannot carry styled cells — the table renders it as
// one styled span, so a nested color's reset would drop the highlight for the
// rest of the line — so its extra state goes into the highlight itself.
type cursorTint struct {
	failed   bool
	selected bool
}

// screenTableStyles is the shared list styling for the given cursor state.
func screenTableStyles(t cursorTint) table.Styles {
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).BorderForeground(lipgloss.Color("8"))

	bg := lipgloss.Color("6")
	if t.failed {
		bg = lipgloss.Color("9")
	}
	st.Selected = st.Selected.Foreground(lipgloss.Color("0")).Background(bg)
	if t.selected {
		// The background is already spoken for by the cursor, so selection
		// shows as an underline on top of it — a continuous bar, since
		// lipgloss underlines spaces too by default. (It does that by styling
		// the spaces separately, so such a row renders as many short escape
		// runs instead of one; only ever one row, so it does not matter.)
		st.Selected = st.Selected.Underline(true)
	}
	return st
}

// syncCursorTint restyles the cursor row only when what it must convey
// changed, since restyling re-renders every visible row. Callers keep the
// returned state and pass it back as have.
func syncCursorTint(t *table.Model, want, have cursorTint) cursorTint {
	if want != have {
		t.SetStyles(screenTableStyles(want))
	}
	return want
}

// rowStyleFunc reports the base style for a row, identified by the text of its
// key column, and whether it needs one at all.
type rowStyleFunc func(key string) (lipgloss.Style, bool)

// renderStatusTable renders a table with the STATUS column at statusCol
// colored by status.
func renderStatusTable(t table.Model, statusCol int) string {
	return styleTableView(t.View(), t.Columns(), statusCol, -1, nil)
}

// renderSelectableTable renders a table with the STATUS column colored by
// status and whole-row highlights for the rows rowStyle claims, identified by
// their keyCol cell.
func renderSelectableTable(t table.Model, statusCol, keyCol int, rowStyle rowStyleFunc) string {
	return styleTableView(t.View(), t.Columns(), statusCol, keyCol, rowStyle)
}

// styleTableView re-styles the plain body lines of a laid-out table: the row's
// base style (from rowStyle, keyed on its keyCol cell) covers the whole line,
// and a STATUS cell reading "Failed" is reddened on top of it. Both spans are
// rendered separately so each opens and closes its own escape sequence, which
// is what keeps the row highlight intact across the colored word.
//
// Lines that already carry escape sequences — the header, its underline, and
// the cursor row, all styled by the table itself — are skipped; the cursor row
// conveys the same state through its highlight (see cursorTint).
func styleTableView(view string, cols []table.Column, statusCol, keyCol int, rowStyle rowStyleFunc) string {
	start, ok := cellStart(cols, statusCol)
	if !ok {
		return view
	}
	width := cols[statusCol].Width

	lines := strings.Split(view, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "\x1b") {
			continue
		}

		base, hasBase := lipgloss.NewStyle(), false
		if rowStyle != nil {
			if key := cellText(ln, cols, keyCol); key != "" {
				if st, ok := rowStyle(key); ok {
					base, hasBase = st, true
				}
			}
		}

		head, rest, okHead := cutAtWidth(ln, start)
		cell, tail, okCell := "", "", false
		if okHead {
			cell, tail, okCell = cutAtWidth(rest, width)
		}
		word := strings.TrimSpace(cell)
		if !okCell || !alarming(model.Status(word)) {
			if hasBase {
				lines[i] = renderSpan(base, ln)
			}
			continue
		}

		accent := base.Foreground(failedColor)
		at := strings.Index(cell, word)
		lines[i] = renderSpan(base, head+cell[:at]) + renderSpan(accent, word) +
			renderSpan(base, cell[at+len(word):]+tail)
	}
	return strings.Join(lines, "\n")
}

// renderSpan applies a style to one span of a rendered row. It is a package
// var so tests can assert where the styling lands without depending on the
// terminal's color profile.
var renderSpan = func(st lipgloss.Style, s string) string { return st.Render(s) }

// cellText returns the trimmed text of one column's cell in a plain rendered
// row, or "" when the row is too short or that column is not rendered.
func cellText(line string, cols []table.Column, idx int) string {
	start, ok := cellStart(cols, idx)
	if !ok {
		return ""
	}
	_, rest, ok := cutAtWidth(line, start)
	if !ok {
		return ""
	}
	cell, _, ok := cutAtWidth(rest, cols[idx].Width)
	if !ok {
		return ""
	}
	return strings.TrimSpace(cell)
}

// cellStart returns the display column where the given table column's text
// begins in a rendered row. Every cell is padded by one space on each side and
// zero-width columns are dropped, exactly as the table's own row rendering
// does. ok is false when that column is not rendered at all.
func cellStart(cols []table.Column, idx int) (int, bool) {
	if idx < 0 || idx >= len(cols) || cols[idx].Width <= 0 {
		return 0, false
	}
	off := 0
	for i, c := range cols {
		if c.Width <= 0 {
			continue
		}
		if i == idx {
			return off + 1, true
		}
		off += c.Width + 2
	}
	return 0, false
}

// cutAtWidth splits an escape-free string at the given display width. ok is
// false when no rune boundary lands exactly there — the string is shorter, or
// a wide rune straddles the cut — in which case the caller leaves the line
// alone rather than slicing through a character.
func cutAtWidth(s string, w int) (head, tail string, ok bool) {
	if w <= 0 {
		return "", s, w == 0
	}
	acc := 0
	for i, r := range s {
		if acc == w {
			return s[:i], s[i:], true
		}
		// Table cells are overwhelmingly printable ASCII (ids, timestamps,
		// statuses), and ansi.StringWidth needs a string per rune, so take the
		// one-column shortcut before paying for it.
		if r >= 0x20 && r < utf8.RuneSelf {
			acc++
		} else {
			acc += ansi.StringWidth(string(r))
		}
		if acc > w {
			return "", s, false
		}
	}
	if acc == w {
		return s, "", true
	}
	return "", s, false
}
