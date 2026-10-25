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
connected app's API key. arr-trickle does the same job, and its web UI is built so that class of
problem cannot come back:

| | arr-trickle |
|---|---|
| Web UI | Login on every page, no default credentials, CSRF tokens, LAN-only by default, no JavaScript. Can be turned off |
| API keys | **Never sent to the browser.** Keys are read from each app's own `config.xml`, a secret file or an env var. A key typed into the UI is write-only and stored in a 0600 file, never in `config.toml` |
| Code | One static Go binary. Two dependencies, both hash-pinned in `go.sum`: `golang.org/x/crypto` (scrypt, from the Go team) and `BurntSushi/toml`. The HTTP server and HTML templates are Go's standard library |
| Container | `FROM scratch`: the binary and CA certificates, nothing else (about 10 MB). No shell, no OS packages. Non-root, read-only filesystem, all capabilities dropped |
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
    image: ghcr.io/cstupi/arr-trickle:0.3
    user: "1000:1000"            # a uid that can read the *arr config.xml files
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    ports:
      - "8790:8790"              # the web UI, for your LAN only (see "Network exposure" below)
    volumes:
      - ./arr-trickle/config:/config     # read-write: the UI saves config.toml here
      - ./arr-trickle/state:/state       # budget, activity log, login, keys entered in the UI
      - ./sonarr/config.xml:/arr/sonarr/config.xml:ro   # mount only config.xml, never the app's database
      - ./radarr/config.xml:/arr/radarr/config.xml:ro
      - ./lidarr/config.xml:/arr/lidarr/config.xml:ro
    restart: unless-stopped
```

The config is now a mounted **folder**, not a single file: saving from the UI replaces `config.toml`
atomically (a temp file renamed over it), which a single bind-mounted file doesn't allow. The rest of the
container stays read-only. Create both folders owned by the `user:` above, then:

```sh
docker compose up -d arr-trickle
docker compose logs arr-trickle | grep "Setup code"
```

You can start without a `config.toml` and add every instance in the UI, or copy
[`examples/config.toml`](examples/config.toml) into `./arr-trickle/config/` first. Either way,
`docker compose run --rm arr-trickle check` probes every endpoint without sending searches.

To run headless as before, set `enabled = false` in the `[web]` table, drop the `ports:` entry, and
mount `config.toml` read-only if you like. There is then no listening port at all.

## The web UI

Open `http://<nas-address>:8790` from any machine on your LAN.

**First run.** There are no default credentials. On first start the container log prints a
one-time setup code:

```
No admin account yet. Open the web UI (port 8790) and go to /setup.
Setup code: 7KQ4M-X2PZD-H9WNR-3TCFB
```

Enter it at `/setup` with a username and a password (12 characters or more) to create the single admin
account. After that `/setup` no longer exists. If you forget the password, run
`docker compose exec arr-trickle /arr-trickle reset-password`. This removes the account, ends
every session and prints a new setup code.

**Pages** (all server-rendered, no JavaScript):

- **Dashboard.** For each instance: health, missing and cutoff-unmet backlog sizes, queue size, searches
  used today against the daily cap, the last pass and its result. It also shows when the next pass runs
  and whether passes are paused or in dry-run mode. Buttons: **Run now**, **Pause/Resume** (remembered
  across restarts), **Dry run on/off**, and a per-instance **Check** that runs the read-only `check` probe.
- **Activity.** Every search sent, newest first: time, instance, missing or cutoff, command, the titles
  searched, the cost, and whether it was a dry run. You can filter by instance, and the list is paged. It is
  kept in `/state/activity.jsonl`, rotated at 1 MiB, with one old file kept.
- **Settings.** Global settings, and adding, editing or removing instances, including several of the same app. Input is validated
  by exactly the same rules as the file, with each error shown next to its field. Saving writes
  `config.toml` atomically, keeps the previous version as `config.toml.bak`, and applies the change without a
  restart. **Comments you wrote in `config.toml` are not preserved** when saving from the UI.
  The state directory and the `[web]` table are shown but can only be changed in the file, because changing them
  needs a restart and could lock you out.

**API keys in the UI.** Each instance shows where its key comes from (a `config.xml` path, a key file or
an environment variable name), and you can change that. You can also paste a key into a password field. It is
written to `/state/secrets/<instance>.key` (mode 0600), and the instance is switched to read it from there.
No page ever displays a key, not even a stored one, and keys are never logged or written to `config.toml`.

