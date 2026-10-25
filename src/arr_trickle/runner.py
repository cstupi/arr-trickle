"""One pass over every configured instance, and the `check` connectivity probe."""

from __future__ import annotations

import datetime as dt
import logging
from dataclasses import dataclass, field
from typing import Protocol

from arr_trickle.apps import APPS, Record, describe
from arr_trickle.client import ArrClient, ArrError
from arr_trickle.config import ConfigError, Instance
from arr_trickle.planner import build_commands, query_cost, records_for, select
from arr_trickle.state import Budget

log = logging.getLogger("arr-trickle")

# Health checks meaning searches would fail or pile up right now (indexer in backoff, no client).
BLOCKING_HEALTH = frozenset(
    {
        "IndexerStatusCheck",
        "IndexerLongTermStatusCheck",
        "IndexerSearchCheck",
        "DownloadClientCheck",
        "DownloadClientStatusCheck",
    }
)


class Client(Protocol):
    def health(self) -> list[dict[str, object]]: ...
    def queue_size(self) -> int: ...
    def upgrade_profiles(self) -> set[int]: ...
    def wanted(self, kind: str, page_size: int) -> tuple[list[Record], str | None]: ...
    def command(self, body: dict[str, object]) -> dict[str, object]: ...


def run_instance(inst: Instance, client: Client, budget: Budget, dry_run: bool, now: dt.datetime | None = None) -> int:
    """Send this run's searches for one instance; returns the approximate queries spent."""
    now = now or dt.datetime.now(dt.UTC)
    blocking = [h for h in client.health() if h.get("source") in BLOCKING_HEALTH]
    if blocking:
        log.info("%s: skipped: %s", inst.name, blocking[0].get("message", "indexer or client unavailable"))
        return 0
    if inst.max_queue and client.queue_size() >= inst.max_queue:
        log.info("%s: skipped: download queue at or above %d", inst.name, inst.max_queue)
        return 0

    remaining = inst.daily_cap - budget.used(inst.name) if inst.daily_cap else None
    spent = 0
    for wanted, configured in (("missing", inst.missing), ("cutoff", inst.cutoff)):
        if configured <= 0:
            continue
        count = configured
        if remaining is not None:
            if remaining <= 0:
                log.info("%s: daily cap of %d searches reached", inst.name, inst.daily_cap)
                break
            count = min(count, remaining)
        upgrades = client.upgrade_profiles() if wanted == "cutoff" else None
        # Over-fetch: unreleased, excluded and no-upgrade records are filtered here.
        records, _ = client.wanted(wanted, max(count * 10, 50))
        picked = select(
            inst.kind,
            records,
            count,
            now,
            exclude=inst.exclude,
            upgrade_profiles=upgrades,
            per_season=inst.kind == "sonarr" and wanted == "missing",
        )
        for cmd in build_commands(inst.kind, wanted, picked):
            cost = query_cost(cmd)
            names = [describe(inst.kind, r) for r in records_for(cmd, picked)]
            shown = "; ".join(names[:10]) + (f" (+{len(names) - 10} more)" if len(names) > 10 else "")
            log.info("%s: %s%s %s: %s", inst.name, "[dry run] " if dry_run else "", wanted, cmd["name"], shown)
            if not dry_run:
                client.command(cmd)
                budget.add(inst.name, cost)
            spent += cost
            if remaining is not None:
                remaining -= cost
    return spent


def run_once(instances: tuple[Instance, ...], budget: Budget, dry_run: bool) -> bool:
    """Returns False if any instance failed (the others still run)."""
    ok = True
    for inst in instances:
        try:
            client = ArrClient(inst.kind, inst.url, inst.api_key(), inst.timeout_seconds)
            run_instance(inst, client, budget, dry_run)
        except (ArrError, ConfigError, KeyError, TypeError, ValueError) as e:
            log.warning("%s: run failed: %s", inst.name, e)
            ok = False
    return ok


@dataclass
class CheckResult:
    name: str
    ok: bool = True
    lines: list[str] = field(default_factory=list)

    def fail(self, msg: str) -> None:
        self.ok = False
        self.lines.append(f"FAIL {msg}")

    def good(self, msg: str) -> None:
        self.lines.append(f"ok   {msg}")


def check_instance(inst: Instance, client: ArrClient | None = None) -> CheckResult:
    """Read-only probe of every endpoint arr-trickle uses. Sends no searches."""
    res = CheckResult(inst.name)
    try:
        client = client or ArrClient(inst.kind, inst.url, inst.api_key(), inst.timeout_seconds)
        status = client.status()
        expected = APPS[inst.kind].app_name
        if status.get("appName") != expected:
            res.fail(f"{inst.url} is {status.get('appName')!r}, expected {expected!r}")
            return res
        res.good(f"connected: {expected} {status.get('version')}")
        problems = [h for h in client.health() if h.get("source") in BLOCKING_HEALTH]
        if problems:
            res.good(f"health: searches would be skipped now: {problems[0].get('message')}")
        else:
            res.good("health: no indexer or download client problems")
        res.good(f"queue: {client.queue_size()} item(s)")
        res.good(f"quality profiles allowing upgrades: {sorted(client.upgrade_profiles()) or 'none'}")
        for wanted in ("missing", "cutoff"):
            _records, sort_key = client.wanted(wanted, 1)
            if sort_key != APPS[inst.kind].sort_key:
                res.fail(f"wanted/{wanted}: app ignored sortKey (got {sort_key!r}); oldest-first order not guaranteed")
            else:
                res.good(f"wanted/{wanted}: reachable, sorted by {sort_key}")
    except (ArrError, ConfigError, KeyError, TypeError, ValueError) as e:
        res.fail(str(e))
    return res
