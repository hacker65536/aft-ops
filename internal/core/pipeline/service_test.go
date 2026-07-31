package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	cptypes "github.com/aws/aws-sdk-go-v2/service/codepipeline/types"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// mockAPI is a hand-rolled stub of the read-side CodePipeline API.
type mockAPI struct {
	state      *codepipeline.GetPipelineStateOutput
	executions *codepipeline.ListPipelineExecutionsOutput
	// actionPages is returned page by page across ListActionExecutions calls
	// (a NextToken links each page to the next).
	actionPages []*codepipeline.ListActionExecutionsOutput
	stateErr    error

	execCalls   int // ListPipelineExecutions invocations
	actionCalls int // ListActionExecutions invocations
}

func (m *mockAPI) ListPipelines(context.Context, *codepipeline.ListPipelinesInput,
	...func(*codepipeline.Options)) (*codepipeline.ListPipelinesOutput, error) {
	return &codepipeline.ListPipelinesOutput{}, nil
}

func (m *mockAPI) ListPipelineExecutions(context.Context, *codepipeline.ListPipelineExecutionsInput,
	...func(*codepipeline.Options)) (*codepipeline.ListPipelineExecutionsOutput, error) {
	m.execCalls++
	if m.executions == nil {
		return &codepipeline.ListPipelineExecutionsOutput{}, nil
	}
	return m.executions, nil
}

func (m *mockAPI) GetPipelineState(context.Context, *codepipeline.GetPipelineStateInput,
	...func(*codepipeline.Options)) (*codepipeline.GetPipelineStateOutput, error) {
	if m.stateErr != nil {
		return nil, m.stateErr
	}
	return m.state, nil
}

func (m *mockAPI) GetPipeline(context.Context, *codepipeline.GetPipelineInput,
	...func(*codepipeline.Options)) (*codepipeline.GetPipelineOutput, error) {
	return &codepipeline.GetPipelineOutput{}, nil
}

func (m *mockAPI) ListActionExecutions(_ context.Context, in *codepipeline.ListActionExecutionsInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.ListActionExecutionsOutput, error) {
	m.actionCalls++
	if len(m.actionPages) == 0 {
		return &codepipeline.ListActionExecutionsOutput{}, nil
	}
	page := 0
	if in.NextToken != nil {
		_, _ = fmt.Sscanf(*in.NextToken, "page-%d", &page)
	}
	return m.actionPages[page], nil
}

func TestDetailMapsStagesActionsAndHistory(t *testing.T) {
	const buildID = "aft-customizations:1111-2222-3333"
	api := &mockAPI{
		state: &codepipeline.GetPipelineStateOutput{
			StageStates: []cptypes.StageState{
				{
					StageName:       aws.String("Source"),
					LatestExecution: &cptypes.StageExecution{Status: cptypes.StageExecutionStatusSucceeded},
					ActionStates: []cptypes.ActionState{{
						ActionName:      aws.String("Source"),
						LatestExecution: &cptypes.ActionExecution{Status: cptypes.ActionExecutionStatusSucceeded},
					}},
				},
				{
					StageName:       aws.String("Apply"),
					LatestExecution: &cptypes.StageExecution{Status: cptypes.StageExecutionStatusFailed},
					ActionStates: []cptypes.ActionState{{
						ActionName: aws.String("terraform-apply"),
						LatestExecution: &cptypes.ActionExecution{
							Status:              cptypes.ActionExecutionStatusFailed,
							ExternalExecutionId: aws.String(buildID),
							Summary:             aws.String("build failed"),
							ErrorDetails:        &cptypes.ErrorDetails{Message: aws.String("exit status 1")},
						},
					}},
				},
			},
		},
		executions: &codepipeline.ListPipelineExecutionsOutput{
			PipelineExecutionSummaries: []cptypes.PipelineExecutionSummary{
				{
					PipelineExecutionId: aws.String("exec-1"),
					Status:              cptypes.PipelineExecutionStatusFailed,
					SourceRevisions: []cptypes.SourceRevision{{
						ActionName:      aws.String("Source"),
						RevisionId:      aws.String("abcdef123456"),
						RevisionSummary: aws.String("fix vpc"),
					}},
				},
			},
		},
	}
	svc := &Service{Read: api}

	d, err := svc.Detail(context.Background(), "123456789012-customizations-pipeline", 5, nil)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}

	if d.AccountID != "123456789012" {
		t.Errorf("AccountID = %q, want 123456789012", d.AccountID)
	}
	if len(d.Stages) != 2 {
		t.Fatalf("got %d stages, want 2", len(d.Stages))
	}
	if d.Stages[1].Status != model.StatusFailed {
		t.Errorf("Apply stage status = %q, want Failed", d.Stages[1].Status)
	}

	failed := d.FailedActions()
	if len(failed) != 1 {
		t.Fatalf("got %d failed actions, want 1", len(failed))
	}
	if failed[0].CodeBuildID != buildID {
		t.Errorf("CodeBuildID = %q, want %q", failed[0].CodeBuildID, buildID)
	}
	if failed[0].ErrorMessage != "exit status 1" {
		t.Errorf("ErrorMessage = %q, want %q", failed[0].ErrorMessage, "exit status 1")
	}

	if len(d.History) != 1 {
		t.Fatalf("got %d history entries, want 1", len(d.History))
	}
	if len(d.History[0].Revisions) != 1 || d.History[0].Revisions[0].Summary != "fix vpc" {
		t.Errorf("history revision not mapped: %+v", d.History[0].Revisions)
	}
}

