// Package state holds small persistent state: the per-instance daily search budget, a heartbeat
// for health checks, and the paused flag.
package state

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var tmpCounter atomic.Uint64

// AtomicWrite writes via a temp file and rename, so readers never see a partial file. The temp file
// is created with perm, so a secret is never briefly readable by others.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.%d.tmp", filepath.Base(path), os.Getpid(), tmpCounter.Add(1)))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm) // in case the umask stripped bits we asked for
	}
	if err == nil {
		err = rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// rename is os.Rename; a variable so tests can make it fail.
var rename = os.Rename

func utcDay() string { return time.Now().UTC().Format("2006-01-02") }

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
	if path == "" {
		return b
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return b
	}
	var loaded map[string]map[string]int
	if json.Unmarshal(raw, &loaded) != nil {
		slog.Warn(fmt.Sprintf("could not read %s; starting the daily budget fresh", path))
		return b
	}
	for day, counts := range loaded {
		if counts != nil {
			b.data[day] = counts
		}
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
	today := map[string]int{}
	for k, v := range b.data[day] {
		today[k] = v
	}
	today[name] += n
	b.data = map[string]map[string]int{day: today}
	if b.path == "" {
		return nil
	}
	raw, _ := json.Marshal(b.data)
	return AtomicWrite(b.path, raw, 0o644)
}

// Beat records that the scheduler is alive.
func Beat(path string) error {
	return AtomicWrite(path, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
}

// HeartbeatAge is how long ago the last beat was, or false if there was none.
func HeartbeatAge(path string) (time.Duration, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Since(time.Unix(ts, 0)), true
}

// Paused is whether scheduled passes are paused: a file in the state dir, so it survives restarts.
type Paused struct{ Path string }

// Get reports whether passes are paused.
func (p Paused) Get() bool {
	_, err := os.Stat(p.Path)
	return err == nil
}

// Set pauses or resumes.
func (p Paused) Set(paused bool) error {
	if paused {
		return AtomicWrite(p.Path, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
	}
	if err := os.Remove(p.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
