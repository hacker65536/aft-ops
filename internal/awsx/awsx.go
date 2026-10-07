// Package awsx builds AWS SDK v2 configurations with the tool-wide retry
// policy and the metrics middleware attached. Credentials are always
// delegated to the SDK's standard chain (SSO profiles included); the tool
// never handles credentials itself.
package awsx

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/smithy-go/middleware"
	"golang.org/x/time/rate"

	"github.com/hacker65536/aft-ops/internal/metrics"
)

// maxAttempts bounds SDK-level retries; throttling additionally engages
// the adaptive client-side rate limiter.
const maxAttempts = 8

// ConfigFileLabel names the shared config file that profile lookups resolve
// against, for diagnostics. An empty configFile hands the decision to the
// SDK, so report what the SDK will decide rather than saying nothing: the
// case worth reporting is precisely the one where an ambient AWS_CONFIG_FILE
// is not the file the operator had in mind.
func ConfigFileLabel(configFile string) string {
	if configFile != "" {
		return configFile
	}
	if v := os.Getenv("AWS_CONFIG_FILE"); v != "" {
		return v + " (from AWS_CONFIG_FILE)"
	}
	return "~/.aws/config (SDK default)"
}

// Load builds an aws.Config for the given profile/region. rec and limits
// may be nil.
//
// configFile pins the shared config file the profile is looked up in; empty
// leaves the SDK's own resolution alone (AWS_CONFIG_FILE, else ~/.aws/config).
//
// limits, when set, admits every API call attempt made through the config
// (see RateLimit). Pass the same Limits to every config of a run so that
// they all draw from one budget per service.
func Load(ctx context.Context, profile, region, configFile string, rec *metrics.Recorder,
	limits *Limits) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.NewAdaptiveMode(func(o *retry.AdaptiveModeOptions) {
				o.StandardOptions = append(o.StandardOptions, func(so *retry.StandardOptions) {
					so.MaxAttempts = maxAttempts
				})
			})
		}),
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if configFile != "" {
		opts = append(opts, awsconfig.WithSharedConfigFiles([]string{configFile}))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		// Name the file the profile was looked up in. The SDK's own message
		// says only that the profile is missing, which is the wrong half of
		// the story when several config files are in rotation.
		return aws.Config{}, fmt.Errorf("load AWS config (profile=%q, config file=%s): %w",
			profile, ConfigFileLabel(configFile), err)
	}
	if limits != nil {
		cfg.APIOptions = append(cfg.APIOptions, RateLimit(limits))
	}
	if rec != nil {
		cfg.APIOptions = append(cfg.APIOptions, metrics.Middleware(rec))
	}
	return cfg, nil
}

// Limits is a run's API budget: one token bucket per AWS service, created on
// first use at the rate RateFor gives that service (<= 0 = unlimited).
//
// Per service because that is how AWS meters: CodePipeline, CodeBuild and
// CloudWatch Logs each have their own request quotas, so a call to one does
// not spend another's allowance. One shared bucket made the log reads of the
// list's terraform results queue behind CodePipeline calls, holding the whole
// run to the slowest service's rate (docs/design.md §4.6).
type Limits struct {
	// RateFor returns the calls-per-second limit of a service, by its SDK
	// service id ("CodePipeline", "CodeBuild", "CloudWatch Logs", ...).
	RateFor func(service string) float64

	mu      sync.Mutex
	buckets map[string]*rate.Limiter // nil entry = unlimited
}

// Wait blocks until the service's bucket admits one call.
func (l *Limits) Wait(ctx context.Context, service string) error {
	if b := l.bucket(service); b != nil {
		return b.Wait(ctx)
	}
	return ctx.Err()
}

func (l *Limits) bucket(service string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[service]; ok {
		return b
	}
	var b *rate.Limiter
	if l.RateFor != nil {
		if r := l.RateFor(service); r > 0 {
			b = rate.NewLimiter(rate.Limit(r), 1)
		}
	}
	if l.buckets == nil {
		l.buckets = map[string]*rate.Limiter{}
	}
	l.buckets[service] = b
	return b
}

// RateLimit returns an aws.Config.APIOptions hook that makes every API call
// attempt — retries included, like the metrics — wait for a token from its
// service's bucket before it is sent.
//
// The budget lives here, at the call, rather than in the batch engine,
// because the engine only sees items: a fan-out whose item makes several
// calls (the terraform results of the list, docs/design.md §4.6) would run
// at a multiple of the configured rate, and two fan-outs running at once
// (the TUI's status poll beside the results fetch) would each get the full
// rate. One Limits shared by every client of a run is what makes the
// configured rates mean "API calls per second", which is what they are
// tuned against.
func RateLimit(limits *Limits) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		mw := middleware.FinalizeMiddlewareFunc("aftOpsRateLimit",
			func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
				middleware.FinalizeOutput, middleware.Metadata, error,
			) {
				if err := limits.Wait(ctx, awsmiddleware.GetServiceID(ctx)); err != nil {
					return middleware.FinalizeOutput{}, middleware.Metadata{}, err
				}
				return next.HandleFinalize(ctx, in)
			})
		// After the retry middleware, so each attempt waits for its own
		// token; a stack without one (none of ours) still gets the limit.
		if err := stack.Finalize.Insert(mw, "Retry", middleware.After); err != nil {
			return stack.Finalize.Add(mw, middleware.After)
		}
		return nil
	}
}
