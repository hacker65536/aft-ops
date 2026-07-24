// Package logs retrieves an AFT customizations build's CloudWatch Logs via
// its CodeBuild id and extracts the terraform portion. It is the read side
// of F2's log view (docs/design.md §4.2): the CodeBuild id comes from a
// failed action in pipeline.Detail, and the output feeds either a human
// (terraform section / raw) or an AI (--summary JSON boundary).
package logs

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
)

// CodeBuildAPI is the subset of the CodeBuild client used here.
type CodeBuildAPI interface {
	BatchGetBuilds(ctx context.Context, in *codebuild.BatchGetBuildsInput,
		opts ...func(*codebuild.Options)) (*codebuild.BatchGetBuildsOutput, error)
}

// LogsAPI is the subset of the CloudWatch Logs client used here.
type LogsAPI interface {
	GetLogEvents(ctx context.Context, in *cloudwatchlogs.GetLogEventsInput,
		opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// Service resolves a CodeBuild id to its log stream and fetches it.
// Completed builds' logs are memoized in memory for the Service's lifetime
// (a build's log is immutable once the build finishes), so revisiting the
// same log within a TUI session performs no requests. Nothing is persisted
// to disk — the on-disk cache stays limited to accounts and the pipeline
// inventory (docs/design.md §4.1).
type Service struct {
	CodeBuild CodeBuildAPI
	Logs      LogsAPI

	mu   sync.Mutex
	memo map[string]*BuildLog // by build id; completed builds only
}

// BuildLog is a fetched build log with its CloudWatch location.
type BuildLog struct {
	BuildID string   `json:"build_id"`
	Group   string   `json:"log_group"`
	Stream  string   `json:"log_stream"`
	Lines   []string `json:"lines"`
}

// Fetch resolves buildID (a CodeBuild build id, "<project>:<uuid>") to its
// CloudWatch Logs stream and returns every log line in order. A completed
// build is served from the in-memory memo on repeat calls; an in-flight
// build is always refetched (its log is still growing).
func (s *Service) Fetch(ctx context.Context, buildID string) (*BuildLog, error) {
	s.mu.Lock()
	if bl, ok := s.memo[buildID]; ok {
		s.mu.Unlock()
		return bl, nil
	}
	s.mu.Unlock()

	out, err := s.CodeBuild.BatchGetBuilds(ctx, &codebuild.BatchGetBuildsInput{
		Ids: []string{buildID},
	})
	if err != nil {
		return nil, fmt.Errorf("BatchGetBuilds(%s): %w", buildID, err)
	}
	if len(out.Builds) == 0 {
		return nil, fmt.Errorf("build %q not found", buildID)
	}
	build := out.Builds[0]
	loc := build.Logs
	if loc == nil || loc.GroupName == nil || loc.StreamName == nil {
		return nil, fmt.Errorf("build %q has no CloudWatch Logs location", buildID)
	}
	group, stream := aws.ToString(loc.GroupName), aws.ToString(loc.StreamName)
	lines, err := s.fetchStream(ctx, group, stream)
	if err != nil {
		return nil, err
	}
	bl := &BuildLog{BuildID: buildID, Group: group, Stream: stream, Lines: lines}
	if build.BuildComplete {
		s.mu.Lock()
		if s.memo == nil {
			s.memo = map[string]*BuildLog{}
		}
		s.memo[buildID] = bl
		s.mu.Unlock()
	}
	return bl, nil
}

// fetchStream pages GetLogEvents from the head until the forward token stops
// advancing (CloudWatch returns the same token when the stream is drained).
func (s *Service) fetchStream(ctx context.Context, group, stream string) ([]string, error) {
	var lines []string
	var token *string
	for {
		out, err := s.Logs.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
			LogGroupName:  aws.String(group),
			LogStreamName: aws.String(stream),
			StartFromHead: aws.Bool(true),
			NextToken:     token,
		})
		if err != nil {
			return nil, fmt.Errorf("GetLogEvents(%s/%s): %w", group, stream, err)
		}
		for _, e := range out.Events {
			lines = append(lines, strings.TrimRight(aws.ToString(e.Message), "\r\n"))
		}
		// Termination: an empty page, or a forward token that no longer
		// advances, means the stream is fully consumed.
		if len(out.Events) == 0 || out.NextForwardToken == nil ||
			(token != nil && *out.NextForwardToken == *token) {
			return lines, nil
		}
		token = out.NextForwardToken
	}
}

