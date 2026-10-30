package service

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/runner"
	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

func inst(name string, mod func(*config.Instance)) config.Instance {
	i := config.NewInstance("lidarr", name, "http://lidarr:1")
	i.APIKeyEnv, i.Missing = "K", 2
	if mod != nil {
		mod(&i)
	}
	return i
}

func settings(dir string, instances ...config.Instance) config.Settings {
	if len(instances) == 0 {
		instances = []config.Instance{inst("lidarr", nil)}
	}
	return config.Settings{Instances: instances, IntervalMinutes: 60, StateDir: dir, LogLevel: "INFO", Web: config.DefaultWeb()}
}

type clients struct {
	mu    sync.Mutex
	made  map[string]*tu.Fake
	calls atomic.Int32 // one per instance per pass
}

func (c *clients) factory(i config.Instance) (runner.Prober, error) {
	c.calls.Add(1)
	if strings.HasPrefix(i.Name, "broken") {
		return nil, config.Errorf("", "%s: environment variable K is empty", i.Name)
	}
	f := tu.NewFake()
	for n := 1; n <= 5; n++ {
		f.Missing = append(f.Missing, tu.A(n))
	}
	f.Queue = 3
	c.mu.Lock()
	defer c.mu.Unlock()
	c.made[i.Name] = f
	return f, nil
}

func newClients() *clients { return &clients{made: map[string]*tu.Fake{}} }

func TestRunPassRecordsActivityStatusAndHeartbeat(t *testing.T) {
	dir := t.TempDir()
	c := newClients()
	s := New(settings(dir, inst("lidarr", func(i *config.Instance) { i.DailyCap = 10 }), inst("broken", nil)), c.factory, nil)
	reports := s.RunPass()
	if !reports[0].OK() || reports[1].OK() {
		t.Fatal(reports)
	}
	if len(c.made["lidarr"].Sent()) != 1 || len(s.Activity.Entries("")) != 1 {
		t.Fatal("one search expected")
	}
	if _, err := os.Stat(filepath.Join(dir, "heartbeat")); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	ok, broken := snap.Instances[0], snap.Instances[1]
	if ok.BudgetUsed != 2 || ok.BudgetCap != 10 || *ok.Report.MissingTotal != 5 || *ok.Report.Queue != 3 || ok.LastPass.IsZero() {
		t.Fatal(ok)
	}
	if snap.LastPass.IsZero() || snap.Running || !strings.Contains(broken.Report.Error, "empty") || strings.Contains(broken.Report.Error, tu.Key) {
		t.Fatal(snap)
	}
}

func TestPassSendsSearchesAndLogsThem(t *testing.T) {
	c := newClients()
	s := New(settings(t.TempDir()), c.factory, nil)
	s.RunPass()
	sent, entries := c.made["lidarr"].Sent(), s.Activity.Entries("lidarr")
	if len(sent) == 0 || len(entries) != len(sent) || entries[0].Cost == 0 {
		t.Fatal(sent, entries)
	}
}

// The loop tests run in a synctest bubble: fake time, and synctest.Wait returns once the
// scheduler goroutine is idle again.

func TestBackgroundLoopFirstPassRunNowAndSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newClients()
		s := New(settings(t.TempDir()), c.factory, nil)
		s.Start()
		defer s.Stop(time.Second)
		passes := func() int32 { synctest.Wait(); return c.calls.Load() }
		if passes() != 1 || s.Snapshot().NextPass.IsZero() {
			t.Fatal("the first pass runs at startup")
		}
		s.RunNow()
		if passes() != 2 {
			t.Fatal("run now")
		}
		time.Sleep(59 * time.Minute)
		if passes() != 2 {
			t.Fatal("too early")
		}
		time.Sleep(2 * time.Minute)
		if passes() != 3 {
			t.Fatal("the scheduled pass")
		}
	})
}

