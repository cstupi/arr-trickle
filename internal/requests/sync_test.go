package requests

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/client"
)

var now = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func init() { client.CommandRetryDelays = []time.Duration{0, 0} }

// fakeArr is an in-memory Radarr or Sonarr.
type fakeArr struct {
	kind       string // "movie" or "tv"
	mu         sync.Mutex
	library    map[int]bool // tmdb or tvdb ids present
	excluded   []int
	roots      map[int]string
	known      map[int]string // lookup: id -> title
	seasons    []int          // lookup: the series' seasons
	added      []map[string]any
	failPosts  int // answer this many POSTs with a 500 first
	err        map[string]error
	calls      []string
	lookupJunk bool
}

func newArr(kind string) *fakeArr {
	return &fakeArr{kind: kind, library: map[int]bool{}, roots: map[int]string{2: "/data/" + kind}, known: map[int]string{}, err: map[string]error{}}
}

func (f *fakeArr) Request(method, path string, data any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
	route, query, _ := strings.Cut(path, "?")
	query, _ = url.QueryUnescape(query)
	if err := f.err[route]; err != nil {
		return nil, err
	}
	idOf := func() int { n, _ := strconv.Atoi(query[strings.LastIndexAny(query, "=:")+1:]); return n }
	switch {
	case method == http.MethodPost:
		if f.failPosts > 0 {
			f.failPosts--
			return nil, &client.Error{Msg: "POST: HTTP 500", Status: 500}
		}
		body := data.(map[string]any)
		f.added = append(f.added, body)
		n, _ := apps.Int(body, map[string]string{"movie": "tmdbId", "tv": "tvdbId"}[f.kind])
		f.library[n] = true
		return body, nil
	case route == "/movie" || route == "/series":
		if f.library[idOf()] {
			return []any{map[string]any{"title": "x"}}, nil
		}
		return []any{}, nil
	case route == "/exclusions" || route == "/importlistexclusion":
		var out []any
		for _, n := range f.excluded {
			out = append(out, map[string]any{map[string]string{"movie": "tmdbId", "tv": "tvdbId"}[f.kind]: float64(n)})
		}
		return out, nil
	case route == "/rootfolder":
		var out []any
		for id, p := range f.roots {
			out = append(out, map[string]any{"id": float64(id), "path": p})
		}
		return out, nil
	case route == "/movie/lookup/tmdb":
		if f.lookupJunk {
			return "junk", nil
		}
		title, ok := f.known[idOf()]
		if !ok {
			return nil, nil
		}
		return map[string]any{"title": title, "year": float64(2026), "tmdbId": float64(idOf()), "id": float64(0)}, nil
	case route == "/series/lookup":
		title, ok := f.known[idOf()]
		if !ok {
			return []any{}, nil
		}
		var seasons []any
		for _, n := range f.seasons {
			seasons = append(seasons, map[string]any{"seasonNumber": float64(n), "monitored": true})
		}
		return []any{map[string]any{"title": title, "tvdbId": float64(idOf()), "seasons": seasons}}, nil
	}
	return nil, fmt.Errorf("fake: unexpected %s %s", method, path)
}

type fakeSource struct {
	name     string
	reqs     []Request
	defaults Defaults
	err, def error
	sinces   []time.Time
}

func (s *fakeSource) Name() string { return s.name }
func (s *fakeSource) Requests(since time.Time) ([]Request, error) {
	s.sinces = append(s.sinces, since)
	return s.reqs, s.err
}
func (s *fakeSource) Defaults() (Defaults, error) { return s.defaults, s.def }

var defs = Defaults{MovieProfile: 4, MovieRoot: "/data/movie", MinAvailability: "inCinemas", TVProfile: 4, TVRoot: "2"}

func movie(tmdb int, age time.Duration) Request {
	return Request{Source: "seerr", Kind: "movie", TMDB: tmdb, Created: now.Add(-age)}
}