func TestExecutionsMapsSummaries(t *testing.T) {
	api := &mockAPI{
		executions: &codepipeline.ListPipelineExecutionsOutput{
			PipelineExecutionSummaries: []cptypes.PipelineExecutionSummary{
				{PipelineExecutionId: aws.String("exec-2"), Status: cptypes.PipelineExecutionStatusInProgress},
				{PipelineExecutionId: aws.String("exec-1"), Status: cptypes.PipelineExecutionStatusFailed},
			},
		},
	}
	svc := &Service{Read: api}

	execs, err := svc.Executions(context.Background(), "123456789012-customizations-pipeline", 25, false)
	if err != nil {
		t.Fatalf("Executions: %v", err)
	}
	if len(execs) != 2 {
		t.Fatalf("got %d executions, want 2", len(execs))
	}
	if execs[0].ID != "exec-2" || execs[0].Status != model.StatusInProgress {
		t.Errorf("execs[0] = %+v, want exec-2 InProgress", execs[0])
	}
}

func TestActionExecutionsPaginatesAndSortsChronologically(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Minute)
	t2 := t0.Add(5 * time.Minute)
	// API order is newest-first, split across two pages.
	api := &mockAPI{
		actionPages: []*codepipeline.ListActionExecutionsOutput{
			{
				ActionExecutionDetails: []cptypes.ActionExecutionDetail{{
					StageName:      aws.String("Apply"),
					ActionName:     aws.String("terraform-apply"),
					Status:         cptypes.ActionExecutionStatusFailed,
					StartTime:      aws.Time(t1),
					LastUpdateTime: aws.Time(t2),
					Input: &cptypes.ActionExecutionInput{
						ActionTypeId: &cptypes.ActionTypeId{Provider: aws.String("CodeBuild")},
					},
					Output: &cptypes.ActionExecutionOutput{
						ExecutionResult: &cptypes.ActionExecutionResult{
							ExternalExecutionId: aws.String("aft-customizations:1111"),
							ErrorDetails:        &cptypes.ErrorDetails{Message: aws.String("exit status 1")},
						},
					},
				}},
				NextToken: aws.String("page-1"),
			},
			{
				ActionExecutionDetails: []cptypes.ActionExecutionDetail{{
					StageName:      aws.String("Source"),
					ActionName:     aws.String("aft-global-customizations"),
					Status:         cptypes.ActionExecutionStatusSucceeded,
					StartTime:      aws.Time(t0),
					LastUpdateTime: aws.Time(t1),
					Input: &cptypes.ActionExecutionInput{
						ActionTypeId: &cptypes.ActionTypeId{Provider: aws.String("CodeStarSourceConnection")},
					},
					Output: &cptypes.ActionExecutionOutput{
						ExecutionResult: &cptypes.ActionExecutionResult{
							// A source action's "external execution id" is the
							// commit SHA — it must not surface as a build id.
							ExternalExecutionId: aws.String("5368c27b9a831a1e05958c1e2fe769b46a070bfc"),
							// ...and its summary is the CodeConnections JSON —
							// it must be unwrapped to the commit message.
							ExternalExecutionSummary: aws.String(
								`{"ProviderType":"GitHub","CommitMessage":"fix vpc"}`),
						},
					},
				}},
			},
		},
	}
	svc := &Service{Read: api}

	actions, err := svc.ActionExecutions(context.Background(),
		"123456789012-customizations-pipeline", "exec-1", false)
	if err != nil {
		t.Fatalf("ActionExecutions: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions))
	}
	// Chronological: Source before Apply despite newest-first API order.
	if actions[0].StageName != "Source" || actions[1].StageName != "Apply" {
		t.Errorf("order = [%s %s], want [Source Apply]", actions[0].StageName, actions[1].StageName)
	}
	if actions[1].CodeBuildID != "aft-customizations:1111" {
		t.Errorf("CodeBuildID = %q", actions[1].CodeBuildID)
	}
	// The source action's commit SHA must be stripped, so the log screen
	// never feeds it to BatchGetBuilds.
	if actions[0].CodeBuildID != "" {
		t.Errorf("source CodeBuildID = %q, want empty", actions[0].CodeBuildID)
	}
	if actions[0].Summary != "fix vpc" {
		t.Errorf("source Summary = %q, want the unwrapped commit message", actions[0].Summary)
	}
	if actions[1].ErrorMessage != "exit status 1" {
		t.Errorf("ErrorMessage = %q", actions[1].ErrorMessage)
	}
	if got := actions[1].Duration(); got != 4*time.Minute {
		t.Errorf("Duration = %v, want 4m", got)
	}

	// Only the CodeBuild-backed action carries a log; the source action does not.
	if builds := model.LogActions(actions); len(builds) != 1 ||
		builds[0].ActionName != "terraform-apply" {
		t.Errorf("LogActions = %+v, want just terraform-apply", builds)
	}
}

