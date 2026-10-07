package logs

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"

	"github.com/hacker65536/aft-ops/internal/core/model"
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

// ParseVerdict concludes the run from the one line that decides it: the
// (possibly boxed) error header first, else the apply verdict, else the plan
// verdict — and reads the counts out of it.
func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  model.TerraformResult
	}{
		{
			name: "applied",
			lines: []string{
				"Plan: 1 to add, 0 to change, 0 to destroy.",
				"Apply complete! Resources: 1 added, 0 changed, 2 destroyed.",
			},
			want: model.TerraformResult{Kind: model.ResultApplied, Add: 1, Destroy: 2,
				Line: "Apply complete! Resources: 1 added, 0 changed, 2 destroyed."},
		},
		{
			// sampleLog fails with a boxed error; that beats the plan line.
			name:  "error beats plan",
			lines: sampleLog,
			want: model.TerraformResult{Kind: model.ResultError,
				Line: "Error: creating S3 Bucket: BucketAlreadyExists"},
		},
		{
			name:  "no changes",
			lines: []string{"No changes. Your infrastructure matches the configuration."},
			want: model.TerraformResult{Kind: model.ResultNoChanges,
				Line: "No changes. Your infrastructure matches the configuration."},
		},
		{
			// An all-zero apply says nothing happened, exactly like "No
			// changes." — it must not show up as numbers in the list.
			name:  "zero apply folds into no changes",
			lines: []string{"Apply complete! Resources: 0 added, 0 changed, 0 destroyed."},
			want: model.TerraformResult{Kind: model.ResultNoChanges,
				Line: "Apply complete! Resources: 0 added, 0 changed, 0 destroyed."},
		},
		{
			name:  "destroy names only what it destroyed",
			lines: []string{"Destroy complete! Resources: 3 destroyed."},
			want: model.TerraformResult{Kind: model.ResultApplied, Destroy: 3,
				Line: "Destroy complete! Resources: 3 destroyed."},
		},
		{
			name:  "plan only",
			lines: []string{"Plan: 2 to add, 1 to change, 0 to destroy."},
			want: model.TerraformResult{Kind: model.ResultPlan, Add: 2, Change: 1,
				Line: "Plan: 2 to add, 1 to change, 0 to destroy."},
		},
		{
			name:  "no terraform",
			lines: []string{"no terraform here"},
			want:  model.TerraformResult{Kind: model.ResultUnknown},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseVerdict(tc.lines); got != tc.want {
				t.Errorf("ParseVerdict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// pagedBuilds answers BatchGetBuilds for every id it is asked about and
// records the size of each request.
type pagedBuilds struct{ requests []int }

func (f *pagedBuilds) BatchGetBuilds(_ context.Context, in *codebuild.BatchGetBuildsInput,
	_ ...func(*codebuild.Options)) (*codebuild.BatchGetBuildsOutput, error) {
	f.requests = append(f.requests, len(in.Ids))
	out := &codebuild.BatchGetBuildsOutput{}
	for _, id := range in.Ids {
		out.Builds = append(out.Builds, cbtypes.Build{
			Id: aws.String(id), BuildComplete: true,
			Logs: &cbtypes.LogsLocation{GroupName: aws.String("g"), StreamName: aws.String(id)},
		})
	}
	return out, nil
}

// Locate batches ids up to the API's limit of 100 per call.
func TestLocateBatchesIDs(t *testing.T) {
	cb := &pagedBuilds{}
	s := &Service{CodeBuild: cb}
	ids := make([]string, 150)
	for i := range ids {
		ids[i] = fmt.Sprintf("proj:%d", i)
	}
	got, err := s.Locate(context.Background(), ids)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if len(got) != 150 {
		t.Errorf("located %d builds, want 150", len(got))
	}
	if len(cb.requests) != 2 || cb.requests[0] != 100 || cb.requests[1] != 50 {
		t.Errorf("BatchGetBuilds request sizes = %v, want [100 50]", cb.requests)
	}
}

// tailEvents records whether it was read from the end, and with what limit.
type tailEvents struct {
	fromHead []bool
	limits   []int32
}

func (f *tailEvents) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput,
	_ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.fromHead = append(f.fromHead, aws.ToBool(in.StartFromHead))
	f.limits = append(f.limits, aws.ToInt32(in.Limit))
	return &cloudwatchlogs.GetLogEventsOutput{
		Events: []cwltypes.OutputLogEvent{{Message: aws.String("Apply complete! Resources: 1 added, 0 changed, 0 destroyed.\n")}},
	}, nil
}

// Tail reads one page from the end of the stream — and none at all when the
// build's full log is already memoized.
func TestTailReadsOnePageFromTheEnd(t *testing.T) {
	cwl := &tailEvents{}
	s := &Service{CodeBuild: &fakeBuilds{complete: true}, Logs: cwl}
	b := Build{ID: "proj:uuid", Group: "g", Stream: "s", Complete: true}

	lines, err := s.Tail(context.Background(), b)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(cwl.fromHead) != 1 || cwl.fromHead[0] {
		t.Errorf("GetLogEvents calls (fromHead) = %v, want one read from the end", cwl.fromHead)
	}
	// Only the last lines are asked for, not the API's default page of up to
	// 1 MB — the fan-out reads two logs per pipeline.
	if cwl.limits[0] != tailLines {
		t.Errorf("tail limit = %d, want %d", cwl.limits[0], tailLines)
	}
	if got := ParseVerdict(lines).Kind; got != model.ResultApplied {
		t.Errorf("verdict of the tail = %s, want applied", got)
	}

	if _, err := s.FetchBuild(context.Background(), b); err != nil {
		t.Fatalf("FetchBuild: %v", err)
	}
	calls := len(cwl.fromHead)
	if _, err := s.Tail(context.Background(), b); err != nil {
		t.Fatalf("Tail (memo): %v", err)
	}
	if len(cwl.fromHead) != calls {
		t.Error("Tail of a memoized build should not call the API")
	}
}

// DefaultLocation splits a build id into CodeBuild's default log group and
// stream; an id without that shape has none.
func TestDefaultLocation(t *testing.T) {
	b, ok := DefaultLocation("aft-account-customizations-terraform:0149-uuid", true)
	if !ok || b.Group != "/aws/codebuild/aft-account-customizations-terraform" ||
		b.Stream != "0149-uuid" || !b.Complete {
		t.Errorf("DefaultLocation = %+v, %v", b, ok)
	}
	for _, id := range []string{"no-colon", ":uuid", "project:"} {
		if _, ok := DefaultLocation(id, true); ok {
			t.Errorf("DefaultLocation(%q) should have no location", id)
		}
	}
}
