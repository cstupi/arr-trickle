import json
import time

import pytest

from arr_trickle import cli
from arr_trickle.state import Budget, beat, heartbeat_age
from conftest import KEY


def test_budget_persists_and_drops_old_days(tmp_path):
    path = tmp_path / "b.json"
    path.write_text(json.dumps({"2000-01-01": {"x": 99}}))
    b = Budget(path)
    assert b.used("x") == 0
    b.add("x", 3)
    assert Budget(path).used("x") == 3
    assert "2000-01-01" not in path.read_text()


@pytest.mark.parametrize("content", ["not json", "[1, 2]", '{"d": {"x": "nan"}}'])
def test_budget_survives_bad_file(tmp_path, content):
    path = tmp_path / "b.json"
    path.write_text(content)
    assert Budget(path).used("x") == 0


def test_heartbeat(tmp_path):
    hb = tmp_path / "hb"
    assert heartbeat_age(hb) is None
    beat(hb)
    assert heartbeat_age(hb) < 5


@pytest.fixture
def config(tmp_path, arr_config_xml):
    path = tmp_path / "config.toml"
    path.write_text(
        f'state_dir = "{tmp_path}"\n[[radarr]]\nurl = "http://127.0.0.1:9"\n'
        f'api_key_from = "{arr_config_xml}"\ntimeout_seconds = 5\n'
    )
    return path


def test_validate(config, capsys):
    assert cli.main(["--config", str(config), "validate"]) == 0
    assert "radarr" in capsys.readouterr().out


def test_validate_bad_key_source(tmp_path, capsys):
    path = tmp_path / "c.toml"
    path.write_text(f'[[radarr]]\nurl = "http://r:1"\napi_key_from = "{tmp_path}/missing.xml"\n')
    assert cli.main(["--config", str(path), "validate"]) == 2
    assert "cannot read" in capsys.readouterr().err


def test_config_error_exit_code(tmp_path, capsys):
    path = tmp_path / "c.toml"
    path.write_text("[[radarr]]\nurl = 3\n")
    assert cli.main(["--config", str(path), "validate"]) == 2
    assert "configuration error" in capsys.readouterr().err


def test_check_unreachable_fails_without_leaking_key(config, capsys):
    assert cli.main(["--config", str(config), "check"]) == 1
    out = capsys.readouterr().out
    assert "FAIL" in out and KEY not in out


def test_run_once_writes_heartbeat_and_healthcheck(config, tmp_path, capsys):
    assert cli.main(["--config", str(config), "healthcheck"]) == 1
    assert cli.main(["--config", str(config), "run", "--once"]) == 1  # unreachable app -> nonzero
    assert (tmp_path / "heartbeat").exists()
    assert cli.main(["--config", str(config), "healthcheck"]) == 0


def test_healthcheck_stale(config, tmp_path):
    (tmp_path / "heartbeat").write_text(str(int(time.time()) - 10 * 86400))
    assert cli.main(["--config", str(config), "healthcheck"]) == 1
