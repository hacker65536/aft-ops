package logs

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
)

// a representative slice of a CodeBuild log wrapping a terraform apply.
var sampleLog = strings.Split(strings.TrimSpace(`
[Container] 2026/07/25 00:00:01 Running command terraform apply
Initializing the backend...
Initializing provider plugins...
Terraform used the selected providers to generate the following execution plan.
Terraform will perform the following actions:
  # aws_s3_bucket.example will be created
Plan: 1 to add, 0 to change, 0 to destroy.
╷
│ Error: creating S3 Bucket: BucketAlreadyExists
│
│   with aws_s3_bucket.example,
│   on main.tf line 3, in resource "aws_s3_bucket" "example":
│    3: resource "aws_s3_bucket" "example" {
╵
[Container] 2026/07/25 00:00:42 Phase complete: BUILD State: FAILED
`), "\n")

func TestExtractTerraformStartsAtMarker(t *testing.T) {
	got := ExtractTerraform(sampleLog)
	if len(got) == 0 {
		t.Fatal("got no lines")
	}
	if !strings.HasPrefix(got[0], "Initializing the backend") {
		t.Errorf("first line = %q, want the terraform init banner", got[0])
	}
	// Neither the CodeBuild preamble nor the trailing agent line survives.
	for _, ln := range got {
		if strings.Contains(ln, "Running command terraform apply") {
			t.Error("CodeBuild preamble leaked into terraform extraction")
		}
		if strings.Contains(ln, "Phase complete") {
			t.Error("CodeBuild POST_BUILD tail leaked into terraform extraction")
		}
	}
}

// The AFT buildspec prints the terraform version and dumps every *.tf file
// BEFORE terraform init runs; neither may leak into the extraction. The
// true start is init's first banner — "Initializing modules..." when the
// configuration has modules (it precedes the backend banner).
func TestExtractTerraformSkipsVersionAndTfDump(t *testing.T) {
	in := []string{
		"[Container] 2026/07/25 00:00:01 Running command /opt/aft/bin/terraform -no-color --version",
		"Terraform v1.5.7",
		"on linux_amd64",
		"[Container] 2026/07/25 00:00:02 Running command for f in *.tf; do echo; echo $f; cat $f; done",
		"backend.tf",
		`resource "aws_s3_bucket" "example" {`,
		"}",
		"[Container] 2026/07/25 00:00:03 Running command /opt/aft/bin/terraform init -no-color",
		"Initializing modules...",
		"- example in modules/example",
		"Initializing the backend...",
		"Initializing provider plugins...",
	}
	got := ExtractTerraform(in)
	if len(got) == 0 {
		t.Fatal("got no lines")
	}
	if got[0] != "Initializing modules..." {
		t.Errorf("first line = %q, want Initializing modules...", got[0])
	}
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "Terraform v1.5.7") {
		t.Error("the version banner leaked into terraform extraction")
	}
	if strings.Contains(joined, "aws_s3_bucket") {
		t.Error("the *.tf dump leaked into terraform extraction")
	}
}

// The POST_BUILD tail — [Container] lines and the helper output after them —
// must be cut even when non-container lines are interleaved.
func TestExtractTerraformCutsPostBuildTail(t *testing.T) {
	in := []string{
		"[Container] 2026/07/25 00:00:01 Running command terraform apply",
		"Initializing the backend...",
		"Apply complete! Resources: 0 added, 0 changed, 0 destroyed.",
		"",
		"Outputs:",
		"",
		`account_id = "123456789012"`,
		"",
		"[Container] 2026/07/25 00:01:00 Phase complete: BUILD State: SUCCEEDED",
		"[Container] 2026/07/25 00:01:01 Running command . post-api-helpers.sh",
		"Executing Post-API Helpers",
	}
	got := ExtractTerraform(in)
	if len(got) == 0 {
		t.Fatal("got no lines")
	}
	if got[len(got)-1] != `account_id = "123456789012"` {
		t.Errorf("last line = %q, want the outputs value (tail cut + blanks trimmed)", got[len(got)-1])
	}
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "Phase complete") || strings.Contains(joined, "Post-API Helpers") {
		t.Error("POST_BUILD tail leaked into terraform extraction")
	}
}

func TestExtractTerraformFallsBackToAllLines(t *testing.T) {
	in := []string{"[Container] no terraform here", "just build noise"}
	got := ExtractTerraform(in)
	if len(got) != len(in) {
		t.Errorf("without a marker, want all %d lines, got %d", len(in), len(got))
	}
}