func run(t *testing.T, srcs []Source, targets Targets, done *Done) *Result {
	t.Helper()
	return Sync(srcs, targets, done, now, 14*24*time.Hour, 10*time.Minute)
}

func TestAddsAMissingMovieWithTheAppsDefaults(t *testing.T) {
	radarr := newArr("movie")
	radarr.known[100] = "The Odyssey"
	src := &fakeSource{name: "seerr", reqs: []Request{movie(100, time.Hour)}, defaults: defs}
	r := run(t, []Source{src}, Targets{"movie": {Name: "radarr", Arr: radarr}}, NewDone(""))
	if !r.OK() || r.Checked != 1 || len(r.Added) != 1 || r.Added[0] != (Added{"seerr", "radarr", "movie", "The Odyssey (2026)"}) {
		t.Fatalf("%+v", r)
	}
	body := radarr.added[0]
	if body["qualityProfileId"] != 4 || body["rootFolderPath"] != "/data/movie" || body["monitored"] != true ||
		body["minimumAvailability"] != "inCinemas" || body["id"] != nil {
		t.Fatal(body)
	}
	if opts := body["addOptions"].(map[string]any); opts["searchForMovie"] != true {
		t.Fatal(opts)
	}
	if !src.sinces[0].Equal(now.Add(-14 * 24 * time.Hour)) {
		t.Fatal(src.sinces)
	}
}

func TestOverridesAndRootFolderIDs(t *testing.T) {
	radarr := newArr("movie")
	radarr.known[1] = "A"
	req := movie(1, time.Hour)
	req.Profile, req.Root = 7, "2" // Ombi gives root folders by id
	r := run(t, []Source{&fakeSource{name: "ombi", reqs: []Request{req}, defaults: Defaults{}}}, Targets{"movie": {Name: "radarr", Arr: radarr}}, NewDone(""))
	if !r.OK() || radarr.added[0]["qualityProfileId"] != 7 || radarr.added[0]["rootFolderPath"] != "/data/movie" ||
		radarr.added[0]["minimumAvailability"] != "released" {
		t.Fatalf("%+v %v", r, radarr.added)
	}
	req.TMDB, req.Root = 2, "9" // a stale id
	radarr.known[2] = "B"
	r = run(t, []Source{&fakeSource{name: "ombi", reqs: []Request{req}}}, Targets{"movie": {Name: "radarr", Arr: radarr}}, NewDone(""))
	if r.OK() || !strings.Contains(r.Errors[0], "root folder id 9 does not exist") {
		t.Fatalf("%+v", r)
	}
}

func TestLeavesAlone(t *testing.T) {
	radarr := newArr("movie")
	radarr.library[1] = true
	radarr.excluded = []int{2}
	radarr.known[3] = "Three"
	done := NewDone("")
	done.Add("movie", 3, now)
	reqs := []Request{
		movie(1, time.Hour),                 // already in Radarr
		movie(2, time.Hour),                 // excluded
		movie(3, time.Hour),                 // added by an earlier sync, then removed
		movie(4, time.Minute),               // inside the grace period
		{Kind: "movie", Created: now},       // no id
		{Kind: "tv", TVDB: 5, Created: now}, // no Sonarr target
	}
	r := run(t, []Source{&fakeSource{name: "seerr", reqs: reqs, defaults: defs}}, Targets{"movie": {Name: "radarr", Arr: radarr}}, done)
	if !r.OK() || r.Checked != 3 || len(r.Added) != 0 || len(radarr.added) != 0 || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "exclusion") {
		t.Fatalf("%+v", r)
	}
	if n := slices.IndexFunc(radarr.calls, func(c string) bool { return strings.Contains(c, "exclusions") }); n < 0 {
		t.Fatal(radarr.calls)
	}
}

