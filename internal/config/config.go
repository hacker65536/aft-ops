// Package config loads tool configuration with the precedence:
// flags > environment (AFT_OPS_*) > config file (YAML) > defaults.
// Flags are applied by the CLI layer on top of the Config returned here.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// AccountSource selects where account id/name mappings come from.
type AccountSource string

const (
	SourceAFTDynamoDB   AccountSource = "aft-dynamodb"
	SourceOrganizations AccountSource = "organizations"
	SourceStatic        AccountSource = "static"
)

// Duration wraps time.Duration for YAML ("30s", "6h", ...).
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
func (d Duration) D() time.Duration          { return time.Duration(d) }

type Config struct {
	Profile      string `yaml:"profile"`
	WriteProfile string `yaml:"write_profile"` // optional; falls back to Profile
	Region       string `yaml:"region"`

	AccountSource      AccountSource `yaml:"account_source"`
	AFTMetadataTable   string        `yaml:"aft_metadata_table"`
	StaticAccountsFile string        `yaml:"static_accounts_file"`

	Batch   Batch   `yaml:"batch"`
	Cache   Cache   `yaml:"cache"`
	Release Release `yaml:"release"`
	TUI     TUI     `yaml:"tui"`
	Metrics Metrics `yaml:"metrics"`
}

type Batch struct {
	Concurrency int      `yaml:"concurrency"`
	RPS         float64  `yaml:"rps"`
	ChunkSize   int      `yaml:"chunk_size"`
	ChunkPause  Duration `yaml:"chunk_pause"`
}

type Cache struct {
	Dir         string   `yaml:"dir"`
	AccountTTL  Duration `yaml:"account_ttl"`
	PipelineTTL Duration `yaml:"pipeline_ttl"`
	// StatusTTL bounds how long a cached execution status is served before a
	// refetch. In-flight statuses are always refetched regardless (see
	// pipeline.StatusOptions). 0 disables status caching (always fan out).
	StatusTTL Duration `yaml:"status_ttl"`
}

type Release struct {
	MaxTargets     int  `yaml:"max_targets"`
	SkipInProgress bool `yaml:"skip_in_progress"`
}

type TUI struct {
	PollInterval Duration `yaml:"poll_interval"`
}

type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
}

// Default returns the built-in defaults (see docs/design.md §10).
func Default() Config {
	return Config{
		Region:           "ap-northeast-1",
		AccountSource:    SourceAFTDynamoDB,
		AFTMetadataTable: "aft-request-metadata",
		Batch: Batch{
			Concurrency: 10,
			RPS:         8,
			ChunkSize:   0,
			ChunkPause:  0,
		},
		Cache: Cache{
			Dir:         DefaultCacheDir(),
			AccountTTL:  Duration(24 * time.Hour),
			PipelineTTL: Duration(6 * time.Hour),
			StatusTTL:   Duration(10 * time.Minute),
		},
		Release: Release{
			MaxTargets:     50,
			SkipInProgress: true,
		},
		TUI:     TUI{PollInterval: Duration(30 * time.Second)},
		Metrics: Metrics{Enabled: true, Dir: DefaultMetricsDir()},
	}
}

// Load reads the config file at path (or the default path when path is
// empty) and applies AFT_OPS_* environment overrides. A missing file is an
// error only when the path was given explicitly. Unknown keys in the file
// are rejected to catch typos early.
func Load(path string) (Config, error) {
	cfg := Default()

	explicit := path != ""
	if path == "" {
		path = DefaultConfigPath()
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := yaml.NewDecoder(newBytesReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		// no config file: defaults + env only
	default:
		return cfg, fmt.Errorf("read config: %w", err)
	}

	applyEnv(&cfg)
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.AccountSource {
	case SourceAFTDynamoDB, SourceOrganizations, SourceStatic:
	default:
		return fmt.Errorf("invalid account_source %q (want %s|%s|%s)",
			c.AccountSource, SourceAFTDynamoDB, SourceOrganizations, SourceStatic)
	}
	if c.AccountSource == SourceStatic && c.StaticAccountsFile == "" {
		return errors.New("account_source is static but static_accounts_file is not set")
	}
	if c.Batch.Concurrency < 1 {
		return fmt.Errorf("batch.concurrency must be >= 1 (got %d)", c.Batch.Concurrency)
	}
	return nil
}

// EffectiveWriteProfile returns the profile used for mutating operations.
func (c *Config) EffectiveWriteProfile() string {
	if c.WriteProfile != "" {
		return c.WriteProfile
	}
	return c.Profile
}

func applyEnv(c *Config) {
	if v := os.Getenv("AFT_OPS_PROFILE"); v != "" {
		c.Profile = v
	}
	if v := os.Getenv("AFT_OPS_WRITE_PROFILE"); v != "" {
		c.WriteProfile = v
	}
	if v := os.Getenv("AFT_OPS_REGION"); v != "" {
		c.Region = v
	}
	if v := os.Getenv("AFT_OPS_ACCOUNT_SOURCE"); v != "" {
		c.AccountSource = AccountSource(v)
	}
	if v := os.Getenv("AFT_OPS_CACHE_DIR"); v != "" {
		c.Cache.Dir = v
	}
	if v := os.Getenv("AFT_OPS_STATUS_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Cache.StatusTTL = Duration(d)
		}
	}
	if v := os.Getenv("AFT_OPS_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Batch.Concurrency = n
		}
	}
	if v := os.Getenv("AFT_OPS_RPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Batch.RPS = f
		}
	}
}
