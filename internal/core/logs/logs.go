// Package logs retrieves an AFT customizations build's CloudWatch Logs via
// its CodeBuild id and extracts the terraform portion. It is the read side
// of F2's log view (docs/design.md §4.2): the CodeBuild id comes from a
// failed action in pipeline.Detail, and the output feeds either a human
// (terraform section / raw) or an AI (--summary JSON boundary).
package logs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/smithy-go"

	"github.com/hacker65536/aft-ops/internal/core/model"
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
// same log within a TUI session performs no requests. Log bodies are never
// persisted to disk; only the terraform conclusion drawn from them is
// (core/result, docs/design.md §4.6).
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
	if bl := s.memoized(buildID); bl != nil {
		return bl, nil
	}
	builds, err := s.Locate(ctx, []string{buildID})
	if err != nil {
		return nil, err
	}
	b, ok := builds[buildID]
	if !ok {
		return nil, fmt.Errorf("build %q not found", buildID)
	}
	return s.FetchBuild(ctx, b)
}

// FetchBuild is Fetch for a build that has already been located: it returns
// every log line, memoizing a completed build's log.
func (s *Service) FetchBuild(ctx context.Context, b Build) (*BuildLog, error) {
	if bl := s.memoized(b.ID); bl != nil {
		return bl, nil
	}
	if b.Group == "" || b.Stream == "" {
		return nil, fmt.Errorf("build %q has no CloudWatch Logs location", b.ID)
	}
	lines, err := s.fetchStream(ctx, b.Group, b.Stream)
	if err != nil {
		return nil, err
	}
	bl := &BuildLog{BuildID: b.ID, Group: b.Group, Stream: b.Stream, Lines: lines}
	if b.Complete {
		s.mu.Lock()
		if s.memo == nil {
			s.memo = map[string]*BuildLog{}
		}
		s.memo[b.ID] = bl
		s.mu.Unlock()
	}
	return bl, nil
}

func (s *Service) memoized(buildID string) *BuildLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.memo[buildID]
}

// Build is a located CodeBuild build: where its log lives and whether it
// has finished (only a finished build's log is immutable).
type Build struct {
	ID       string
	Group    string
	Stream   string
	Complete bool
}

// DefaultLocation is where CodeBuild puts a build's log when its project
// does not configure one: group "/aws/codebuild/<project>", stream "<uuid>"
// (the two halves of the build id). AFT's customizations projects use it, so
// the log can be read without asking BatchGetBuilds where it is — the call
// CodeBuild throttles hardest. ok is false for an id without that shape.
// complete is the caller's to say (an action that has finished has a
// finished build).
func DefaultLocation(buildID string, complete bool) (Build, bool) {
	project, uuid, ok := strings.Cut(buildID, ":")
	if !ok || project == "" || uuid == "" {
		return Build{}, false
	}
	return Build{ID: buildID, Group: "/aws/codebuild/" + project, Stream: uuid, Complete: complete}, true
}

// IsNotFound reports whether err says a log group or stream does not exist
// (a guessed DefaultLocation that the project does not use).
func IsNotFound(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "ResourceNotFoundException"
}

// batchGetBuildsMax is the API's limit on ids per BatchGetBuilds call.
const batchGetBuildsMax = 100

// Locate resolves build ids to their log locations, batching the ids into
// as few BatchGetBuilds calls as the API allows. Ids the API does not know
// are absent from the result (BatchGetBuilds omits them silently).
func (s *Service) Locate(ctx context.Context, ids []string) (map[string]Build, error) {
	out := make(map[string]Build, len(ids))
	for len(ids) > 0 {
		n := min(len(ids), batchGetBuildsMax)
		resp, err := s.CodeBuild.BatchGetBuilds(ctx, &codebuild.BatchGetBuildsInput{Ids: ids[:n]})
		if err != nil {
			return nil, fmt.Errorf("BatchGetBuilds(%s): %w", strings.Join(ids[:n], ","), err)
		}
		for _, b := range resp.Builds {
			id := aws.ToString(b.Id)
			lb := Build{ID: id, Complete: b.BuildComplete}
			if loc := b.Logs; loc != nil {
				lb.Group, lb.Stream = aws.ToString(loc.GroupName), aws.ToString(loc.StreamName)
			}
			out[id] = lb
		}
		ids = ids[n:]
	}
	return out, nil
}

