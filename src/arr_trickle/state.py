"""Small persistent state: the per-instance daily search budget and a heartbeat for health checks."""

from __future__ import annotations

import datetime as dt
import json
import logging
import os
import time
from pathlib import Path

log = logging.getLogger("arr-trickle")


def _utc_day() -> str:
    return dt.datetime.now(dt.UTC).strftime("%Y-%m-%d")


def _atomic_write(path: Path, text: str) -> None:
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(text, encoding="utf-8")
    os.replace(tmp, path)


class Budget:
    """Searches sent per instance per UTC day; survives restarts so a crash loop can't overspend."""

    def __init__(self, path: Path | None) -> None:
        self.path = path
        self._data: dict[str, dict[str, int]] = {}
        if path and path.exists():
            try:
                loaded = json.loads(path.read_text(encoding="utf-8"))
                if isinstance(loaded, dict):
                    self._data = {
                        str(day): {str(k): int(v) for k, v in counts.items()}
                        for day, counts in loaded.items()
                        if isinstance(counts, dict)
                    }
            except (OSError, ValueError, TypeError, AttributeError):
                log.warning("could not read %s; starting the daily budget fresh", path)

    def used(self, name: str) -> int:
        return self._data.get(_utc_day(), {}).get(name, 0)

    def add(self, name: str, n: int) -> None:
        day = _utc_day()
        today = self._data.get(day, {})
        today[name] = today.get(name, 0) + n
        self._data = {day: today}  # older days are dropped
        if self.path:
            _atomic_write(self.path, json.dumps(self._data))


def beat(path: Path) -> None:
    _atomic_write(path, str(int(time.time())))


def heartbeat_age(path: Path) -> float | None:
    try:
        return time.time() - int(path.read_text(encoding="utf-8").strip())
    except (OSError, ValueError):
        return None
