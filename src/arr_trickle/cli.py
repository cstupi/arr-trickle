"""Command line: `run` (default), `check`, `validate`, `healthcheck`."""

from __future__ import annotations

import argparse
import logging
import sys
import time
from pathlib import Path

from arr_trickle import __version__
from arr_trickle.config import ConfigError, Settings, load
from arr_trickle.runner import check_instance, run_once
from arr_trickle.state import Budget, beat, heartbeat_age

log = logging.getLogger("arr-trickle")


def _setup_logging(level: str) -> None:
    logging.basicConfig(level=level, format="%(asctime)s %(levelname)s %(message)s", stream=sys.stdout)


def cmd_run(settings: Settings, once: bool) -> int:
    state = Path(settings.state_dir)
    budget = Budget(state / "budget.json")
    names = ", ".join(f"{i.name}({i.kind})" for i in settings.instances)
    log.info(
        "arr-trickle %s: %s every %d min%s",
        __version__,
        names,
        settings.interval_minutes,
        " [dry run]" if settings.dry_run else "",
    )
    while True:
        ok = run_once(settings.instances, budget, settings.dry_run)
        beat(state / "heartbeat")
        if once:
            return 0 if ok else 1
        time.sleep(settings.interval_minutes * 60)


def cmd_check(settings: Settings) -> int:
    all_ok = True
    for inst in settings.instances:
        res = check_instance(inst)
        print(f"[{res.name}] {inst.url}")
        for line in res.lines:
            print(f"  {line}")
        all_ok &= res.ok
    return 0 if all_ok else 1


def cmd_healthcheck(settings: Settings) -> int:
    age = heartbeat_age(Path(settings.state_dir) / "heartbeat")
    limit = settings.interval_minutes * 60 * 2 + 300
    if age is None or age > limit:
        print(f"unhealthy: last run {'never' if age is None else f'{int(age)}s ago'}")
        return 1
    print(f"healthy: last run {int(age)}s ago")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="arr-trickle", description=__doc__)
    parser.add_argument("--config", help="TOML settings file (default: $ARR_TRICKLE_CONFIG or /config/config.toml)")
    parser.add_argument("--version", action="version", version=f"arr-trickle {__version__}")
    sub = parser.add_subparsers(dest="command")
    run_p = sub.add_parser("run", help="search on a schedule (default)")
    run_p.add_argument("--once", action="store_true", help="do a single pass and exit")
    sub.add_parser("check", help="read-only connectivity and API check; sends no searches")
    sub.add_parser("validate", help="validate the settings and exit")
    sub.add_parser("healthcheck", help="exit 0 if a pass completed recently (for Docker HEALTHCHECK)")
    args = parser.parse_args(argv)

    try:
        settings = load(args.config)
    except ConfigError as e:
        print(f"configuration error: {e}", file=sys.stderr)
        return 2
    _setup_logging(settings.log_level)

    if args.command == "validate":
        for inst in settings.instances:
            try:
                inst.api_key()
            except ConfigError as e:
                print(f"configuration error: {e}", file=sys.stderr)
                return 2
        print(f"ok: {len(settings.instances)} instance(s): " + ", ".join(i.name for i in settings.instances))
        return 0
    if args.command == "check":
        return cmd_check(settings)
    if args.command == "healthcheck":
        return cmd_healthcheck(settings)
    return cmd_run(settings, once=getattr(args, "once", False))
