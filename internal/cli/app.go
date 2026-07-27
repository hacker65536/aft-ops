package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/term"

	"github.com/hacker65536/aft-ops/internal/awsx"
	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/config"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/logs"
	"github.com/hacker65536/aft-ops/internal/core/pipeline"
	"github.com/hacker65536/aft-ops/internal/demo"
	"github.com/hacker65536/aft-ops/internal/metrics"
	"github.com/hacker65536/aft-ops/internal/output"
)

// App holds resolved configuration and lazily-built AWS clients shared by
// all subcommands.
//
// The lazy fields are guarded by mu: the TUI runs its core calls from
// concurrent tea.Cmd goroutines (several at once — the actions screen fetches
// every build's verdict), so first-use construction must be serialized.
type App struct {
	Cfg     config.Config
	Format  output.Format
	NoColor bool
	Refresh bool
	// Demo, when set (--demo), serves every read and write from a local
	// fixture instead of AWS. Each AWS-facing constructor below branches on
	// it; nothing else in the tool knows the difference.
	Demo *demo.Env

	demoBanner sync.Once

	mu        sync.Mutex
	rec       *metrics.Recorder
	readCfg   *aws.Config
	writeCfg  *aws.Config
	logsSvc   *logs.Service
	pipeSvc   *pipeline.Service
	accountID string // resolved caller identity (see Identity)
}

// Color reports whether stdout table output should be colorized.
func (a *App) Color() bool {
	return !a.NoColor && term.IsTerminal(int(os.Stdout.Fd()))
}

// StderrIsTTY gates interactive progress rendering.
func (a *App) StderrIsTTY() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

func (a *App) initMetrics() {
	if !a.Cfg.Metrics.Enabled {
		return
	}
	rec, err := metrics.NewRecorder(a.Cfg.Metrics.Dir, a.Cfg.Metrics.KeepRuns)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: metrics disabled:", err)
		return
	}
	a.rec = rec
}

// closeMetrics is idempotent: it runs from a defer in Execute (cobra skips
// PersistentPostRun when a command returns an error, which would otherwise
// leave empty metrics files behind).
func (a *App) closeMetrics() {
	a.mu.Lock()
	rec := a.rec
	a.rec = nil
	a.mu.Unlock()
	_ = rec.Close()
}

// ReadAWS returns the aws.Config for read operations (lazy, cached).
func (a *App) ReadAWS(ctx context.Context) (aws.Config, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.readAWSLocked(ctx)
}

func (a *App) readAWSLocked(ctx context.Context) (aws.Config, error) {
	if a.Demo != nil {
		// Every demo-mode path is routed to a fake before it gets here, so
		// reaching this point would mean an AWS client was about to be built
		// for a run that promised not to touch AWS. Fail loudly rather than
		// quietly resolve credentials.
		return aws.Config{}, errors.New("demo mode: no AWS client is available for this operation")
	}
	if a.readCfg != nil {
		return *a.readCfg, nil
	}
	cfg, err := awsx.Load(ctx, a.Cfg.Profile, a.Cfg.Region, a.Cfg.AWSConfigFile, a.rec)
	if err != nil {
		return aws.Config{}, err
	}
	a.readCfg = &cfg
	// First contact with AWS: pin down which account these credentials
	// actually resolve to before any data is fetched or cached. --refresh
	// bypasses caches, and the recorded identity is one of them.
	a.resolveIdentityLocked(ctx, cfg, a.Refresh)
	return cfg, nil
}

// WriteAWS returns the aws.Config for mutating operations. It reuses the
// read config when no distinct write_profile is set.
//
// Either way the caller identity is verified against AWS here, never taken
// from the recorded value: this is the path that starts pipeline executions,
// and "which account am I about to change?" is not a question to answer from
// a day-old note. The extra call costs a fraction of a second on an operation
// that already asks a human to confirm.
func (a *App) WriteAWS(ctx context.Context) (aws.Config, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	wp := a.Cfg.EffectiveWriteProfile()
	if wp == a.Cfg.Profile {
		cfg, err := a.readAWSLocked(ctx)
		if err != nil {
			return aws.Config{}, err
		}
		a.resolveIdentityLocked(ctx, cfg, true)
		return cfg, nil
	}
	if a.writeCfg != nil {
		return *a.writeCfg, nil
	}
	cfg, err := awsx.Load(ctx, wp, a.Cfg.Region, a.Cfg.AWSConfigFile, a.rec)
	if err != nil {
		return aws.Config{}, err
	}
	a.writeCfg = &cfg
	// A distinct write profile is a distinct set of credentials: say which
	// account they land in before anything is triggered.
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not resolve the write profile's identity:", err)
		return cfg, nil
	}
	fmt.Fprintf(os.Stderr, "aws (write): account %s · region %s · profile %s%s\n",
		aws.ToString(out.Account), cfg.Region, wp, a.awsConfigFileNote())
	return cfg, nil
}

// CacheStore returns the profile/region-scoped cache.
func (a *App) CacheStore() cache.Store {
	return cache.New(a.Cfg.Cache.Dir, a.Cfg.Profile, a.Cfg.Region)
}

