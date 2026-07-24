package pipeline

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	cptypes "github.com/aws/aws-sdk-go-v2/service/codepipeline/types"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// mockAPI is a hand-rolled stub of the read-side CodePipeline API.
type mockAPI struct {
	state      *codepipeline.GetPipelineStateOutput
	executions *codepipeline.ListPipelineExecutionsOutput
	stateErr   error
}

func (m *mockAPI) ListPipelines(context.Context, *codepipeline.ListPipelinesInput,
	...func(*codepipeline.Options)) (*codepipeline.ListPipelinesOutput, error) {
	return &codepipeline.ListPipelinesOutput{}, nil
}

func (m *mockAPI) ListPipelineExecutions(context.Context, *codepipeline.ListPipelineExecutionsInput,
	...func(*codepipeline.Options)) (*codepipeline.ListPipelineExecutionsOutput, error) {
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
