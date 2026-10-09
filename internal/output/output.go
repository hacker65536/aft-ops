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
	pipelineTable(w, items, color, false)
}

// PipelineResultsTable renders `pipeline list --results` rows: the plain
// list plus the latest execution's terraform result per layer
// (docs/design.md §4.6).
func PipelineResultsTable(w io.Writer, items []model.PipelineSummary, color bool) {
	pipelineTable(w, items, color, true)
}

func pipelineTable(w io.Writer, items []model.PipelineSummary, color, results bool) {
	var tw tableWriter
	if results {
		tw.row("ACCOUNT NAME", "ACCOUNT ID", "STATUS", "GLOBAL", "ACCOUNT", "LAST UPDATE", "EXECUTION")
	} else {
		tw.row("ACCOUNT NAME", "ACCOUNT ID", "STATUS", "LAST UPDATE", "EXECUTION")
	}
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
		if !results {
			tw.row(name, p.AccountID, status, last, exec)
			continue
		}
		global, acct := "-", "-"
		if p.Results != nil {
			global = styleResult(p.Results.Global.Short(), color)
			acct = styleResult(p.Results.Account.Short(), color)
		}
		tw.row(name, p.AccountID, status, global, acct, last, exec)
	}
	tw.flush(w)
}

// styleResult colors a terraform result cell word by word, as the TUI does:
// non-zero counts in terraform's plan colors, failures red, a running build
// yellow, and everything that means "nothing to see" dim.
func styleResult(s string, color bool) string {
	if !color {
		return s
	}
	words := strings.Split(s, " ")
	for i, w := range words {
		words[i] = resultWordStyle(w).Render(w)
	}
	return strings.Join(words, " ")
}

func resultWordStyle(w string) lipgloss.Style {
	switch w {
	case "✗", "error", "failed":
		return styleFailed
	case "running":
		return styleInFlight
	}
	if len(w) > 1 && strings.Trim(w[1:], "0123456789") == "" && strings.Trim(w[1:], "0") != "" {
		switch w[0] {
		case '+':
			return styleSucceeded
		case '~':
			return styleInFlight
		case '-':
			return styleFailed
		}
	}
	return styleDim
}

// ResultsNote reports on stderr how many rows' terraform results could not
// be read (shown as "?" in the table), with the first reason. Silent when
// every row was read.
func ResultsNote(w io.Writer, items []model.PipelineSummary) {
	n, first := 0, ""
	for _, p := range items {
		if p.ResultsError != "" {
			if n == 0 {
				first = p.ResultsError
			}
			n++
		}
	}
	if n > 0 {
		fmt.Fprintf(w, "results: %d unreadable (first: %s)\n", n, truncate(first, 120))
	}
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
	// Build the line rather than always interpolating a separator: with no
	// items there are no parts, and "total=0 " went out with a trailing
	// space on it.
	line := fmt.Sprintf("total=%d", len(items))
	if len(parts) > 0 {
		line += " " + strings.Join(parts, " ")
	}
	fmt.Fprintln(w, line)
}

// Freshness prints how fresh a cached fan-out's rows are: how many were just
// refetched versus served from cache, the age of the oldest cached entry, and
// how many refreshes failed (those keep serving their previous value, so
// without this line the failure would be invisible). Companion to the counts
// line on stderr; the numbers come from the core, not from guessing at
// timestamps.
//
// what names the thing being counted ("statuses", "triggers"). Both cached
// fan-outs report through here, and a drift report in particular has to say
// out loud when its verdict came from cache.
func Freshness(w io.Writer, what string, stats model.FetchStats) {
	if stats.Fetched == 0 && stats.FromCache == 0 && stats.Failed == 0 {
		return
	}
	msg := fmt.Sprintf("%s: %d refetched", what, stats.Fetched)
	if stats.FromCache > 0 {
		msg += fmt.Sprintf(", %d from cache (oldest %s ago, ttl %s)",
			stats.FromCache, humanDuration(time.Since(stats.Oldest)), humanDuration(stats.TTL))
	}
	if stats.Failed > 0 {
		msg += fmt.Sprintf(" · %d refetch(es) failed, previous values kept", stats.Failed)
	}
	fmt.Fprintln(w, msg)
}

// TriggerTable renders `pipeline triggers` rows.
func TriggerTable(w io.Writer, items []model.TriggerSummary, color bool) {
	var tw tableWriter
	tw.row("ACCOUNT NAME", "ACCOUNT ID", "TRIGGER", "DETAIL")
	for _, t := range items {
		name := t.AccountName
		if name == "" {
			name = "-"
		}
		tw.row(name, t.AccountID, styleTriggerState(t.State, color), triggerDetail(t))
	}
	tw.flush(w)
}

// styleTriggerState colors a verdict by what it asks of the reader: drift is
// a discrepancy to look at, a missing trigger means the pipeline is not being
// started by pushes at all, and unknown is the report admitting it could not
// judge — dim, because it is not a finding about the pipeline.
func styleTriggerState(s model.TriggerState, color bool) string {
	if !color {
		return string(s)
	}
	switch s {
	case model.TriggerOK:
		return styleSucceeded.Render(string(s))
	case model.TriggerDrift:
		return styleInFlight.Render(string(s))
	case model.TriggerMissing, model.TriggerFetchError:
		return styleFailed.Render(string(s))
	default:
		return styleDim.Render(string(s))
	}
}

