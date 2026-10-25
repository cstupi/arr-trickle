// Package apps holds the per-app API differences and helpers for reading wanted-list records.
package apps

import (
	"fmt"
	"time"
)

// Record is one item from a /wanted page, as decoded from JSON.
type Record = map[string]any

// Spec describes how one *arr app's API differs from the others.
type Spec struct {
	Kind           string
	APIVersion     string
	AppName        string // appName reported by /system/status
	SortKey        string // least-recently-searched first
	Include        string // query flag that embeds the parent series/artist
	MissingCommand string
	CutoffCommand  string
	IDsField       string
}

// Kinds lists the supported apps in their canonical order.
var Kinds = []string{"sonarr", "radarr", "lidarr"}

// Specs maps each kind to its Spec.
var Specs = map[string]Spec{
	"sonarr": {
		Kind: "sonarr", APIVersion: "v3", AppName: "Sonarr", SortKey: "episodes.lastSearchTime",
		Include: "includeSeries", MissingCommand: "SeasonSearch", // per season, so season packs are found
		CutoffCommand: "EpisodeSearch", IDsField: "episodeIds",
	},
	"radarr": {
		Kind: "radarr", APIVersion: "v3", AppName: "Radarr", SortKey: "movies.lastSearchTime",
		MissingCommand: "MoviesSearch", CutoffCommand: "MoviesSearch", IDsField: "movieIds",
	},
	"lidarr": {
		Kind: "lidarr", APIVersion: "v1", AppName: "Lidarr", SortKey: "albums.lastSearchTime",
		Include: "includeArtist", MissingCommand: "AlbumSearch", CutoffCommand: "AlbumSearch", IDsField: "albumIds",
	},
}

// IsKnown reports whether kind is a supported app.
func IsKnown(kind string) bool {
	_, ok := Specs[kind]
	return ok
}

func parseTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil { // no zone given: the apps mean UTC
		t, err = time.Parse("2006-01-02T15:04:05", s)
	}
	return t, err == nil
}

// IsReleased reports whether an item can exist yet; the apps list future items as missing too.
func IsReleased(kind string, r Record, now time.Time) bool {
	if kind == "radarr" {
		b, _ := r["isAvailable"].(bool)
		return b
	}
	field := "airDateUtc"
	if kind == "lidarr" {
		field = "releaseDate"
	}
	t, ok := parseTime(r[field])
	return ok && !t.After(now)
}

// Int reads a JSON number field as an int.
func Int(r Record, key string) (int, bool) {
	switch v := r[key].(type) {
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

func sub(r Record, key string) Record {
	m, _ := r[key].(map[string]any)
	return m
}

// OwnerID is the id `exclude` matches: the series (Sonarr), movie (Radarr) or artist (Lidarr).
func OwnerID(kind string, r Record) (int, bool) {
	key := map[string]string{"sonarr": "seriesId", "radarr": "id", "lidarr": "artistId"}[kind]
	return Int(r, key)
}

// ProfileID is the item's quality profile id.
func ProfileID(kind string, r Record) (int, bool) {
	switch kind {
	case "sonarr":
		return Int(sub(r, "series"), "qualityProfileId")
	case "lidarr":
		return Int(sub(r, "artist"), "qualityProfileId")
	}
	return Int(r, "qualityProfileId")
}

func str(v any, fallback string) string {
	switch t := v.(type) {
	case nil:
		return fallback
	case string:
		return t
	case float64:
		if t == float64(int(t)) {
			return fmt.Sprint(int(t))
		}
	}
	return fmt.Sprint(v)
}

// Describe is a human-readable name for a record, e.g. "Firefly S01E02".
func Describe(kind string, r Record) string {
	switch kind {
	case "sonarr":
		title := r["seriesId"]
		if s := sub(r, "series"); s != nil {
			if t, ok := s["title"]; ok {
				title = t
			}
		}
		season, _ := Int(r, "seasonNumber")
		episode, _ := Int(r, "episodeNumber")
		return fmt.Sprintf("%s S%02dE%02d", str(title, "None"), season, episode)
	case "lidarr":
		artist := "?"
		if a := sub(r, "artist"); a != nil {
			artist = str(a["artistName"], "?")
		}
		return fmt.Sprintf("%s - %s", artist, str(r["title"], "None"))
	}
	return fmt.Sprintf("%s (%s)", str(r["title"], "None"), str(r["year"], "None"))
}
