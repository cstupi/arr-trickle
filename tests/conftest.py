import datetime as dt
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

NOW = dt.datetime(2026, 10, 24, tzinfo=dt.UTC)
PAST, FUTURE = "2020-01-01T00:00:00Z", "2027-01-01T00:00:00Z"
KEY = "fakefakefakefakefakefakefakefake"  # obviously fake; real keys never appear in tests


def album(i, date=PAST, profile=1, artist_id=1):
    return {
        "id": i,
        "title": f"A{i}",
        "releaseDate": date,
        "artistId": artist_id,
        "artist": {"artistName": "X", "qualityProfileId": profile},
    }


def episode(i, season, series=10, date=PAST, profile=1):
    return {
        "id": i,
        "seriesId": series,
        "seasonNumber": season,
        "episodeNumber": i,
        "airDateUtc": date,
        "series": {"title": "S", "qualityProfileId": profile},
    }


def movie(i, available=True, profile=1):
    return {"id": i, "title": f"M{i}", "year": 2000, "isAvailable": available, "qualityProfileId": profile}


@pytest.fixture
def arr_config_xml(tmp_path):
    path = tmp_path / "config.xml"
    path.write_text(f"<Config>\n  <Port>8989</Port>\n  <ApiKey>{KEY}</ApiKey>\n</Config>\n")
    return path
