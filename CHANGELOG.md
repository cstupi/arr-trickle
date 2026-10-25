# Changelog

## 0.3.1

- Removed dry run: the dashboard toggle, the Settings checkbox, the `dry_run` setting and `DRY_RUN`. Every pass
  sends its searches. A config that still sets it fails at startup and says it was removed.
- Removed `allow_insecure_http`: plain http to a public host is always refused (an API key would cross the
  internet in clear text). The Settings page no longer offers it, and an old config that sets it says so at startup.
- Activity entries written by 0.3.0 (which carried a `dry_run` field) still display.

## 0.3.0

- **Rewritten in Go.** arr-trickle is now one static binary, and the image is `FROM scratch` (about 10 MB, no
  OS packages; Trivy finds no HIGH/CRITICAL issues). Settings, CLI commands, behaviour and on-disk state are unchanged:
  `config.toml`, `budget.json`, the activity log and the web UI's `auth.json` carry over as-is, so an
  existing install upgrades without redoing setup. Dependencies: `golang.org/x/crypto` (scrypt) and
  `BurntSushi/toml`, pinned in `go.sum`.
- **Web UI** (port 8790; turn it off with `[web] enabled = false`). It is server-rendered with no JavaScript. Pages:
  - Dashboard: per-instance health, backlog sizes, queue, daily budget, and the last and next pass. Controls for
    Run now, Pause/Resume (persisted), dry-run toggle, and a per-instance Check.
  - Activity: every search sent, with paging and a filter by instance.
  - Settings: edit global settings and add, edit or remove instances, validated by the same rules as the file.
    Saves are atomic, keep a `.bak`, and are hot-reloaded. Comments in `config.toml` are not kept when saving from the UI.
- Security for the UI: login on every route (deny by default), no default credentials (a one-time setup code
  in the log, `reset-password` command), scrypt password hashes, server-side sessions with idle and absolute
  timeouts, CSRF tokens plus an Origin/Referer check, login rate limiting, a strict CSP, a LAN-only
  `allowed_networks` default, and `X-Forwarded-For` honoured only from `trusted_proxies`. API keys are never
  shown. A key typed into the UI is write-only and stored in `/state/secrets/<instance>.key` (0600), never in `config.toml`.
- New `[web]` settings table: `enabled`, `listen`, `port`, `cookie_secure`, `session_hours`, `allowed_networks`,
  `trusted_proxies`.
- Engine: a background scheduler with a status snapshot, a thread-safe daily budget, and an activity log
  (`/state/activity.jsonl`, rotated). Each pass returns a per-instance report, and the wanted lists report their totals.
- Tests: unit tests (race detector, 97% coverage), `go fix` modernizers, staticcheck and govulncheck, plus the integration suite
  against real Sonarr/Radarr/Lidarr, now including an end-to-end run through the web UI.
- **Upgrading from 0.2:** mount the config *folder* read-write at `/config` (not just the file) if you
  want to save from the UI, and publish port 8790 to your LAN. The image's entrypoint is now `/arr-trickle`
  (there is no `python3` in it), so use `docker compose exec arr-trickle /arr-trickle <command>`. Startup now allows a config with no instances
  when the UI is enabled. While passes are paused, the heartbeat keeps updating, so `healthcheck` stays healthy.

## 0.2.0

- TOML settings file with strict validation. It supports multiple instances per app and keeps env-var
  configuration as a fallback.
- Key sources are `api_key_from` (the app's config.xml), `api_key_file` and `api_key_env`. Keys are never stored or logged.
- New commands: `check` (read-only connectivity probe), `validate`, and `healthcheck`, which backs the image `HEALTHCHECK`.
- Hardening: redirects aren't followed, plain http to public hosts is refused, response size is capped,
  and the apps' validation errors are reported without leaking bodies.
- Unit tests (95%+ coverage, mypy --strict, ruff) and integration tests against real
  Sonarr/Radarr/Lidarr containers. CI also runs Trivy, CodeQL and Dependabot.
  Releases are signed multi-arch images on GHCR.

## 0.1.0

- First version: a single script configured by environment variables.
