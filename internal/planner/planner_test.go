package planner

import (
	"reflect"
	"testing"

	"github.com/cstupi/arr-trickle/internal/apps"
	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

func ids(recs []apps.Record) []int {
	out := []int{}
	for _, r := range recs {
		id, _ := apps.Int(r, "id")
		out = append(out, id)
	}
	return out
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestSkipsUnreleasedAndRespectsCount(t *testing.T) {
	recs := []apps.Record{tu.Album(1, tu.Future, 1, 1), tu.A(2), tu.A(3), tu.A(4)}
	eq(t, ids(Select("lidarr", recs, 2, tu.Now, Options{})), []int{2, 3})
}

func TestMissingDatesAreNotReleased(t *testing.T) {
	if apps.IsReleased("lidarr", apps.Record{"releaseDate": nil}, tu.Now) || apps.IsReleased("sonarr", apps.Record{}, tu.Now) {
		t.Fatal("records without dates must not count as released")
	}
}

func TestRadarrUsesIsAvailable(t *testing.T) {
	eq(t, ids(Select("radarr", []apps.Record{tu.Movie(1, false, 1), tu.M(2)}, 5, tu.Now, Options{})), []int{2})
}

func TestExcludeByOwner(t *testing.T) {
	recs := []apps.Record{tu.Episode(1, 1, 134, tu.Past, 1), tu.Episode(2, 1, 20, tu.Past, 1)}
	eq(t, ids(Select("sonarr", recs, 5, tu.Now, Options{Exclude: map[int]bool{134: true}})), []int{2})
	if id, _ := apps.OwnerID("radarr", tu.M(7)); id != 7 {
		t.Fatal("radarr owner is the movie")
	}
	if id, _ := apps.OwnerID("lidarr", tu.Album(1, tu.Past, 1, 9)); id != 9 {
		t.Fatal("lidarr owner is the artist")
	}
}

func TestCutoffOnlyForProfilesAllowingUpgrades(t *testing.T) {
	recs := []apps.Record{tu.Movie(1, true, 4), tu.Movie(2, true, 1)}
	eq(t, ids(Select("radarr", recs, 5, tu.Now, Options{UpgradeProfiles: map[int]bool{1: true}})), []int{2})
}

func TestPerSeasonTakesCountDistinctSeasons(t *testing.T) {
	recs := []apps.Record{tu.E(1, 1), tu.E(2, 1), tu.E(3, 2), tu.E(4, 3), tu.E(5, 1)}
	eq(t, ids(Select("sonarr", recs, 2, tu.Now, Options{PerSeason: true})), []int{1, 2, 3, 5})
}

func TestSonarrMissingBecomesOneSearchPerSeason(t *testing.T) {
	picked := []apps.Record{tu.E(1, 1), tu.E(2, 1), tu.E(3, 2)}
	cmds := BuildCommands("sonarr", "missing", picked)
	eq(t, cmds, []Command{
		{"name": "SeasonSearch", "seriesId": 10, "seasonNumber": 1},
		{"name": "SeasonSearch", "seriesId": 10, "seasonNumber": 2},
	})
	eq(t, ids(RecordsFor(cmds[0], picked)), []int{1, 2})
}

func TestOtherCommands(t *testing.T) {
	eq(t, BuildCommands("lidarr", "missing", []apps.Record{tu.A(7), tu.A(8)}), []Command{{"name": "AlbumSearch", "albumIds": []int{7, 8}}})
	eq(t, BuildCommands("sonarr", "cutoff", []apps.Record{tu.E(5, 1)}), []Command{{"name": "EpisodeSearch", "episodeIds": []int{5}}})
	eq(t, BuildCommands("radarr", "missing", []apps.Record{tu.M(3)}), []Command{{"name": "MoviesSearch", "movieIds": []int{3}}})
	if BuildCommands("radarr", "missing", nil) != nil {
		t.Fatal("no records, no commands")
	}
	eq(t, len(RecordsFor(Command{"name": "AlbumSearch"}, []apps.Record{tu.A(1)})), 1)
}

func TestCost(t *testing.T) {
	eq(t, QueryCost(Command{"name": "AlbumSearch", "albumIds": []int{1, 2, 3}}), 3)
	eq(t, QueryCost(Command{"name": "SeasonSearch", "seriesId": 1, "seasonNumber": 1}), 1)
}

func TestDescribe(t *testing.T) {
	eq(t, apps.Describe("sonarr", tu.E(3, 2)), "S S02E03")
	eq(t, apps.Describe("lidarr", tu.A(1)), "X - A1")
	eq(t, apps.Describe("radarr", tu.M(1)), "M1 (2000)")
	eq(t, apps.Describe("lidarr", apps.Record{"title": "T"}), "? - T")
	eq(t, apps.Describe("sonarr", apps.Record{"seriesId": float64(4)}), "4 S00E00")
}

func TestTimesWithoutZoneAreUTC(t *testing.T) {
	if !apps.IsReleased("lidarr", apps.Record{"releaseDate": "2020-01-01T00:00:00"}, tu.Now) {
		t.Fatal("a zone-less past date is released")
	}
}
