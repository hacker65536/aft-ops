// Package awsx builds AWS SDK v2 configurations with the tool-wide retry
// policy and the metrics middleware attached. Credentials are always
// delegated to the SDK's standard chain (SSO profiles included); the tool
// never handles credentials itself.
package awsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/hacker65536/aft-ops/internal/metrics"
)

// maxAttempts bounds SDK-level retries; throttling additionally engages
// the adaptive client-side rate limiter.
const maxAttempts = 8

// Load builds an aws.Config for the given profile/region. rec may be nil.
func Load(ctx context.Context, profile, region string, rec *metrics.Recorder) (aws.Config, error) {
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

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS config (profile=%q): %w", profile, err)
	}
	if rec != nil {
		cfg.APIOptions = append(cfg.APIOptions, metrics.Middleware(rec))
	}
	return cfg, nil
}
