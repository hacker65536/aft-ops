package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultConfigPath honors XDG_CONFIG_HOME, defaulting to
// ~/.config/aft-ops/config.yaml.
func DefaultConfigPath() string {
	return filepath.Join(configHome(), "aft-ops", "config.yaml")
}

// DefaultCacheDir honors XDG_CACHE_HOME, defaulting to ~/.cache/aft-ops.
func DefaultCacheDir() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "aft-ops")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "aft-ops-cache")
	}
	return filepath.Join(home, ".cache", "aft-ops")
}

// DefaultMetricsDir honors XDG_STATE_HOME, defaulting to
// ~/.local/state/aft-ops/metrics.
func DefaultMetricsDir() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "aft-ops", "metrics")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "aft-ops-metrics")
	}
	return filepath.Join(home, ".local", "state", "aft-ops", "metrics")
}

// ExpandHome resolves a leading ~ to the user's home directory. Paths come
// from a YAML file and from flags, where "~/.aws/my-sso-config" is what a
// person naturally writes but no shell is around to expand it.
func ExpandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

func configHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config")
}

func newBytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
