# arr-trickle

Searches the missing and upgrade backlog of Sonarr, Radarr and Lidarr a few items per hour, within a
daily budget, so indexers never rate-limit you. A secure replacement for Huntarr ([SECURITY.md](SECURITY.md)).

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

All settings: [`examples/config.toml`](examples/config.toml).
