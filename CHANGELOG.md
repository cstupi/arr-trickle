# Changelog

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
