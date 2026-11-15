package requests

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/client"
	"github.com/cstupi/arr-trickle/internal/state"
)

// Arr is what request sync needs from Radarr or Sonarr (client.Client, or a fake in tests).
type Arr interface {
	Request(method, path string, data any) (any, error)
}

// Target is the app one kind of request is added to.
type Target struct {
	Name string // the instance name, for the activity log
	Arr  Arr
}

// Targets maps "movie" and "tv" to where they go; a missing entry means that kind is not synced.
type Targets map[string]Target

// Added is one request sync added.
type Added struct{ Source, Target, Kind, Title string }

// Result is what one sync did.
type Result struct {
	At      time.Time
	Checked int // requests old enough to check, in the lookback window
	Added   []Added
	Notes   []string // requests deliberately left alone (e.g. on the app's exclusion list)
	Errors  []string
}

// OK reports whether the sync finished without an error.
func (r *Result) OK() bool { return len(r.Errors) == 0 }

// Done is the requests request sync has added, so one is never added twice: if it disappears again, someone
// removed it on purpose. Kept in <state_dir>/readded.json; safe for concurrent use.
type Done struct {
	path string
	mu   sync.Mutex
	data map[string]string // "movie:<tmdb>" / "tv:<tvdb>" -> when it was added (RFC 3339)
}

// NewDone loads the file at path ("" keeps it in memory only).
func NewDone(path string) *Done {
	d := &Done{path: path, data: map[string]string{}}
	if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &d.data) != nil {
		slog.Warn("could not read the request sync list; starting it fresh", "path", path)
		d.data = map[string]string{}
	}
	return d
}

func doneKey(kind string, id int) string { return kind + ":" + strconv.Itoa(id) }

// Has reports whether the request was added before.
func (d *Done) Has(kind string, id int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.data[doneKey(kind, id)]
	return ok
}

// Count is how many requests have been added.
func (d *Done) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.data)
}

// Add records a request as added and saves.
func (d *Done) Add(kind string, id int, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[doneKey(kind, id)] = now.UTC().Format(time.RFC3339)
	if d.path == "" {
		return nil
	}
	raw, _ := json.Marshal(d.data)
	return state.AtomicWrite(d.path, raw, 0o644)
}

// post sends a POST, retrying a 5xx after each of client.CommandRetryDelays: Radarr answers "database is
// locked" with a 500, which is how these requests went missing in the first place.
func post(a Arr, path string, body any) (any, error) {
	for attempt := 0; ; attempt++ {
		v, err := a.Request(http.MethodPost, path, body)
		e, ok := errors.AsType[*client.Error](err)
		if !ok || e.Status < 500 || attempt == len(client.CommandRetryDelays) {
			return v, err
		}
		slog.Warn("add failed, retrying", "path", path, "err", err, "in", client.CommandRetryDelays[attempt])
		time.Sleep(client.CommandRetryDelays[attempt])
	}
}