func TestAddsASeriesWithTheRequestedSeasons(t *testing.T) {
	sonarr := newArr("tv")
	sonarr.known[72449] = "Stargate SG-1"
	sonarr.seasons = []int{0, 1, 2, 3}
	req := Request{Source: "ombi", Kind: "tv", TVDB: 72449, Title: "SG-1", Created: now.Add(-time.Hour), Seasons: []int{2}}
	all := Request{Source: "ombi", Kind: "tv", TVDB: 1, Created: now.Add(-time.Hour)}
	sonarr.known[1] = "All"
	r := run(t, []Source{&fakeSource{name: "ombi", reqs: []Request{req, all}, defaults: defs}}, Targets{"tv": {Name: "sonarr", Arr: sonarr}}, NewDone(""))
	if !r.OK() || len(r.Added) != 2 || r.Added[0].Title != "Stargate SG-1" {
		t.Fatalf("%+v", r)
	}
	monitored := func(body map[string]any) (out []int) {
		for _, s := range body["seasons"].([]any) {
			m := s.(map[string]any)
			if m["monitored"] == true {
				n, _ := apps.Int(m, "seasonNumber")
				out = append(out, n)
			}
		}
		return out
	}
	if got := monitored(sonarr.added[0]); !slices.Equal(got, []int{2}) {
		t.Fatal(got)
	}
	if got := monitored(sonarr.added[1]); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatal("no seasons named: all but specials", got)
	}
	if b := sonarr.added[0]; b["rootFolderPath"] != "/data/tv" || b["seasonFolder"] != true ||
		b["addOptions"].(map[string]any)["searchForMissingEpisodes"] != true || b["tags"] == nil {
		t.Fatal(b)
	}
}

func TestRetriesAnAddThatHitADatabaseLock(t *testing.T) {
	radarr := newArr("movie")
	radarr.known[1] = "A"
	radarr.failPosts = 2
	r := run(t, []Source{&fakeSource{name: "seerr", reqs: []Request{movie(1, time.Hour)}, defaults: defs}}, Targets{"movie": {Name: "radarr", Arr: radarr}}, NewDone(""))
	if !r.OK() || len(r.Added) != 1 {
		t.Fatalf("%+v", r)
	}
	radarr.known[2] = "B"
	radarr.failPosts = 5
	r = run(t, []Source{&fakeSource{name: "seerr", reqs: []Request{movie(2, time.Hour)}, defaults: defs}}, Targets{"movie": {Name: "radarr", Arr: radarr}}, NewDone(""))
	if r.OK() || len(r.Added) != 0 || !strings.Contains(r.Errors[0], "HTTP 500") {
		t.Fatalf("%+v", r)
	}
}

