// Package testutil has shared test helpers: record builders, a fake *arr client and log capture.
package testutil

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/client"
)

// Key is obviously fake; real keys never appear in tests.
const Key = "fakefakefakefakefakefakefakefake"

// Now is a fixed "today" for planner tests.
var Now = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)

// Past and Future are release dates either side of Now.
const (
	Past   = "2020-01-01T00:00:00Z"
	Future = "2027-01-01T00:00:00Z"
)

// Album is a Lidarr wanted record.
func Album(id int, date string, profile, artist int) apps.Record {
	return apps.Record{"id": float64(id), "title": fmt.Sprintf("A%d", id), "releaseDate": date, "artistId": float64(artist),
		"artist": map[string]any{"artistName": "X", "qualityProfileId": float64(profile)}}
}

// A is a released album with defaults.
func A(id int) apps.Record { return Album(id, Past, 1, 1) }

// Episode is a Sonarr wanted record.
func Episode(id, season, series int, date string, profile int) apps.Record {
	return apps.Record{"id": float64(id), "seriesId": float64(series), "seasonNumber": float64(season), "episodeNumber": float64(id),
		"airDateUtc": date, "series": map[string]any{"title": "S", "qualityProfileId": float64(profile)}}
}

// E is an aired episode of series 10.
func E(id, season int) apps.Record { return Episode(id, season, 10, Past, 1) }

// Movie is a Radarr wanted record.
func Movie(id int, available bool, profile int) apps.Record {
	return apps.Record{"id": float64(id), "title": fmt.Sprintf("M%d", id), "year": float64(2000), "isAvailable": available,
		"qualityProfileId": float64(profile)}
}

// M is an available movie.
func M(id int) apps.Record { return Movie(id, true, 1) }

// Fake stands in for an *arr app: canned answers; it records the commands it is sent.
type Fake struct {
	Missing, Cutoff []apps.Record
	HealthItems     []map[string]any
	Queue           int
	Profiles        map[int]bool
	App, SortKey    string
	Err             map[string]error // per method: "status", "health", "queue", "profiles", "wanted", "command"
	mu              sync.Mutex
	Commands        []map[string]any
}

// NewFake is a healthy Lidarr with profile 1 allowing upgrades.
func NewFake() *Fake {
	return &Fake{Profiles: map[int]bool{1: true}, App: "Lidarr", SortKey: "albums.lastSearchTime", Err: map[string]error{}}
}

// For makes a fake that looks like the given app.
func For(kind string) *Fake {
	f := NewFake()
	f.App, f.SortKey = apps.Specs[kind].AppName, apps.Specs[kind].SortKey
	return f
}

func (f *Fake) Status() (map[string]any, error) {
	return map[string]any{"appName": f.App, "version": "9.9"}, f.Err["status"]
}
func (f *Fake) Health() ([]map[string]any, error)      { return f.HealthItems, f.Err["health"] }
func (f *Fake) QueueSize() (int, error)                { return f.Queue, f.Err["queue"] }
func (f *Fake) UpgradeProfiles() (map[int]bool, error) { return f.Profiles, f.Err["profiles"] }
func (f *Fake) Wanted(kind string, pageSize int) (client.WantedPage, error) {
	if err := f.Err["wanted"]; err != nil {
		return client.WantedPage{}, err
	}
	recs := f.Missing
	if kind == "cutoff" {
		recs = f.Cutoff
	}
	return client.WantedPage{Records: recs[:min(pageSize, len(recs))], SortKey: f.SortKey, Total: len(recs)}, nil
}
func (f *Fake) Command(body map[string]any) (map[string]any, error) {
	if err := f.Err["command"]; err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Commands = append(f.Commands, body)
	return map[string]any{"id": 1.0}, nil
}

// Sent is a copy of the commands received so far.
func (f *Fake) Sent() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.Commands...)
}

// Logs captures slog output for the rest of the test.
type Logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *Logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type logHandler struct{ l *Logs }

func (h logHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h logHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h logHandler) WithGroup(string) slog.Handler            { return h }
func (h logHandler) Handle(_ context.Context, r slog.Record) error {
	h.l.mu.Lock()
	defer h.l.mu.Unlock()
	fmt.Fprintf(&h.l.buf, "%s %s\n", r.Level, r.Message)
	return nil
}

// CaptureLogs routes slog to a buffer until the test ends.
func CaptureLogs(t testing.TB) *Logs {
	l := &Logs{}
	old := slog.Default()
	slog.SetDefault(slog.New(logHandler{l}))
	t.Cleanup(func() { slog.SetDefault(old) })
	return l
}

// ArrConfigXML writes an *arr config.xml holding Key and returns its path.
func ArrConfigXML(t testing.TB) string {
	path := filepath.Join(t.TempDir(), "config.xml")
	if err := os.WriteFile(path, []byte("<Config>\n  <Port>8989</Port>\n  <ApiKey>"+Key+"</ApiKey>\n</Config>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
