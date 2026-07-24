package logs

import (
	"strings"
	"testing"
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
	// The CodeBuild preamble line must be dropped.
	for _, ln := range got {
		if strings.Contains(ln, "Running command terraform apply") {
			t.Error("CodeBuild preamble leaked into terraform extraction")
		}
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