### Network exposure

The UI listens on all interfaces **inside the container** so that other machines on your LAN can reach it.
Each request is then checked against `allowed_networks`, which by default is loopback plus the private ranges
(10/8, 172.16/12, 192.168/16 and IPv6 unique-local fc00::/7). Requests from anywhere else get a 403.

- **Never expose the UI to the internet.** Don't port-forward 8790 on your router or publish it through a
  tunnel. arr-trickle controls apps whose API keys give full control of them. For access from outside your
  home, use a VPN to your LAN.
- To publish the port on one interface only, use `ports: ["192.168.1.10:8790:8790"]`.
- Narrow `allowed_networks` to your own subnet, e.g. `["192.168.1.0/24"]`. Note that with Docker's
  userland proxy, clients may appear to come from the Docker gateway (a 172.x address). The
  `ports:` binding and your firewall then decide who can reach the UI.

### Behind a reverse proxy with TLS

Plain http on a home LAN is the default. To get https, put a reverse proxy *on your LAN* in front of
arr-trickle and tell arr-trickle to trust it:

```toml
[web]
cookie_secure = true                 # the session cookie is only sent over https
trusted_proxies = ["172.20.0.5"]     # the proxy's address as arr-trickle sees it
allowed_networks = ["192.168.1.0/24", "172.20.0.5/32"]   # your LAN (checked against the real client)
```

Only addresses in `trusted_proxies` may set `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host`.
From anyone else those headers are ignored, so they can't be used to fake an allowed address.
The proxy must pass the original `Host` header, because form submissions are checked against it. Caddy does
this by default (`reverse_proxy arr-trickle:8790`). For nginx:

```nginx
location / {
    proxy_pass http://arr-trickle:8790;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

Don't publish arr-trickle's own port when it sits behind a proxy, and keep the proxy itself off the internet.

## Configuration

See [`examples/config.toml`](examples/config.toml) for every setting with comments, or use the UI's
Settings page. Settings are validated strictly: an unknown key, a wrong type or an out-of-range value
stops startup with a message naming the setting (in the UI, it's shown next to the field).

Each instance takes exactly one key source:

| Setting | Reads the key from |
|---|---|
| `api_key_from = "/arr/sonarr/config.xml"` | the app's own config file (recommended, nothing duplicated) |
| `api_key_file = "/run/secrets/sonarr"` | a file such as a Docker secret (warns if world-readable); keys pasted into the UI end up here |
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
| `run` (default) | Search on a schedule and serve the web UI (unless `[web] enabled = false`); `run --once` does one pass |
| `check` | Read-only probe of every endpoint arr-trickle uses (no searches) |
| `validate` | Validate settings and key sources, then exit |
| `healthcheck` | Exit 0 if the scheduler is alive and passes are running (used by the image's `HEALTHCHECK`; doesn't touch the UI) |
| `reset-password` | Remove the web UI's admin account, end all sessions, and print a new setup code |

## Development

```sh
go build ./cmd/arr-trickle   # or: docker build -t arr-trickle .
scripts/check.sh         # gofmt, go vet, staticcheck, govulncheck, tests with -race and coverage >= 90% (in Docker)
scripts/integration.sh   # starts throwaway Sonarr/Radarr/Lidarr and runs the contract tests against them
```

The web UI tests drive the real handler. They walk the whole route table to prove that every route
needs a login, that every form needs a CSRF token and a same-origin request, and that a known fake API key
never appears in any page, header, log line or activity entry.

The integration tests start fresh apps on a private Docker network with no host ports. They
check every endpoint and command arr-trickle uses, and run a full pass end to end: add a real
series, movie and artist, let arr-trickle pick it from the missing list, and confirm the
app completes the search. A second test does the same through the web UI: it completes first-run setup
with the code from the log, adds each app on the Settings page using the write-only key field, presses
Run now, and finds the searches in the apps and on the Activity page. CI runs them on every push and weekly against the latest app
releases, so upstream API changes surface as test failures, not silent no-ops.

## Security

See [SECURITY.md](SECURITY.md) for the threat model and how to report a vulnerability.

## License

MIT
