"""ArrClient against a local throwaway HTTP server (no mocks of urllib)."""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import ClassVar

import pytest

from arr_trickle import client as client_mod
from arr_trickle.client import ArrClient, ArrError
from conftest import KEY


class Handler(BaseHTTPRequestHandler):
    seen: ClassVar[list] = []

    def log_message(self, *args):
        pass

    def _send(self, code, body, ctype="application/json", extra=None):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        Handler.seen.append((self.command, self.path, dict(self.headers)))
        if self.headers.get("X-Api-Key") != KEY:
            return self._send(401, {"error": "unauthorized"})
        routes = {
            "/api/v1/system/status": {"appName": "Lidarr", "version": "3.1"},
            "/api/v1/health": [{"source": "UpdateCheck"}],
            "/api/v1/queue?page=1&pageSize=1": {"totalRecords": 7},
            "/api/v1/qualityprofile": [{"id": 1, "upgradeAllowed": True}, {"id": 2, "upgradeAllowed": False}],
        }
        if self.path.startswith("/api/v1/wanted/missing"):
            return self._send(200, {"records": [{"id": 1}], "sortKey": "albums.lastSearchTime"})
        if self.path == "/api/v1/redirect":
            return self._send(302, b"", extra={"Location": "http://evil.example/steal"})
        if self.path == "/api/v1/invalid":
            return self._send(400, [{"propertyName": "Path", "errorMessage": "Folder does not exist"}])
        if self.path == "/api/v1/problem":
            return self._send(
                400,
                {
                    "title": "One or more validation errors occurred.",
                    "errors": {"$.addOptions.monitor": ["could not be converted"]},
                },
            )
        if self.path == "/api/v1/message":
            return self._send(409, {"message": "already exists"})
        if self.path == "/api/v1/servererror":
            return self._send(500, {"message": "stack trace with internals"})
        if self.path == "/api/v1/html":
            return self._send(200, b"<html>login</html>", "text/html")
        if self.path == "/api/v1/huge":
            return self._send(200, b"[" + b"1," * 600 + b"1]")
        if self.path in routes:
            return self._send(200, routes[self.path])
        return self._send(404, {"error": "nope"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        Handler.seen.append((self.command, self.path, json.loads(self.rfile.read(length))))
        self._send(201, {"id": 42, "name": "AlbumSearch"})


@pytest.fixture(scope="module")
def server():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


@pytest.fixture
def client(server):
    Handler.seen.clear()
    return ArrClient("lidarr", server, KEY, timeout=5)


def test_endpoints(client):
    assert client.status()["appName"] == "Lidarr"
    assert client.health() == [{"source": "UpdateCheck"}]
    assert client.queue_size() == 7
    assert client.upgrade_profiles() == {1}
    records, sort_key = client.wanted("missing", 5)
    assert records == [{"id": 1}] and sort_key == "albums.lastSearchTime"
    path = Handler.seen[-1][1]
    assert "sortKey=albums.lastSearchTime" in path and "includeArtist=true" in path and "monitored=true" in path


def test_sends_key_header_and_user_agent(client):
    client.status()
    headers = Handler.seen[-1][2]
    assert headers["X-Api-Key"] == KEY and headers["User-Agent"].startswith("arr-trickle/")


def test_command_posts_json(client):
    assert client.command({"name": "AlbumSearch", "albumIds": [1]})["id"] == 42
    assert Handler.seen[-1] == ("POST", "/api/v1/command", {"name": "AlbumSearch", "albumIds": [1]})


def test_bad_key_is_explained_without_leaking(server):
    with pytest.raises(ArrError, match="401") as e:
        ArrClient("lidarr", server, "wrongkey" * 4, timeout=5).status()
    assert "wrongkey" not in str(e.value)


def test_redirects_are_not_followed(client):
    with pytest.raises(ArrError, match="302"):
        client.request("/redirect")


def test_non_json_explained(client):
    with pytest.raises(ArrError, match="not JSON"):
        client.request("/html")


def test_oversized_response_refused(client, monkeypatch):
    monkeypatch.setattr(client_mod, "MAX_RESPONSE_BYTES", 100)
    with pytest.raises(ArrError, match="larger than"):
        client.request("/huge")


def test_unreachable_host():
    with pytest.raises(ArrError):
        ArrClient("radarr", "http://127.0.0.1:9", KEY, timeout=2).status()


def test_repr_hides_key(client):
    assert KEY not in repr(client)


def test_rejects_non_http_scheme():
    with pytest.raises(ArrError, match="scheme"):
        ArrClient("radarr", "file:///etc/passwd", KEY)


def test_validation_errors_are_reported(client):
    with pytest.raises(ArrError, match="Path: Folder does not exist"):
        client.request("/invalid")
    with pytest.raises(ArrError, match=r"\$\.addOptions\.monitor: could not be converted"):
        client.request("/problem")
    with pytest.raises(ArrError, match="already exists"):
        client.request("/message")
    with pytest.raises(ArrError) as e:
        client.request("/servererror")
    assert "internals" not in str(e.value)
