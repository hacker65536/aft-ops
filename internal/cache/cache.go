// Package cache is a TTL'd JSON file cache for slowly-changing data
// (account maps, pipeline inventories). Execution statuses are never
// cached — freshness there is the whole point of the tool.
//
// Layout: <base>/<scope>/<key>.json where scope isolates AWS profiles so
// that production and PoC orgs can never serve each other's data.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const schemaVersion = 1

// Store is a cache scoped to one AWS profile/region pair.
type Store struct {
	dir string
}

// New returns a Store rooted at base and scoped by profile+region.
func New(base, profile, region string) Store {
	scope := sanitize(profile) + "_" + sanitize(region)
	if len(scope) > 80 { // SSO profile names can be long
		sum := sha256.Sum256([]byte(scope))
		scope = scope[:64] + "-" + hex.EncodeToString(sum[:6])
	}
	return Store{dir: filepath.Join(base, scope)}
}

func (s Store) Dir() string { return s.dir }

type envelope struct {
	SchemaVersion int             `json:"schema_version"`
	FetchedAt     time.Time       `json:"fetched_at"`
	Data          json.RawMessage `json:"data"`
}

// Get returns the cached value for key when present, schema-compatible,
// and younger than ttl. The second return is the fetch time (for "cached
// 3h ago" displays).
func Get[T any](s Store, key string, ttl time.Duration) (T, time.Time, bool) {
	var zero T
	data, err := os.ReadFile(s.path(key))
	if err != nil {
		return zero, time.Time{}, false
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil || env.SchemaVersion != schemaVersion {
		return zero, time.Time{}, false
	}
	if time.Since(env.FetchedAt) > ttl {
		return zero, time.Time{}, false
	}
	var v T
	if err := json.Unmarshal(env.Data, &v); err != nil {
		return zero, time.Time{}, false
	}
	return v, env.FetchedAt, true
}

// Put stores v under key. Cache write failures are returned but callers
// may treat them as non-fatal (the fetched data is still usable).
func Put[T any](s Store, key string, v T) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	env := envelope{SchemaVersion: schemaVersion, FetchedAt: time.Now(), Data: raw}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp := s.path(key) + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(key))
}

// Invalidate removes one key (missing is fine).
func (s Store) Invalidate(key string) error {
	err := os.Remove(s.path(key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Info describes one cache entry for `cache status`.
type Info struct {
	Key       string    `json:"key"`
	FetchedAt time.Time `json:"fetched_at"`
	SizeBytes int64     `json:"size_bytes"`
}

// Entries lists all entries in this scope.
func (s Store) Entries() ([]Info, error) {
	paths, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var infos []Info
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		info := Info{Key: strings.TrimSuffix(filepath.Base(p), ".json"), SizeBytes: st.Size()}
		if data, err := os.ReadFile(p); err == nil {
			var env envelope
			if json.Unmarshal(data, &env) == nil {
				info.FetchedAt = env.FetchedAt
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// Clear removes every entry in this scope.
func (s Store) Clear() error {
	paths, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}

func (s Store) path(key string) string {
	return filepath.Join(s.dir, sanitize(key)+".json")
}

func sanitize(s string) string {
	repl := strings.NewReplacer("/", "_", ":", "_", " ", "_", "\\", "_")
	return repl.Replace(s)
}
