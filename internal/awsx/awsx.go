// Package awsx builds AWS SDK v2 configurations with the tool-wide retry
// policy and the metrics middleware attached. Credentials are always
// delegated to the SDK's standard chain (SSO profiles included); the tool
// never handles credentials itself.
package awsx

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

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

// Load builds an aws.Config for the given profile/region. rec may be nil.
//
// configFile pins the shared config file the profile is looked up in; empty
// leaves the SDK's own resolution alone (AWS_CONFIG_FILE, else ~/.aws/config).
func Load(ctx context.Context, profile, region, configFile string, rec *metrics.Recorder) (aws.Config, error) {
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
	if rec != nil {
		cfg.APIOptions = append(cfg.APIOptions, metrics.Middleware(rec))
	}
	return cfg, nil
}
