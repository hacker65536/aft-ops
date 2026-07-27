// Package output renders core results for humans (tables) and machines
// (JSON). Contract: stdout carries data, stderr carries progress and
// diagnostics; JSON documents carry a schema_version for compatibility.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// SchemaVersion is bumped only on breaking JSON shape changes.
//
//	2: `pipeline logs` emits a list of per-build sections instead of one
//	   build object, so an execution's two terraform runs both appear.
const SchemaVersion = 2

// Format selects the rendering mode.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
)

func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatTable, FormatJSON:
		return Format(s), nil
	default:
		return "", fmt.Errorf("invalid output format %q (want table|json)", s)
	}
}

// Document is the stable JSON envelope.
type Document struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Items         any       `json:"items"`
}

// JSON writes items wrapped in the versioned envelope.
func JSON(w io.Writer, items any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(Document{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Items:         items,
	})
}

var (
	styleSucceeded = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleFailed    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleInFlight  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleDim       = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// colPad is the gap between columns.
const colPad = 2

// tableWriter accumulates rows and renders them column-aligned.
//
// text/tabwriter is the obvious tool, but it measures a cell by its rune
// count — and a colorized cell's runes include its ANSI escape sequence.
// Every column after a colored STATUS then gets padded for width the
// terminal never shows, and the headers drift right of the values they
// name. Measuring with ansi.StringWidth, as truncate already does, is what
// keeps a colored table aligned.
type tableWriter struct {
	rows [][]string
}

func (t *tableWriter) row(cells ...string) { t.rows = append(t.rows, cells) }

func (t *tableWriter) flush(w io.Writer) {
	var widths []int
	for _, r := range t.rows {
		for i, c := range r {
			for i >= len(widths) {
				widths = append(widths, 0)
			}
			if n := ansi.StringWidth(c); n > widths[i] {
				widths[i] = n
			}
		}
	}
	var b strings.Builder
	for _, r := range t.rows {
		b.Reset()
		for i, c := range r {
			b.WriteString(c)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-ansi.StringWidth(c)+colPad))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

func styleStatus(s model.Status, color bool) string {
	if !color {
		return string(s)
	}
	switch s {
	case model.StatusSucceeded:
		return styleSucceeded.Render(string(s))
	case model.StatusFailed:
		return styleFailed.Render(string(s))
	case model.StatusInProgress, model.StatusStopping:
		return styleInFlight.Render(string(s))
	default:
		return styleDim.Render(string(s))
	}
}

// PipelineTable renders `pipeline list` rows.
func PipelineTable(w io.Writer, items []model.PipelineSummary, color bool) {
	var tw tableWriter
	tw.row("ACCOUNT NAME", "ACCOUNT ID", "STATUS", "LAST UPDATE", "EXECUTION")
	for _, p := range items {
		last, exec := "-", "-"
		if p.Latest != nil {
			if p.Latest.LastUpdate != nil {
				last = humanTime(*p.Latest.LastUpdate)
			}
			exec = shortID(p.Latest.ID)
		}
		status := styleStatus(p.Status(), color)
		if p.FetchError != "" {
			status = string(model.StatusFetchError)
			if color {
				status = styleFailed.Render(status)
			}
		}
		name := p.AccountName
		if name == "" {
			name = "-"
		}
		tw.row(name, p.AccountID, status, last, exec)
	}
	tw.flush(w)
}

// PipelineCounts prints the per-status tally (stderr companion of the table).
func PipelineCounts(w io.Writer, items []model.PipelineSummary) {
	counts := map[model.Status]int{}
	fetchErrs := 0
	for _, p := range items {
		if p.FetchError != "" {
			fetchErrs++
			continue
		}
		counts[p.Status()]++
	}
	var parts []string
	for _, s := range []model.Status{
		model.StatusSucceeded, model.StatusFailed, model.StatusInProgress,
		model.StatusStopped, model.StatusStopping, model.StatusSuperseded,
		model.StatusCancelled, model.StatusUnknown,
	} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", s, counts[s]))
		}
	}
	if fetchErrs > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", model.StatusFetchError, fetchErrs))
	}
	fmt.Fprintf(w, "total=%d %s\n", len(items), strings.Join(parts, " "))
}

// StatusFreshness prints how fresh the listed statuses are: how many were
// just refetched versus served from cache, the age of the oldest cached
// entry, and how many refreshes failed (those keep serving their previous
// value, so without this line the failure would be invisible). Companion to
// PipelineCounts on stderr; the numbers come from the core, not from
// guessing at timestamps.
func StatusFreshness(w io.Writer, stats model.StatusStats) {
	if stats.Fetched == 0 && stats.FromCache == 0 && stats.Failed == 0 {
		return
	}
	msg := fmt.Sprintf("statuses: %d refetched", stats.Fetched)
	if stats.FromCache > 0 {
		msg += fmt.Sprintf(", %d from cache (oldest %s ago, ttl %s)",
			stats.FromCache, humanDuration(time.Since(stats.Oldest)), humanDuration(stats.TTL))
	}
	if stats.Failed > 0 {
		msg += fmt.Sprintf(" · %d refetch(es) failed, previous values kept", stats.Failed)
	}
	fmt.Fprintln(w, msg)
}

