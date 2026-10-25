// Package service is the scheduler: it runs passes in a background goroutine and keeps a status
// snapshot for the web UI.
package service

import (
	"fmt"
	"log/slog"
	"path/filepath"
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
	DryRun          bool
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
	paused    state.Paused
	heartbeat string
	factory   ClientFactory
	onReload  func(config.Settings)

	mu        sync.Mutex // guards the fields below
	settings  config.Settings
	requested bool
	running   bool
	lastTick  time.Time // when the schedule last fired; zero = never
	lastPass  time.Time
	reports   map[string]stamped
	passes    int
	passDone  *sync.Cond

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
	sc := &Scheduler{
		Budget:    state.NewBudget(filepath.Join(s.StateDir, "budget.json")),
		Activity:  activity.New(filepath.Join(s.StateDir, "activity.jsonl"), activity.MaxBytes),
		paused:    state.Paused{Path: filepath.Join(s.StateDir, "paused")},
		heartbeat: filepath.Join(s.StateDir, "heartbeat"),
		factory:   factory,
		onReload:  onReload,
		settings:  s,
		reports:   map[string]stamped{},
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	sc.passDone = sync.NewCond(&sc.mu)
	return sc
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
	names := map[string]bool{}
	for _, i := range n.Instances {
		names[i.Name] = true
	}
	for name := range s.reports {
		if !names[name] {
			delete(s.reports, name)
		}
	}
	s.mu.Unlock()
	if s.onReload != nil {
		s.onReload(n)
	}
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
func (s *Scheduler) Paused() bool { return s.paused.Get() }

// Pause stops scheduled passes (persisted across restarts). Run now still works.
func (s *Scheduler) Pause() error {
	if err := s.paused.Set(true); err != nil {
		return err
	}
	slog.Info("scheduled passes paused")
	return nil
}

// Resume restarts scheduled passes.
func (s *Scheduler) Resume() error {
	if err := s.paused.Set(false); err != nil {
		return err
	}
	slog.Info("scheduled passes resumed")
	s.poke()
	return nil
}

// Check runs the read-only probe for the named instance.
func (s *Scheduler) Check(name string) runner.CheckResult {
	for _, inst := range s.Settings().Instances {
		if inst.Name == name {
			c, err := s.factory(inst)
			if err != nil {
				return runner.Fail(name, err.Error())
			}
			return runner.CheckInstance(inst, c)
		}
	}
	return runner.Fail(name, "no such instance")
}

// RunPass does one pass over every instance now.
func (s *Scheduler) RunPass() []*runner.Report {
	s.passMu.Lock()
	defer s.passMu.Unlock()
	settings := s.Settings()
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	reports := make([]*runner.Report, 0, len(settings.Instances))
	defer func() {
		s.mu.Lock()
		s.running = false
		s.lastPass = time.Now()
		s.passes++
		s.passDone.Broadcast()
		s.mu.Unlock()
		s.beat()
	}()
	for _, inst := range settings.Instances {
		reports = append(reports, s.runOne(inst, settings.DryRun))
	}
	return reports
}

func (s *Scheduler) beat() {
	if err := state.Beat(s.heartbeat); err != nil {
		slog.Warn(fmt.Sprintf("could not write the heartbeat: %s", err))
	}
}

func (s *Scheduler) runOne(inst config.Instance, dryRun bool) *runner.Report {
	var r *runner.Report
	c, err := s.factory(inst)
	if err != nil {
		slog.Warn(fmt.Sprintf("%s: run failed: %s", inst.Name, err))
		r = &runner.Report{Name: inst.Name, Kind: inst.Kind, Error: err.Error()}
	} else {
		r = runner.RunInstance(inst, c, s.Budget, dryRun, time.Time{})
	}
	for _, search := range r.Searches {
		s.Activity.Record(inst.Name, search, time.Time{})
	}
	s.mu.Lock()
	s.reports[inst.Name] = stamped{time.Now(), r}
	s.mu.Unlock()
	return r
}

// WaitForPasses blocks until at least count passes have finished, or timeout (used by tests).
func (s *Scheduler) WaitForPasses(count int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		s.passDone.Broadcast()
		s.mu.Unlock()
	})
	defer timer.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.passes < count {
		if !time.Now().Before(deadline) {
			return false
		}
		s.passDone.Wait()
	}
	return true
}

// nextTick is when the schedule fires next. Never fired: the zero time, i.e. due now.
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
		wait := time.Until(s.nextTick())
		timer := time.NewTimer(max(wait, 0))
		select {
		case <-s.stop:
			timer.Stop()
			return
		case <-s.wake:
		case <-timer.C:
		}
		timer.Stop()
		now := time.Now()
		s.mu.Lock()
		requested := s.requested
		s.requested = false
		s.mu.Unlock()
		due := !now.Before(s.nextTick())
		if due {
			s.mu.Lock()
			s.lastTick = now
			s.mu.Unlock()
		}
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
			slog.Error(fmt.Sprintf("pass failed unexpectedly: %v", p))
		}
	}()
	s.RunPass()
}

// Start runs the loop in a background goroutine.
func (s *Scheduler) Start() { go s.loop() }

// Serve runs the loop in the calling goroutine until Stop (no web UI).
func (s *Scheduler) Serve() { s.loop() }

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
	paused := s.Paused()
	next := s.nextTick()
	s.mu.Lock()
	settings, running, lastPass := s.settings, s.running, s.lastPass
	reports := make(map[string]stamped, len(s.reports))
	for k, v := range s.reports {
		reports[k] = v
	}
	s.mu.Unlock()
	snap := Snapshot{
		Paused: paused, DryRun: settings.DryRun, Running: running,
		IntervalMinutes: settings.IntervalMinutes, LastPass: lastPass,
	}
	if !paused {
		snap.NextPass = next
		if now := time.Now(); next.Before(now) {
			snap.NextPass = now
		}
	}
	for _, i := range settings.Instances {
		st := InstanceStatus{Name: i.Name, Kind: i.Kind, URL: i.URL, BudgetUsed: s.Budget.Used(i.Name), BudgetCap: i.DailyCap}
		if r, ok := reports[i.Name]; ok {
			st.LastPass, st.Report = r.at, r.report
		}
		snap.Instances = append(snap.Instances, st)
	}
	return snap
}