func TestSummarizeKeepsVerdictAndErrorBlock(t *testing.T) {
	got := Summarize(sampleLog)
	joined := strings.Join(got, "\n")

	if !strings.Contains(joined, "Plan: 1 to add") {
		t.Error("summary dropped the plan verdict")
	}
	if !strings.Contains(joined, "Error: creating S3 Bucket") {
		t.Error("summary dropped the error header")
	}
	// Routine plan detail must not survive summarization.
	if strings.Contains(joined, "will be created") {
		t.Error("summary kept routine plan detail")
	}
	// The boxed error context lines should be retained.
	if !strings.Contains(joined, "on main.tf line 3") {
		t.Error("summary dropped the error context box")
	}
}

func TestRenderMode(t *testing.T) {
	if got := Render(sampleLog, ModeRaw); len(got) != len(sampleLog) {
		t.Errorf("ModeRaw changed line count: %d != %d", len(got), len(sampleLog))
	}
	if got := Render(sampleLog, ModeSummary); len(got) == 0 || len(got) >= len(sampleLog) {
		t.Errorf("ModeSummary should be a non-empty subset, got %d lines", len(got))
	}
}

// fakeBuilds serves BatchGetBuilds for one build id and counts calls.
type fakeBuilds struct {
	calls    int
	complete bool
}

func (f *fakeBuilds) BatchGetBuilds(_ context.Context, in *codebuild.BatchGetBuildsInput,
	_ ...func(*codebuild.Options)) (*codebuild.BatchGetBuildsOutput, error) {
	f.calls++
	return &codebuild.BatchGetBuildsOutput{
		Builds: []cbtypes.Build{{
			Id:            aws.String(in.Ids[0]),
			BuildComplete: f.complete,
			Logs: &cbtypes.LogsLocation{
				GroupName:  aws.String("/aws/codebuild/aft"),
				StreamName: aws.String("stream-1"),
			},
		}},
	}, nil
}

// fakeEvents serves a single GetLogEvents page and counts calls.
type fakeEvents struct {
	calls int
}

func (f *fakeEvents) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput,
	_ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.calls++
	return &cloudwatchlogs.GetLogEventsOutput{
		Events:           []cwltypes.OutputLogEvent{{Message: aws.String("line-1")}},
		NextForwardToken: in.NextToken, // unchanged token = stream drained
	}, nil
}

// A completed build's log is memoized: the second Fetch performs no requests.
func TestFetchMemoizesCompletedBuild(t *testing.T) {
	cb := &fakeBuilds{complete: true}
	cwl := &fakeEvents{}
	s := &Service{CodeBuild: cb, Logs: cwl}

	first, err := s.Fetch(context.Background(), "proj:uuid")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	second, err := s.Fetch(context.Background(), "proj:uuid")
	if err != nil {
		t.Fatalf("Fetch (memo): %v", err)
	}
	if cb.calls != 1 || cwl.calls < 1 {
		t.Errorf("completed build should hit the API once, got BatchGetBuilds=%d", cb.calls)
	}
	if second != first {
		t.Error("second Fetch should return the memoized BuildLog")
	}
}

// An in-flight build is never memoized: every Fetch refetches.
func TestFetchRefetchesInFlightBuild(t *testing.T) {
	cb := &fakeBuilds{complete: false}
	s := &Service{CodeBuild: cb, Logs: &fakeEvents{}}

	for i := 0; i < 2; i++ {
		if _, err := s.Fetch(context.Background(), "proj:uuid"); err != nil {
			t.Fatalf("Fetch #%d: %v", i+1, err)
		}
	}
	if cb.calls != 2 {
		t.Errorf("in-flight build should refetch every time, got BatchGetBuilds=%d", cb.calls)
	}
}

// Verdict picks the one line that concludes the run: the (possibly boxed)
// error header first, else the apply verdict, else the plan verdict.
func TestVerdict(t *testing.T) {
	applied := []string{
		"Plan: 1 to add, 0 to change, 0 to destroy.",
		"Apply complete! Resources: 1 added, 0 changed, 0 destroyed.",
	}
	if got := Verdict(applied); got != "Apply complete! Resources: 1 added, 0 changed, 0 destroyed." {
		t.Errorf("applied verdict = %q", got)
	}

	// sampleLog fails with a boxed error; that beats the plan line.
	if got := Verdict(sampleLog); got != "Error: creating S3 Bucket: BucketAlreadyExists" {
		t.Errorf("failed verdict = %q", got)
	}

	if got := Verdict([]string{"no terraform here"}); got != "" {
		t.Errorf("no-verdict log should yield empty, got %q", got)
	}
}
