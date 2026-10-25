# arr-trickle

Works through the **missing** and **upgrade** (cutoff-unmet) backlog of Sonarr, Radarr and
Lidarr a few searches at a time, least-recently-searched first, within a daily budget.

## Why it exists

The \*arr apps search an item once, when it is added. After that they only grab what
appears in indexer RSS feeds. That works for TV and movies, which are re-uploaded
constantly, but back catalog that nobody re-posts is never found. *Search All Missing*
fires thousands of queries at once, hits indexer API limits, and the indexers are then
disabled for a day.

[Huntarr](https://github.com/MGHazz/huntarr.io-archive) did this job until February 2026, when its
repository was pulled after unauthenticated endpoints were found to expose every
connected app's API key. arr-trickle is built so that class of problem cannot exist:

| | arr-trickle |
|---|---|
| Network surface | **None.** No web UI, no listening port |
| Stored credentials | **None.** Keys are read at run time from each app's own `config.xml`, a secret file, or an env var |
| Runtime dependencies | **None.** Python standard library only |
| Container | Non-root, read-only filesystem, all capabilities dropped |
| Redirects | Not followed, so the API key is never re-sent to another host |
| Plain HTTP | Refused to public hosts unless you opt in |

## What a pass does

For each configured instance:

1. It skips the instance if the app's health shows an unusable indexer or no download client. Searching
   then would only spend budget. It also skips while the download queue is at or above `max_queue`.
2. It searches the `missing` least-recently-searched released items:
   - Sonarr: one `SeasonSearch` per season, so season packs are found (`missing` counts seasons).
   - Radarr: `MoviesSearch` for movies Radarr considers available.
   - Lidarr: `AlbumSearch` for albums already released.
3. It searches `cutoff` upgrade candidates, but only for items whose quality profile allows upgrades.
4. It records the searches against `daily_cap` (per UTC day, persisted in `/state`).

## Quick start (docker compose)

```yaml
services:
  arr-trickle:
    image: ghcr.io/OWNER/arr-trickle:0.2
    user: "1000:1000"            # a uid that can read the *arr config.xml files
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    volumes:
      - ./arr-trickle/config.toml:/config/config.toml:ro
      - ./arr-trickle/state:/state
      - ./sonarr/config.xml:/arr/sonarr/config.xml:ro   # mount only config.xml, never the app's database
      - ./radarr/config.xml:/arr/radarr/config.xml:ro
      - ./lidarr/config.xml:/arr/lidarr/config.xml:ro
    restart: unless-stopped
```

Copy [`examples/config.toml`](examples/config.toml) next to it and adjust, then:

```sh
docker compose run --rm arr-trickle check      # read-only: verifies every endpoint, sends no searches
docker compose up -d arr-trickle
docker compose logs -f arr-trickle
```

## Configuration

See [`examples/config.toml`](examples/config.toml) for every setting with comments. Settings are
validated strictly at startup: an unknown key, a wrong type or an out-of-range value stops startup with
a message naming the setting.

Each instance takes exactly one key source:

| Setting | Reads the key from |
|---|---|
| `api_key_from = "/arr/sonarr/config.xml"` | the app's own config file (recommended, nothing duplicated) |
| `api_key_file = "/run/secrets/sonarr"` | a file such as a Docker secret (warns if world-readable) |
| `api_key_env = "SONARR_API_KEY"` | an environment variable |

**Budgeting.** Each Lidarr album, Radarr movie, Sonarr season or cutoff episode costs about one
query per indexer. RSS sync already uses about 96 queries per day per indexer at the default
15-minute interval. Set `daily_cap` below your indexer's limit minus that.

**Environment-only setup.** Without a config file, single instances can be configured with
`SONARR_URL`, `SONARR_MISSING`, `SONARR_CUTOFF`, `SONARR_DAILY_CAP`, `SONARR_MAX_QUEUE`,
`SONARR_EXCLUDE`, `SONARR_CONFIG` (the config.xml path) or `SONARR_API_KEY_FILE`, plus `INTERVAL_MINUTES`, `DRY_RUN`, `STATE_DIR`
and `LOG_LEVEL`. The same variables exist for `RADARR_*` and `LIDARR_*`.

## Commands

| Command | Purpose |
|---|---|
| `run` (default) | Search on a schedule; `run --once` does one pass |
| `check` | Read-only probe of every endpoint arr-trickle uses (no searches) |
| `validate` | Validate settings and key sources, then exit |
| `healthcheck` | Exit 0 if a pass finished recently (used by the image's `HEALTHCHECK`) |

## Development

```sh
scripts/check.sh         # ruff format + lint, mypy --strict, unit tests with coverage (in Docker)
scripts/integration.sh   # starts throwaway Sonarr/Radarr/Lidarr and runs the contract tests against them
```

The integration tests start fresh apps on a private Docker network with no host ports. They
check every endpoint and command arr-trickle uses, and run a full pass end to end: add a real
series, movie and artist, let arr-trickle pick it from the missing list, and confirm the
app completes the search. CI runs them on every push and weekly against the latest app
releases, so upstream API changes surface as test failures, not silent no-ops.

## Security

See [SECURITY.md](SECURITY.md) for the threat model and how to report a vulnerability.

## License

MIT
