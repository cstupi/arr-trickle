package requests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cstupi/arr-trickle/internal/client"
)

const testKey = "fakeseerrfakeseerrfakeseerrfakeseerr=="

// serve answers paths under base with JSON; it checks the key header on every call.
func serve(t *testing.T, header string, routes map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(header) != testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		key := r.URL.Path
		if r.URL.Query().Has("skip") {
			key += "?skip=" + r.URL.Query().Get("skip")
		}
		v, ok := routes[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if s, ok := v.(string); ok {
			fmt.Fprint(w, s)
			return
		}
		json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func seerrReq(status int, created time.Time, kind string, tmdb, tvdb, media int, mod func(map[string]any)) map[string]any {
	r := map[string]any{
		"status": status, "createdAt": created.Format(time.RFC3339Nano), "type": kind, "is4k": false,
		"profileId": 4, "rootFolder": "/data/movie", "seasons": []any{},
		"media": map[string]any{"tmdbId": tmdb, "tvdbId": tvdb, "status": media},
	}
	if mod != nil {
		mod(r)
	}
	return r
}

func TestSeerrRequests(t *testing.T) {
	recent, old := now.Add(-time.Hour), now.Add(-30*24*time.Hour)
	page0 := []any{
		seerrReq(2, recent, "movie", 1, 0, 3, nil),                                             // approved, processing: yes
		seerrReq(4, recent, "movie", 2, 0, 1, func(r map[string]any) { r["profileId"] = nil }), // failed: yes
		seerrReq(5, recent, "movie", 3, 0, 5, nil),                                             // completed: no
		seerrReq(2, recent, "movie", 4, 0, 5, nil),                                             // available: no
		seerrReq(2, recent, "movie", 5, 0, 7, nil),                                             // deleted: no
		seerrReq(1, recent, "movie", 6, 0, 2, nil),                                             // pending: no
		seerrReq(2, recent, "movie", 7, 0, 3, func(r map[string]any) { r["is4k"] = true }),     // 4K: no
		seerrReq(2, recent, "tv", 0, 99, 4, func(r map[string]any) {
			r["seasons"] = []any{map[string]any{"seasonNumber": 1}, map[string]any{"seasonNumber": 3}}
			r["rootFolder"] = "/data/tv"
		}),
	}
	for len(page0) < seerrPageSize {
		page0 = append(page0, seerrReq(3, recent, "movie", 1000+len(page0), 0, 1, nil)) // declined filler
	}
	page1 := []any{seerrReq(2, recent, "movie", 8, 0, 3, nil), seerrReq(2, old, "movie", 9, 0, 3, nil), seerrReq(2, recent, "movie", 10, 0, 3, nil)}
	srv := serve(t, "X-Api-Key", map[string]any{
		"/api/v1/request?skip=0":   map[string]any{"results": page0},
		"/api/v1/request?skip=100": map[string]any{"results": page1},
	})
	got, err := NewSeerr(srv.URL, testKey, 5*time.Second).Requests(now.Add(-14 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, r := range got {
		ids = append(ids, r.TMDB+r.TVDB)
	}
	if !slices.Equal(ids, []int{1, 2, 99, 8}) { // stops at the first request older than since
		t.Fatal(ids)
	}
	if got[0].Profile != 4 || got[1].Profile != 0 || got[0].Root != "/data/movie" || got[0].Source != "seerr" {
		t.Fatalf("%+v", got[:2])
	}
	if tv := got[2]; tv.Kind != "tv" || !slices.Equal(tv.Seasons, []int{1, 3}) || tv.Root != "/data/tv" {
		t.Fatalf("%+v", tv)
	}

	short := serve(t, "X-Api-Key", map[string]any{"/api/v1/request?skip=0": map[string]any{"results": []any{seerrReq(2, recent, "movie", 1, 0, 3, nil)}}})
	if got, err := NewSeerr(short.URL, testKey, time.Second).Requests(time.Time{}); err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
}

func TestSeerrDefaults(t *testing.T) {
	srv := serve(t, "X-Api-Key", map[string]any{
		"/api/v1/settings/radarr": []any{
			map[string]any{"isDefault": true, "is4k": true, "activeProfileId": 9, "activeDirectory": "/4k"},
			map[string]any{"isDefault": true, "is4k": false, "activeProfileId": 4, "activeDirectory": "/data/movie", "minimumAvailability": "inCinemas"},
		},
		"/api/v1/settings/sonarr": []any{map[string]any{"isDefault": false, "activeProfileId": 5}},
	})
	d, err := NewSeerr(srv.URL, testKey, time.Second).Defaults()
	if err != nil || d != (Defaults{MovieProfile: 4, MovieRoot: "/data/movie", MinAvailability: "inCinemas"}) {
		t.Fatal(d, err)
	}
	noSonarr := serve(t, "X-Api-Key", map[string]any{"/api/v1/settings/radarr": []any{}})
	if _, err := NewSeerr(noSonarr.URL, testKey, time.Second).Defaults(); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatal(err)
	}
	nothing := serve(t, "X-Api-Key", map[string]any{})
	if _, err := NewSeerr(nothing.URL, testKey, time.Second).Defaults(); err == nil {
		t.Fatal("expected an error")
	}
}

func ombiChild(approved, available bool, denied any, created time.Time, seasons ...int) map[string]any {
	var sr []any
	for _, n := range seasons {
		sr = append(sr, map[string]any{"seasonNumber": n})
	}
	return map[string]any{"approved": approved, "available": available, "denied": denied, "requestedDate": created.Format(time.RFC3339Nano), "seasonRequests": sr}
}

func TestOmbiRequests(t *testing.T) {
	recent, newer, old := now.Add(-2*time.Hour), now.Add(-time.Hour), now.Add(-30*24*time.Hour)
	m := func(id int, approved, available bool, denied any, created time.Time, mod func(map[string]any)) map[string]any {
		r := map[string]any{"theMovieDbId": id, "title": "M" + strconv.Itoa(id), "approved": approved, "available": available,
			"denied": denied, "is4kRequest": false, "requestedDate": created.Format(time.RFC3339Nano), "qualityOverride": 0, "rootPathOverride": 0}
		if mod != nil {
			mod(r)
		}
		return r
	}
	srv := serve(t, "ApiKey", map[string]any{
		"/ombi/api/v1/Request/movie": []any{
			m(1, true, false, nil, recent, nil),
			m(2, true, false, false, recent, func(r map[string]any) { r["qualityOverride"], r["rootPathOverride"] = 7, 2 }),
			m(3, true, true, nil, recent, nil),   // available
			m(4, false, false, nil, recent, nil), // not approved
			m(5, true, false, true, recent, nil), // denied
			m(6, true, false, nil, old, nil),     // too old
			m(7, true, false, nil, recent, func(r map[string]any) { r["is4kRequest"] = true }),
			m(0, true, false, nil, recent, nil), // no id
		},
		"/ombi/api/v1/Request/tv": []any{
			map[string]any{"tvDbId": 72449, "title": "SG-1", "qualityOverride": 0, "rootFolder": 0, "childRequests": []any{
				ombiChild(true, false, nil, recent, 2, 1),
				ombiChild(true, false, nil, newer, 2, 3),
				ombiChild(true, true, nil, newer, 4),   // available
				ombiChild(false, false, nil, newer, 5), // not approved
				ombiChild(true, false, true, newer, 6), // denied
				ombiChild(true, false, nil, old, 7),    // too old
			}},
			map[string]any{"tvDbId": 0, "title": "No TVDB id", "childRequests": []any{ombiChild(true, false, nil, recent, 1)}},
			map[string]any{"tvDbId": 5, "title": "Done", "childRequests": []any{ombiChild(true, true, nil, recent, 1)}},
		},
	})
	got, err := NewOmbi(srv.URL+"/ombi", testKey, time.Second).Requests(now.Add(-14 * 24 * time.Hour))
	if err != nil || len(got) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if got[0].TMDB != 1 || got[0].Title != "M1" || got[0].Root != "" || got[1].Profile != 7 || got[1].Root != "2" {
		t.Fatalf("%+v", got[:2])
	}
	if tv := got[2]; tv.TVDB != 72449 || !slices.Equal(tv.Seasons, []int{1, 2, 3}) || !tv.Created.Equal(newer) || tv.Kind != "tv" {
		t.Fatalf("%+v", tv)
	}
	moviesOnly := serve(t, "ApiKey", map[string]any{"/api/v1/Request/movie": []any{}})
	if _, err := NewOmbi(moviesOnly.URL, testKey, time.Second).Requests(now); err == nil {
		t.Fatal("expected the tv list to fail")
	}
	if _, err := NewOmbi(serve(t, "ApiKey", map[string]any{}).URL, testKey, time.Second).Requests(now); err == nil {
		t.Fatal("expected the movie list to fail")
	}
}

func TestOmbiDefaults(t *testing.T) {
	srv := serve(t, "ApiKey", map[string]any{
		"/api/v1/Settings/radarr": map[string]any{"radarr": map[string]any{"defaultQualityProfile": "4", "defaultRootPath": "/data/movie", "minimumAvailability": "InCinemas"}},
		"/api/v1/Settings/sonarr": map[string]any{"qualityProfile": "4", "rootPath": "2"},
	})
	d, err := NewOmbi(srv.URL, testKey, time.Second).Defaults()
	if err != nil || d != (Defaults{MovieProfile: 4, MovieRoot: "/data/movie", MinAvailability: "inCinemas", TVProfile: 4, TVRoot: "2"}) {
		t.Fatal(d, err)
	}
	radarrOnly := serve(t, "ApiKey", map[string]any{"/api/v1/Settings/radarr": map[string]any{}})
	if _, err := NewOmbi(radarrOnly.URL, testKey, time.Second).Defaults(); err == nil {
		t.Fatal("expected sonarr settings to fail")
	}
	if _, err := NewOmbi(serve(t, "ApiKey", map[string]any{}).URL, testKey, time.Second).Defaults(); err == nil {
		t.Fatal("expected radarr settings to fail")
	}
	if NewOmbi("http://x", testKey, time.Second).Name() != "ombi" || NewSeerr("http://x", testKey, time.Second).Name() != "seerr" {
		t.Fatal("names")
	}
}

func TestGetterErrorsNeverShowTheKey(t *testing.T) {
	srv := serve(t, "X-Api-Key", map[string]any{"/api/v1/request": "not json", "/api/v1/big": strings.Repeat(" ", 2048)})
	cases := []struct {
		g    *getter
		path string
		want string
	}{
		{newGetter("seerr", srv.URL+"/api/v1", "X-Api-Key", "wrongwrongwrongwrongwrong", time.Second), "/request", "HTTP 401 (check the API key)"},
		{newGetter("seerr", srv.URL+"/api/v1", "X-Api-Key", testKey, time.Second), "/request?take=1", "unexpected response"},
		{newGetter("seerr", srv.URL+"/api/v1", "X-Api-Key", testKey, time.Second), "/missing", "HTTP 404"},
		{newGetter("seerr", "http://127.0.0.1:1", "X-Api-Key", testKey, time.Second), "/request", "no answer"},
		{newGetter("seerr", "http://bad host", "X-Api-Key", testKey, time.Second), "/request", "invalid request"},
	}
	for _, c := range cases {
		var v any
		err := c.g.get(c.path, &v)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "take=") {
			t.Errorf("%s: %v", c.path, err)
		}
	}
	old := client.MaxResponseBytes
	client.MaxResponseBytes = 1024
	defer func() { client.MaxResponseBytes = old }()
	var v any
	if err := newGetter("seerr", srv.URL+"/api/v1", "X-Api-Key", testKey, time.Second).get("/big", &v); err == nil {
		t.Fatal("expected the size cap to apply")
	}
	if !parseTime("nope").IsZero() {
		t.Fatal("bad time")
	}
}
