package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	os.WriteFile(path, []byte(`
profile: from-file
batch:
  concurrency: 5
  chunk_pause: 30s
`), 0o644)

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
	os.WriteFile(path, []byte("profle: typo\n"), 0o644)
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