func TestDetailSkipsHistoryWhenZero(t *testing.T) {
	api := &mockAPI{
		state: &codepipeline.GetPipelineStateOutput{
			StageStates: []cptypes.StageState{{StageName: aws.String("Source")}},
		},
		// executions intentionally left to fail the test if called
		executions: &codepipeline.ListPipelineExecutionsOutput{
			PipelineExecutionSummaries: []cptypes.PipelineExecutionSummary{
				{PipelineExecutionId: aws.String("should-not-appear")},
			},
		},
	}
	svc := &Service{Read: api}

	d, err := svc.Detail(context.Background(), "123456789012-customizations-pipeline", 0, nil)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if len(d.History) != 0 {
		t.Errorf("history should be empty when historyN=0, got %d", len(d.History))
	}
}

// A memoized execution history is served within the TTL; refresh and an
// in-flight head both force a refetch.
func TestExecutionsMemo(t *testing.T) {
	const name = "123456789012-customizations-pipeline"
	terminal := &codepipeline.ListPipelineExecutionsOutput{
		PipelineExecutionSummaries: []cptypes.PipelineExecutionSummary{
			{PipelineExecutionId: aws.String("exec-1"), Status: cptypes.PipelineExecutionStatusSucceeded},
		},
	}
	api := &mockAPI{executions: terminal}
	svc := &Service{Read: api, ExecutionsTTL: time.Minute}

	for i := 0; i < 2; i++ {
		if _, err := svc.Executions(context.Background(), name, 25, false); err != nil {
			t.Fatalf("Executions #%d: %v", i+1, err)
		}
	}
	if api.execCalls != 1 {
		t.Errorf("within TTL, want 1 API call, got %d", api.execCalls)
	}

	if _, err := svc.Executions(context.Background(), name, 25, true); err != nil {
		t.Fatalf("Executions (refresh): %v", err)
	}
	if api.execCalls != 2 {
		t.Errorf("refresh should force a refetch, got %d calls", api.execCalls)
	}
}

// A history whose head execution is in-flight bypasses the memo (a running
// pipeline moves fast); a zero TTL disables the memo entirely.
func TestExecutionsMemoBypasses(t *testing.T) {
	const name = "123456789012-customizations-pipeline"
	inflight := &codepipeline.ListPipelineExecutionsOutput{
		PipelineExecutionSummaries: []cptypes.PipelineExecutionSummary{
			{PipelineExecutionId: aws.String("exec-2"), Status: cptypes.PipelineExecutionStatusInProgress},
		},
	}

	api := &mockAPI{executions: inflight}
	svc := &Service{Read: api, ExecutionsTTL: time.Minute}
	for i := 0; i < 2; i++ {
		if _, err := svc.Executions(context.Background(), name, 25, false); err != nil {
			t.Fatalf("Executions #%d: %v", i+1, err)
		}
	}
	if api.execCalls != 2 {
		t.Errorf("in-flight head should refetch every time, got %d calls", api.execCalls)
	}

	api = &mockAPI{}
	svc = &Service{Read: api} // ExecutionsTTL zero: memo disabled
	for i := 0; i < 2; i++ {
		if _, err := svc.Executions(context.Background(), name, 25, false); err != nil {
			t.Fatalf("Executions #%d: %v", i+1, err)
		}
	}
	if api.execCalls != 2 {
		t.Errorf("zero TTL should disable the memo, got %d calls", api.execCalls)
	}
}

// A terminal execution's actions are memoized; an in-flight one refetches.
func TestActionExecutionsMemo(t *testing.T) {
	const name = "123456789012-customizations-pipeline"
	api := &mockAPI{}
	svc := &Service{Read: api}

	for i := 0; i < 2; i++ {
		if _, err := svc.ActionExecutions(context.Background(), name, "exec-1", true); err != nil {
			t.Fatalf("ActionExecutions #%d: %v", i+1, err)
		}
	}
	if api.actionCalls != 1 {
		t.Errorf("terminal execution should hit the API once, got %d", api.actionCalls)
	}

	for i := 0; i < 2; i++ {
		if _, err := svc.ActionExecutions(context.Background(), name, "exec-2", false); err != nil {
			t.Fatalf("ActionExecutions (in-flight) #%d: %v", i+1, err)
		}
	}
	if api.actionCalls != 3 {
		t.Errorf("in-flight execution should refetch every time, got %d total calls", api.actionCalls)
	}
}
