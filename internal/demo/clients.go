package demo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	cptypes "github.com/aws/aws-sdk-go-v2/service/codepipeline/types"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// logPageSize is how many lines one fake GetLogEvents page returns. Real
// CloudWatch pages too, and the paging loop in core/logs is part of what the
// demo should exercise rather than bypass.
const logPageSize = 200

// tick applies the fixture's per-call latency, respecting cancellation.
func (e *Env) tick(ctx context.Context) error {
	e.mu.Lock()
	d := e.fx.Latency.D()
	e.mu.Unlock()
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- CodePipeline (read) ----

// PipelineAPI returns a fake implementing pipeline.API.
func (e *Env) PipelineAPI() *PipelineClient { return &PipelineClient{env: e} }

// PipelineClient serves the read side of CodePipeline from the fixture.
type PipelineClient struct{ env *Env }

func (c *PipelineClient) ListPipelines(ctx context.Context, _ *codepipeline.ListPipelinesInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.ListPipelinesOutput, error) {
	if err := c.env.tick(ctx); err != nil {
		return nil, err
	}
	e := c.env
	e.mu.Lock()
	defer e.mu.Unlock()

	// Non-account pipelines are returned alongside the account ones, exactly
	// as a real AFT management account does: filtering them out is the
	// inventory's job, not the fixture's.
	out := &codepipeline.ListPipelinesOutput{}
	for _, n := range e.fx.OtherPipelines {
		out.Pipelines = append(out.Pipelines, cptypes.PipelineSummary{Name: aws.String(n)})
	}
	for _, p := range e.fx.Pipelines {
		out.Pipelines = append(out.Pipelines, cptypes.PipelineSummary{Name: aws.String(p.Name())})
	}
	return out, nil
}

func (c *PipelineClient) ListPipelineExecutions(ctx context.Context,
	in *codepipeline.ListPipelineExecutionsInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.ListPipelineExecutionsOutput, error) {
	if err := c.env.tick(ctx); err != nil {
		return nil, err
	}
	e := c.env
	e.mu.Lock()
	defer e.mu.Unlock()

	name := aws.ToString(in.PipelineName)
	i, ok := e.pipelineByName(name)
	if !ok {
		return nil, notFound(name)
	}
	now := e.now()
	limit := len(e.fx.Pipelines[i].Executions)
	if in.MaxResults != nil && int(*in.MaxResults) < limit {
		limit = int(*in.MaxResults)
	}

	out := &codepipeline.ListPipelineExecutionsOutput{}
	for _, ex := range e.fx.Pipelines[i].Executions[:limit] {
		st := e.state(ex, now)
		sum := cptypes.PipelineExecutionSummary{
			PipelineExecutionId: aws.String(ex.ID),
			Status:              cptypes.PipelineExecutionStatus(st.Status),
			StartTime:           aws.Time(st.Start),
			LastUpdateTime:      aws.Time(st.LastUpdate),
		}
		if r := ex.Revision; r.RevisionID != "" {
			sum.SourceRevisions = []cptypes.SourceRevision{{
				ActionName:      aws.String(r.ActionName),
				RevisionId:      aws.String(r.RevisionID),
				RevisionSummary: aws.String(r.Summary),
				RevisionUrl:     aws.String(r.URL),
			}}
		}
		out.PipelineExecutionSummaries = append(out.PipelineExecutionSummaries, sum)
	}
	return out, nil
}

// GetPipelineState is derived from the pipeline's newest execution rather
// than stored separately: the state view *is* the latest run's actions, and
// a fixture holding both would let the two disagree.
func (c *PipelineClient) GetPipelineState(ctx context.Context,
	in *codepipeline.GetPipelineStateInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.GetPipelineStateOutput, error) {
	if err := c.env.tick(ctx); err != nil {
		return nil, err
	}
	e := c.env
	e.mu.Lock()
	defer e.mu.Unlock()

	name := aws.ToString(in.Name)
	i, ok := e.pipelineByName(name)
	if !ok {
		return nil, notFound(name)
	}
	out := &codepipeline.GetPipelineStateOutput{PipelineName: aws.String(name)}
	if len(e.fx.Pipelines[i].Executions) == 0 {
		return out, nil
	}
	ex := e.fx.Pipelines[i].Executions[0]
	now := e.now()
	es := e.state(ex, now)

	for _, stage := range stageOrder(ex.Actions) {
		ss := cptypes.StageState{StageName: aws.String(stage)}
		worst := model.StatusUnknown
		for _, a := range ex.Actions {
			if a.Stage != stage {
				continue
			}
			as := cptypes.ActionState{ActionName: aws.String(a.Name)}
			st, _, last, started := e.actionState(es, a, now)
			if started {
				le := &cptypes.ActionExecution{
					Status:           cptypes.ActionExecutionStatus(st),
					Summary:          aws.String(a.Summary),
					LastStatusChange: aws.Time(last),
				}
				if a.BuildID != "" {
					le.ExternalExecutionId = aws.String(a.BuildID)
					le.LogStreamARN = aws.String(logStreamARN(e.fx.Identity, a.BuildID))
				} else if a.RevisionID != "" {
					le.ExternalExecutionId = aws.String(a.RevisionID)
				}
				if a.URL != "" {
					le.ExternalExecutionUrl = aws.String(a.URL)
				}
				if a.Error != "" && st == model.StatusFailed {
					le.ErrorDetails = &cptypes.ErrorDetails{Message: aws.String(a.Error)}
				}
				as.LatestExecution = le
				worst = worseStatus(worst, st)
			}
			ss.ActionStates = append(ss.ActionStates, as)
		}
		if worst != model.StatusUnknown {
			ss.LatestExecution = &cptypes.StageExecution{
				PipelineExecutionId: aws.String(ex.ID),
				Status:              cptypes.StageExecutionStatus(worst),
			}
		}
		out.StageStates = append(out.StageStates, ss)
	}
	return out, nil
}

func (c *PipelineClient) ListActionExecutions(ctx context.Context,
	in *codepipeline.ListActionExecutionsInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.ListActionExecutionsOutput, error) {
	if err := c.env.tick(ctx); err != nil {
		return nil, err
	}
	e := c.env
	e.mu.Lock()
	defer e.mu.Unlock()

	name := aws.ToString(in.PipelineName)
	i, ok := e.pipelineByName(name)
	if !ok {
		return nil, notFound(name)
	}
	want := ""
	if in.Filter != nil {
		want = aws.ToString(in.Filter.PipelineExecutionId)
	}
	now := e.now()

	out := &codepipeline.ListActionExecutionsOutput{}
	for _, ex := range e.fx.Pipelines[i].Executions {
		if want != "" && ex.ID != want {
			continue
		}
		es := e.state(ex, now)
		// The API yields newest-first; the core re-sorts chronologically, so
		// emit them reversed to keep that normalization under test.
		for j := len(ex.Actions) - 1; j >= 0; j-- {
			a := ex.Actions[j]
			st, start, last, started := e.actionState(es, a, now)
			if !started {
				continue // not yet run: the API would not list it
			}
			d := cptypes.ActionExecutionDetail{
				PipelineExecutionId: aws.String(ex.ID),
				StageName:           aws.String(a.Stage),
				ActionName:          aws.String(a.Name),
				Status:              cptypes.ActionExecutionStatus(st),
				StartTime:           aws.Time(start),
				LastUpdateTime:      aws.Time(last),
				Input: &cptypes.ActionExecutionInput{
					ActionTypeId: &cptypes.ActionTypeId{Provider: aws.String(providerOf(a))},
				},
			}
			res := &cptypes.ActionExecutionResult{
				ExternalExecutionSummary: aws.String(a.Summary),
			}
			switch {
			case a.BuildID != "":
				res.ExternalExecutionId = aws.String(a.BuildID)
				res.LogStreamARN = aws.String(logStreamARN(e.fx.Identity, a.BuildID))
			case a.RevisionID != "":
				res.ExternalExecutionId = aws.String(a.RevisionID)
			}
			if a.URL != "" {
				res.ExternalExecutionUrl = aws.String(a.URL)
			}
			if a.Error != "" && st == model.StatusFailed {
				res.ErrorDetails = &cptypes.ErrorDetails{Message: aws.String(a.Error)}
			}
			d.Output = &cptypes.ActionExecutionOutput{ExecutionResult: res}
			out.ActionExecutionDetails = append(out.ActionExecutionDetails, d)
		}
	}
	return out, nil
}

// ---- CodePipeline (write) ----

// StartAPI returns a fake implementing pipeline.StartAPI. A Release change
// in demo mode grows a real in-flight execution on the fixture, so the
// list's poll picks it up and watches it finish — the whole point of
// demonstrating the operation.
func (e *Env) StartAPI() *StartClient { return &StartClient{env: e} }

// StartClient serves StartPipelineExecution from the fixture.
type StartClient struct{ env *Env }

func (c *StartClient) StartPipelineExecution(ctx context.Context,
	in *codepipeline.StartPipelineExecutionInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.StartPipelineExecutionOutput, error) {
	if err := c.env.tick(ctx); err != nil {
		return nil, err
	}
	e := c.env
	e.mu.Lock()
	defer e.mu.Unlock()

	name := aws.ToString(in.Name)
	i, ok := e.pipelineByName(name)
	if !ok {
		return nil, notFound(name)
	}
	p := &e.fx.Pipelines[i]

	takes := e.fx.Release.Takes.D()
	if takes <= 0 {
		takes = 45 * time.Second
	}
	// Times are relative to the fixture's origin, so a run started now must
	// be placed at "elapsed since load" — hence the offset, not zero.
	elapsed := e.now().Sub(e.base)

	ex := Execution{
		ID:          e.nextID("d"),
		Status:      string(model.StatusInProgress),
		StartedAgo:  Duration(-elapsed),
		CompletesIn: Duration(elapsed + takes),
		CompletesAs: string(model.StatusSucceeded),
		Revision: Revision{
			ActionName: "aft-account-customizations",
			RevisionID: e.nextID("c"),
			Summary:    `{"ProviderType":"GitHub","CommitMessage":"chore: re-run customizations"}`,
		},
		Actions: e.releaseActions(*p, takes),
	}
	p.Executions = append([]Execution{ex}, p.Executions...)
	return &codepipeline.StartPipelineExecutionOutput{
		PipelineExecutionId: aws.String(ex.ID),
	}, nil
}

// releaseActions builds the action set of a released run. It reuses the
// pipeline's own most recent shape (same stages and action names, rescaled
// to the release duration) so a demo release looks like that pipeline's
// other runs rather than a generic stand-in.
func (e *Env) releaseActions(p Pipeline, takes time.Duration) []Action {
	tmpl := defaultActions()
	if len(p.Executions) > 0 && len(p.Executions[0].Actions) > 0 {
		tmpl = p.Executions[0].Actions
	}
	var span time.Duration
	for _, a := range tmpl {
		if end := a.StartsAfter.D() + a.Takes.D(); end > span {
			span = end
		}
	}
	scale := 1.0
	if span > 0 {
		scale = float64(takes) / float64(span)
	}

	out := make([]Action, 0, len(tmpl))
	for _, a := range tmpl {
		a.Status = string(model.StatusSucceeded)
		a.Error = ""
		a.StartsAfter = Duration(float64(a.StartsAfter.D()) * scale)
		a.Takes = Duration(float64(a.Takes.D()) * scale)
		if a.BuildID != "" {
			project := a.BuildID[:strings.Index(a.BuildID, ":")]
			a.BuildID = project + ":" + e.nextID("b")
			if e.fx.Release.Log != "" {
				a.Log = e.fx.Release.Log
			}
			a.Summary = ""
		}
		out = append(out, a)
	}
	return out
}

// defaultActions is the AFT customizations pipeline's shape, used when a
// released pipeline has never run and so has nothing to copy.
func defaultActions() []Action {
	return []Action{
		{Stage: "Source", Name: "aft-global-customizations", Status: "Succeeded",
			Takes: Duration(4 * time.Second), RevisionID: "0000000"},
		{Stage: "Source", Name: "aft-account-customizations", Status: "Succeeded",
			Takes: Duration(4 * time.Second), RevisionID: "0000000"},
		{Stage: "Global-Customizations", Name: "Apply", Status: "Succeeded",
			StartsAfter: Duration(5 * time.Second), Takes: Duration(60 * time.Second),
			BuildID: "aft-global-customizations:00000000"},
		{Stage: "Account-Customizations", Name: "Apply", Status: "Succeeded",
			StartsAfter: Duration(66 * time.Second), Takes: Duration(90 * time.Second),
			BuildID: "aft-account-customizations:00000000"},
	}
}

// ---- CodeBuild ----

// CodeBuildAPI returns a fake implementing logs.CodeBuildAPI.
func (e *Env) CodeBuildAPI() *CodeBuildClient { return &CodeBuildClient{env: e} }

// CodeBuildClient resolves a fixture build id to its log location.
type CodeBuildClient struct{ env *Env }

func (c *CodeBuildClient) BatchGetBuilds(ctx context.Context, in *codebuild.BatchGetBuildsInput,
	_ ...func(*codebuild.Options)) (*codebuild.BatchGetBuildsOutput, error) {
	if err := c.env.tick(ctx); err != nil {
		return nil, err
	}
	e := c.env
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	out := &codebuild.BatchGetBuildsOutput{}
	for _, id := range in.Ids {
		ref, ok := e.findBuild(id)
		if !ok {
			continue // BatchGetBuilds silently omits unknown ids
		}
		ex := e.fx.Pipelines[ref.pipeline].Executions[ref.exec]
		a := ex.Actions[ref.action]
		st, _, _, started := e.actionState(e.state(ex, now), a, now)
		project, stream := splitBuildID(id)
		out.Builds = append(out.Builds, cbtypes.Build{
			Id: aws.String(id),
			Logs: &cbtypes.LogsLocation{
				GroupName:  aws.String("/aws/codebuild/" + project),
				StreamName: aws.String(stream),
			},
			// Only a finished build's log is immutable, and that is what the
			// core memoizes on — so a running one must say so.
			BuildComplete: started && !st.InFlight(),
		})
	}
	return out, nil
}

// ---- CloudWatch Logs ----

// LogsAPI returns a fake implementing logs.LogsAPI.
func (e *Env) LogsAPI() *LogsClient { return &LogsClient{env: e} }

// LogsClient serves fixture log bodies with the real API's paging shape.
type LogsClient struct{ env *Env }

func (c *LogsClient) GetLogEvents(ctx context.Context, in *cloudwatchlogs.GetLogEventsInput,
	_ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	if err := c.env.tick(ctx); err != nil {
		return nil, err
	}
	e := c.env
	e.mu.Lock()
	defer e.mu.Unlock()

	stream := aws.ToString(in.LogStreamName)
	group := aws.ToString(in.LogGroupName)
	project := strings.TrimPrefix(group, "/aws/codebuild/")
	buildID := project + ":" + stream

	ref, ok := e.findBuild(buildID)
	if !ok {
		return nil, fmt.Errorf("ResourceNotFoundException: log stream %s/%s not found", group, stream)
	}
	ex := e.fx.Pipelines[ref.pipeline].Executions[ref.exec]
	a := ex.Actions[ref.action]
	lines := e.logLines(a.Log)

	// A build that is still running has only produced part of its log.
	st, start, _, started := e.actionState(e.state(ex, e.now()), a, e.now())
	if started && st.InFlight() && a.Takes > 0 {
		frac := float64(e.now().Sub(start)) / float64(a.Takes.D())
		if n := int(float64(len(lines)) * frac); n < len(lines) {
			lines = lines[:max(n, 1)]
		}
	}

	offset := parseForwardToken(in.NextToken)
	if offset >= len(lines) {
		return &cloudwatchlogs.GetLogEventsOutput{
			NextForwardToken: aws.String(forwardToken(len(lines))),
		}, nil
	}
	end := min(offset+logPageSize, len(lines))
	ts := start.UnixMilli()
	out := &cloudwatchlogs.GetLogEventsOutput{
		NextForwardToken: aws.String(forwardToken(end)),
	}
	for i := offset; i < end; i++ {
		out.Events = append(out.Events, cwtypes.OutputLogEvent{
			Message:   aws.String(lines[i] + "\n"),
			Timestamp: aws.Int64(ts + int64(i)*250),
		})
	}
	return out, nil
}

// ---- account source ----

// AccountSource returns a fake implementing account.Source.
func (e *Env) AccountSource() *AccountSource { return &AccountSource{env: e} }

// AccountSource serves the fixture's account list.
type AccountSource struct{ env *Env }

func (s *AccountSource) Name() string { return "demo(" + s.env.label() + ")" }

func (s *AccountSource) Fetch(ctx context.Context) ([]model.Account, error) {
	if err := s.env.tick(ctx); err != nil {
		return nil, err
	}
	s.env.mu.Lock()
	defer s.env.mu.Unlock()
	out := make([]model.Account, len(s.env.fx.Accounts))
	copy(out, s.env.fx.Accounts)
	return out, nil
}

// ---- helpers ----

func notFound(name string) error {
	return fmt.Errorf("PipelineNotFoundException: pipeline %s does not exist", name)
}

// stageOrder lists the stages in first-appearance order.
func stageOrder(actions []Action) []string {
	var order []string
	seen := map[string]bool{}
	for _, a := range actions {
		if !seen[a.Stage] {
			seen[a.Stage] = true
			order = append(order, a.Stage)
		}
	}
	return order
}

// worseStatus folds an action status into a stage's, keeping the one that
// most determines the stage's outcome.
func worseStatus(acc, s model.Status) model.Status {
	rank := func(x model.Status) int {
		switch x {
		case model.StatusFailed:
			return 0
		case model.StatusStopped, model.StatusStopping:
			return 1
		case model.StatusInProgress:
			return 2
		case model.StatusSucceeded:
			return 3
		}
		return 4
	}
	if rank(s) < rank(acc) {
		return s
	}
	return acc
}

// providerOf names the action's provider the way CodePipeline reports it.
// The core uses this to decide whether an external execution id is a
// CodeBuild build id or a commit SHA.
func providerOf(a Action) string {
	if a.BuildID != "" {
		return "CodeBuild"
	}
	return "CodeStarSourceConnection"
}

func splitBuildID(id string) (project, stream string) {
	if i := strings.Index(id, ":"); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

func logStreamARN(id Identity, buildID string) string {
	project, stream := splitBuildID(buildID)
	return fmt.Sprintf("arn:aws:logs:%s:%s:log-group:/aws/codebuild/%s:log-stream:%s",
		id.Region, id.Account, project, stream)
}

func forwardToken(offset int) string { return "f/" + strconv.Itoa(offset) }

func parseForwardToken(t *string) int {
	if t == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(*t, "f/"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
