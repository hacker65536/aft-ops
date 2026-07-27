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
	// AWSConfigFile pins which shared config file the profiles above are
	// looked up in. Operators who keep one file per AWS organization switch
	// between them with AWS_CONFIG_FILE, which leaves a configured profile
	// name pointing at a file that may not define it. Setting this ties the
	// profile to the file that defines it, so the pair travels together.
	//
	// Empty (the default) leaves the SDK's own resolution alone: AWS_CONFIG_FILE
	// if set, otherwise ~/.aws/config. A value here overrides both.
	AWSConfigFile string `yaml:"aws_config_file"`

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
	// ExecutionsTTL bounds how long one pipeline's cached execution history
	// (the TUI executions screen) is served before a refetch. A history whose
	// head execution is in-flight is always refetched, and the screen's r key
	// forces one. AFT pipelines are mostly idle, so a generous TTL saves
	// round-trips without staleness in practice. 0 disables the memo.
	ExecutionsTTL Duration `yaml:"executions_ttl"`
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
	// KeepRuns bounds how many per-run JSONL files are retained; older ones
	// are pruned at startup. 0 keeps everything.
	KeepRuns int `yaml:"keep_runs"`
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
			Dir:           DefaultCacheDir(),
			AccountTTL:    Duration(24 * time.Hour),
			PipelineTTL:   Duration(6 * time.Hour),
			StatusTTL:     Duration(10 * time.Minute),
			ExecutionsTTL: Duration(15 * time.Minute),
		},
		Release: Release{
			MaxTargets:     50,
			SkipInProgress: true,
		},
		TUI:     TUI{PollInterval: Duration(30 * time.Second)},
		Metrics: Metrics{Enabled: true, Dir: DefaultMetricsDir(), KeepRuns: 100},
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

	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate checks the merged configuration and normalizes the paths it can
// (a leading ~). It runs once here over file+env and again in the CLI layer
// once flags are merged on top — flags are applied after Load returns, so a
// single pass would let `--concurrency 0` or a nonexistent --aws-config-file
// straight through. It is therefore idempotent by construction.
func (c *Config) Validate() error {
	if c.AWSConfigFile != "" {
		c.AWSConfigFile = ExpandHome(c.AWSConfigFile)
		// Check here rather than letting the SDK find out: a missing config
		// file surfaces from the SDK as "profile not found", which sends the
		// diagnosis after the wrong thing entirely.
		if _, err := os.Stat(c.AWSConfigFile); err != nil {
			return fmt.Errorf("aws_config_file %q is not readable: %w", c.AWSConfigFile, err)
		}
	}
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

// applyEnv overlays the AFT_OPS_* environment on top of the file.
//
// A value that will not parse is an error, not something to step over. The
// numeric keys used to assign only when err == nil, so AFT_OPS_RPS=x ran at
// the configured rate and said nothing — the operator gets the behavior they
// asked to change, with no sign their request was ever read.
func applyEnv(c *Config) error {
	if v := os.Getenv("AFT_OPS_PROFILE"); v != "" {
		c.Profile = v
	}
	if v := os.Getenv("AFT_OPS_WRITE_PROFILE"); v != "" {
		c.WriteProfile = v
	}
	if v := os.Getenv("AFT_OPS_REGION"); v != "" {
		c.Region = v
	}
	if v := os.Getenv("AFT_OPS_AWS_CONFIG_FILE"); v != "" {
		c.AWSConfigFile = v
	}
	if v := os.Getenv("AFT_OPS_ACCOUNT_SOURCE"); v != "" {
		c.AccountSource = AccountSource(v)
	}
	if v := os.Getenv("AFT_OPS_CACHE_DIR"); v != "" {
		c.Cache.Dir = v
	}
	if v := os.Getenv("AFT_OPS_STATUS_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return envErr("AFT_OPS_STATUS_TTL", v, "a duration such as 10m or 0")
		}
		c.Cache.StatusTTL = Duration(d)
	}
	if v := os.Getenv("AFT_OPS_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return envErr("AFT_OPS_CONCURRENCY", v, "an integer")
		}
		c.Batch.Concurrency = n
	}
	if v := os.Getenv("AFT_OPS_RPS"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return envErr("AFT_OPS_RPS", v, "a number, 0 for unlimited")
		}
		c.Batch.RPS = f
	}
	return nil
}

// envErr names the variable, what was in it, and what was expected. The
// wrapped parse error adds nothing an operator can act on ("invalid syntax"),
// so it is dropped in favor of saying what a valid value looks like.
func envErr(key, value, want string) error {
	return fmt.Errorf("invalid %s=%q: want %s", key, value, want)
}
