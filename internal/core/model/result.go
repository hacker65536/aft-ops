package model

import (
	"fmt"
	"strings"
)

// ResultKind classifies what one terraform run in a customizations build
// concluded (docs/design.md §4.6).
type ResultKind string

const (
	// ResultApplied: "Apply complete!" or "Destroy complete!" with at least
	// one resource touched.
	ResultApplied ResultKind = "applied"
	// ResultNoChanges: "No changes.", or an apply whose three counts are all
	// zero. Folding the zero apply in keeps the rows that did something the
	// only ones showing numbers.
	ResultNoChanges ResultKind = "no_changes"
	// ResultError: the log carries a terraform "Error:" header.
	ResultError ResultKind = "error"
	// ResultFailed: the action failed but the log has no terraform error —
	// the build died before or after terraform ran.
	ResultFailed ResultKind = "failed"
	// ResultPlan: only a "Plan:" line; the run never reached apply.
	ResultPlan ResultKind = "plan"
	// ResultRunning: the build (or the stage it belongs to) has not finished.
	ResultRunning ResultKind = "running"
	// ResultNotRun: the execution is over and never reached this stage
	// (an earlier stage failed, or the run was stopped).
	ResultNotRun ResultKind = "not_run"
	// ResultUnknown: no conclusion could be read — no verdict line in the
	// log, or the log could not be fetched.
	ResultUnknown ResultKind = "unknown"
)

// TerraformResult is the conclusion of one terraform run (one CodeBuild
// build of a customizations stage).
type TerraformResult struct {
	Kind    ResultKind `json:"kind"`
	Add     int        `json:"add"`
	Change  int        `json:"change"`
	Destroy int        `json:"destroy"`
	// Line is the log line the conclusion was drawn from ("Apply complete!
	// Resources: ..." / "Error: ..."). Empty when there was none.
	Line string `json:"line,omitempty"`
}

// Short renders the result for a narrow table cell: the counts as
// "+add ~change -destroy" (terraform's own plan symbols), or a word for the
// states that carry no counts. A missing result renders as "…" — not yet
// fetched.
func (r *TerraformResult) Short() string {
	if r == nil {
		return "…"
	}
	switch r.Kind {
	case ResultApplied:
		return fmt.Sprintf("+%d ~%d -%d", r.Add, r.Change, r.Destroy)
	case ResultNoChanges:
		return "·"
	case ResultError:
		return "✗ error"
	case ResultFailed:
		return "✗ failed"
	case ResultPlan:
		return fmt.Sprintf("plan +%d ~%d -%d", r.Add, r.Change, r.Destroy)
	case ResultRunning:
		return "running"
	case ResultNotRun:
		return "—"
	default:
		return "?"
	}
}

// ExecutionResults is the terraform outcome of one pipeline execution, one
// result per customizations layer. An AFT customizations run applies the
// global layer, then the account layer; the two are reported separately
// because they answer different questions (did the fleet-wide change land /
// did this account's own change land).
type ExecutionResults struct {
	ExecutionID string           `json:"execution_id"`
	Global      *TerraformResult `json:"global,omitempty"`
	Account     *TerraformResult `json:"account,omitempty"`
}

// Layer is the customizations layer a pipeline stage applies.
type Layer string

const (
	LayerGlobal  Layer = "global"
	LayerAccount Layer = "account"
)

// StageLayer maps a pipeline stage to the customizations layer it applies,
// or "" for any other stage (Source). AFT names the stages
// "Global-Customizations" / "Account-Customizations" (some versions prefix
// them with "AFT-"), so the match is on the suffix.
func StageLayer(stage string) Layer {
	s := strings.ToLower(stage)
	switch {
	case strings.HasSuffix(s, "global-customizations"):
		return LayerGlobal
	case strings.HasSuffix(s, "account-customizations"):
		return LayerAccount
	}
	return ""
}

// LayerAction returns the action run that carries the given layer's build:
// the last CodeBuild action of that layer's stage, in pipeline order. nil
// when the execution has none (yet).
func LayerAction(actions []ActionExecution, layer Layer) *ActionExecution {
	var out *ActionExecution
	for i := range actions {
		a := &actions[i]
		if a.CodeBuildID != "" && StageLayer(a.StageName) == layer {
			out = a
		}
	}
	return out
}
