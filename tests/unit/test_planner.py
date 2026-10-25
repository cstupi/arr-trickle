from arr_trickle.apps import describe, is_released, owner_id
from arr_trickle.planner import build_commands, query_cost, records_for, select
from conftest import FUTURE, NOW, album, episode, movie


def ids(records):
    return [r["id"] for r in records]


def test_skips_unreleased_and_respects_count():
    assert ids(select("lidarr", [album(1, FUTURE), album(2), album(3), album(4)], 2, NOW)) == [2, 3]


def test_missing_dates_are_not_released():
    assert not is_released("lidarr", {"releaseDate": None}, NOW)
    assert not is_released("sonarr", {}, NOW)


def test_radarr_uses_is_available():
    assert ids(select("radarr", [movie(1, available=False), movie(2)], 5, NOW)) == [2]


def test_exclude_by_owner():
    recs = [episode(1, 1, series=134), episode(2, 1, series=20)]
    assert ids(select("sonarr", recs, 5, NOW, exclude={134})) == [2]
    assert owner_id("radarr", movie(7)) == 7
    assert owner_id("lidarr", album(1, artist_id=9)) == 9


def test_cutoff_only_for_profiles_allowing_upgrades():
    assert ids(select("radarr", [movie(1, profile=4), movie(2, profile=1)], 5, NOW, upgrade_profiles={1})) == [2]


def test_per_season_takes_count_distinct_seasons():
    recs = [episode(1, 1), episode(2, 1), episode(3, 2), episode(4, 3), episode(5, 1)]
    assert ids(select("sonarr", recs, 2, NOW, per_season=True)) == [1, 2, 3, 5]


def test_sonarr_missing_becomes_one_search_per_season():
    picked = [episode(1, 1), episode(2, 1), episode(3, 2)]
    cmds = build_commands("sonarr", "missing", picked)
    assert cmds == [
        {"name": "SeasonSearch", "seriesId": 10, "seasonNumber": 1},
        {"name": "SeasonSearch", "seriesId": 10, "seasonNumber": 2},
    ]
    assert ids(records_for(cmds[0], picked)) == [1, 2]


def test_other_commands():
    assert build_commands("lidarr", "missing", [album(7), album(8)]) == [{"name": "AlbumSearch", "albumIds": [7, 8]}]
    assert build_commands("sonarr", "cutoff", [episode(5, 1)]) == [{"name": "EpisodeSearch", "episodeIds": [5]}]
    assert build_commands("radarr", "missing", [movie(3)]) == [{"name": "MoviesSearch", "movieIds": [3]}]
    assert build_commands("radarr", "missing", []) == []


def test_cost():
    assert query_cost({"name": "AlbumSearch", "albumIds": [1, 2, 3]}) == 3
    assert query_cost({"name": "SeasonSearch", "seriesId": 1, "seasonNumber": 1}) == 1


def test_describe():
    assert describe("sonarr", episode(3, 2)) == "S S02E03"
    assert describe("lidarr", album(1)) == "X - A1"
    assert describe("radarr", movie(1)) == "M1 (2000)"
