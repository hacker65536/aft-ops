// Package config loads tool configuration with the precedence:
// flags > environment (AFT_OPS_*) > config file (YAML) > defaults.
// Flags are applied by the CLI layer on top of the Config returned here.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"strconv"
	"strings"
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
	Trigger Trigger `yaml:"trigger"`
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
	// TriggerTTL bounds how long a cached pipeline trigger is served before
	// `pipeline triggers` refetches it. Pipeline definitions change only when
	// something rewrites them (aft-create-pipeline, an out-of-band update), so
	// a generous TTL costs little — but this is a drift report, and a cached
	// answer is only honest because the freshness line says so. 0 disables the
	// cache (always fan out).
	TriggerTTL Duration `yaml:"trigger_ttl"`
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

// Trigger describes the push trigger an AFT account customizations pipeline
// is expected to carry, which `pipeline triggers` compares reality against.
//
// There is no per-account setting here on purpose. FilePathTemplate is
// expanded with the account's own account_customizations_name from AFT's
// metadata table, so a fleet of several hundred pipelines is covered by three
// lines instead of several hundred — and the expectation cannot drift away
// from what AFT itself recorded. The defaults are the shape AFT's
// customizations repository layout implies.
type Trigger struct {
	SourceAction     string `yaml:"source_action"`
	Branch           string `yaml:"branch"`
	FilePathTemplate string `yaml:"file_path_template"`
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
			TriggerTTL:    Duration(time.Hour),
			ExecutionsTTL: Duration(15 * time.Minute),
		},
		Release: Release{
			MaxTargets:     50,
			SkipInProgress: true,
		},
		Trigger: Trigger{
			SourceAction:     "aft-account-customizations",
			Branch:           "main",
			FilePathTemplate: "{customizations_name}/terraform/*.tf",
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
	// An empty trigger key cannot be judged against: it makes every pipeline
	// report "unknown", which reads as a broken tool rather than as the
	// unset value it is. All three default to non-empty, so reaching this
	// means someone blanked one out.
	for _, kv := range []struct{ key, value string }{
		{"trigger.source_action", c.Trigger.SourceAction},
		{"trigger.branch", c.Trigger.Branch},
		{"trigger.file_path_template", c.Trigger.FilePathTemplate},
	} {
		if strings.TrimSpace(kv.value) == "" {
			return fmt.Errorf("%s must not be empty", kv.key)
		}
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

// EnvPrefix starts the environment name of every configuration key.
const EnvPrefix = "AFT_OPS_"

// EnvName returns the variable that overrides one configuration key, derived
// from its YAML path: "cache.status_ttl" → "AFT_OPS_CACHE_STATUS_TTL".
//
// The mapping is a rule rather than a list on purpose. It used to be a
// hand-written sequence of nine lookups with names that had drifted from the
// keys they set (batch.concurrency was AFT_OPS_CONCURRENCY, cache.status_ttl
// was AFT_OPS_STATUS_TTL, but cache.dir was AFT_OPS_CACHE_DIR), while the
// other thirteen keys had no variable at all — even though the documented
// precedence promised one for everything. Deriving the name means a new
// field is overridable the moment it is declared, and the documentation
// stays true without anyone maintaining it.
//
// AFT_OPS_DEMO and AFT_OPS_DEMO_LATENCY are not covered here: they select a
// test fixture rather than set a configuration key.
func EnvName(yamlPath string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(yamlPath, ".", "_"))
}

// applyEnv overlays the AFT_OPS_* environment on top of the file, one
// variable per configuration key.
//
// A value that will not parse is an error, not something to step over. The
// numeric keys used to assign only when err == nil, so AFT_OPS_RPS=x ran at
// the configured rate and said nothing — the operator gets the behavior they
// asked to change, with no sign their request was ever read.
func applyEnv(c *Config) error {
	return walkFields(reflect.ValueOf(c).Elem(), "", func(path string, f reflect.Value) error {
		key := EnvName(path)
		v := os.Getenv(key)
		if v == "" {
			return nil
		}
		return setFromString(f, key, v)
	})
}

// walkFields visits every leaf field of a config struct, passing its dotted
// YAML path. Nested structs are descended into; every other type is a leaf
// (Duration is one despite wrapping an integer).
func walkFields(v reflect.Value, prefix string, fn func(path string, f reflect.Value) error) error {
	t := v.Type()
	for i := range t.NumField() {
		sf := t.Field(i)
		tag, _, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		f := v.Field(i)
		if f.Kind() == reflect.Struct && f.Type() != reflect.TypeOf(Duration(0)) {
			if err := walkFields(f, path, fn); err != nil {
				return err
			}
			continue
		}
		if err := fn(path, f); err != nil {
			return err
		}
	}
	return nil
}

var durationType = reflect.TypeOf(Duration(0))

// setFromString parses one environment value into one config field. An
// unhandled kind is a programming error rather than an operator error: a new
// field type has to be taught here before it can be set from the environment,
// and failing loudly is what keeps EnvName's promise honest.
func setFromString(f reflect.Value, key, raw string) error {
	if f.Type() == durationType {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return envErr(key, raw, "a duration such as 10m or 0")
		}
		f.SetInt(int64(d))
		return nil
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString(raw)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return envErr(key, raw, "an integer")
		}
		f.SetInt(n)
	case reflect.Float64:
		x, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return envErr(key, raw, "a number")
		}
		f.SetFloat(x)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return envErr(key, raw, "true or false")
		}
		f.SetBool(b)
	default:
		return fmt.Errorf("config: %s cannot be set from the environment (%s)", key, f.Kind())
	}
	return nil
}

// envErr names the variable, what was in it, and what was expected. The
// wrapped parse error adds nothing an operator can act on ("invalid syntax"),
// so it is dropped in favor of saying what a valid value looks like.
func envErr(key, value, want string) error {
	return fmt.Errorf("invalid %s=%q: want %s", key, value, want)
}
