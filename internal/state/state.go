// Package state holds small persistent state: the per-instance daily search budget, a heartbeat
// for health checks, and the paused flag.
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rename is os.Rename; a variable so tests can make it fail.
var rename = os.Rename

// AtomicWrite writes via a temp file and rename, so readers never see a partial file. The temp file
// is created 0600 and only then given perm, so a secret is never briefly readable by others.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	err = errors.Join(err, f.Close(), os.Chmod(f.Name(), perm))
	if err == nil {
		err = rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func utcDay() string { return time.Now().UTC().Format(time.DateOnly) }

// Budget counts searches sent per instance per UTC day. It survives restarts, so a crash loop can't
// overspend, and it is safe to share between the scheduler and the web UI.
type Budget struct {
	path string
	mu   sync.Mutex
	data map[string]map[string]int
}

// NewBudget loads the budget file at path ("" keeps it in memory only).
func NewBudget(path string) *Budget {
	b := &Budget{path: path, data: map[string]map[string]int{}}
	if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &b.data) != nil {
		slog.Warn("could not read the daily budget; starting it fresh", "path", path)
		b.data = map[string]map[string]int{}
	}
	return b
}

// Path is where the budget is stored.
func (b *Budget) Path() string { return b.path }

// Used is how many searches name has sent today.
func (b *Budget) Used(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data[utcDay()][name]
}

// Add records n more searches for name today and persists the budget. Older days are dropped.
func (b *Budget) Add(name string, n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	day := utcDay()
	today := maps.Clone(b.data[day])
	if today == nil {
		today = map[string]int{}
	}
	today[name] += n
	b.data = map[string]map[string]int{day: today}
	if b.path == "" {
		return nil
	}
	raw, _ := json.Marshal(b.data)
	return AtomicWrite(b.path, raw, 0o644)
}

func stamp() []byte { return strconv.AppendInt(nil, time.Now().Unix(), 10) }

// Beat records that the scheduler is alive.
func Beat(path string) error { return AtomicWrite(path, stamp(), 0o644) }

// HeartbeatAge is how long ago the last beat was, or false if there was none.
func HeartbeatAge(path string) (time.Duration, bool) {
	raw, err := os.ReadFile(path)
	ts, perr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || perr != nil {
		return 0, false
	}
	return time.Since(time.Unix(ts, 0)), true
}

// Flag is an on/off setting kept as the presence of a file, so it survives restarts (e.g. "paused").
type Flag string

// IsSet reports whether the flag is on.
func (f Flag) IsSet() bool {
	_, err := os.Stat(string(f))
	return err == nil
}

// Set turns the flag on or off.
func (f Flag) Set(on bool) error {
	if on {
		return AtomicWrite(string(f), stamp(), 0o644)
	}
	if err := os.Remove(string(f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
