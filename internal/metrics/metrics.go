// Package metrics records every AWS API call (per attempt, including
// retries) to a JSONL file so that rate limits can be analyzed from real
// measurements and the batch defaults tuned accordingly.
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is one API call attempt.
type Entry struct {
	Time       time.Time `json:"time"`
	Service    string    `json:"service"`
	Operation  string    `json:"operation"`
	DurationMs int64     `json:"duration_ms"`
	Throttled  bool      `json:"throttled,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Recorder appends entries to <dir>/<start-timestamp>.jsonl.
type Recorder struct {
	mu   sync.Mutex
	f    *os.File
	enc  *json.Encoder
	path string
}

// NewRecorder creates the metrics directory and opens a new JSONL file.
func NewRecorder(dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create metrics dir: %w", err)
	}
	path := filepath.Join(dir, time.Now().Format("20060102_150405")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open metrics file: %w", err)
	}
	return &Recorder{f: f, enc: json.NewEncoder(f), path: path}, nil
}

// Record appends one entry. Failures to record are ignored: metrics must
// never break an operation.
func (r *Recorder) Record(e Entry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.enc.Encode(e)
}

// Path returns the JSONL file path.
func (r *Recorder) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// Close flushes and closes the file, removing it when nothing was recorded.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.f.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(r.path); err == nil && st.Size() == 0 {
		_ = os.Remove(r.path)
	}
	return nil
}

// OpStat is an aggregate over one service/operation pair.
type OpStat struct {
	Service    string  `json:"service"`
	Operation  string  `json:"operation"`
	Calls      int     `json:"calls"`
	Errors     int     `json:"errors"`
	Throttles  int     `json:"throttles"`
	AvgMs      int64   `json:"avg_ms"`
	MaxMs      int64   `json:"max_ms"`
	ThrottlePc float64 `json:"throttle_pct"`
}

// Summarize aggregates entries per service/operation.
func Summarize(entries []Entry) []OpStat {
	type acc struct {
		OpStat
		totalMs int64
	}
	m := map[string]*acc{}
	for _, e := range entries {
		k := e.Service + "/" + e.Operation
		a, ok := m[k]
		if !ok {
			a = &acc{OpStat: OpStat{Service: e.Service, Operation: e.Operation}}
			m[k] = a
		}
		a.Calls++
		a.totalMs += e.DurationMs
		if e.DurationMs > a.MaxMs {
			a.MaxMs = e.DurationMs
		}
		if e.Error != "" {
			a.Errors++
		}
		if e.Throttled {
			a.Throttles++
		}
	}
	out := make([]OpStat, 0, len(m))
	for _, a := range m {
		if a.Calls > 0 {
			a.AvgMs = a.totalMs / int64(a.Calls)
			a.ThrottlePc = float64(a.Throttles) / float64(a.Calls) * 100
		}
		out = append(out, a.OpStat)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Operation < out[j].Operation
	})
	return out
}

// ReadFile parses a JSONL metrics file.
func ReadFile(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	dec := json.NewDecoder(newReader(data))
	for dec.More() {
		var e Entry
		if err := dec.Decode(&e); err != nil {
			break // tolerate a truncated tail
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// LatestFiles returns up to n metrics file paths, newest first.
func LatestFiles(dir string, n int) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths))) // timestamp-named
	if n > 0 && len(paths) > n {
		paths = paths[:n]
	}
	return paths, nil
}
