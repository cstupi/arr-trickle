package service

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/requests"
	"github.com/cstupi/arr-trickle/internal/runner"
	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

type oneSource struct {
	calls *atomic.Int32
	reqs  []requests.Request
}

func (o oneSource) Name() string { return "seerr" }
func (o oneSource) Requests(time.Time) ([]requests.Request, error) {
	o.calls.Add(1)
	return o.reqs, nil
}
func (o oneSource) Defaults() (requests.Defaults, error) {
	return requests.Defaults{MovieProfile: 4, MovieRoot: "/data/movie"}, nil
}

// radarrFake answers the calls request sync makes: nothing in the library, so every movie is added.
func radarrFake(added *atomic.Int32) *tu.Fake {
	f := tu.For("radarr")
	f.RequestFunc = func(method, path string, data any) (any, error) {
		switch {
		case method == http.MethodPost:
			added.Add(1)
			return data, nil
		case strings.HasPrefix(path, "/movie/lookup"):
			return map[string]any{"title": "The Odyssey", "year": float64(2026)}, nil
		}
		return []any{}, nil
	}
	return f
}

func syncSettings(dir string, instances ...config.Instance) config.Settings {
	s := settings(dir, instances...)
	s.Requests = config.DefaultRequests()
	s.Requests.Enabled = true
	s.Requests.Apps = map[string]config.RequestApp{"seerr": {URL: "http://seerr:5055", KeyEnv: "K"}}
	return s
}

func radarrInst() config.Instance {
	i := config.NewInstance("radarr", "radarr", "http://radarr:7878")
	i.APIKeyEnv = "K"
	return i
}

func newSyncScheduler(t *testing.T, s config.Settings, added, polled *atomic.Int32) *Scheduler {
	t.Helper()
	sched := New(s, func(i config.Instance) (runner.Prober, error) {
		if strings.HasPrefix(i.Name, "broken") {
			return nil, errors.New("broken: no key")
		}
		return radarrFake(added), nil
	}, nil)
	req := requests.Request{Source: "seerr", Kind: "movie", TMDB: 1, Created: time.Now().Add(-time.Hour)}
	sched.Sources = func(config.Requests, string) (requests.Source, error) {
		return oneSource{polled, []requests.Request{req}}, nil
	}
	return sched
}

func TestRunSyncAddsRecordsActivityAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	var added, polled atomic.Int32
	s := newSyncScheduler(t, syncSettings(dir, radarrInst()), &added, &polled)
	r := s.RunSync()
	if !r.OK() || len(r.Added) != 1 || added.Load() != 1 {
		t.Fatalf("%+v", r)
	}
	entries := s.Activity.Entries("radarr")
	if len(entries) != 1 || entries[0].Kind != "request" || entries[0].Command != "Added from seerr" || entries[0].Items[0] != "The Odyssey (2026)" {
		t.Fatal(entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "readded.json")); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot().Requests
	if !snap.Enabled || snap.Running || snap.Added != 1 || snap.Last != r || len(snap.Apps) != 1 || snap.NextSync.IsZero() {
		t.Fatalf("%+v", snap)
	}
	if r := s.RunSync(); len(r.Added) != 0 || added.Load() != 1 {
		t.Fatal("a request is added once")
	}
}

type noRequests struct{ runner.Prober }

func TestRunSyncReportsSetupProblems(t *testing.T) {
	var added, polled atomic.Int32
	broken := radarrInst()
	broken.Name = "broken"
	s := newSyncScheduler(t, syncSettings(t.TempDir(), broken), &added, &polled)
	s.Sources = func(config.Requests, string) (requests.Source, error) {
		return nil, errors.New("seerr: environment variable K is empty")
	}
	r := s.RunSync()
	if len(r.Errors) != 2 || !strings.Contains(r.Errors[0], "broken: no key") || !strings.Contains(r.Errors[1], "is empty") {
		t.Fatalf("%+v", r)
	}
	s = New(syncSettings(t.TempDir(), radarrInst()), func(config.Instance) (runner.Prober, error) { return noRequests{tu.For("radarr")}, nil }, nil)
	s.Sources = func(config.Requests, string) (requests.Source, error) { return oneSource{&polled, nil}, nil }
	if r := s.RunSync(); len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "cannot add") {
		t.Fatalf("%+v", r)
	}
}

func TestMakeSource(t *testing.T) {
	r := config.DefaultRequests()
	r.Apps = map[string]config.RequestApp{"seerr": {URL: "http://s", KeyEnv: "ARR_TRICKLE_TEST_SEERR"}, "ombi": {URL: "http://o", KeyEnv: "ARR_TRICKLE_TEST_OMBI"}}
	t.Setenv("ARR_TRICKLE_TEST_SEERR", "fakeseerrfakeseerrfakeseerrfakeseerr==")
	t.Setenv("ARR_TRICKLE_TEST_OMBI", "fakeombifakeombifakeombifakeombi")
	for _, app := range []string{"seerr", "ombi"} {
		src, err := MakeSource(r, app)
		if err != nil || src.Name() != app {
			t.Fatal(app, src, err)
		}
	}
	t.Setenv("ARR_TRICKLE_TEST_OMBI", "")
	if _, err := MakeSource(r, "ombi"); err == nil {
		t.Fatal("expected a key error")
	}
}

func TestSyncSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var added, polled atomic.Int32
		st := syncSettings(t.TempDir(), radarrInst())
		st.Requests.IntervalMinutes = 30
		s := newSyncScheduler(t, st, &added, &polled)
		s.Start()
		defer s.Stop(time.Second)
		syncs := func() int32 { synctest.Wait(); return polled.Load() }
		if syncs() != 1 {
			t.Fatal("the first sync runs at startup")
		}
		time.Sleep(29 * time.Minute)
		if syncs() != 1 {
			t.Fatal("too early")
		}
		time.Sleep(2 * time.Minute)
		if syncs() != 2 {
			t.Fatal("the scheduled sync")
		}
		if err := s.Pause(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Minute)
		if syncs() != 2 || !s.Snapshot().Requests.NextSync.IsZero() {
			t.Fatal("paused: no scheduled sync")
		}
		s.SyncNow()
		if syncs() != 3 {
			t.Fatal("sync now works while paused")
		}
		s.Resume()
		off := s.Settings()
		off.Requests.Enabled = false
		s.UpdateSettings(off)
		time.Sleep(3 * time.Hour)
		s.SyncNow()
		if syncs() != 3 {
			t.Fatal("off: no sync at all")
		}
	})
}

func TestSyncPanicIsContained(t *testing.T) {
	var added, polled atomic.Int32
	s := newSyncScheduler(t, syncSettings(t.TempDir(), radarrInst()), &added, &polled)
	s.Sources = func(config.Requests, string) (requests.Source, error) { panic("boom") }
	s.safeSync() // must not panic
	if s.Snapshot().Requests.Running {
		t.Fatal("still marked running")
	}
}
