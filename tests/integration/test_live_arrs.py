"""Contract tests against real Sonarr/Radarr/Lidarr (started by scripts/integration.sh).

They check that every endpoint and command arr-trickle relies on still behaves as expected,
so an upstream API change shows up here rather than as silently skipped searches.
"""

import datetime as dt
import os
import time

import pytest

from arr_trickle.apps import APPS
from arr_trickle.client import ArrClient, ArrError
from arr_trickle.config import Instance
from arr_trickle.runner import check_instance, run_instance
from arr_trickle.state import Budget

pytestmark = pytest.mark.integration

# Small, stable items used to populate a wanted list. Metadata comes from each app's
# upstream service; if that is unreachable the end-to-end test is skipped, not failed.
SAMPLES = {
    "sonarr": {"lookup": "/series/lookup?term=tvdb:78874", "add": "/series"},  # Firefly
    "radarr": {"lookup": "/movie/lookup/tmdb?tmdbId=603", "add": "/movie"},  # The Matrix
    "lidarr": {"lookup": "/artist/lookup?term=lidarr:a74b1b7f-71a5-4011-9441-d0b5e4122711", "add": "/artist"},
}


def instance(kind: str) -> Instance:
    url = os.environ.get(f"ARR_IT_{kind.upper()}_URL")
    if not url:
        pytest.skip(f"ARR_IT_{kind.upper()}_URL not set (run scripts/integration.sh)")
    return Instance(kind=kind, name=f"it-{kind}", url=url, api_key_from=os.environ[f"ARR_IT_{kind.upper()}_CONFIG"])


def client_for(kind: str, cls: type[ArrClient] = ArrClient) -> ArrClient:
    inst = instance(kind)
    return cls(kind, inst.url, inst.api_key(), timeout=60)


NO_INDEXER_OR_CLIENT = ("IndexerSearchCheck", "IndexerRssCheck", "DownloadClientCheck")


class WithoutNoIndexerHealth(ArrClient):
    """The throwaway apps have no indexers, which arr-trickle rightly treats as "don't search".

    For the end-to-end test only, hide exactly those warnings so the search path is exercised;
    test_fresh_app_without_indexers_is_skipped covers the real behaviour.
    """

    def health(self):
        return [h for h in super().health() if h.get("source") not in NO_INDEXER_OR_CLIENT]


def wait_for_command(client: ArrClient, command_id: int, timeout: float = 120) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        cmd = client.request(f"/command/{command_id}")
        if cmd["status"] in ("completed", "failed", "aborted", "cancelled", "orphaned"):
            return cmd
        time.sleep(1)
    pytest.fail(f"command {command_id} did not finish within {timeout}s")


@pytest.mark.parametrize("kind", list(APPS))
def test_check_passes(kind):
    res = check_instance(instance(kind))
    assert res.ok, "\n".join(res.lines)


@pytest.mark.parametrize("kind", list(APPS))
def test_wrong_key_is_rejected(kind):
    inst = instance(kind)
    with pytest.raises(ArrError, match="401"):
        ArrClient(kind, inst.url, "0" * 32, timeout=30).status()


@pytest.mark.parametrize("kind", list(APPS))
@pytest.mark.parametrize("wanted", ["missing", "cutoff"])
def test_wanted_pages_are_sorted_by_last_search(kind, wanted):
    records, sort_key = client_for(kind).wanted(wanted, 5)
    assert isinstance(records, list)
    assert sort_key == APPS[kind].sort_key


@pytest.mark.parametrize("kind", list(APPS))
def test_search_commands_are_accepted(kind):
    client = client_for(kind)
    spec = APPS[kind]
    for name in {spec.missing_command, spec.cutoff_command} - {"SeasonSearch"}:
        cmd = client.command({"name": name, spec.ids_field: []})
        assert cmd["name"] == name
        assert wait_for_command(client, cmd["id"])["status"] == "completed"


def _add_sample(kind: str, client: ArrClient) -> dict:
    sample = SAMPLES[kind]
    try:
        found = client.request(sample["lookup"])
    except ArrError as e:
        pytest.skip(f"{kind} metadata lookup unavailable: {e}")
    item = found[0] if isinstance(found, list) else found
    if not item:
        pytest.skip(f"{kind} metadata lookup returned nothing")
    root = "/config/media"
    if not any(r["path"].rstrip("/") == root for r in client.request("/rootfolder")):
        body = {"path": root}
        if kind == "lidarr":
            body |= {"name": "media", "defaultQualityProfileId": 1, "defaultMetadataProfileId": 1}
        client.request("/rootfolder", body, "POST")
    item |= {
        "qualityProfileId": client.request("/qualityprofile")[0]["id"],
        "rootFolderPath": root,
        "monitored": True,
        # Never search on add: the test drives the search itself.
        "addOptions": {
            "sonarr": {"monitor": "all", "searchForMissingEpisodes": False},
            "radarr": {"monitor": "movieOnly", "searchForMovie": False},
            "lidarr": {"monitor": "all", "searchForMissingAlbums": False},
        }[kind],
    }
    if kind == "sonarr":
        item["seasonFolder"] = True
        item.pop("languageProfileId", None)
    if kind == "radarr":
        item["minimumAvailability"] = "released"
    if kind == "lidarr":
        item["metadataProfileId"] = client.request("/metadataprofile")[0]["id"]
    return client.request(sample["add"], item, "POST")


@pytest.mark.parametrize("kind", list(APPS))
def test_fresh_app_without_indexers_is_skipped(kind, tmp_path):
    inst = instance(kind)
    client = client_for(kind)
    assert any(h.get("source") == "IndexerSearchCheck" for h in client.health())
    assert run_instance(inst, client, Budget(tmp_path / "b.json"), dry_run=False) == 0


@pytest.mark.parametrize("kind", list(APPS))
def test_end_to_end_search_of_a_real_missing_item(kind, tmp_path):
    client = client_for(kind, WithoutNoIndexerHealth)
    _add_sample(kind, client)

    # The app fills in episodes/albums asynchronously after the add.
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        records, _ = client.wanted("missing", 50)
        if records:
            break
        time.sleep(3)
    else:
        pytest.skip(f"{kind}: wanted list stayed empty (metadata refresh unavailable)")

    before = {c["id"] for c in client.request("/command")}
    inst = instance(kind)
    spent = run_instance(
        Instance(kind=kind, name=inst.name, url=inst.url, api_key_from=inst.api_key_from, missing=1),
        client,
        Budget(tmp_path / "budget.json"),
        dry_run=False,
        now=dt.datetime.now(dt.UTC),
    )
    assert spent >= 1
    new = [c for c in client.request("/command") if c["id"] not in before and c["name"] == APPS[kind].missing_command]
    assert new, "arr-trickle's search command never reached the app"
    assert wait_for_command(client, new[0]["id"])["status"] == "completed"