// tailLines is how many events Tail asks for. The verdict is followed only
// by the build's POST_BUILD output, so a few hundred lines reach it with room
// to spare — against the API's default page of up to 10,000 events / 1 MB,
// which a fan-out over every pipeline would otherwise download in full.
const tailLines = 300

// Tail returns the last tailLines events of a build's log (one GetLogEvents
// call read from the end). A terraform verdict sits at the end of the run, so
// this is usually all a summary needs; the caller reads the whole stream only
// when the tail has no verdict, and the log screen reads it when opened
// (docs/design.md §4.6). A completed build's full log, when already
// memoized, is used instead.
func (s *Service) Tail(ctx context.Context, b Build) ([]string, error) {
	if bl := s.memoized(b.ID); bl != nil {
		return bl.Lines, nil
	}
	if b.Group == "" || b.Stream == "" {
		return nil, fmt.Errorf("build %q has no CloudWatch Logs location", b.ID)
	}
	out, err := s.Logs.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(b.Group),
		LogStreamName: aws.String(b.Stream),
		StartFromHead: aws.Bool(false),
		Limit:         aws.Int32(tailLines),
	})
	if err != nil {
		return nil, fmt.Errorf("GetLogEvents(%s/%s): %w", b.Group, b.Stream, err)
	}
	lines := make([]string, 0, len(out.Events))
	for _, e := range out.Events {
		lines = append(lines, strings.TrimRight(aws.ToString(e.Message), "\r\n"))
	}
	return lines, nil
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

// ParseVerdict concludes a terraform run from its log: the first error
// header when the run failed, else the final apply/destroy verdict, else the
// plan verdict. The kind is ResultUnknown when the log carries none (e.g. a
// build that died before terraform ran) — the caller knows whether the
// action failed and refines that (see result.Service). It is the one place
// the verdict rule lives: the TUI list, the actions screen and the CLI all
// read their summaries from it (docs/design.md §4.6).
func ParseVerdict(lines []string) model.TerraformResult {
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
		return model.TerraformResult{Kind: model.ResultError, Line: errLine}
	case strings.HasPrefix(applyLine, "No changes."):
		return model.TerraformResult{Kind: model.ResultNoChanges, Line: applyLine}
	case applyLine != "":
		r := model.TerraformResult{Kind: model.ResultApplied, Line: applyLine}
		r.Add, r.Change, r.Destroy = countsOf(applyLine)
		if r.Add == 0 && r.Change == 0 && r.Destroy == 0 {
			r.Kind = model.ResultNoChanges
		}
		return r
	case planLine != "":
		r := model.TerraformResult{Kind: model.ResultPlan, Line: planLine}
		r.Add, r.Change, r.Destroy = countsOf(planLine)
		return r
	}
	return model.TerraformResult{Kind: model.ResultUnknown}
}

// countRe captures the counts of both apply/destroy verdicts ("1 added,
// 0 changed, 2 destroyed") and plan verdicts ("1 to add, 0 to change,
// 2 to destroy").
var countRe = regexp.MustCompile(`(\d+) (added|changed|destroyed|to add|to change|to destroy)`)

// countsOf reads the add/change/destroy counts out of a verdict line. A count
// the line does not mention is 0 ("Destroy complete! Resources: 3 destroyed.").
func countsOf(line string) (add, change, destroy int) {
	for _, m := range countRe.FindAllStringSubmatch(line, -1) {
		n, _ := strconv.Atoi(m[1])
		switch {
		case strings.HasSuffix(m[2], "add") || m[2] == "added":
			add = n
		case strings.HasSuffix(m[2], "change") || m[2] == "changed":
			change = n
		default:
			destroy = n
		}
	}
	return add, change, destroy
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
