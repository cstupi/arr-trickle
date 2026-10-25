"""Choosing what to search: pure functions, no I/O."""

from __future__ import annotations

import datetime as dt
from collections.abc import Collection, Iterable
from typing import Any

from arr_trickle.apps import APPS, Record, is_released, owner_id, profile_id


def select(
    kind: str,
    records: Iterable[Record],
    count: int,
    now: dt.datetime,
    *,
    exclude: Collection[int] = (),
    upgrade_profiles: Collection[int] | None = None,
    per_season: bool = False,
) -> list[Record]:
    """Pick searchable records in the order given (the app sorts oldest-search first).

    Normally up to `count` records. With per_season, records from up to `count` distinct
    seasons: every missing episode of a chosen season rides along on its SeasonSearch.
    """
    picked: list[Record] = []
    seasons: set[tuple[Any, Any]] = set()
    for r in records:
        if not is_released(kind, r, now) or owner_id(kind, r) in exclude:
            continue
        if upgrade_profiles is not None and profile_id(kind, r) not in upgrade_profiles:
            continue
        if per_season:
            key = (r.get("seriesId"), r.get("seasonNumber"))
            if key not in seasons:
                if len(seasons) >= count:
                    continue
                seasons.add(key)
        elif len(picked) >= count:
            break
        picked.append(r)
    return picked


def build_commands(kind: str, wanted: str, records: list[Record]) -> list[dict[str, Any]]:
    """Turn selected records into /command bodies."""
    if not records:
        return []
    spec = APPS[kind]
    if kind == "sonarr" and wanted == "missing":
        seasons: list[tuple[int, int]] = []
        for r in records:
            key = (r["seriesId"], r["seasonNumber"])
            if key not in seasons:
                seasons.append(key)
        return [{"name": "SeasonSearch", "seriesId": s, "seasonNumber": n} for s, n in seasons]
    name = spec.missing_command if wanted == "missing" else spec.cutoff_command
    return [{"name": name, spec.ids_field: [r["id"] for r in records]}]


def query_cost(command: dict[str, Any]) -> int:
    """Approximate indexer queries a command causes, per indexer."""
    for field in ("albumIds", "episodeIds", "movieIds"):
        if field in command:
            return len(command[field])
    return 1


def records_for(command: dict[str, Any], records: list[Record]) -> list[Record]:
    """The selected records a command covers (for logging)."""
    if command.get("name") == "SeasonSearch":
        return [
            r
            for r in records
            if r.get("seriesId") == command["seriesId"] and r.get("seasonNumber") == command["seasonNumber"]
        ]
    return records