// Mode selects how a build log is rendered.
type Mode int

const (
	// ModeTerraform (default) returns the terraform portion of the log.
	ModeTerraform Mode = iota
	// ModeRaw returns every line unchanged.
	ModeRaw
	// ModeSummary returns only plan-result and error lines.
	ModeSummary
)

// Render applies the selected mode to a build log's lines.
func Render(lines []string, mode Mode) []string {
	switch mode {
	case ModeRaw:
		return lines
	case ModeSummary:
		return Summarize(lines)
	default:
		return ExtractTerraform(lines)
	}
}

// tfStartRe marks where terraform output begins inside the CodeBuild log:
// the first `terraform init` banner (modules are initialized before the
// backend when present), with later-stage banners as fallbacks. The first
// match wins.
//
// The version banner ("Terraform v...") is deliberately NOT a marker: the
// AFT buildspec runs `terraform --version` and then dumps every *.tf file
// (`for f in *.tf; do cat $f; done`) BEFORE `terraform init`, so matching
// the version line would pull that whole dump in as noise.
var tfStartRe = regexp.MustCompile(
	`Initializing modules|Initializing the backend|Initializing provider plugins|` +
		`Terraform has been successfully initialized|` +
		`Terraform will perform the following actions|` +
		`Terraform used the selected providers`)

// containerLineRe matches the CodeBuild agent's own log lines. Terraform
// never emits these mid-run, so the first one after the terraform start
// marker signals that the build phase (and the terraform output) is over —
// everything from there on is POST_BUILD noise.
var containerLineRe = regexp.MustCompile(`^\[Container\] `)

// ExtractTerraform returns the log from the first terraform marker up to
// (not including) the next CodeBuild agent line, dropping both the setup
// preamble and the POST_BUILD tail. When no marker is present it returns
// every line, so the caller never sees an empty result by accident (fall
// back to raw).
func ExtractTerraform(lines []string) []string {
	start := -1
	for i, ln := range lines {
		if tfStartRe.MatchString(strings.TrimSpace(ln)) {
			start = i
			break
		}
	}
	if start < 0 {
		return lines
	}
	out := lines[start:]
	for i, ln := range out {
		if containerLineRe.MatchString(ln) {
			out = out[:i]
			break
		}
	}
	// Drop trailing blank lines left by the cut.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

// summaryHeadRe matches the terraform lines an operator or AI cares about
// most: the plan/apply verdict and error/warning headers.
var summaryHeadRe = regexp.MustCompile(
	`^(Plan:|Apply complete!|Destroy complete!|No changes\.|Error:|Warning:)`)

// Verdict returns the single line that best concludes a terraform run: the
// first error header when the run failed, else the final apply/destroy
// verdict, else the plan verdict. Empty when the log carries none (e.g. a
// build that died before terraform ran). It feeds one-line summaries such
// as the TUI actions screen.
func Verdict(lines []string) string {
	var errLine, applyLine, planLine string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		// Terraform boxes diagnostics ("│ Error: ..."); unwrap the border so
		// the prefix checks below see the header itself.
		t = strings.TrimSpace(strings.TrimPrefix(t, "│"))
		switch {
		case strings.HasPrefix(t, "Error:"):
			if errLine == "" {
				errLine = t
			}
		case strings.HasPrefix(t, "Apply complete!"),
			strings.HasPrefix(t, "Destroy complete!"),
			strings.HasPrefix(t, "No changes."):
			applyLine = t
		case strings.HasPrefix(t, "Plan:"):
			planLine = t
		}
	}
	switch {
	case errLine != "":
		return errLine
	case applyLine != "":
		return applyLine
	}
	return planLine
}

// Summarize returns the plan-result verdict and the terraform error blocks
// (the boxed `╷ │ ╵` diagnostics), dropping the routine output. This is the
// primary machine-readable boundary for AI-assisted triage.
func Summarize(lines []string) []string {
	var out []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		switch {
		case summaryHeadRe.MatchString(t):
			out = append(out, t)
		case strings.HasPrefix(t, "│"), strings.HasPrefix(t, "╷"), strings.HasPrefix(t, "╵"):
			out = append(out, t)
		}
	}
	return out
}