// identityCacheKey records which AWS account this cache scope was filled
// from, so a scope can never silently serve another account's data.
const identityCacheKey = "identity"

// identityTTL is how long a recorded account id is reused for a *configured*
// profile before being re-verified. A profile pins its account in
// ~/.aws/config, so the mapping only changes when someone edits that file;
// re-checking daily catches that, while a fully-cached `pipeline list` stays
// instant (verifying costs ~0.6s — credential resolution plus the STS call —
// which is 30x the entire cached run).
//
// An unconfigured profile gets no such reuse: there the account is whatever
// the ambient environment says today, which is exactly the mix-up this
// guards against.
const identityTTL = 24 * time.Hour

// Identity returns the AWS account id the current credentials resolve to
// (empty when it could not be determined). It forces the read config to be
// built, which is what announces the target on stderr — callers that must
// print before entering an alternate screen (the TUI) use it for that.
func (a *App) Identity(ctx context.Context) string {
	if a.Demo != nil {
		a.announceDemoTarget()
		return a.Demo.Identity().Account
	}
	if _, err := a.ReadAWS(ctx); err != nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accountID
}

// resolveIdentityLocked works out which account the credentials belong to,
// announces it on stderr, and guards the cache scope against a profile that
// now points somewhere else.
//
// This exists because the cache scope is keyed by profile+region: with no
// profile configured the SDK falls back to the ambient credential chain
// (AWS_PROFILE and friends), so the same scope can be filled from different
// accounts on different days. Naming the account on every run makes the
// mix-up visible immediately, and the recorded id makes it impossible to
// serve one account's pipelines under another's.
//
// A configured profile reuses its recorded id within identityTTL so the
// cached read path stays instant; force skips that reuse (mutating commands
// verify unconditionally — see WriteAWS).
func (a *App) resolveIdentityLocked(ctx context.Context, cfg aws.Config, force bool) {
	store := a.CacheStore()
	if !force && a.Cfg.Profile != "" {
		if prev, _, ok := cache.Get[string](store, identityCacheKey, identityTTL); ok {
			a.accountID = prev
			a.announceTarget(cfg.Region, false)
			return
		}
	}

	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not resolve the caller identity:", err)
		return
	}
	a.accountID = aws.ToString(out.Account)

	prev, _, ok := cache.Get[string](store, identityCacheKey, cache.Forever)
	if ok && prev != a.accountID {
		fmt.Fprintf(os.Stderr,
			"warning: this cache scope was filled from account %s but the credentials now resolve to %s; discarding the cached data\n",
			prev, a.accountID)
		if err := store.Clear(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: failed to clear the stale cache:", err)
		}
	}
	if !ok || prev != a.accountID {
		if err := cache.Put(store, identityCacheKey, a.accountID); err != nil {
			fmt.Fprintln(os.Stderr, "warning: failed to record the cache identity:", err)
		}
	}
	a.announceTarget(cfg.Region, true)
}

// announceTarget names the account every command is about to act on. It is
// one line on stderr, printed once per run, and it is the thing that makes a
// wrong-account operation obvious before it happens rather than after.
//
// A reused record is labelled as such: the banner may only assert what was
// actually checked this run, or it becomes a source of false confidence
// rather than a guard against it.
func (a *App) announceTarget(region string, verified bool) {
	line := fmt.Sprintf("aws: account %s · region %s · profile %s%s",
		a.accountID, region, a.profileLabel(), a.awsConfigFileNote())
	if !verified {
		line += "  (identity from cache; --refresh re-checks)"
	}
	fmt.Fprintln(os.Stderr, line)
}

// announceDemoTarget is the demo-mode counterpart of announceTarget. It
// says the same thing in the same place — and says that none of it is real,
// so a demo recording can never be mistaken for a session against an actual
// management account.
func (a *App) announceDemoTarget() {
	a.demoBanner.Do(func() {
		id := a.Demo.Identity()
		fmt.Fprintf(os.Stderr,
			"aws: account %s · region %s · profile %s  (demo mode: %s, no AWS calls)\n",
			id.Account, id.Region, id.Profile, filepath.Base(a.Demo.Path()))
	})
}

// profileLabel names the credential source for the target banner, calling
// out the case where nothing was configured and the ambient environment
// decided which account we are talking to.
func (a *App) profileLabel() string {
	if a.Cfg.Profile != "" {
		return a.Cfg.Profile
	}
	if env := os.Getenv("AWS_PROFILE"); env != "" {
		return fmt.Sprintf("(unset — using AWS_PROFILE=%s)", env)
	}
	return "(unset — using the default credential chain)"
}

// awsConfigFileNote names the shared config file the profile was looked up
// in, but only when it is not the SDK's default one — the banner earns its
// keep by being short enough to read every run.
//
// The ambient AWS_CONFIG_FILE is reported too, not just our own setting.
// Operators who keep one file per organization switch between them in the
// shell, and "the profile resolved against a file I did not mean" is the
// failure this line is here to make visible.
func (a *App) awsConfigFileNote() string {
	if a.Cfg.AWSConfigFile == "" && os.Getenv("AWS_CONFIG_FILE") == "" {
		return ""
	}
	return " · config " + awsx.ConfigFileLabel(a.Cfg.AWSConfigFile)
}

