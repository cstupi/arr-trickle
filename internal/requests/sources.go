// Package requests is request sync: it finds approved Seerr and Ombi requests that never reached Radarr or
// Sonarr (the request app's add failed, e.g. on "database is locked", and it never retried) and adds them.
package requests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cstupi/arr-trickle/internal/client"
	"github.com/cstupi/arr-trickle/internal/version"
)

// Request is one approved request, as either app describes it.
type Request struct {
	Source  string // "seerr" or "ombi"
	Kind    string // "movie" or "tv"
	TMDB    int    // movies
	TVDB    int    // series
	Title   string // may be empty (Seerr's request list has none)
	Created time.Time
	Seasons []int  // series: the requested seasons
	Profile int    // quality profile id; 0 = the app's default
	Root    string // root folder path, or a root folder id as text; "" = the app's default
}

// Defaults is where a request app sends requests that don't override them.
type Defaults struct {
	MovieProfile    int
	MovieRoot       string // a path, or a root folder id as text
	MinAvailability string // Radarr's minimumAvailability, e.g. "inCinemas"
	TVProfile       int
	TVRoot          string
}

// Source is one request app (or a fake in tests).
type Source interface {
	Name() string
	// Requests is the approved requests made at or after since that the app hasn't marked available.
	Requests(since time.Time) ([]Request, error)
	Defaults() (Defaults, error)
}

// getter is a minimal JSON GET client for a request app. Like client.Client it never follows redirects,
// caps responses, and never puts the key in an error.
type getter struct {
	name, base, header, key string
	http                    *http.Client
}

