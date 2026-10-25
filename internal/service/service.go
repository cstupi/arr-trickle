// Package service is the scheduler: it runs passes in a background goroutine and keeps a status
// snapshot for the web UI.
package service

import (
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/cstupi/arr-trickle/internal/activity"
	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/runner"
	"github.com/cstupi/arr-trickle/internal/state"
)

// ClientFactory builds the client for an instance (runner.MakeClient, or a fake in tests).
type ClientFactory func(config.Instance) (runner.Prober, error)

// InstanceStatus is one row of the dashboard.
type InstanceStatus struct {
	Name, Kind, URL string
	BudgetUsed      int
	BudgetCap       int // 0 = no cap
	LastPass        time.Time
	Report          *runner.Report // from the last pass; nil before the first
}

// Snapshot is the scheduler's state at one moment.
type Snapshot struct {
	Paused          bool
	Running         bool // a pass is in progress
	IntervalMinutes int
	LastPass        time.Time
	NextPass        time.Time // zero while paused
	Instances       []InstanceStatus
}

type stamped struct {
	at     time.Time
	report *runner.Report
}

// Scheduler is safe for concurrent use.
type Scheduler struct {
	Budget    *state.Budget
	Activity  *activity.Log
	paused    state.Flag
	heartbeat string
	factory   ClientFactory
	onReload  func(config.Settings)

	mu        sync.Mutex // guards the fields below
	settings  config.Settings
	requested bool
	running   bool
	lastTick  time.Time // when the schedule last fired; zero = never, so the first pass runs at startup
	lastPass  time.Time
	reports   map[string]stamped

	passMu sync.Mutex // one pass at a time
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

// New builds a scheduler. onReload (may be nil) is called after settings are hot-swapped.
func New(s config.Settings, factory ClientFactory, onReload func(config.Settings)) *Scheduler {
	if factory == nil {
		factory = runner.MakeClient
	}
	if onReload == nil {
		onReload = func(config.Settings) {}
	}
	return &Scheduler{
		Budget:    state.NewBudget(filepath.Join(s.StateDir, "budget.json")),
		Activity:  activity.New(filepath.Join(s.StateDir, "activity.jsonl"), activity.MaxBytes),
		paused:    state.Flag(filepath.Join(s.StateDir, "paused")),
		heartbeat: filepath.Join(s.StateDir, "heartbeat"),
		factory:   factory,
		onReload:  onReload,
		settings:  s,
		reports:   map[string]stamped{},
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Settings is the current settings.
func (s *Scheduler) Settings() config.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// UpdateSettings hot-swaps settings (after a save in the UI); the next pass uses them.
func (s *Scheduler) UpdateSettings(n config.Settings) {
	s.mu.Lock()
	s.settings = n
	maps.DeleteFunc(s.reports, func(name string, _ stamped) bool {
		return !slices.ContainsFunc(n.Instances, func(i config.Instance) bool { return i.Name == name })
	})
	s.mu.Unlock()
	s.onReload(n)
	s.poke() // recompute the next pass time for a changed interval
}

// RunNow runs a pass as soon as possible, even while paused.
func (s *Scheduler) RunNow() {
	s.mu.Lock()
	s.requested = true
	s.mu.Unlock()
	s.poke()
}

// Paused reports whether scheduled passes are paused.
func (s *Scheduler) Paused() bool { return s.paused.IsSet() }

// Pause stops scheduled passes (persisted across restarts). Run now still works.
func (s *Scheduler) Pause() error { return s.setPaused(true) }

// Resume restarts scheduled passes.
func (s *Scheduler) Resume() error { return s.setPaused(false) }

func (s *Scheduler) setPaused(on bool) error {
	if err := s.paused.Set(on); err != nil {
		return err
	}
	slog.Info("scheduled passes", "paused", on)
	s.poke()
	return nil
}

// Check runs the read-only probe for the named instance.
func (s *Scheduler) Check(name string) runner.CheckResult {
	i := slices.IndexFunc(s.Settings().Instances, func(i config.Instance) bool { return i.Name == name })
	if i < 0 {
		return runner.Fail(name, "no such instance")
	}
	inst := s.Settings().Instances[i]
	c, err := s.factory(inst)
	if err != nil {
		return runner.Fail(name, err.Error())
	}
	return runner.CheckInstance(inst, c)
}

// RunPass does one pass over every instance now.
func (s *Scheduler) RunPass() []*runner.Report {
	s.passMu.Lock()
	defer s.passMu.Unlock()
	s.mu.Lock()
	settings := s.settings
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running, s.lastPass = false, time.Now()
		s.mu.Unlock()
		s.beat()
	}()
	reports := make([]*runner.Report, 0, len(settings.Instances))
	for _, inst := range settings.Instances {
		reports = append(reports, s.runOne(inst))
	}
	return reports
}

func (s *Scheduler) beat() {
	if err := state.Beat(s.heartbeat); err != nil {
		slog.Warn("could not write the heartbeat", "err", err)
	}
}

func (s *Scheduler) runOne(inst config.Instance) *runner.Report {
	r := &runner.Report{Name: inst.Name, Kind: inst.Kind}
	if c, err := s.factory(inst); err != nil {
		slog.Warn("run failed", "instance", inst.Name, "err", err)
		r.Error = err.Error()
	} else {
		r = runner.RunInstance(inst, c, s.Budget, time.Time{})
	}
	for _, search := range r.Searches {
		s.Activity.Record(inst.Name, search, time.Time{})
	}
	s.mu.Lock()
	s.reports[inst.Name] = stamped{time.Now(), r}
	s.mu.Unlock()
	return r
}

// nextTick is when the schedule fires next (the zero time if it never has: due now).
func (s *Scheduler) nextTick() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastTick.IsZero() {
		return time.Time{}
	}
	return s.lastTick.Add(time.Duration(s.settings.IntervalMinutes) * time.Minute)
}

func (s *Scheduler) loop() {
	defer close(s.done)
	for {
		timer := time.NewTimer(max(time.Until(s.nextTick()), 0))
		select {
		case <-s.stop:
			timer.Stop()
			return
		case <-s.wake:
		case <-timer.C:
		}
		timer.Stop()
		now, next := time.Now(), s.nextTick()
		s.mu.Lock()
		requested, due := s.requested, !now.Before(next)
		s.requested = false
		if due {
			s.lastTick = now
		}
		s.mu.Unlock()
		switch {
		case requested || (due && !s.Paused()):
			s.safePass()
		case due:
			s.beat() // paused, but alive: the healthcheck stays green
		}
	}
}

func (s *Scheduler) safePass() {
	defer func() { // keep the scheduler alive whatever happens in one pass
		if p := recover(); p != nil {
			slog.Error("pass failed unexpectedly", "panic", p)
		}
	}()
	s.RunPass()
}

// Start runs the loop in a background goroutine.
func (s *Scheduler) Start() { go s.loop() }

// Stop ends the loop and waits for it (up to timeout).
func (s *Scheduler) Stop(timeout time.Duration) {
	close(s.stop)
	select {
	case <-s.done:
	case <-time.After(timeout):
	}
}

// Snapshot is a consistent copy of the scheduler's status.
func (s *Scheduler) Snapshot() Snapshot {
	paused, next := s.Paused(), s.nextTick()
	s.mu.Lock()
	settings, reports := s.settings, maps.Clone(s.reports)
	snap := Snapshot{Paused: paused, Running: s.running, IntervalMinutes: settings.IntervalMinutes, LastPass: s.lastPass}
	s.mu.Unlock()
	if !paused {
		snap.NextPass = time.Now()
		if next.After(snap.NextPass) {
			snap.NextPass = next
		}
	}
	for _, i := range settings.Instances {
		st := InstanceStatus{Name: i.Name, Kind: i.Kind, URL: i.URL, BudgetUsed: s.Budget.Used(i.Name), BudgetCap: i.DailyCap}
		st.LastPass, st.Report = reports[i.Name].at, reports[i.Name].report
		snap.Instances = append(snap.Instances, st)
	}
	return snap
}