func TestPauseIsPersistedAndRunNowStillWorks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		c := newClients()
		s := New(settings(dir), c.factory, nil)
		if err := s.Pause(); err != nil || !New(settings(dir), c.factory, nil).Paused() {
			t.Fatal("pause must survive a restart", err)
		}
		s.Start()
		defer s.Stop(time.Second)
		synctest.Wait()
		if c.calls.Load() != 0 {
			t.Fatal("the due pass must be skipped while paused")
		}
		if _, err := os.Stat(filepath.Join(dir, "heartbeat")); err != nil {
			t.Fatal("paused, but alive: the heartbeat keeps the healthcheck green")
		}
		if snap := s.Snapshot(); !snap.Paused || !snap.NextPass.IsZero() {
			t.Fatal(snap)
		}
		s.RunNow()
		synctest.Wait()
		if c.calls.Load() != 1 {
			t.Fatal("run now while paused")
		}
		if s.Resume(); New(settings(dir), c.factory, nil).Paused() {
			t.Fatal("resume must persist")
		}
	})
}

func TestLoopSurvivesAPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := tu.CaptureLogs(t)
		var calls atomic.Int32
		s := New(settings(t.TempDir()), func(config.Instance) (runner.Prober, error) {
			calls.Add(1)
			panic("bug")
		}, nil)
		s.Start()
		defer s.Stop(time.Second)
		synctest.Wait()
		s.RunNow()
		synctest.Wait()
		if calls.Load() != 2 || !strings.Contains(logs.String(), "pass failed unexpectedly") {
			t.Fatal(calls.Load(), logs.String())
		}
	})
}

func TestHotReload(t *testing.T) {
	dir := t.TempDir()
	c := newClients()
	var reloaded atomic.Int32
	s := New(settings(dir, inst("a", nil), inst("b", nil)), c.factory, func(config.Settings) { reloaded.Add(1) })
	s.RunPass()
	n := settings(dir, inst("b", nil), inst("c", nil))
	n.IntervalMinutes = 5
	s.UpdateSettings(n)
	snap := s.Snapshot()
	if snap.Instances[0].Name != "b" || snap.Instances[0].Report == nil || snap.Instances[1].Report != nil || snap.IntervalMinutes != 5 {
		t.Fatal(snap)
	}
	s.RunPass()
	if names := slices.Sorted(maps.Keys(c.made)); !slices.Equal(names, []string{"a", "b", "c"}) || reloaded.Load() != 1 {
		t.Fatal(names)
	}
}

func TestCheck(t *testing.T) {
	s := New(settings(t.TempDir(), inst("lidarr", nil), inst("broken", nil)), newClients().factory, nil)
	if !s.Check("lidarr").OK {
		t.Fatal("check")
	}
	if r := s.Check("broken"); r.OK || !strings.Contains(r.Lines[0], "empty") {
		t.Fatal(r)
	}
	if s.Check("nope").OK {
		t.Fatal("unknown")
	}
}

func TestPassesNeverOverlap(t *testing.T) {
	var active, overlaps atomic.Int32
	s := New(settings(t.TempDir()), func(i config.Instance) (runner.Prober, error) {
		if active.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		f := tu.NewFake()
		f.Missing = []apps.Record{tu.A(1)}
		f.Statuses = map[int]string{1: "failed"} // never retired, so every pass searches it
		return f, nil
	}, nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { s.RunPass() })
	}
	wg.Wait()
	if overlaps.Load() != 0 || len(s.Activity.Entries("")) != 4 {
		t.Fatal(overlaps.Load())
	}
}

func TestPauseFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	s := New(settings(filepath.Join(dir, "missing")), newClients().factory, nil)
	if s.Pause() == nil {
		t.Fatal("pausing without a state dir must fail")
	}
	os.MkdirAll(filepath.Join(dir, "missing", "paused", "x"), 0o755) // a directory where the flag file goes
	if s.Resume() == nil {
		t.Fatal("resume failure")
	}
}

func TestDefaultFactoryAndStopWithoutStart(t *testing.T) {
	s := New(settings(t.TempDir(), inst("lidarr", func(i *config.Instance) { i.APIKeyEnv = "UNSET_FOR_TEST" })), nil, nil)
	if r := s.RunPass(); r[0].OK() || !strings.Contains(r[0].Error, "empty") {
		t.Fatal(r[0])
	}
	s.Stop(10 * time.Millisecond) // never started: returns after the timeout
}