// BatchConfig maps config to the batch engine.
func (a *App) BatchConfig() batch.Config {
	return batch.Config{
		Concurrency: a.Cfg.Batch.Concurrency,
		RPS:         a.Cfg.Batch.RPS,
		ChunkSize:   a.Cfg.Batch.ChunkSize,
		ChunkPause:  a.Cfg.Batch.ChunkPause.D(),
	}
}

// statusOptions builds the status-cache policy for read commands: a cached
// status is served within StatusTTL, --refresh forces a full refetch, and
// in-flight statuses are always refetched (they change fastest).
func (a *App) statusOptions() pipeline.StatusOptions {
	return pipeline.StatusOptions{
		TTL:             a.Cfg.Cache.StatusTTL.D(),
		RefreshAll:      a.Refresh,
		RefreshInFlight: true,
	}
}

// PipelineService builds the read-side pipeline service (lazy, cached — the
// instance carries the session-scoped executions/actions memos).
func (a *App) PipelineService(ctx context.Context) (*pipeline.Service, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pipeSvc != nil {
		return a.pipeSvc, nil
	}
	var read pipeline.API
	if a.Demo != nil {
		a.announceDemoTarget()
		read = a.Demo.PipelineAPI()
	} else {
		cfg, err := a.readAWSLocked(ctx)
		if err != nil {
			return nil, err
		}
		read = codepipeline.NewFromConfig(cfg)
	}
	a.pipeSvc = &pipeline.Service{
		Read:          read,
		Batch:         a.BatchConfig(),
		Cache:         a.CacheStore(),
		PipelineTTL:   a.Cfg.Cache.PipelineTTL.D(),
		ExecutionsTTL: a.Cfg.Cache.ExecutionsTTL.D(),
	}
	return a.pipeSvc, nil
}

// LogsService builds the read-side CodeBuild + CloudWatch Logs service
// (lazy, cached — the instance carries the session-scoped memo of completed
// builds' logs, so the TUI revisiting a log performs no requests).
func (a *App) LogsService(ctx context.Context) (*logs.Service, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logsSvc != nil {
		return a.logsSvc, nil
	}
	if a.Demo != nil {
		a.announceDemoTarget()
		a.logsSvc = &logs.Service{
			CodeBuild: a.Demo.CodeBuildAPI(),
			Logs:      a.Demo.LogsAPI(),
		}
		return a.logsSvc, nil
	}
	cfg, err := a.readAWSLocked(ctx)
	if err != nil {
		return nil, err
	}
	a.logsSvc = &logs.Service{
		CodeBuild: codebuild.NewFromConfig(cfg),
		Logs:      cloudwatchlogs.NewFromConfig(cfg),
	}
	return a.logsSvc, nil
}

// StartClient builds the write-side CodePipeline client.
func (a *App) StartClient(ctx context.Context) (pipeline.StartAPI, error) {
	if a.Demo != nil {
		a.announceDemoTarget()
		return a.Demo.StartAPI(), nil
	}
	cfg, err := a.WriteAWS(ctx)
	if err != nil {
		return nil, err
	}
	return codepipeline.NewFromConfig(cfg), nil
}

// AccountSource builds the configured account source.
func (a *App) AccountSource(ctx context.Context) (account.Source, error) {
	if a.Demo != nil {
		a.announceDemoTarget()
		return a.Demo.AccountSource(), nil
	}
	switch a.Cfg.AccountSource {
	case config.SourceAFTDynamoDB:
		cfg, err := a.ReadAWS(ctx)
		if err != nil {
			return nil, err
		}
		return &account.DynamoSource{
			Client: dynamodb.NewFromConfig(cfg),
			Table:  a.Cfg.AFTMetadataTable,
		}, nil
	case config.SourceOrganizations:
		cfg, err := a.ReadAWS(ctx)
		if err != nil {
			return nil, err
		}
		return &account.OrgSource{Client: organizations.NewFromConfig(cfg)}, nil
	case config.SourceStatic:
		return &account.StaticSource{Path: a.Cfg.StaticAccountsFile}, nil
	default:
		return nil, fmt.Errorf("unknown account_source %q", a.Cfg.AccountSource)
	}
}

// Resolver loads accounts (cache-aware) and prints a staleness note.
func (a *App) Resolver(ctx context.Context) (*account.Resolver, error) {
	src, err := a.AccountSource(ctx)
	if err != nil {
		return nil, err
	}
	r, err := account.Load(ctx, src, a.CacheStore(), a.Cfg.Cache.AccountTTL.D(), a.Refresh)
	if err != nil {
		return nil, err
	}
	if r.FromCache {
		output.CacheNote(os.Stderr, "accounts", r.FetchedAt)
	}
	return r, nil
}