// ReleaseTable renders release results.
func ReleaseTable(w io.Writer, items []model.ReleaseResult, color bool) {
	var tw tableWriter
	tw.row("ACCOUNT NAME", "ACCOUNT ID", "RESULT", "EXECUTION/REASON")
	for _, r := range items {
		result, detail := "started", shortID(r.ExecutionID)
		switch {
		case r.Skipped:
			result, detail = "skipped", r.SkipReason
			if color {
				result = styleDim.Render(result)
			}
		case r.Error != "":
			result, detail = "error", r.Error
			if color {
				result = styleFailed.Render(result)
			}
		default:
			if color {
				result = styleSucceeded.Render(result)
			}
		}
		name := r.AccountName
		if name == "" {
			name = "-"
		}
		tw.row(name, r.AccountID, result, detail)
	}
	tw.flush(w)
}

// ExecutionTable renders `pipeline executions` rows: one pipeline's runs,
// newest first.
func ExecutionTable(w io.Writer, items []model.Execution, color bool) {
	var tw tableWriter
	tw.row("EXECUTION", "STATUS", "STARTED", "DURATION", "REVISION")
	now := time.Now()
	for _, e := range items {
		started, duration := "-", "-"
		if e.StartTime != nil {
			started = humanTime(*e.StartTime)
			duration = fmtDuration(e.Elapsed(now))
		}
		tw.row(e.ID, styleStatus(e.Status, color), started, duration,
			revisionSummary(e.Revisions))
	}
	tw.flush(w)
}

// ActionExecutionTable renders one execution's per-action runs, including the
// CodeBuild id that `pipeline logs --build` takes.
func ActionExecutionTable(w io.Writer, items []model.ActionExecution, color bool) {
	var tw tableWriter
	tw.row("  STAGE", "ACTION", "STATUS", "DURATION", "BUILD", "DETAIL")
	now := time.Now()
	for _, a := range items {
		duration := fmtDuration(a.Elapsed(now))
		build := a.CodeBuildID
		if build == "" {
			build = "-"
		}
		detail := "-"
		switch {
		case a.ErrorMessage != "":
			detail = truncate(a.ErrorMessage, 60)
		case a.Summary != "":
			detail = truncate(a.Summary, 60)
		}
		tw.row("  "+a.StageName, a.ActionName, styleStatus(a.Status, color),
			duration, build, detail)
	}
	tw.flush(w)
}

// PipelineDetailText renders `pipeline show` for humans: a header, the
// stage/action state table, and recent execution history.
func PipelineDetailText(w io.Writer, d model.PipelineDetail, color bool) {
	name := d.AccountName
	if name == "" {
		name = "-"
	}
	fmt.Fprintf(w, "Pipeline: %s\n", d.PipelineName)
	fmt.Fprintf(w, "Account:  %s (%s)\n\n", name, d.AccountID)

	var tw tableWriter
	tw.row("STAGE", "ACTION", "STATUS", "LAST CHANGE", "DETAIL")
	for _, st := range d.Stages {
		if len(st.Actions) == 0 {
			tw.row(st.Name, "-", styleStatus(st.Status, color), "-", "-")
			continue
		}
		for i, a := range st.Actions {
			stageCol := st.Name
			if i > 0 {
				stageCol = "" // group actions under their stage
			}
			last := "-"
			if a.LastChange != nil {
				last = humanTime(*a.LastChange)
			}
			tw.row(stageCol, a.Name, styleStatus(a.Status, color), last, actionDetail(a))
		}
	}
	tw.flush(w)

	if len(d.History) > 0 {
		fmt.Fprintln(w, "\nRecent executions:")
		var htw tableWriter
		htw.row("EXECUTION", "STATUS", "STARTED", "REVISION")
		for _, e := range d.History {
			started := "-"
			if e.StartTime != nil {
				started = humanTime(*e.StartTime)
			}
			htw.row(shortID(e.ID), styleStatus(e.Status, color), started,
				revisionSummary(e.Revisions))
		}
		htw.flush(w)
	}
}

// actionDetail is the most useful one-liner for an action: its error
// message when failed, otherwise its summary.
func actionDetail(a model.ActionState) string {
	switch {
	case a.ErrorMessage != "":
		return truncate(a.ErrorMessage, 60)
	case a.Summary != "":
		return truncate(a.Summary, 60)
	default:
		return "-"
	}
}

func revisionSummary(revs []model.Revision) string {
	if len(revs) == 0 {
		return "-"
	}
	r := revs[0]
	if msg := r.Message(); msg != "" {
		return truncate(msg, 40)
	}
	return shortID(r.RevisionID)
}

// truncate clips s to n terminal cells, appending an ellipsis when it had to
// cut. Both the measurement and the cut are display-width aware: byte
// slicing would split a multi-byte rune (commit messages and account names
// are routinely non-ASCII) and emit invalid UTF-8.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if n <= 0 || ansi.StringWidth(s) <= n {
		return s
	}
	return ansi.Truncate(s, n, "…")
}

// AccountTable renders `account list`.
func AccountTable(w io.Writer, items []model.Account) {
	var tw tableWriter
	tw.row("ACCOUNT NAME", "ACCOUNT ID", "EMAIL")
	for _, a := range items {
		email := a.Email
		if email == "" {
			email = "-"
		}
		tw.row(a.Name, a.ID, email)
	}
	tw.flush(w)
}

// CacheNote prints a staleness banner ("accounts: cached 3h ago ...").
func CacheNote(w io.Writer, what string, fetchedAt time.Time) {
	fmt.Fprintf(w, "%s: cached %s ago (use --refresh to refetch)\n",
		what, humanDuration(time.Since(fetchedAt)))
}

func humanTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04") + " (" + humanDuration(time.Since(t)) + " ago)"
}

// fmtDuration renders an elapsed span for the DURATION columns, where a run
// that has not measurably started yet reads as "-" rather than "0s".
func fmtDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return d.Truncate(time.Second).String()
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "-"
	}
	return id
}
