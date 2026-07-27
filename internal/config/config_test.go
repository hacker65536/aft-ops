package config

import (
	"fmt"
	"os"
	"path/filepath"
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
