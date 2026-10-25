// Package activity is the activity log: one JSON line per search sent, in the state dir,
// size-bounded with one rotated file.
//
// Entries hold only what the runner reports (instance name, wanted kind, command, item titles,
// cost, dry_run), never settings or keys.
package activity

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/cstupi/arr-trickle/internal/runner"
)

const (
	// MaxBytes is per file; with the rotated file, at most ~2 MiB on disk.
	MaxBytes = 1024 * 1024
	// MaxItems is the item titles kept per entry.
	MaxItems = 50
	maxTitle = 200
)

// Entry is one search in the log.
type Entry struct {
	Time     string   `json:"time"`
	Instance string   `json:"instance"`
	Kind     string   `json:"kind"` // "missing" or "cutoff"
	Command  string   `json:"command"`
	Items    []string `json:"items"`
	Cost     int      `json:"cost"`
	DryRun   bool     `json:"dry_run"`
}

// Log is safe for concurrent use.
type Log struct {
	path, rotated string
	maxBytes      int64
	mu            sync.Mutex
}

// New opens (lazily) the log at path.
func New(path string, maxBytes int64) *Log {
	return &Log{path: path, rotated: path + ".1", maxBytes: maxBytes}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Record appends a search. A write failure is logged, not returned: the search was still sent.
func (l *Log) Record(instance string, s runner.Search, now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	items := make([]string, 0, min(len(s.Items), MaxItems)+1)
	for _, it := range s.Items[:min(len(s.Items), MaxItems)] {
		items = append(items, truncate(it, maxTitle))
	}
	if len(s.Items) > MaxItems {
		items = append(items, fmt.Sprintf("(+%d more)", len(s.Items)-MaxItems))
	}
	line, _ := json.Marshal(Entry{
		Time: now.UTC().Format("2006-01-02T15:04:05+00:00"), Instance: instance, Kind: s.Wanted,
		Command: s.Command, Items: items, Cost: s.Cost, DryRun: s.DryRun,
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	if info, err := os.Stat(l.path); err == nil && info.Size()+int64(len(line))+1 > l.maxBytes {
		if err := os.Rename(l.path, l.rotated); err != nil {
			slog.Warn("could not rotate the activity log", "path", l.path, "err", err)
		}
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err == nil {
		_, err = f.Write(append(line, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		slog.Warn("could not write the activity log", "path", l.path, "err", err)
	}
}

func valid(e Entry) bool {
	return e.Time != "" && e.Instance != "" && e.Command != "" && e.Items != nil
}

// Entries is every entry, newest first, optionally only one instance's.
func (l *Log) Entries(instance string) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Entry
	for _, path := range []string{l.rotated, l.path} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && valid(e) && (instance == "" || e.Instance == instance) {
				out = append(out, e)
			}
		}
		f.Close()
	}
	slices.Reverse(out)
	return out
}
