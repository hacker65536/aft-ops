package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain cuts the package off from the developer's real XDG directories.
//
// Load("") falls back to DefaultConfigPath(), so without this a developer who
// actually uses the tool has their own ~/.config/aft-ops/config.yaml decide
// what "the defaults" are — while CI, which has no such file, stays green
// forever and can never catch it. Isolating here rather than per test keeps
// that true for tests added later.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aft-ops-config-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	os.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// write puts a config file in place, failing the test if it cannot.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadDefaultsWithoutFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil {
		t.Fatal("explicit missing path must error")
	}
	// default path missing is fine
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "ap-northeast-1" || cfg.Batch.Concurrency != 10 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

// The other half of the contract TestMain relies on: an empty path really
// does mean DefaultConfigPath(), so isolating XDG_CONFIG_HOME isolates Load.
func TestLoadUsesDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	dir := filepath.Join(home, "aft-ops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "config.yaml"), "region: eu-west-1\n")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "eu-west-1" {
		t.Fatalf("default path not read: region = %q", cfg.Region)
	}
}

func TestLoadFileAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, `
profile: from-file
batch:
  concurrency: 5
  chunk_pause: 30s
`)

	t.Setenv("AFT_OPS_PROFILE", "from-env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "from-env" {
		t.Fatalf("env must beat file: got %q", cfg.Profile)
	}
	if cfg.Batch.Concurrency != 5 {
		t.Fatalf("file value lost: %d", cfg.Batch.Concurrency)
	}
	if cfg.Batch.ChunkPause.D() != 30*time.Second {
		t.Fatalf("duration parse: %v", cfg.Batch.ChunkPause.D())
	}
	// unset keys keep defaults
	if cfg.Release.MaxTargets != 50 {
		t.Fatalf("default lost: %d", cfg.Release.MaxTargets)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, "profle: typo\n")
	if _, err := Load(path); err == nil {
		t.Fatal("unknown keys must be rejected")
	}
}

func TestWriteProfileFallback(t *testing.T) {
	c := Default()
	c.Profile = "read"
	if c.EffectiveWriteProfile() != "read" {
		t.Fatal("fallback to profile")
	}
	c.WriteProfile = "admin"
	if c.EffectiveWriteProfile() != "admin" {
		t.Fatal("explicit write profile")
	}
}

func TestAWSConfigFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "config-sandbox")
	write(t, real, "[profile poc]\nregion = us-east-1\n")
	other := filepath.Join(dir, "other-config")
	write(t, other, "[profile work]\nregion = ap-northeast-1\n")

	path := filepath.Join(dir, "config.yaml")
	write(t, path, "aws_config_file: "+other+"\n")

	// env beats the file, same as every other key
	t.Setenv("AFT_OPS_AWS_CONFIG_FILE", real)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AWSConfigFile != real {
		t.Fatalf("env must beat file: got %q", cfg.AWSConfigFile)
	}

	// Unset means "let the SDK decide": the default must stay empty rather
	// than name a file of our own choosing, or the SDK's AWS_CONFIG_FILE
	// would silently stop working.
	if d := Default(); d.AWSConfigFile != "" {
		t.Fatalf("default must stay empty: %q", d.AWSConfigFile)
	}
}

// A config file that does not exist is caught here rather than surfacing
// from the SDK as "profile not found" — the message that sends the diagnosis
// after the wrong thing entirely.
func TestAWSConfigFileMustExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, "aws_config_file: "+filepath.Join(dir, "nope")+"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("a missing aws_config_file must be rejected")
	}
	if !strings.Contains(err.Error(), "aws_config_file") {
		t.Fatalf("the error must name the key: %v", err)
	}
}

// Validate runs twice (file+env, then again after flags), so it must not
// change the config on the second pass.
func TestValidateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "cfg")
	write(t, real, "")

	c := Default()
	c.AWSConfigFile = real
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	first := c.AWSConfigFile
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.AWSConfigFile != first {
		t.Fatalf("second pass changed the path: %q -> %q", first, c.AWSConfigFile)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := ExpandHome("~/.aws/config"); got != filepath.Join(home, ".aws", "config") {
		t.Fatalf("ExpandHome: %q", got)
	}
	// A ~ that is not a home reference is left alone.
	for _, in := range []string{"/abs/path", "relative/path", "~user/path", ""} {
		if got := ExpandHome(in); got != in {
			t.Fatalf("ExpandHome(%q) = %q, want unchanged", in, got)
		}
	}
}

// A malformed AFT_OPS_* value is an error, not something to step over. It
// used to be assigned only when it parsed, so the run proceeded on the old
// value and never said the request had been dropped.
func TestEnvParseFailuresAreErrors(t *testing.T) {
	cases := []struct{ key, value string }{
		{"AFT_OPS_STATUS_TTL", "1z"},
		{"AFT_OPS_CONCURRENCY", "abc"},
		{"AFT_OPS_RPS", "x"},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			t.Setenv(c.key, c.value)
			// No config file: the env is the only thing under test.
			_, err := Load("")
			if err == nil {
				t.Fatalf("%s=%s should be rejected", c.key, c.value)
			}
			// The message has to be actionable on its own: which variable,
			// what was in it.
			if !strings.Contains(err.Error(), c.key) || !strings.Contains(err.Error(), c.value) {
				t.Errorf("error must name the key and the value: %v", err)
			}
		})
	}
}

// Valid values still apply, so the check above is not passing by rejecting
// everything.
func TestEnvNumericValuesApply(t *testing.T) {
	t.Setenv("AFT_OPS_STATUS_TTL", "0")
	t.Setenv("AFT_OPS_CONCURRENCY", "3")
	t.Setenv("AFT_OPS_RPS", "0")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.StatusTTL.D() != 0 {
		t.Errorf("status_ttl = %v, want 0 (caching disabled)", cfg.Cache.StatusTTL.D())
	}
	if cfg.Batch.Concurrency != 3 {
		t.Errorf("concurrency = %d", cfg.Batch.Concurrency)
	}
	if cfg.Batch.RPS != 0 {
		t.Errorf("rps = %v, want 0 (unlimited)", cfg.Batch.RPS)
	}
}
