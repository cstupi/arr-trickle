import logging

import pytest

from arr_trickle.client import ArrError
from arr_trickle.config import Instance
from arr_trickle.planner import query_cost
from arr_trickle.runner import check_instance, run_instance, run_once
from arr_trickle.state import Budget
from conftest import KEY, NOW, album, episode, movie


class FakeClient:
    def __init__(
        self,
        *,
        missing=(),
        cutoff=(),
        health=(),
        queue=0,
        upgrade_profiles=(1,),
        app="Lidarr",
        sort_key="albums.lastSearchTime",
    ):
        self._wanted = {"missing": list(missing), "cutoff": list(cutoff)}
        self._health, self._queue, self._profiles = list(health), queue, set(upgrade_profiles)
        self._app, self._sort_key = app, sort_key
        self.commands = []

    def status(self):
        return {"appName": self._app, "version": "9.9"}

    def health(self):
        return self._health

    def queue_size(self):
        return self._queue

    def upgrade_profiles(self):
        return self._profiles

    def wanted(self, kind, page_size):
        return self._wanted[kind][:page_size], self._sort_key

    def command(self, body):
        self.commands.append(body)
        return {"id": 1}


def inst(kind="lidarr", **kw):
    base = {"kind": kind, "name": kind, "url": f"http://{kind}:1", "api_key_env": "K", "missing": 3}
    base.update(kw)
    return Instance(**base)


@pytest.fixture
def budget(tmp_path):
    return Budget(tmp_path / "budget.json")


def test_sends_missing_and_records_budget(budget):
    c = FakeClient(missing=[album(i) for i in range(1, 10)])
    assert run_instance(inst(), c, budget, False, NOW) == 3
    assert c.commands == [{"name": "AlbumSearch", "albumIds": [1, 2, 3]}]
    assert budget.used("lidarr") == 3


def test_dry_run_sends_nothing(budget, caplog):
    caplog.set_level(logging.INFO)
    c = FakeClient(missing=[album(1)])
    run_instance(inst(), c, budget, True, NOW)
    assert c.commands == []
    assert budget.used("lidarr") == 0
    assert "[dry run]" in caplog.text


def test_skips_when_indexer_unhealthy(budget):
    c = FakeClient(missing=[album(1)], health=[{"source": "IndexerStatusCheck", "message": "down"}])
    assert run_instance(inst(), c, budget, False, NOW) == 0
    assert c.commands == []


def test_ignores_unrelated_health_warnings(budget):
    c = FakeClient(missing=[album(1)], health=[{"source": "UpdateCheck", "message": "update"}])
    assert run_instance(inst(), c, budget, False, NOW) == 1


def test_skips_when_queue_full(budget):
    c = FakeClient(missing=[album(1)], queue=50)
    assert run_instance(inst(max_queue=50), c, budget, False, NOW) == 0


def test_daily_cap_limits_across_runs_and_restarts(budget):
    c = FakeClient(missing=[album(i) for i in range(1, 10)], cutoff=[album(20)])
    i = inst(missing=3, cutoff=1, daily_cap=4)
    run_instance(i, c, budget, False, NOW)
    run_instance(i, c, Budget(budget.path), False, NOW)
    assert sum(query_cost(x) for x in c.commands) == 4


def test_cutoff_skips_profiles_without_upgrades(budget):
    c = FakeClient(cutoff=[movie(1, profile=4)], upgrade_profiles=())
    assert run_instance(inst("radarr", missing=0, cutoff=2), c, budget, False, NOW) == 0


def test_sonarr_season_search_and_long_log_line(budget, caplog):
    caplog.set_level(logging.INFO)
    c = FakeClient(missing=[episode(i, 1) for i in range(1, 15)])
    assert run_instance(inst("sonarr", missing=1), c, budget, False, NOW) == 1
    assert c.commands == [{"name": "SeasonSearch", "seriesId": 10, "seasonNumber": 1}]
    assert "+4 more" in caplog.text


def test_run_once_continues_after_a_failure(tmp_path, caplog, monkeypatch):
    ok_inst = inst("radarr", api_key_env="GOOD")
    bad_inst = inst("lidarr", api_key_env="MISSING")
    monkeypatch.setenv("GOOD", KEY)
    calls = []
    monkeypatch.setattr("arr_trickle.runner.run_instance", lambda i, *a: calls.append(i.name))
    assert run_once((bad_inst, ok_inst), Budget(tmp_path / "b.json"), False) is False
    assert calls == ["radarr"]
    assert "lidarr: run failed" in caplog.text


def test_check_reports_everything():
    res = check_instance(inst(), FakeClient())
    assert res.ok
    text = "\n".join(res.lines)
    assert "Lidarr 9.9" in text and "wanted/missing" in text and "wanted/cutoff" in text


def test_check_flags_wrong_app_and_ignored_sort():
    assert not check_instance(inst(), FakeClient(app="Sonarr")).ok
    res = check_instance(inst(), FakeClient(sort_key="title"))
    assert not res.ok and "sortKey" in "\n".join(res.lines)


def test_check_reports_blocking_health_without_failing():
    res = check_instance(inst(), FakeClient(health=[{"source": "IndexerStatusCheck", "message": "down"}]))
    assert res.ok and "would be skipped" in "\n".join(res.lines)


def test_check_catches_client_errors():
    class Broken(FakeClient):
        def status(self):
            raise ArrError("GET /system/status: HTTP 401 (check the API key)")

    res = check_instance(inst(), Broken())
    assert not res.ok and "401" in res.lines[0]
