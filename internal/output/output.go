// Package output renders core results for humans (tables) and machines
// (JSON). Contract: stdout carries data, stderr carries progress and
// diagnostics; JSON documents carry a schema_version for compatibility.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// SchemaVersion is bumped only on breaking JSON shape changes.
const SchemaVersion = 1

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
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT NAME\tACCOUNT ID\tSTATUS\tLAST UPDATE\tEXECUTION")
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
			status = "fetch-error"
			if color {
				status = styleFailed.Render(status)
			}
		}
		name := p.AccountName
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, p.AccountID, status, last, exec)
	}
	tw.Flush()
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
		parts = append(parts, fmt.Sprintf("fetch-error=%d", fetchErrs))
	}
	fmt.Fprintf(w, "total=%d %s\n", len(items), strings.Join(parts, " "))
}

// StatusFreshness prints how fresh the listed statuses are: how many were
// just refetched versus served from cache, and the age of the oldest cached
// entry. Companion to PipelineCounts on stderr; makes staleness visible now
// that statuses are cached.
func StatusFreshness(w io.Writer, items []model.PipelineSummary, ttl time.Duration) {
	if len(items) == 0 {
		return
	}
	fresh, cached := 0, 0
	var oldest time.Time
	now := time.Now()
	for _, p := range items {
		if p.StatusFetchedAt == nil {
			continue
		}
		age := now.Sub(*p.StatusFetchedAt)
		if age <= 2*time.Second {
			fresh++
		} else {
			cached++
			if oldest.IsZero() || p.StatusFetchedAt.Before(oldest) {
				oldest = *p.StatusFetchedAt
			}
		}
	}
	if cached == 0 {
		fmt.Fprintf(w, "statuses: %d refetched\n", fresh)
		return
	}
	fmt.Fprintf(w, "statuses: %d refetched, %d from cache (oldest %s ago, ttl %s)\n",
		fresh, cached, humanDuration(now.Sub(oldest)), humanDuration(ttl))
}

// ReleaseTable renders release results.
func ReleaseTable(w io.Writer, items []model.ReleaseResult, color bool) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT NAME\tACCOUNT ID\tRESULT\tEXECUTION/REASON")
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, r.AccountID, result, detail)
	}
	tw.Flush()
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

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STAGE\tACTION\tSTATUS\tLAST CHANGE\tDETAIL")
	for _, st := range d.Stages {
		if len(st.Actions) == 0 {
			fmt.Fprintf(tw, "%s\t-\t%s\t-\t-\n", st.Name, styleStatus(st.Status, color))
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
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				stageCol, a.Name, styleStatus(a.Status, color), last, actionDetail(a))
		}
	}
	tw.Flush()

	if len(d.History) > 0 {
		fmt.Fprintln(w, "\nRecent executions:")
		htw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(htw, "EXECUTION\tSTATUS\tSTARTED\tREVISION")
		for _, e := range d.History {
			started := "-"
			if e.StartTime != nil {
				started = humanTime(*e.StartTime)
			}
			fmt.Fprintf(htw, "%s\t%s\t%s\t%s\n",
				shortID(e.ID), styleStatus(e.Status, color), started, revisionSummary(e.Revisions))
		}
		htw.Flush()
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

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// AccountTable renders `account list`.
func AccountTable(w io.Writer, items []model.Account) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT NAME\tACCOUNT ID\tEMAIL")
	for _, a := range items {
		email := a.Email
		if email == "" {
			email = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", a.Name, a.ID, email)
	}
	tw.Flush()
}

// CacheNote prints a staleness banner ("accounts: cached 3h ago ...").
func CacheNote(w io.Writer, what string, fetchedAt time.Time) {
	fmt.Fprintf(w, "%s: cached %s ago (use --refresh to refetch)\n",
		what, humanDuration(time.Since(fetchedAt)))
}

func humanTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04") + " (" + humanDuration(time.Since(t)) + " ago)"
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
