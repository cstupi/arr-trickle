# arr-trickle

Works through the **missing** and **cutoff-unmet** backlog of Sonarr, Radarr and Lidarr a few
searches at a time, least-recently-searched first, within a daily budget.

The \*arr apps search an item once, when it's added, then rely on RSS. Back catalog that nobody
re-posts is never found, and *Search All Missing* fires thousands of queries at once and gets you
rate-limited by your indexers. arr-trickle searches a little every hour instead.

It replaces [Huntarr](https://github.com/MGHazz/huntarr.io-archive), which was pulled in February 2026
after unauthenticated endpoints were found to expose every connected app's API key. arr-trickle is
built so that can't happen: one static Go binary in a `FROM scratch` image, API keys never sent to the
browser, a login on every page with no default credentials, and LAN-only access by default. See
[SECURITY.md](SECURITY.md).

Each pass, per app: it skips if indexers or the download client are unhealthy (or the queue is full),
searches the oldest-searched released items (Sonarr: whole seasons), searches upgrades only where the
quality profile allows them, and counts everything against `daily_cap`.

## Setup

```yaml
services:
  arr-trickle:
    image: ghcr.io/cstupi/arr-trickle:0.3
    user: "1000:1000"            # a uid that can read the *arr config.xml files
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    ports:
      - "8790:8790"              # the web UI: your LAN only, never the internet
    volumes:
      - ./arr-trickle/config:/config     # read-write: the UI saves config.toml here
      - ./arr-trickle/state:/state       # budget, activity, login, keys entered in the UI
      - ./sonarr/config.xml:/arr/sonarr/config.xml:ro   # only config.xml, never the app's database
      - ./radarr/config.xml:/arr/radarr/config.xml:ro
      - ./lidarr/config.xml:/arr/lidarr/config.xml:ro
    restart: unless-stopped
```

1. Create both folders owned by the `user:` above, then `docker compose up -d`.
2. Get the one-time setup code: `docker compose logs arr-trickle | grep "Setup code"`.
3. Open `http://<nas-address>:8790`, enter the code, and create the admin account.
4. In **Settings**, add each app: its URL (e.g. `http://sonarr:8989`) and where to read its API key,
   usually its config.xml (`/arr/sonarr/config.xml`). Press **Check** on the dashboard to verify.

That's it. The dashboard shows each app's health and backlog, with **Run now**, **Pause** and
**Dry run** controls. **Activity** lists every search sent.

**API keys** come from the app's `config.xml` (recommended), a key file, or an environment variable.
A key pasted into the UI is write-only: it's stored in `/state/secrets` (mode 0600), never shown again,
and never written to `config.toml`.

**Budgeting.** Each search costs about one query per indexer, and RSS already uses ~96/day per indexer.
Set `daily_cap` below your indexer's limit minus that.

**Prefer a file?** Copy [`examples/config.toml`](examples/config.toml) (every setting, commented) into
`./arr-trickle/config/`. Saving from the UI rewrites the file without your comments, keeping a
`.bak`. To run headless with no listening port, set `enabled = false` under `[web]`. Without a file,
single instances can also be set with environment variables (`SONARR_URL`, `SONARR_MISSING`, …,
`SONARR_CONFIG` for the config.xml path; likewise `RADARR_*`, `LIDARR_*`).

## Access and security

The UI accepts loopback and private-network clients only (`[web] allowed_networks`). Don't
port-forward it or put it on the internet; use a VPN for remote access. For https behind a reverse
proxy, see [SECURITY.md](SECURITY.md#behind-a-reverse-proxy-with-tls).

Forgot the password? `docker compose exec arr-trickle /arr-trickle reset-password` prints a new setup code.

## Commands

`run` (default; `--once` for one pass), `check` (read-only probe, sends no searches), `validate`,
`healthcheck` (used by the image), `reset-password`.

## Development

```sh
scripts/check.sh         # gofmt, go fix, vet, staticcheck, govulncheck, tests with -race (in Docker)
scripts/integration.sh   # tests against throwaway Sonarr/Radarr/Lidarr containers, including the web UI
```

MIT licensed.
