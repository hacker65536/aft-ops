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
type Service struct {
	CodeBuild CodeBuildAPI
	Logs      LogsAPI
}

// BuildLog is a fetched build log with its CloudWatch location.
type BuildLog struct {
	BuildID string   `json:"build_id"`
	Group   string   `json:"log_group"`
	Stream  string   `json:"log_stream"`
	Lines   []string `json:"lines"`
}

// Fetch resolves buildID (a CodeBuild build id, "<project>:<uuid>") to its
// CloudWatch Logs stream and returns every log line in order.
func (s *Service) Fetch(ctx context.Context, buildID string) (*BuildLog, error) {
	out, err := s.CodeBuild.BatchGetBuilds(ctx, &codebuild.BatchGetBuildsInput{
		Ids: []string{buildID},
	})
	if err != nil {
		return nil, fmt.Errorf("BatchGetBuilds(%s): %w", buildID, err)
	}
	if len(out.Builds) == 0 {
		return nil, fmt.Errorf("build %q not found", buildID)
	}
	loc := out.Builds[0].Logs
	if loc == nil || loc.GroupName == nil || loc.StreamName == nil {
		return nil, fmt.Errorf("build %q has no CloudWatch Logs location", buildID)
	}
	group, stream := aws.ToString(loc.GroupName), aws.ToString(loc.StreamName)
	lines, err := s.fetchStream(ctx, group, stream)
	if err != nil {
		return nil, err
	}
	return &BuildLog{BuildID: buildID, Group: group, Stream: stream, Lines: lines}, nil
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

// tfStartRe marks where terraform output begins inside the CodeBuild log
// (init/plan/apply banners). The first match wins.
var tfStartRe = regexp.MustCompile(
	`Initializing the backend|Initializing provider plugins|` +
		`Terraform has been successfully initialized|` +
		`Terraform will perform the following actions|` +
		`Terraform used the selected providers|^Terraform v`)

// ExtractTerraform returns the log from the first terraform marker to the
// end. When no marker is present it returns every line, so the caller never
// sees an empty result by accident (fall back to raw).
func ExtractTerraform(lines []string) []string {
	for i, ln := range lines {
		if tfStartRe.MatchString(strings.TrimSpace(ln)) {
			return lines[i:]
		}
	}
	return lines
}

// summaryHeadRe matches the terraform lines an operator or AI cares about
// most: the plan/apply verdict and error/warning headers.
var summaryHeadRe = regexp.MustCompile(
	`^(Plan:|Apply complete!|Destroy complete!|No changes\.|Error:|Warning:)`)

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
