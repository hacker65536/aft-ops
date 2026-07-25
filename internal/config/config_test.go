package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
