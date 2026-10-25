"""Minimal *arr REST client (standard library only)."""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

from arr_trickle import __version__
from arr_trickle.apps import APPS, Record

# Wanted pages are small; anything far larger means something is wrong (or hostile).
MAX_RESPONSE_BYTES = 16 * 1024 * 1024


class ArrError(Exception):
    """A request to an *arr app failed. The message never includes the API key."""


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    # Following a redirect would resend X-Api-Key to wherever the app (or a proxy) points.
    def redirect_request(self, *args: Any, **kwargs: Any) -> None:
        return None


_OPENER = urllib.request.build_opener(_NoRedirect)


def _error_detail(err: urllib.error.HTTPError) -> str:
    """The app's own validation messages for a 4xx, e.g. ': Path: Folder does not exist'.

    Only the `errorMessage`/`message` fields are used, never raw bodies, and the result is
    truncated; the *arr apps don't echo request headers, so the key cannot appear here.
    """
    if not 400 <= err.code < 500:
        return ""
    try:
        body = json.loads(err.read(64 * 1024) or b"null")
    except (ValueError, OSError):
        return ""
    if isinstance(body, dict) and isinstance(body.get("errors"), dict):
        # ASP.NET problem details: {"title": ..., "errors": {"$.field": ["message", ...]}}
        messages = [f"{field}: {msgs[0]}" for field, msgs in body["errors"].items() if msgs]
        messages = messages or [str(body.get("title", ""))]
        return f": {'; '.join(messages)[:300]}"
    items = body if isinstance(body, list) else [body]
    messages = [
        f"{i.get('propertyName')}: {i['errorMessage']}" if i.get("propertyName") else str(i.get("errorMessage"))
        for i in items
        if isinstance(i, dict) and i.get("errorMessage")
    ] or [str(i["message"]) for i in items if isinstance(i, dict) and i.get("message")]
    return f": {'; '.join(messages)[:300]}" if messages else ""


class ArrClient:
    def __init__(self, kind: str, url: str, api_key: str, timeout: float = 60) -> None:
        if urllib.parse.urlsplit(url).scheme not in ("http", "https"):
            raise ArrError(f"unsupported URL scheme in {url!r}")
        self.spec = APPS[kind]
        self.base = f"{url.rstrip('/')}/api/{self.spec.api_version}"
        self._api_key = api_key
        self.timeout = timeout

    def __repr__(self) -> str:  # never show the key
        return f"ArrClient({self.spec.kind!r}, {self.base!r})"

    def request(self, path: str, data: Any = None, method: str = "GET") -> Any:
        body = json.dumps(data).encode() if data is not None else None
        req = urllib.request.Request(  # noqa: S310 (scheme checked in __init__)
            self.base + path,
            data=body,
            method=method,
            headers={
                "X-Api-Key": self._api_key,
                "Content-Type": "application/json",
                "Accept": "application/json",
                "User-Agent": f"arr-trickle/{__version__}",
            },
        )
        try:
            # Scheme is restricted to http/https in __init__ (and in config validation).
            with _OPENER.open(req, timeout=self.timeout) as resp:
                raw = resp.read(MAX_RESPONSE_BYTES + 1)
        except urllib.error.HTTPError as e:
            hint = " (check the API key)" if e.code == 401 else _error_detail(e)
            raise ArrError(f"{method} {path}: HTTP {e.code}{hint}") from None
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            reason = getattr(e, "reason", e)
            raise ArrError(f"{method} {path}: {reason}") from None
        if len(raw) > MAX_RESPONSE_BYTES:
            raise ArrError(f"{method} {path}: response larger than {MAX_RESPONSE_BYTES} bytes")
        try:
            return json.loads(raw) if raw else None
        except ValueError:
            raise ArrError(f"{method} {path}: response is not JSON (wrong URL or base path?)") from None

    def status(self) -> dict[str, Any]:
        result: dict[str, Any] = self.request("/system/status")
        return result

    def health(self) -> list[dict[str, Any]]:
        result: list[dict[str, Any]] = self.request("/health")
        return result

    def queue_size(self) -> int:
        return int(self.request("/queue?page=1&pageSize=1")["totalRecords"])

    def upgrade_profiles(self) -> set[int]:
        """Ids of quality profiles that allow upgrades; cutoff searches elsewhere are wasted."""
        return {p["id"] for p in self.request("/qualityprofile") if p.get("upgradeAllowed")}

    def wanted(self, kind: str, page_size: int) -> tuple[list[Record], str | None]:
        """Records from /wanted/{missing,cutoff}, least recently searched first, and the sort key echoed back."""
        query = {
            "page": 1,
            "pageSize": page_size,
            "monitored": "true",
            "sortKey": self.spec.sort_key,
            "sortDirection": "ascending",
        }
        if self.spec.include:
            query[self.spec.include] = "true"
        page = self.request(f"/wanted/{kind}?{urllib.parse.urlencode(query)}")
        return list(page["records"]), page.get("sortKey")

    def command(self, body: dict[str, Any]) -> dict[str, Any]:
        result: dict[str, Any] = self.request("/command", body, "POST")
        return result