// triggerDetail is the one line that says what to do about a row: the file
// paths being watched when all is well, and what differs when it is not.
//
// The ok row shows the includes only. The excludes are the same two or three
// patterns on every row of the fleet, so spending the column on them would
// push the account-specific part off the end of the line; they are in the
// JSON output, and in the drift row whenever they are what differs.
func triggerDetail(t model.TriggerSummary) string {
	switch t.State {
	case model.TriggerOK:
		return strings.Join(t.Expected.FilePaths, ", ")
	case model.TriggerMissing:
		return "no trigger configured"
	case model.TriggerUnknown:
		return "no account_customizations_name for this account"
	case model.TriggerFetchError:
		return truncate(t.FetchError, 60)
	}
	// Drift: name every reason, but spell out only the first difference. The
	// full before/after of each one is in the JSON output; a table row that
	// wrapped over three lines would stop the table being scannable, which is
	// the only thing it is better at than the JSON.
	//
	// The budget covers the reason list plus a diff rather than the reason
	// list alone: a pipeline on the pre-migration pattern names two reasons
	// (file_paths and file_path_excludes), and a shorter cut would spend the
	// line on the names and drop the "want", which is the part that says what
	// to do about the row.
	parts := make([]string, 0, 2)
	parts = append(parts, strings.Join(t.Reasons, ","))
	if d := triggerDiff(t); d != "" {
		parts = append(parts, d)
	}
	return truncate(strings.Join(parts, ": "), 100)
}

// triggerDiff renders the first comparable difference as "got X, want Y".
func triggerDiff(t model.TriggerSummary) string {
	if t.Expected == nil || len(t.Actual) == 0 {
		return ""
	}
	got, want := t.Actual[0], *t.Expected
	for _, reason := range t.Reasons {
		switch reason {
		case model.ReasonFilePaths:
			return diffLine(got.FilePaths, want.FilePaths)
		case model.ReasonFilePathExcludes:
			return diffLine(got.FilePathExcludes, want.FilePathExcludes)
		case model.ReasonBranches:
			return diffLine(got.Branches, want.Branches)
		case model.ReasonSourceAction:
			return diffLine([]string{got.SourceAction}, []string{want.SourceAction})
		case model.ReasonProviderType:
			return diffLine([]string{got.ProviderType}, []string{want.ProviderType})
		}
	}
	return ""
}

func diffLine(got, want []string) string {
	return fmt.Sprintf("got %s, want %s", joinOrDash(got), joinOrDash(want))
}

func joinOrDash(v []string) string {
	s := strings.Join(v, ",")
	if s == "" {
		return "-"
	}
	return s
}

// TriggerCounts prints the per-state tally (stderr companion of the table).
func TriggerCounts(w io.Writer, items []model.TriggerSummary) {
	counts := model.TriggerCounts(items)
	var parts []string
	for _, s := range model.TriggerStates {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", s, counts[s]))
		}
	}
	line := fmt.Sprintf("total=%d", len(items))
	if len(parts) > 0 {
		line += " " + strings.Join(parts, " ")
	}
	fmt.Fprintln(w, line)
}

// TriggerFixTable renders `pipeline triggers fix` results: what happened to
// each pipeline, and the one thing to know about it — the version the write
// produced, why it was skipped or refused, or the error.
func TriggerFixTable(w io.Writer, items []model.TriggerFixResult, color bool) {
	var tw tableWriter
	tw.row("ACCOUNT NAME", "ACCOUNT ID", "RESULT", "DETAIL")
	for _, r := range items {
		result := string(r.Outcome)
		var detail string
		switch r.Outcome {
		case model.FixUpdated:
			detail = fmt.Sprintf("v%d → v%d", r.VersionBefore, r.VersionAfter)
			if color {
				result = styleSucceeded.Render(result)
			}
		case model.FixSkipped:
			detail = strings.Join(r.Reasons, ",")
			if color {
				result = styleDim.Render(result)
			}
		case model.FixRefused:
			detail = strings.Join(r.Reasons, ",")
			if color {
				result = styleInFlight.Render(result)
			}
		default:
			detail = truncate(r.Error, 80)
			if color {
				result = styleFailed.Render(result)
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

// TriggerFixCounts prints the per-outcome tally of a fix run.
func TriggerFixCounts(w io.Writer, items []model.TriggerFixResult) {
	counts := map[model.TriggerFixOutcome]int{}
	for _, r := range items {
		counts[r.Outcome]++
	}
	parts := []string{fmt.Sprintf("total=%d", len(items))}
	for _, o := range []model.TriggerFixOutcome{
		model.FixUpdated, model.FixSkipped, model.FixRefused, model.FixFailed,
	} {
		if counts[o] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", o, counts[o]))
		}
	}
	fmt.Fprintln(w, strings.Join(parts, " "))
}

// ReleaseTable renders release results.
func ReleaseTable(w io.Writer, items []model.StartExecutionResult, color bool) {
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