func newGetter(name, rawURL, header, key string, timeout time.Duration) *getter {
	return &getter{name: name, base: strings.TrimRight(rawURL, "/"), header: header, key: key, http: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (g *getter) get(path string, v any) error {
	route, _, _ := strings.Cut(path, "?")
	req, err := http.NewRequest(http.MethodGet, g.base+path, nil)
	if err != nil {
		return &client.Error{Msg: fmt.Sprintf("%s: GET %s: invalid request", g.name, route)}
	}
	req.Header.Set(g.header, g.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "arr-trickle/"+version.Version)
	resp, err := g.http.Do(req)
	if err != nil {
		return &client.Error{Msg: fmt.Sprintf("%s: GET %s: no answer", g.name, route), NoConn: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			hint = " (check the API key)"
		}
		return &client.Error{Msg: fmt.Sprintf("%s: GET %s: HTTP %d%s", g.name, route, resp.StatusCode, hint), Status: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, client.MaxResponseBytes+1))
	if err == nil && int64(len(b)) > client.MaxResponseBytes {
		err = fmt.Errorf("response too large")
	}
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		return &client.Error{Msg: fmt.Sprintf("%s: GET %s: unexpected response (wrong URL or base path?)", g.name, route)}
	}
	return nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// --- Seerr ---------------------------------------------------------------------

// Seerr request and media statuses (the app's enums).
const (
	seerrApproved     = 2
	seerrFailed       = 4
	seerrAvailable    = 5
	seerrDeleted      = 7
	seerrPageSize     = 100
	seerrMaxRequested = 50 // pages; the list is newest first, so a pass rarely reads more than one
)

// Seerr reads Seerr (or Overseerr/Jellyseerr, which share its API).
type Seerr struct{ g *getter }

// NewSeerr builds a Seerr source.
func NewSeerr(rawURL, key string, timeout time.Duration) *Seerr {
	return &Seerr{newGetter("seerr", rawURL+"/api/v1", "X-Api-Key", key, timeout)}
}

// Name is "seerr".
func (s *Seerr) Name() string { return "seerr" }

// Requests reads the request list newest first until it passes since. Approved and failed requests count:
// "failed" is Seerr saying its own add was refused.
func (s *Seerr) Requests(since time.Time) ([]Request, error) {
	var out []Request
	for page := range seerrMaxRequested {
		var resp struct {
			Results []struct {
				Status     int    `json:"status"`
				CreatedAt  string `json:"createdAt"`
				Type       string `json:"type"`
				Is4k       bool   `json:"is4k"`
				ProfileID  *int   `json:"profileId"`
				RootFolder string `json:"rootFolder"`
				Seasons    []struct {
					SeasonNumber int `json:"seasonNumber"`
				} `json:"seasons"`
				Media struct {
					TmdbID int `json:"tmdbId"`
					TvdbID int `json:"tvdbId"`
					Status int `json:"status"`
				} `json:"media"`
			} `json:"results"`
		}
		q := url.Values{"take": {strconv.Itoa(seerrPageSize)}, "skip": {strconv.Itoa(page * seerrPageSize)}, "filter": {"all"}, "sort": {"added"}}
		if err := s.g.get("/request?"+q.Encode(), &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Results {
			created := parseTime(r.CreatedAt)
			if created.Before(since) {
				return out, nil
			}
			if (r.Status != seerrApproved && r.Status != seerrFailed) || r.Is4k ||
				r.Media.Status == seerrAvailable || r.Media.Status == seerrDeleted {
				continue
			}
			req := Request{Source: "seerr", Kind: r.Type, TMDB: r.Media.TmdbID, TVDB: r.Media.TvdbID, Created: created, Root: r.RootFolder}
			if r.ProfileID != nil {
				req.Profile = *r.ProfileID
			}
			for _, season := range r.Seasons {
				req.Seasons = append(req.Seasons, season.SeasonNumber)
			}
			out = append(out, req)
		}
		if len(resp.Results) < seerrPageSize {
			break
		}
	}
	return out, nil
}

// Defaults is Seerr's default (non-4K) Radarr and Sonarr server.
func (s *Seerr) Defaults() (Defaults, error) {
	type server struct {
		IsDefault           bool   `json:"isDefault"`
		Is4k                bool   `json:"is4k"`
		ActiveProfileID     int    `json:"activeProfileId"`
		ActiveDirectory     string `json:"activeDirectory"`
		MinimumAvailability string `json:"minimumAvailability"`
	}
	pick := func(path string) (server, error) {
		var list []server
		if err := s.g.get(path, &list); err != nil {
			return server{}, err
		}
		if i := slices.IndexFunc(list, func(s server) bool { return s.IsDefault && !s.Is4k }); i >= 0 {
			return list[i], nil
		}
		return server{}, nil
	}
	radarr, err := pick("/settings/radarr")
	if err != nil {
		return Defaults{}, err
	}
	sonarr, err := pick("/settings/sonarr")
	if err != nil {
		return Defaults{}, err
	}
	return Defaults{
		MovieProfile: radarr.ActiveProfileID, MovieRoot: radarr.ActiveDirectory, MinAvailability: radarr.MinimumAvailability,
		TVProfile: sonarr.ActiveProfileID, TVRoot: sonarr.ActiveDirectory,
	}, nil
}

// --- Ombi ----------------------------------------------------------------------

// Ombi reads Ombi. The URL includes Ombi's base path, if it has one (e.g. http://ombi:3579/ombi).
type Ombi struct{ g *getter }

// NewOmbi builds an Ombi source.
func NewOmbi(rawURL, key string, timeout time.Duration) *Ombi {
	return &Ombi{newGetter("ombi", rawURL+"/api/v1", "ApiKey", key, timeout)}
}

// Name is "ombi".
func (o *Ombi) Name() string { return "ombi" }

func ombiOverride(id int) string {
	if id <= 0 {
		return ""
	}
	return strconv.Itoa(id)
}

// Requests is Ombi's approved, not denied, not available movie and series requests.
func (o *Ombi) Requests(since time.Time) ([]Request, error) {
	var movies []struct {
		TheMovieDbID     int    `json:"theMovieDbId"`
		Title            string `json:"title"`
		Approved         bool   `json:"approved"`
		Available        bool   `json:"available"`
		Denied           *bool  `json:"denied"`
		Is4kRequest      bool   `json:"is4kRequest"`
		RequestedDate    string `json:"requestedDate"`
		QualityOverride  int    `json:"qualityOverride"`
		RootPathOverride int    `json:"rootPathOverride"`
	}
	if err := o.g.get("/Request/movie", &movies); err != nil {
		return nil, err
	}
	var out []Request
	for _, m := range movies {
		created := parseTime(m.RequestedDate)
		if !m.Approved || m.Available || (m.Denied != nil && *m.Denied) || m.Is4kRequest || created.Before(since) || m.TheMovieDbID <= 0 {
			continue
		}
		out = append(out, Request{Source: "ombi", Kind: "movie", TMDB: m.TheMovieDbID, Title: m.Title, Created: created,
			Profile: m.QualityOverride, Root: ombiOverride(m.RootPathOverride)})
	}
	var shows []struct {
		TvDbID          int    `json:"tvDbId"`
		Title           string `json:"title"`
		QualityOverride int    `json:"qualityOverride"`
		RootFolder      int    `json:"rootFolder"`
		ChildRequests   []struct {
			Approved       bool   `json:"approved"`
			Available      bool   `json:"available"`
			Denied         *bool  `json:"denied"`
			RequestedDate  string `json:"requestedDate"`
			SeasonRequests []struct {
				SeasonNumber int `json:"seasonNumber"`
			} `json:"seasonRequests"`
		} `json:"childRequests"`
	}
	if err := o.g.get("/Request/tv", &shows); err != nil {
		return nil, err
	}
	for _, s := range shows {
		if s.TvDbID <= 0 { // Ombi couldn't resolve the show either; nothing to add it by
			continue
		}
		req := Request{Source: "ombi", Kind: "tv", TVDB: s.TvDbID, Title: s.Title, Profile: s.QualityOverride, Root: ombiOverride(s.RootFolder)}
		for _, c := range s.ChildRequests {
			created := parseTime(c.RequestedDate)
			if !c.Approved || c.Available || (c.Denied != nil && *c.Denied) || created.Before(since) {
				continue
			}
			if created.After(req.Created) { // the newest approved child request
				req.Created = created
			}
			for _, season := range c.SeasonRequests {
				req.Seasons = append(req.Seasons, season.SeasonNumber)
			}
		}
		if !req.Created.IsZero() {
			slices.Sort(req.Seasons)
			req.Seasons = slices.Compact(req.Seasons)
			out = append(out, req)
		}
	}
	return out, nil
}

// Defaults is Ombi's Radarr and Sonarr settings. Ombi stores Sonarr's root folder as an id.
func (o *Ombi) Defaults() (Defaults, error) {
	var radarr struct {
		Radarr struct {
			DefaultQualityProfile string `json:"defaultQualityProfile"`
			DefaultRootPath       string `json:"defaultRootPath"`
			MinimumAvailability   string `json:"minimumAvailability"`
		} `json:"radarr"`
	}
	if err := o.g.get("/Settings/radarr", &radarr); err != nil {
		return Defaults{}, err
	}
	var sonarr struct {
		QualityProfile string `json:"qualityProfile"`
		RootPath       string `json:"rootPath"`
	}
	if err := o.g.get("/Settings/sonarr", &sonarr); err != nil {
		return Defaults{}, err
	}
	movieProfile, _ := strconv.Atoi(radarr.Radarr.DefaultQualityProfile)
	tvProfile, _ := strconv.Atoi(sonarr.QualityProfile)
	minAvail := radarr.Radarr.MinimumAvailability // Ombi writes "InCinemas"; Radarr's API spells it "inCinemas"
	if minAvail != "" {
		minAvail = strings.ToLower(minAvail[:1]) + minAvail[1:]
	}
	return Defaults{
		MovieProfile: movieProfile, MovieRoot: radarr.Radarr.DefaultRootPath, MinAvailability: minAvail,
		TVProfile: tvProfile, TVRoot: sonarr.RootPath,
	}, nil
}
