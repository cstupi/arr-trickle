"""Per-app API differences and helpers for reading their wanted-list records."""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from typing import Any

Record = dict[str, Any]


@dataclass(frozen=True)
class AppSpec:
    kind: str
    api_version: str
    app_name: str  # appName reported by /system/status
    sort_key: str  # least-recently-searched first
    include: str  # query flag that embeds the parent series/artist
    missing_command: str
    cutoff_command: str
    ids_field: str


APPS: dict[str, AppSpec] = {
    "sonarr": AppSpec(
        kind="sonarr",
        api_version="v3",
        app_name="Sonarr",
        sort_key="episodes.lastSearchTime",
        include="includeSeries",
        missing_command="SeasonSearch",  # per season, so season packs are found
        cutoff_command="EpisodeSearch",
        ids_field="episodeIds",
    ),
    "radarr": AppSpec(
        kind="radarr",
        api_version="v3",
        app_name="Radarr",
        sort_key="movies.lastSearchTime",
        include="",
        missing_command="MoviesSearch",
        cutoff_command="MoviesSearch",
        ids_field="movieIds",
    ),
    "lidarr": AppSpec(
        kind="lidarr",
        api_version="v1",
        app_name="Lidarr",
        sort_key="albums.lastSearchTime",
        include="includeArtist",
        missing_command="AlbumSearch",
        cutoff_command="AlbumSearch",
        ids_field="albumIds",
    ),
}


def _parse_time(value: str | None) -> dt.datetime | None:
    if not value:
        return None
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def is_released(kind: str, record: Record, now: dt.datetime) -> bool:
    """Only search items that can exist yet; the apps list future items as missing too."""
    if kind == "radarr":
        return bool(record.get("isAvailable"))
    when = _parse_time(record.get("releaseDate") if kind == "lidarr" else record.get("airDateUtc"))
    return when is not None and when <= now


def owner_id(kind: str, record: Record) -> int | None:
    """Id used by `exclude`: the series (Sonarr), movie (Radarr) or artist (Lidarr)."""
    key = {"sonarr": "seriesId", "radarr": "id", "lidarr": "artistId"}[kind]
    value = record.get(key)
    return value if isinstance(value, int) else None


def profile_id(kind: str, record: Record) -> int | None:
    if kind == "sonarr":
        return (record.get("series") or {}).get("qualityProfileId")
    if kind == "lidarr":
        return (record.get("artist") or {}).get("qualityProfileId")
    return record.get("qualityProfileId")


def describe(kind: str, record: Record) -> str:
    if kind == "sonarr":
        title = (record.get("series") or {}).get("title", record.get("seriesId"))
        return f"{title} S{record.get('seasonNumber', 0):02d}E{record.get('episodeNumber', 0):02d}"
    if kind == "lidarr":
        return f"{(record.get('artist') or {}).get('artistName', '?')} - {record.get('title')}"
    return f"{record.get('title')} ({record.get('year')})"
