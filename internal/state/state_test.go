package state

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBudgetPersistsAndDropsOldDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	os.WriteFile(path, []byte(`{"2000-01-01": {"x": 99}}`), 0o644)
	b := NewBudget(path)
	if b.Used("x") != 0 || b.Path() != path {
		t.Fatal("old days don't count")
	}
	if err := b.Add("x", 3); err != nil {
		t.Fatal(err)
	}
	if NewBudget(path).Used("x") != 3 {
		t.Fatal("budget not persisted")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) == "" || strings.Contains(string(raw), "2000-01-01") {
		t.Fatal(string(raw))
	}
}

func TestBudgetSurvivesBadFile(t *testing.T) {
	for _, content := range []string{"not json", "[1, 2]", `{"d": {"x": "nan"}}`} {
		path := filepath.Join(t.TempDir(), "b.json")
		os.WriteFile(path, []byte(content), 0o644)
		if NewBudget(path).Used("x") != 0 {
			t.Fatal(content)
		}
	}
	mem := NewBudget("")
	if mem.Add("x", 1) != nil || mem.Used("x") != 1 {
		t.Fatal("in-memory budget")
	}
}

func TestBudgetIsSafeUnderConcurrentUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := NewBudget(path)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if err := b.Add("x", 1); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if b.Used("x") != 1600 || NewBudget(path).Used("x") != 1600 {
		t.Fatal(b.Used("x"))
	}
}

func TestHeartbeat(t *testing.T) {
	hb := filepath.Join(t.TempDir(), "hb")
	if _, ok := HeartbeatAge(hb); ok {
		t.Fatal("no heartbeat yet")
	}
	Beat(hb)
	if age, ok := HeartbeatAge(hb); !ok || age > 5*time.Second {
		t.Fatal(age)
	}
	os.WriteFile(hb, []byte("garbage"), 0o644)
	if _, ok := HeartbeatAge(hb); ok {
		t.Fatal("garbage is no heartbeat")
	}
	os.WriteFile(hb, []byte(strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)), 0o644)
	if age, _ := HeartbeatAge(hb); age < 59*time.Minute {
		t.Fatal(age)
	}
}

func TestPausedFlag(t *testing.T) {
	p := Paused{filepath.Join(t.TempDir(), "paused")}
	if p.Get() {
		t.Fatal("not paused by default")
	}
	p.Set(true)
	if !p.Get() {
		t.Fatal("paused")
	}
	p.Set(false)
	if p.Get() || p.Set(false) != nil {
		t.Fatal("resumed, and resuming twice is fine")
	}
}

func TestAtomicWriteModeAndCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := AtomicWrite(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	rename = func(string, string) error { return errors.New("disk full") }
	defer func() { rename = os.Rename }()
	if err := AtomicWrite(filepath.Join(dir, "y"), []byte("data"), 0o644); err == nil {
		t.Fatal("expected the failure")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temp file left behind:", entries)
	}
	if AtomicWrite(filepath.Join(dir, "missing", "z"), nil, 0o644) == nil {
		t.Fatal("missing directory")
	}
}
