import os
from pathlib import Path

import pytest

from arr_trickle.config import ConfigError, from_env, is_private_host, load, parse
from conftest import KEY


def minimal(**over):
    entry = {"url": "http://sonarr:8989", "api_key_env": "K"}
    entry.update(over)
    return {"sonarr": [entry]}


def test_defaults():
    s = parse(minimal())
    inst = s.instances[0]
    assert (inst.name, inst.missing, inst.cutoff, inst.daily_cap, s.interval_minutes) == ("sonarr", 5, 0, 0, 60)


def test_multiple_instances_get_unique_default_names():
    data = {"sonarr": [{"url": "http://a:1", "api_key_env": "A"}, {"url": "http://b:1", "api_key_env": "B"}]}
    assert [i.name for i in parse(data).instances] == ["sonarr", "sonarr-2"]


def test_single_table_form_accepted():
    assert parse({"radarr": {"url": "http://radarr:7878", "api_key_env": "K"}}).instances[0].kind == "radarr"


@pytest.mark.parametrize(
    ("data", "message"),
    [
        ({}, "no apps configured"),
        ({"sonar": []}, "unknown setting"),
        (minimal(missin=3), "unknown setting"),
        (minimal(missing="3"), "must be an integer"),
        (minimal(missing=True), "must be an integer"),
        (minimal(missing=-1), "between"),
        (minimal(exclude=[1, "2"]), "exclude"),
        (minimal(exclude=[0]), "exclude"),
        (minimal(url="ftp://x"), "http"),
        (minimal(url="http://user:pw@sonarr:8989"), "credentials"),
        (minimal(api_key_file="/x"), "exactly one"),
        ({"sonarr": [{"url": "http://sonarr:8989"}]}, "exactly one"),
        ({"interval_minutes": 1, **minimal()}, "between"),
        ({"log_level": "LOUD", **minimal()}, "log_level"),
        ({"sonarr": "nope"}, "table"),
    ],
)
def test_rejects_bad_settings(data, message):
    with pytest.raises(ConfigError, match=message):
        parse(data)


def test_duplicate_names_rejected():
    data = {
        "sonarr": [{"url": "http://a:1", "api_key_env": "A", "name": "x"}],
        "radarr": [{"url": "http://b:1", "api_key_env": "B", "name": "x"}],
    }
    with pytest.raises(ConfigError, match="duplicate"):
        parse(data)


@pytest.mark.parametrize("host", ["localhost", "sonarr", "127.0.0.1", "192.168.0.10", "10.0.0.2", "nas.local", "::1"])
def test_private_hosts(host):
    assert is_private_host(host)


@pytest.mark.parametrize("host", ["example.com", "8.8.8.8", "sonarr.mydomain.net"])
def test_public_hosts(host):
    assert not is_private_host(host)


def test_plain_http_to_public_host_refused_unless_allowed():
    with pytest.raises(ConfigError, match="plain http"):
        parse(minimal(url="http://sonarr.example.com"))
    assert parse({"allow_insecure_http": True, **minimal(url="http://sonarr.example.com")})
    assert parse(minimal(url="https://sonarr.example.com"))


def test_key_from_arr_config(arr_config_xml):
    inst = parse({"sonarr": [{"url": "http://s:1", "api_key_from": str(arr_config_xml)}]}).instances[0]
    assert inst.api_key() == KEY


def test_key_from_secret_file_warns_if_world_readable(tmp_path, caplog):
    secret = tmp_path / "key"
    secret.write_text(KEY + "\n")
    os.chmod(secret, 0o644)
    inst = parse({"sonarr": [{"url": "http://s:1", "api_key_file": str(secret)}]}).instances[0]
    assert inst.api_key() == KEY
    assert "world-readable" in caplog.text
    assert KEY not in caplog.text


def test_key_from_env_and_format_check():
    inst = parse(minimal()).instances[0]
    assert inst.api_key({"K": KEY}) == KEY
    with pytest.raises(ConfigError, match="empty"):
        inst.api_key({})
    with pytest.raises(ConfigError, match="unexpected format") as e:
        inst.api_key({"K": "short; rm -rf"})
    assert "rm -rf" not in str(e.value)


def test_missing_arr_config(tmp_path):
    inst = parse({"lidarr": [{"url": "http://l:1", "api_key_from": str(tmp_path / "nope.xml")}]}).instances[0]
    with pytest.raises(ConfigError, match="cannot read"):
        inst.api_key()
    (tmp_path / "empty.xml").write_text("<Config></Config>")
    inst = parse({"lidarr": [{"url": "http://l:1", "api_key_from": str(tmp_path / "empty.xml")}]}).instances[0]
    with pytest.raises(ConfigError, match=r"no <ApiKey>"):
        inst.api_key()


def test_load_toml_file(tmp_path, arr_config_xml):
    path = tmp_path / "config.toml"
    path.write_text(
        f'interval_minutes = 30\n[[lidarr]]\nurl = "http://lidarr:8686"\napi_key_from = "{arr_config_xml}"\n'
        "missing = 6\nexclude = [3, 4]\n"
    )
    s = load(str(path), environ={})
    assert s.interval_minutes == 30
    assert s.instances[0].exclude == frozenset({3, 4})


def test_load_bad_toml(tmp_path):
    path = tmp_path / "config.toml"
    path.write_text("missing = = 3")
    with pytest.raises(ConfigError, match=r"config\.toml"):
        load(str(path), environ={})


def test_explicit_missing_file_is_an_error(tmp_path):
    with pytest.raises(ConfigError, match="not found"):
        load(str(tmp_path / "nope.toml"), environ={})


def test_env_fallback(tmp_path):
    env = {
        "ARR_TRICKLE_CONFIG": "",
        "SONARR_URL": "http://sonarr:8989",
        "SONARR_MISSING": "4",
        "SONARR_EXCLUDE": "134, 135",
        "SONARR_CONFIG": "/c.xml",
        "INTERVAL_MINUTES": "15",
        "DRY_RUN": "yes",
        "STATE_DIR": str(tmp_path),
        "LOG_LEVEL": "debug",
        "RADARR_URL": "http://radarr:7878",
        "RADARR_API_KEY_FILE": "/run/secrets/radarr",
    }
    s = parse(from_env(env))
    sonarr, radarr = s.instances
    assert (sonarr.missing, sonarr.exclude, sonarr.api_key_from) == (4, frozenset({134, 135}), "/c.xml")
    assert radarr.api_key_file == "/run/secrets/radarr"
    assert (s.interval_minutes, s.dry_run, s.log_level, s.state_dir) == (15, True, "DEBUG", str(tmp_path))


@pytest.mark.parametrize(
    "env", [{"SONARR_URL": "http://s:1", "SONARR_MISSING": "x"}, {"SONARR_URL": "http://s:1", "SONARR_EXCLUDE": "1,a"}]
)
def test_env_fallback_bad_numbers(env):
    with pytest.raises(ConfigError):
        from_env(env)


def test_example_config_is_valid():
    example = Path(__file__).resolve().parents[2] / "examples" / "config.toml"
    settings = load(str(example), environ={})
    assert [i.kind for i in settings.instances] == ["sonarr", "radarr", "lidarr"]


def test_unreadable_config_is_a_config_error(tmp_path):
    path = tmp_path / "config.toml"
    path.write_text("[[sonarr]]\n")
    path.chmod(0)
    try:
        if os.access(path, os.R_OK):  # running as root: permissions don't apply
            pytest.skip("cannot make a file unreadable as root")
        with pytest.raises(ConfigError, match="permissions"):
            load(str(path), environ={})
    finally:
        path.chmod(0o600)
