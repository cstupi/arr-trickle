# arr-trickle

Searches the missing and upgrade backlog of Sonarr, Radarr and Lidarr a few items per hour, within a
daily budget, so indexers never rate-limit you.

## Setup

```yaml
services:
  arr-trickle:
    image: ghcr.io/cstupi/arr-trickle:0.3
    user: "1000:1000"   # can read the *arr config.xml files
    read_only: true
    cap_drop: [ALL]
    ports: ["8790:8790"]   # LAN only
    volumes:
      - ./arr-trickle/config:/config
      - ./arr-trickle/state:/state
      - ./sonarr/config.xml:/arr/sonarr/config.xml:ro
    restart: unless-stopped
```

1. `docker compose up -d`, then `docker compose logs arr-trickle | grep "Setup code"`.
2. Open `http://<host>:8790`, enter the code, create your login.
3. In **Settings**, add each app: its URL and `/arr/<app>/config.xml` for the key.

## Defaults

- A pass every **60 minutes**, starting at launch.
- Per app and pass: **5 missing** items (Sonarr: 5 seasons), **0 upgrades**, **no daily cap**.
- An app is skipped while its indexers or download client are unhealthy.
- Searches go out for real; turn on **Dry run** to only log them.
- Web UI on port **8790**, open to loopback and private networks only; sessions last 12 hours.

All settings: [`examples/config.toml`](examples/config.toml).