func TestErrorsAreReportedAndTheRestContinues(t *testing.T) {
	broken := &fakeSource{name: "ombi", err: errors.New("ombi: GET /Request/movie: no answer")}
	noDefaults := &fakeSource{name: "seerr", reqs: []Request{movie(1, time.Hour), movie(2, time.Hour)}, def: errors.New("seerr: defaults failed")}
	radarr := newArr("movie")
	r := run(t, []Source{broken, noDefaults}, Targets{"movie": {Name: "radarr", Arr: radarr}}, NewDone(""))
	if len(r.Errors) != 2 || !strings.Contains(r.Errors[0], "no answer") || !strings.Contains(r.Errors[1], "defaults failed") {
		t.Fatalf("%+v", r)
	}

	cases := []struct {
		name  string
		setup func(*fakeArr)
		req   Request
		d     Defaults
		want  string
	}{
		{"exists check", func(f *fakeArr) { f.err["/movie"] = errors.New("GET /movie: timed out") }, movie(1, time.Hour), defs, "timed out"},
		{"exclusions", func(f *fakeArr) { f.err["/exclusions"] = errors.New("GET /exclusions: HTTP 500") }, movie(1, time.Hour), defs, "HTTP 500"},
		{"unknown movie", func(*fakeArr) {}, movie(1, time.Hour), defs, "does not know TMDb id 1"},
		{"lookup error", func(f *fakeArr) { f.err["/movie/lookup/tmdb"] = errors.New("lookup down") }, movie(1, time.Hour), defs, "lookup down"},
		{"lookup junk", func(f *fakeArr) { f.lookupJunk = true }, movie(1, time.Hour), defs, "does not know"},
		{"no defaults", func(*fakeArr) {}, movie(1, time.Hour), Defaults{}, "no default quality profile"},
		{"rootfolder", func(f *fakeArr) { f.err["/rootfolder"] = errors.New("rf down") }, movie(1, time.Hour), Defaults{MovieProfile: 1, MovieRoot: "2"}, "rf down"},
		{"exclusions shape", func(f *fakeArr) { f.err["/exclusions"] = nil; f.excluded = nil }, movie(1, time.Hour), defs, "does not know"},
	}
	for _, c := range cases {
		arr := newArr("movie")
		c.setup(arr)
		r := run(t, []Source{&fakeSource{name: "seerr", reqs: []Request{c.req}, defaults: c.d}}, Targets{"movie": {Name: "radarr", Arr: arr}}, NewDone(""))
		if r.OK() || !strings.Contains(strings.Join(r.Errors, "|"), c.want) {
			t.Errorf("%s: %+v", c.name, r)
		}
	}

	sonarr := newArr("tv")
	tv := Request{Kind: "tv", TVDB: 9, Created: now.Add(-time.Hour)}
	r = run(t, []Source{&fakeSource{name: "seerr", reqs: []Request{tv}, defaults: defs}}, Targets{"tv": {Name: "sonarr", Arr: sonarr}}, NewDone(""))
	if r.OK() || !strings.Contains(r.Errors[0], "does not know TVDB id 9") {
		t.Fatalf("%+v", r)
	}
	sonarr.err["/series/lookup"] = errors.New("lookup down")
	r = run(t, []Source{&fakeSource{name: "seerr", reqs: []Request{tv}, defaults: defs}}, Targets{"tv": {Name: "sonarr", Arr: sonarr}}, NewDone(""))
	if r.OK() || !strings.Contains(r.Errors[0], "lookup down") {
		t.Fatalf("%+v", r)
	}
}

type shapeArr struct{ v any }

func (s shapeArr) Request(string, string, any) (any, error) { return s.v, nil }

func TestUnexpectedShapes(t *testing.T) {
	if _, err := list(shapeArr{map[string]any{}}, "/movie"); err == nil || !strings.Contains(err.Error(), "unexpected response shape") {
		t.Fatal(err)
	}
	items, err := list(shapeArr{[]any{"junk", map[string]any{"a": 1}}}, "/movie")
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	r := Sync([]Source{&fakeSource{name: "seerr", reqs: []Request{movie(1, time.Hour)}}}, Targets{"movie": {Name: "radarr", Arr: shapeArr{"junk"}}}, NewDone(""), time.Time{}, time.Hour, 0)
	if r.OK() || r.At.IsZero() {
		t.Fatalf("%+v", r)
	}
}

func TestDonePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readded.json")
	d := NewDone(path)
	if d.Has("movie", 1) || d.Count() != 0 {
		t.Fatal("empty")
	}
	if err := d.Add("movie", 1, now); err != nil {
		t.Fatal(err)
	}
	again := NewDone(path)
	if !again.Has("movie", 1) || again.Has("tv", 1) || again.Count() != 1 {
		t.Fatal("not persisted")
	}
	os.WriteFile(path, []byte("{nope"), 0o644)
	if NewDone(path).Count() != 0 {
		t.Fatal("bad file should start fresh")
	}
	unwritable := NewDone(filepath.Join(t.TempDir(), "missing", "readded.json"))
	if err := unwritable.Add("movie", 1, now); err == nil {
		t.Fatal("expected a write error")
	}
	radarr := newArr("movie")
	radarr.known[5] = "Five"
	r := run(t, []Source{&fakeSource{name: "seerr", reqs: []Request{movie(5, time.Hour)}, defaults: defs}}, Targets{"movie": {Name: "radarr", Arr: radarr}}, unwritable)
	if len(r.Added) != 1 || r.OK() || !strings.Contains(r.Errors[0], "could not save") {
		t.Fatalf("%+v", r)
	}
}
