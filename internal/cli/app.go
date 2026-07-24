package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"golang.org/x/term"

	"github.com/hacker65536/aft-ops/internal/awsx"
	"github.com/hacker65536/aft-ops/internal/batch"
	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/config"
	"github.com/hacker65536/aft-ops/internal/core/account"
	"github.com/hacker65536/aft-ops/internal/core/logs"
	"github.com/hacker65536/aft-ops/internal/core/pipeline"
	"github.com/hacker65536/aft-ops/internal/metrics"
	"github.com/hacker65536/aft-ops/internal/output"
)

// App holds resolved configuration and lazily-built AWS clients shared by
// all subcommands.
type App struct {
	Cfg     config.Config
	Format  output.Format
	NoColor bool
	Refresh bool

	rec      *metrics.Recorder
	readCfg  *aws.Config
	writeCfg *aws.Config
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
	rec, err := metrics.NewRecorder(a.Cfg.Metrics.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: metrics disabled:", err)
		return
	}
	a.rec = rec
}

func (a *App) closeMetrics() {
	_ = a.rec.Close()
}

// ReadAWS returns the aws.Config for read operations (lazy, cached).
func (a *App) ReadAWS(ctx context.Context) (aws.Config, error) {
	if a.readCfg != nil {
		return *a.readCfg, nil
	}
	cfg, err := awsx.Load(ctx, a.Cfg.Profile, a.Cfg.Region, a.rec)
	if err != nil {
		return aws.Config{}, err
	}
	a.readCfg = &cfg
	return cfg, nil
}

// WriteAWS returns the aws.Config for mutating operations. It reuses the
// read config when no distinct write_profile is set.
func (a *App) WriteAWS(ctx context.Context) (aws.Config, error) {
	wp := a.Cfg.EffectiveWriteProfile()
	if wp == a.Cfg.Profile {
		return a.ReadAWS(ctx)
	}
	if a.writeCfg != nil {
		return *a.writeCfg, nil
	}
	cfg, err := awsx.Load(ctx, wp, a.Cfg.Region, a.rec)
	if err != nil {
		return aws.Config{}, err
	}
	a.writeCfg = &cfg
	return cfg, nil
}

// CacheStore returns the profile/region-scoped cache.
func (a *App) CacheStore() cache.Store {
	return cache.New(a.Cfg.Cache.Dir, a.Cfg.Profile, a.Cfg.Region)
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

// PipelineService builds the read-side pipeline service.
func (a *App) PipelineService(ctx context.Context) (*pipeline.Service, error) {
	cfg, err := a.ReadAWS(ctx)
	if err != nil {
		return nil, err
	}
	return &pipeline.Service{
		Read:        codepipeline.NewFromConfig(cfg),
		Batch:       a.BatchConfig(),
		Cache:       a.CacheStore(),
		PipelineTTL: a.Cfg.Cache.PipelineTTL.D(),
	}, nil
}

// LogsService builds the read-side CodeBuild + CloudWatch Logs service.
func (a *App) LogsService(ctx context.Context) (*logs.Service, error) {
	cfg, err := a.ReadAWS(ctx)
	if err != nil {
		return nil, err
	}
	return &logs.Service{
		CodeBuild: codebuild.NewFromConfig(cfg),
		Logs:      cloudwatchlogs.NewFromConfig(cfg),
	}, nil
}

// StartClient builds the write-side CodePipeline client.
func (a *App) StartClient(ctx context.Context) (pipeline.StartAPI, error) {
	cfg, err := a.WriteAWS(ctx)
	if err != nil {
		return nil, err
	}
	return codepipeline.NewFromConfig(cfg), nil
}

// AccountSource builds the configured account source.
func (a *App) AccountSource(ctx context.Context) (account.Source, error) {
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
