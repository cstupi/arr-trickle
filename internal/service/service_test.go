package service

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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
	mu   sync.Mutex
	made map[string]*tu.Fake
}

func (c *clients) factory(i config.Instance) (runner.Prober, error) {
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
	if snap.LastPass.IsZero() || snap.Running || !strings.Contains(broken.Report.Error, "empty") {
		t.Fatal(snap)
	}
}

func TestDryRunPassIsLoggedAsDryRun(t *testing.T) {
	c := newClients()
	st := settings(t.TempDir())
	st.DryRun = true
	s := New(st, c.factory, nil)
	s.RunPass()
	if len(c.made["lidarr"].Sent()) != 0 || !s.Activity.Entries("")[0].DryRun || !s.Snapshot().DryRun {
		t.Fatal("dry run")
	}
}

func TestBackgroundLoopFirstPassAndRunNow(t *testing.T) {
	s := New(settings(t.TempDir()), newClients().factory, nil)
	s.Start()
	defer s.Stop(5 * time.Second)
	if !s.WaitForPasses(1, 5*time.Second) || s.Snapshot().NextPass.IsZero() {
		t.Fatal("first pass")
	}
	s.RunNow()
	if !s.WaitForPasses(2, 5*time.Second) || len(s.Activity.Entries("")) != 2 {
		t.Fatal("run now")
	}
}

func TestPauseIsPersistedAndRunNowStillWorks(t *testing.T) {
	dir := t.TempDir()
	c := newClients()
	s := New(settings(dir), c.factory, nil)
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	if !New(settings(dir), c.factory, nil).Paused() {
		t.Fatal("pause must survive a restart")
	}
	s.Start()
	defer s.Stop(5 * time.Second)
	if s.WaitForPasses(1, 500*time.Millisecond) {
		t.Fatal("the due pass must be skipped while paused")
	}
	if _, err := os.Stat(filepath.Join(dir, "heartbeat")); err != nil {
		t.Fatal("paused, but alive: the heartbeat keeps the healthcheck green")
	}
	if snap := s.Snapshot(); !snap.Paused || !snap.NextPass.IsZero() {
		t.Fatal(snap)
	}
	s.RunNow()
	if !s.WaitForPasses(1, 5*time.Second) {
		t.Fatal("run now while paused")
	}
	s.Resume()
	if New(settings(dir), c.factory, nil).Paused() {
		t.Fatal("resume must persist")
	}
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
	var names []string
	for k := range c.made {
		names = append(names, k)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"a", "b", "c"}) || reloaded.Load() != 1 {
		t.Fatal(names)
	}
}

func TestLoopSurvivesAPanic(t *testing.T) {
	logs := tu.CaptureLogs(t)
	var calls atomic.Int32
	s := New(settings(t.TempDir()), func(config.Instance) (runner.Prober, error) {
		calls.Add(1)
		panic("bug")
	}, nil)
	s.Start()
	defer s.Stop(5 * time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s.RunNow()
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 2 || !strings.Contains(logs.String(), "pass failed unexpectedly") {
		t.Fatal(calls.Load(), logs.String())
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
		return f, nil
	}, nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); s.RunPass() }()
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
