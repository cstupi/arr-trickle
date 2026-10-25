"""Settings: a TOML file (preferred) or environment variables (single-instance shortcut).

API keys never live in the settings. Each instance names where to read its key from:
the app's own config.xml (`api_key_from`), a file such as a Docker secret (`api_key_file`),
or an environment variable (`api_key_env`).
"""

from __future__ import annotations

import ipaddress
import logging
import os
import re
import stat
import tomllib
import urllib.parse
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from arr_trickle.apps import APPS

log = logging.getLogger("arr-trickle")

DEFAULT_CONFIG_PATH = "/config/config.toml"
_API_KEY_RE = re.compile(r"^[A-Za-z0-9]{20,64}$")
_XML_KEY_RE = re.compile(r"<ApiKey>\s*([^<\s]+)\s*</ApiKey>")


class ConfigError(ValueError):
    """Invalid or unusable settings; the message is safe to show (it never contains a key)."""


@dataclass(frozen=True)
class Instance:
    kind: str
    name: str
    url: str
    api_key_from: str | None = None
    api_key_file: str | None = None
    api_key_env: str | None = None
    missing: int = 5
    cutoff: int = 0
    daily_cap: int = 0
    max_queue: int = 0
    exclude: frozenset[int] = frozenset()
    timeout_seconds: int = 60

    def api_key(self, environ: Mapping[str, str] | None = None) -> str:
        environ = os.environ if environ is None else environ
        if self.api_key_from:
            key = _key_from_arr_config(self.api_key_from)
        elif self.api_key_file:
            key = _read_secret_file(self.api_key_file)
        else:
            assert self.api_key_env
            key = environ.get(self.api_key_env, "").strip()
            if not key:
                raise ConfigError(f"{self.name}: environment variable {self.api_key_env} is empty")
        if not _API_KEY_RE.match(key):
            raise ConfigError(f"{self.name}: API key has an unexpected format")
        return key


@dataclass(frozen=True)
class Settings:
    instances: tuple[Instance, ...]
    interval_minutes: int = 60
    state_dir: str = "/state"
    dry_run: bool = False
    log_level: str = "INFO"
    allow_insecure_http: bool = False


_TOP_KEYS = {"interval_minutes", "state_dir", "dry_run", "log_level", "allow_insecure_http", *APPS}
_INSTANCE_KEYS = {
    "name",
    "url",
    "api_key_from",
    "api_key_file",
    "api_key_env",
    "missing",
    "cutoff",
    "daily_cap",
    "max_queue",
    "exclude",
    "timeout_seconds",
}
_LOG_LEVELS = {"DEBUG", "INFO", "WARNING", "ERROR"}


def _int(where: str, value: Any, lo: int, hi: int) -> int:
    # bool is a subclass of int; `missing = true` is a mistake, not 1.
    if isinstance(value, bool) or not isinstance(value, int):
        raise ConfigError(f"{where} must be an integer")
    if not lo <= value <= hi:
        raise ConfigError(f"{where} must be between {lo} and {hi}")
    return value


def _bool(where: str, value: Any) -> bool:
    if not isinstance(value, bool):
        raise ConfigError(f"{where} must be true or false")
    return value


def _str(where: str, value: Any) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ConfigError(f"{where} must be a non-empty string")
    return value.strip()


def is_private_host(host: str) -> bool:
    """True for hosts where plain http doesn't cross an untrusted network."""
    if host == "localhost" or "." not in host:  # localhost or a Docker service name
        return True
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        return host.endswith((".local", ".internal", ".lan", ".home.arpa"))
    return ip.is_private or ip.is_loopback or ip.is_link_local


def _url(where: str, value: Any, allow_insecure_http: bool) -> str:
    url = _str(where, value).rstrip("/")
    parts = urllib.parse.urlsplit(url)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise ConfigError(f"{where} must be an http:// or https:// URL")
    if parts.username or parts.password or parts.query or parts.fragment:
        raise ConfigError(f"{where} must not contain credentials, a query or a fragment")
    if parts.scheme == "http" and not allow_insecure_http and not is_private_host(parts.hostname):
        raise ConfigError(
            f"{where}: refusing to send an API key over plain http to public host {parts.hostname!r}; "
            "use https or set allow_insecure_http = true"
        )
    return url


def _instance(kind: str, index: int, raw: Any, allow_insecure_http: bool) -> Instance:
    where = f"{kind}[{index}]"
    if not isinstance(raw, dict):
        raise ConfigError(f"{where} must be a table ([[{kind}]])")
    unknown = set(raw) - _INSTANCE_KEYS
    if unknown:
        raise ConfigError(f"{where}: unknown setting(s) {', '.join(sorted(unknown))}")
    sources = [k for k in ("api_key_from", "api_key_file", "api_key_env") if k in raw]
    if len(sources) != 1:
        raise ConfigError(f"{where}: set exactly one of api_key_from, api_key_file, api_key_env")
    exclude = raw.get("exclude", [])
    if not isinstance(exclude, list) or not all(
        isinstance(i, int) and not isinstance(i, bool) and i > 0 for i in exclude
    ):
        raise ConfigError(f"{where}.exclude must be a list of positive integer ids")
    name = _str(f"{where}.name", raw.get("name", kind if index == 0 else f"{kind}-{index + 1}"))
    return Instance(
        kind=kind,
        name=name,
        url=_url(f"{where}.url", raw.get("url"), allow_insecure_http),
        **{k: _str(f"{where}.{k}", raw[k]) for k in sources},
        missing=_int(f"{where}.missing", raw.get("missing", 5), 0, 100),
        cutoff=_int(f"{where}.cutoff", raw.get("cutoff", 0), 0, 100),
        daily_cap=_int(f"{where}.daily_cap", raw.get("daily_cap", 0), 0, 100_000),
        max_queue=_int(f"{where}.max_queue", raw.get("max_queue", 0), 0, 100_000),
        exclude=frozenset(exclude),
        timeout_seconds=_int(f"{where}.timeout_seconds", raw.get("timeout_seconds", 60), 5, 600),
    )