func list(a Arr, path string) ([]apps.Record, error) {
	v, err := a.Request(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("GET %s: unexpected response shape", path)
	}
	out := make([]apps.Record, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// kindAPI is how Radarr and Sonarr differ for request sync.
type kindAPI struct {
	idField, exists, exclusions, noun string
}

var kindAPIs = map[string]kindAPI{
	"movie": {idField: "tmdbId", exists: "/movie?tmdbId=", exclusions: "/exclusions", noun: "movie"},
	"tv":    {idField: "tvdbId", exists: "/series?tvdbId=", exclusions: "/importlistexclusion", noun: "series"},
}

func (r Request) id() int {
	if r.Kind == "tv" {
		return r.TVDB
	}
	return r.TMDB
}

// syncer is one sync's caches: each target's exclusions and root folders, each source's defaults.
type syncer struct {
	exclusions map[string]map[int]bool
	roots      map[string]map[int]string
}

func (s *syncer) excluded(kind string, t Target, id int) (bool, error) {
	if s.exclusions[kind] == nil {
		items, err := list(t.Arr, kindAPIs[kind].exclusions)
		if err != nil {
			return false, err
		}
		ids := map[int]bool{}
		for _, it := range items {
			if n, ok := apps.Int(it, kindAPIs[kind].idField); ok {
				ids[n] = true
			}
		}
		s.exclusions[kind] = ids
	}
	return s.exclusions[kind][id], nil
}

// root is a root folder path; Ombi gives root folders by id, which may also be stale.
func (s *syncer) root(kind string, t Target, value string) (string, error) {
	id, err := strconv.Atoi(value)
	if err != nil {
		return value, nil // already a path
	}
	if s.roots[kind] == nil {
		items, err := list(t.Arr, "/rootfolder")
		if err != nil {
			return "", err
		}
		s.roots[kind] = map[int]string{}
		for _, it := range items {
			if n, ok := apps.Int(it, "id"); ok {
				s.roots[kind][n], _ = it["path"].(string)
			}
		}
	}
	if p := s.roots[kind][id]; p != "" {
		return p, nil
	}
	return "", fmt.Errorf("root folder id %d does not exist in %s", id, t.Name)
}

// add adds one request and returns its title.
func (s *syncer) add(req Request, t Target, d Defaults) (string, error) {
	profile, rootValue := req.Profile, req.Root
	if req.Kind == "movie" {
		profile, rootValue = cmp.Or(profile, d.MovieProfile), cmp.Or(rootValue, d.MovieRoot)
	} else {
		profile, rootValue = cmp.Or(profile, d.TVProfile), cmp.Or(rootValue, d.TVRoot)
	}
	if profile == 0 || rootValue == "" {
		return "", fmt.Errorf("%s has no default quality profile or root folder for this request", req.Source)
	}
	root, err := s.root(req.Kind, t, rootValue)
	if err != nil {
		return "", err
	}
	var body apps.Record
	if req.Kind == "movie" {
		v, err := t.Arr.Request(http.MethodGet, "/movie/lookup/tmdb?tmdbId="+strconv.Itoa(req.TMDB), nil)
		if body, _ = v.(map[string]any); err != nil || body == nil {
			return "", cmp.Or(err, fmt.Errorf("%s does not know TMDb id %d", t.Name, req.TMDB))
		}
		body["minimumAvailability"] = cmp.Or(d.MinAvailability, "released")
		body["addOptions"] = map[string]any{"searchForMovie": true, "monitor": "movieOnly"}
	} else {
		found, err := list(t.Arr, "/series/lookup?term="+url.QueryEscape("tvdb:"+strconv.Itoa(req.TVDB)))
		if err != nil {
			return "", err
		}
		i := slices.IndexFunc(found, func(r apps.Record) bool { n, _ := apps.Int(r, "tvdbId"); return n == req.TVDB })
		if i < 0 {
			return "", fmt.Errorf("%s does not know TVDB id %d", t.Name, req.TVDB)
		}
		body = found[i]
		wanted := map[int]bool{}
		for _, n := range req.Seasons {
			wanted[n] = true
		}
		seasons, _ := body["seasons"].([]any)
		for _, it := range seasons {
			if season, ok := it.(map[string]any); ok {
				n, _ := apps.Int(season, "seasonNumber")
				season["monitored"] = wanted[n] || (len(wanted) == 0 && n > 0) // no seasons named: all but specials
			}
		}
		body["seasonFolder"] = true
		body["addOptions"] = map[string]any{"searchForMissingEpisodes": true}
	}
	delete(body, "id")
	body["qualityProfileId"] = profile
	body["rootFolderPath"] = root
	body["monitored"] = true
	if body["tags"] == nil {
		body["tags"] = []any{}
	}
	title := fmt.Sprintf("%v", cmp.Or[any](body["title"], req.Title))
	if year, ok := apps.Int(body, "year"); ok && year > 0 {
		title = fmt.Sprintf("%s (%d)", title, year)
	}
	if _, err := post(t.Arr, "/"+kindAPIs[req.Kind].noun, body); err != nil {
		return title, err
	}
	return title, nil
}

// Sync checks each source's requests from the last lookback (but older than grace) and adds the ones that
// are not in their target app, not on its exclusion list, and not added by an earlier sync.
func Sync(sources []Source, targets Targets, done *Done, now time.Time, lookback, grace time.Duration) *Result {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	r := &Result{At: now}
	s := &syncer{exclusions: map[string]map[int]bool{}, roots: map[string]map[int]string{}}
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		slog.Warn("request sync", "problem", msg)
		r.Errors = append(r.Errors, msg)
	}
	for _, src := range sources {
		reqs, err := src.Requests(now.Add(-lookback))
		if err != nil {
			fail("%s", err)
			continue
		}
		var defaults *Defaults
		for _, req := range reqs {
			t, ok := targets[req.Kind]
			if !ok || req.id() <= 0 || req.Created.After(now.Add(-grace)) {
				continue
			}
			r.Checked++
			if done.Has(req.Kind, req.id()) {
				continue
			}
			api := kindAPIs[req.Kind]
			existing, err := list(t.Arr, api.exists+strconv.Itoa(req.id()))
			if err != nil {
				fail("%s: %s", t.Name, err)
				continue
			}
			if len(existing) > 0 {
				continue
			}
			if ex, err := s.excluded(req.Kind, t, req.id()); err != nil {
				fail("%s: %s", t.Name, err)
				continue
			} else if ex {
				note := fmt.Sprintf("%s %s %s:%d is on %s's exclusion list; not added", src.Name(), api.noun, api.idField, req.id(), t.Name)
				slog.Info("request sync", "skipped", note)
				r.Notes = append(r.Notes, note)
				continue
			}
			if defaults == nil {
				d, err := src.Defaults()
				if err != nil {
					fail("%s", err)
					break
				}
				defaults = &d
			}
			title, err := s.add(req, t, *defaults)
			if err != nil {
				fail("adding %s %s:%d from %s to %s: %s", api.noun, api.idField, req.id(), src.Name(), t.Name, err)
				continue
			}
			slog.Info("request sync added", "source", src.Name(), "target", t.Name, "title", title)
			r.Added = append(r.Added, Added{Source: src.Name(), Target: t.Name, Kind: req.Kind, Title: title})
			if err := done.Add(req.Kind, req.id(), now); err != nil {
				fail("could not save the request sync list: %s", err)
			}
		}
	}
	return r
}
