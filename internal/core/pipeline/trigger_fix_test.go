package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	cptypes "github.com/aws/aws-sdk-go-v2/service/codepipeline/types"

	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

// fixAPI serves full pipeline definitions — stages, role, artifact store —
// so a test can assert that a fix sends every one of them back untouched.
type fixAPI struct {
	*countingAPI

	fmu      sync.Mutex
	decls    map[string]*cptypes.PipelineDeclaration
	updated  map[string]*cptypes.PipelineDeclaration
	failName string // UpdatePipeline fails for this pipeline
}

func newFixAPI() *fixAPI {
	return &fixAPI{
		countingAPI: newCountingAPI(),
		decls:       map[string]*cptypes.PipelineDeclaration{},
		updated:     map[string]*cptypes.PipelineDeclaration{},
	}
}

func (a *fixAPI) add(name string, triggers ...cptypes.PipelineTriggerDeclaration) {
	a.decls[name] = &cptypes.PipelineDeclaration{
		Name:          aws.String(name),
		RoleArn:       aws.String("arn:aws:iam::123456789012:role/aft-codepipeline-customizations-role"),
		Version:       aws.Int32(3),
		PipelineType:  cptypes.PipelineTypeV2,
		ArtifactStore: &cptypes.ArtifactStore{Location: aws.String("bucket"), Type: cptypes.ArtifactStoreTypeS3},
		Stages: []cptypes.StageDeclaration{{
			Name: aws.String("Source"),
			Actions: []cptypes.ActionDeclaration{{
				Name:          aws.String("aft-account-customizations"),
				Configuration: map[string]string{"DetectChanges": "false", "BranchName": "main"},
			}},
		}},
		Triggers: triggers,
	}
}

func (a *fixAPI) GetPipeline(_ context.Context, in *codepipeline.GetPipelineInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.GetPipelineOutput, error) {
	a.fmu.Lock()
	defer a.fmu.Unlock()
	d, ok := a.decls[aws.ToString(in.Name)]
	if !ok {
		return nil, errors.New("PipelineNotFoundException")
	}
	cp := *d
	return &codepipeline.GetPipelineOutput{Pipeline: &cp}, nil
}

func (a *fixAPI) UpdatePipeline(_ context.Context, in *codepipeline.UpdatePipelineInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.UpdatePipelineOutput, error) {
	a.fmu.Lock()
	defer a.fmu.Unlock()
	name := aws.ToString(in.Pipeline.Name)
	if name == a.failName {
		return nil, errors.New("AccessDeniedException: iam:PassRole")
	}
	cp := *in.Pipeline
	a.updated[name] = &cp
	out := cp
	out.Version = aws.Int32(aws.ToInt32(cp.Version) + 1)
	return &codepipeline.UpdatePipelineOutput{Pipeline: &out}, nil
}

func narrowTrigger(dir string) cptypes.PipelineTriggerDeclaration {
	return declarationFromPushTrigger(model.PushTrigger{
		ProviderType: model.TriggerProviderType,
		SourceAction: "aft-account-customizations",
		Branches:     []string{"main"},
		FilePaths:    []string{dir + "/terraform/*.tf"},
	})
}

// planFor builds the plan row a CLI run would hand over: the expectation,
// and the trigger as read when the plan was shown.
func planFor(name string, decl *cptypes.PipelineDeclaration) model.TriggerSummary {
	want, _ := triggerPolicy.Expect("acct")
	actual := make([]model.PushTrigger, 0, len(decl.Triggers))
	for _, d := range decl.Triggers {
		actual = append(actual, pushTriggerFromDeclaration(d))
	}
	s := model.TriggerSummary{PipelineName: name, AccountID: model.AccountIDFromPipeline(name),
		Expected: &want, Actual: actual}
	s.State, s.Reasons = model.ClassifyTrigger(s.Expected, s.Actual)
	return s
}

func newFixService(api *fixAPI, t *testing.T) *Service {
	return &Service{Read: api, Batch: batch.Config{Concurrency: 4}, Cache: cache.New(t.TempDir(), "", "")}
}

// The write changes the trigger and nothing else: stages, role and artifact
// store go back exactly as they came, the previous trigger is saved first,
// and the trigger cache reflects the write.
func TestUpdateTriggersReplacesOnlyTriggers(t *testing.T) {
	api := newFixAPI()
	api.add(p1, narrowTrigger("acct"))
	api.add(p2) // no trigger at all
	svc := newFixService(api, t)
	dir := t.TempDir()

	res := svc.UpdateTriggers(context.Background(), api, UpdateTriggersRequest{
		Targets:   []model.TriggerSummary{planFor(p1, api.decls[p1]), planFor(p2, api.decls[p2])},
		BackupDir: dir,
	}, nil)

	for _, r := range res {
		if r.Outcome != model.FixUpdated {
			t.Fatalf("%s: outcome %q (%s), want updated", r.PipelineName, r.Outcome, r.Error)
		}
		if r.VersionBefore != 3 || r.VersionAfter != 4 {
			t.Errorf("%s: versions %d → %d, want 3 → 4", r.PipelineName, r.VersionBefore, r.VersionAfter)
		}
	}

	sent, orig := api.updated[p1], api.decls[p1]
	if aws.ToString(sent.RoleArn) != aws.ToString(orig.RoleArn) || sent.ArtifactStore != orig.ArtifactStore ||
		len(sent.Stages) != 1 || sent.Stages[0].Actions[0].Configuration["DetectChanges"] != "false" ||
		sent.PipelineType != orig.PipelineType || aws.ToInt32(sent.Version) != 3 {
		t.Errorf("non-trigger fields changed: %+v", sent)
	}
	got := pushTriggerFromDeclaration(sent.Triggers[0])
	if !slices.Equal(got.FilePaths, []string{"acct/**"}) || !slices.Equal(got.FilePathExcludes, []string{"**/*.md"}) {
		t.Errorf("written trigger = %+v", got)
	}

	// The backup holds the trigger as it was, in CodePipeline's JSON shape.
	data, err := os.ReadFile(res[0].BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Version  int32            `json:"version"`
		Triggers []map[string]any `json:"triggers"`
	}
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if b.Version != 3 || len(b.Triggers) != 1 || b.Triggers[0]["providerType"] != model.TriggerProviderType {
		t.Errorf("backup = %s", data)
	}
	// "No trigger" is backed up as an empty list, which restores as such.
	data, _ = os.ReadFile(res[1].BackupPath)
	if err := json.Unmarshal(data, &b); err != nil || b.Triggers == nil || len(b.Triggers) != 0 {
		t.Errorf("empty backup = %s", data)
	}

	m, _, ok := cache.Get[map[string]triggerEntry](svc.Cache, triggerCacheKey, cache.Forever)
	if !ok || len(m[p1].Actual) != 1 || !slices.Equal(m[p1].Actual[0].FilePaths, []string{"acct/**"}) {
		t.Errorf("trigger cache not updated: %+v", m)
	}
}