def parse(data: Mapping[str, Any]) -> Settings:
    unknown = set(data) - _TOP_KEYS
    if unknown:
        raise ConfigError(f"unknown setting(s) {', '.join(sorted(unknown))}")
    allow_http = _bool("allow_insecure_http", data.get("allow_insecure_http", False))
    instances: list[Instance] = []
    for kind in APPS:
        entries = data.get(kind, [])
        if isinstance(entries, dict):
            entries = [entries]  # tolerate [sonarr] for a single instance
        if not isinstance(entries, list):
            raise ConfigError(f"{kind} must be a table or an array of tables")
        instances += [_instance(kind, i, e, allow_http) for i, e in enumerate(entries)]
    if not instances:
        raise ConfigError("no apps configured; add at least one [[sonarr]], [[radarr]] or [[lidarr]]")
    names = [i.name for i in instances]
    duplicates = sorted({n for n in names if names.count(n) > 1})
    if duplicates:
        raise ConfigError(f"duplicate instance name(s): {', '.join(duplicates)}")
    log_level = _str("log_level", data.get("log_level", "INFO")).upper()
    if log_level not in _LOG_LEVELS:
        raise ConfigError(f"log_level must be one of {', '.join(sorted(_LOG_LEVELS))}")
    return Settings(
        instances=tuple(instances),
        interval_minutes=_int("interval_minutes", data.get("interval_minutes", 60), 5, 1440),
        state_dir=_str("state_dir", data.get("state_dir", "/state")),
        dry_run=_bool("dry_run", data.get("dry_run", False)),
        log_level=log_level,
        allow_insecure_http=allow_http,
    )


def from_env(environ: Mapping[str, str]) -> dict[str, Any]:
    """Build the same structure as the TOML file from SONARR_URL-style variables (one instance per app)."""

    def num(name: str, default: int) -> int:
        raw = environ.get(name, "").strip()
        if not raw:
            return default
        try:
            return int(raw)
        except ValueError:
            raise ConfigError(f"{name} must be an integer") from None

    data: dict[str, Any] = {}
    for kind in APPS:
        p = kind.upper()
        if not environ.get(f"{p}_URL"):
            continue
        entry: dict[str, Any] = {
            "url": environ[f"{p}_URL"],
            "missing": num(f"{p}_MISSING", 5),
            "cutoff": num(f"{p}_CUTOFF", 0),
            "daily_cap": num(f"{p}_DAILY_CAP", 0),
            "max_queue": num(f"{p}_MAX_QUEUE", 0),
            "exclude": _id_list(f"{p}_EXCLUDE", environ.get(f"{p}_EXCLUDE", "")),
        }
        if environ.get(f"{p}_API_KEY_FILE"):
            entry["api_key_file"] = environ[f"{p}_API_KEY_FILE"]
        else:
            entry["api_key_from"] = environ.get(f"{p}_CONFIG", f"/arr/{kind}/config.xml")
        data[kind] = [entry]
    if environ.get("INTERVAL_MINUTES"):
        data["interval_minutes"] = num("INTERVAL_MINUTES", 60)
    if environ.get("STATE_DIR"):
        data["state_dir"] = environ["STATE_DIR"]
    if environ.get("DRY_RUN"):
        data["dry_run"] = environ["DRY_RUN"].strip().lower() in ("1", "true", "yes")
    if environ.get("LOG_LEVEL"):
        data["log_level"] = environ["LOG_LEVEL"]
    return data


def load(path: str | None = None, environ: Mapping[str, str] | None = None) -> Settings:
    """Load the TOML file if it exists (or was named explicitly), else fall back to the environment."""
    environ = os.environ if environ is None else environ
    explicit = path or environ.get("ARR_TRICKLE_CONFIG")
    candidate = Path(explicit or DEFAULT_CONFIG_PATH)
    if candidate.is_file():
        try:
            with candidate.open("rb") as f:
                data = tomllib.load(f)
        except tomllib.TOMLDecodeError as e:
            raise ConfigError(f"{candidate}: {e}") from None
        except OSError as e:
            raise ConfigError(f"cannot read {candidate}: {e.strerror} (check the file's permissions)") from None
        return parse(data)
    if explicit:
        raise ConfigError(f"config file {candidate} not found")
    return parse(from_env(environ))


def _id_list(name: str, raw: str) -> list[int]:
    try:
        return [int(x) for x in re.split(r"[,\s]+", raw) if x.strip()]
    except ValueError:
        raise ConfigError(f"{name} must be a comma-separated list of ids") from None


def _key_from_arr_config(path: str) -> str:
    # The *arr config.xml is a flat, trusted local file; a regex avoids feeding it to an XML
    # parser (no entity expansion, no external references) and is all that's needed.
    try:
        text = Path(path).read_text(encoding="utf-8", errors="replace")
    except OSError as e:
        raise ConfigError(f"cannot read {path}: {e.strerror}") from None
    match = _XML_KEY_RE.search(text)
    if not match:
        raise ConfigError(f"no <ApiKey> found in {path}")
    return match.group(1)


def _read_secret_file(path: str) -> str:
    p = Path(path)
    try:
        mode = p.stat().st_mode
        key = p.read_text(encoding="utf-8").strip()
    except OSError as e:
        raise ConfigError(f"cannot read {path}: {e.strerror}") from None
    if mode & stat.S_IROTH:
        log.warning("%s is world-readable; consider chmod 600", path)
    return key