// A running pipeline is skipped (UpdatePipeline would stop its execution),
// and a trigger that changed after the plan was shown is skipped too: what
// gets written is what the operator confirmed. Neither is written.
func TestUpdateTriggersSkips(t *testing.T) {
	api := newFixAPI()
	api.add(p1, narrowTrigger("acct"))
	api.add(p2, narrowTrigger("acct"))
	api.status[p1] = cptypes.PipelineExecutionStatusInProgress
	svc := newFixService(api, t)

	plan2 := planFor(p2, api.decls[p2])
	// Someone rewrites p2's trigger between the plan and the write.
	api.decls[p2].Triggers = []cptypes.PipelineTriggerDeclaration{narrowTrigger("elsewhere")}

	res := svc.UpdateTriggers(context.Background(), api, UpdateTriggersRequest{
		Targets:   []model.TriggerSummary{planFor(p1, api.decls[p1]), plan2},
		BackupDir: t.TempDir(),
	}, nil)

	if r := res[0]; r.Outcome != model.FixSkipped || !slices.Equal(r.Reasons, []string{model.SkipInProgress}) {
		t.Errorf("in-progress: %+v", r)
	}
	if r := res[1]; r.Outcome != model.FixSkipped || !slices.Equal(r.Reasons, []string{model.SkipChangedSincePlan}) {
		t.Errorf("changed since plan: %+v", r)
	}
	if len(api.updated) != 0 {
		t.Errorf("skipped pipelines were written: %v", api.updated)
	}
}

// A failed write is a failed row, not an aborted run, and nothing is written
// without a backup.
func TestUpdateTriggersFailures(t *testing.T) {
	api := newFixAPI()
	api.add(p1, narrowTrigger("acct"))
	api.add(p2, narrowTrigger("acct"))
	api.failName = p1
	svc := newFixService(api, t)

	res := svc.UpdateTriggers(context.Background(), api, UpdateTriggersRequest{
		Targets:   []model.TriggerSummary{planFor(p1, api.decls[p1]), planFor(p2, api.decls[p2])},
		BackupDir: t.TempDir(),
	}, nil)
	if res[0].Outcome != model.FixFailed || res[0].Error == "" {
		t.Errorf("failed write: %+v", res[0])
	}
	if res[1].Outcome != model.FixUpdated {
		t.Errorf("the next pipeline should still be written: %+v", res[1])
	}

	api2 := newFixAPI()
	api2.add(p3, narrowTrigger("acct"))
	res = newFixService(api2, t).UpdateTriggers(context.Background(), api2, UpdateTriggersRequest{
		Targets: []model.TriggerSummary{planFor(p3, api2.decls[p3])},
	}, nil)
	if res[0].Outcome != model.FixFailed || len(api2.updated) != 0 {
		t.Errorf("no backup dir must mean no write: %+v", res[0])
	}
}

// The default is one write at a time: UpdatePipeline's throttling limit is
// unmeasured.
func TestUpdateTriggersDefaultsToSerial(t *testing.T) {
	api := newFixAPI()
	api.add(p1, narrowTrigger("acct"))
	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	upd := updateFunc(func(ctx context.Context, in *codepipeline.UpdatePipelineInput) (*codepipeline.UpdatePipelineOutput, error) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		defer func() { mu.Lock(); inFlight--; mu.Unlock() }()
		// Hold the slot long enough that overlapping calls really overlap.
		time.Sleep(5 * time.Millisecond)
		return api.UpdatePipeline(ctx, in)
	})
	names := []string{p1, p2, p3}
	var targets []model.TriggerSummary
	for _, n := range names {
		api.add(n, narrowTrigger("acct"))
		targets = append(targets, planFor(n, api.decls[n]))
	}
	newFixService(api, t).UpdateTriggers(context.Background(), upd,
		UpdateTriggersRequest{Targets: targets, BackupDir: t.TempDir()}, nil)
	if peak != 1 {
		t.Errorf("peak concurrent UpdatePipeline = %d, want 1", peak)
	}
}

type updateFunc func(context.Context, *codepipeline.UpdatePipelineInput) (*codepipeline.UpdatePipelineOutput, error)

func (f updateFunc) UpdatePipeline(ctx context.Context, in *codepipeline.UpdatePipelineInput,
	_ ...func(*codepipeline.Options)) (*codepipeline.UpdatePipelineOutput, error) {
	return f(ctx, in)
}
