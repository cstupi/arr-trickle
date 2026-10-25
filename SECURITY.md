# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's **Report a vulnerability** button
(Security → Advisories) on this repository rather than in a public issue. You should get an
acknowledgement within a few days. Fixes are released as a new tagged version with an advisory.

## Threat model

arr-trickle holds the API keys of the \*arr apps it talks to, and those keys give full control of
the apps. The design goal is that nothing outside the container can obtain them, and that only the
owner can change what arr-trickle does.

Version 0.3.0 adds a web UI, so arr-trickle now has an inbound network surface: one HTTP port (8790)
when `[web] enabled = true`, which is the default. With `enabled = false` there is no listening socket at all, as in 0.2.
The UI's controls are listed below so you can judge them yourself.

### Web UI

- **Authentication on every route.** Requests are denied by default. Only `/login`, `/setup`, `/healthz`
  (which returns the two bytes `ok`) and the static stylesheet are reachable without a session. Everything else redirects
  to the login page or returns 401. A unit test walks the whole route table, so a new route is covered
  automatically.
- **No default credentials.** Until an admin account exists, `/setup` needs a one-time code (about 100 bits)
  that is printed only to the container log and stored only as a hash. Once the account exists, `/setup`
  returns 404. `reset-password` (run inside the container) removes the account and prints a new code.
  There is a single admin account.
- **Passwords** must be at least 12 characters. They are stored as scrypt hashes (N=2^15, r=8, p=1, a random 16-byte salt)
  in `/state/auth.json` (mode 0600) and compared in constant time. A login with an unknown username costs
  the same as one with a wrong password.
- **Sessions** are server-side. The cookie holds only a random 32-byte id and is `HttpOnly` and `SameSite=Strict`.
  It is also `Secure` when `cookie_secure = true` or the request arrived over https. Sessions end after
  1 hour idle or `session_hours` (default 12) in total, on logout, on restart, and for everyone when the
  password is reset. A new id is issued on login, so a planted pre-login id is useless.
- **CSRF.** Every POST needs the session's CSRF token, and its `Origin` (or, failing that, `Referer`)
  must match the host the request was sent to. POST bodies must be form-encoded and at most 64 KiB.
- **Login throttling.** After 5 failures, each client gets an exponential backoff (up to 15 minutes); IPv6
  clients are counted per /64, so rotating addresses doesn't help. There is also a global ceiling of 30
  failures per 10 minutes, and at most two password hashes (~32 MiB each) run at once. Failures are logged
  with the client IP, never with the password or username. Note that the global ceiling also locks the
  owner out while an attack is going on.
- **Response headers.** `Content-Security-Policy: default-src 'self'; script-src 'none'; object-src 'none';
  base-uri 'none'; frame-ancestors 'none'; form-action 'self'`, plus `X-Content-Type-Options: nosniff`,
  `X-Frame-Options: DENY` and `Cache-Control: no-store` on every page. The UI has no JavaScript at all, and
  templates are always autoescaped.
  The referrer policy is `same-origin` rather than `no-referrer`, because with `no-referrer` browsers send
  `Origin: null` on form posts, which would break the origin check above. With `same-origin`, nothing is
  sent to other sites.
- **Network allowlist.** Requests from outside `allowed_networks` get a 403. The default is loopback, RFC 1918
  and IPv6 unique-local addresses. `X-Forwarded-*` headers are honoured only from `trusted_proxies`, which is
  empty by default. The UI is meant for your LAN. **Do not expose it to the internet**; use a VPN for remote access.
  With Docker's userland proxy, clients can appear to come from the Docker gateway. In that setup the published-port
  binding and the host firewall are the real boundary.
- **Errors** show a generic page. A panic in a handler is recovered, and no stack trace or error text
  reaches the browser. The server has read, write and idle timeouts, a header size limit, and a ceiling
  of 64 requests in flight.
- **DNS rebinding.** A malicious site could point its own domain at your NAS. The browser would then send no
  session cookie (cookies are bound to the real host name), so that site sees only the login page,
  which is rate-limited.

### API keys

- **Never sent to the browser.** Pages show where a key is read from (a `config.xml` path, a key file or an
  environment variable name), never the key itself.
- **Write-only key field.** A key pasted into the UI is checked for format and written to
  `/state/secrets/<instance>.key` (mode 0600, in a 0700 directory, via a temp file that is created with
  mode 0600). The instance then reads it with `api_key_file`. The key is never rendered again, never logged, and never written to
  `config.toml`. Removing the instance, or switching it to another key source, deletes the file once no
  instance reads it, and a stored key file is never shared between two instances.
  Before 0.3.0, arr-trickle never wrote a key to disk; this field is the one exception, and you choose whether to use it.
- **Otherwise not stored.** Keys are read at run time from the app's own `config.xml`, a secret file or an
  environment variable. They are never included in error messages, the activity log or `%v` output.
  A unit test configures all three kinds of source with a known fake key, then renders every page and error
  path. It asserts that the key appears in no response body, header, log line, activity entry or settings file.
- **Keys are only sent where you configured.** HTTP redirects are not followed, so a redirect cannot
  forward the `X-Api-Key` header elsewhere. Only `http` and `https` URLs are accepted, and they may not contain credentials.
  Plain `http` to a public host is refused unless `allow_insecure_http = true`.
- **Defensive parsing.** `config.xml` is read with a single anchored regex, never an XML parser, so
  there is no entity expansion. Responses over 16 MiB are refused. Error text from the apps is reduced
  to their validation messages and truncated.

### Supply chain and container

- **Dependencies.** arr-trickle is a single static Go binary. It has two dependencies, both pinned
  with hashes in `go.sum`: `golang.org/x/crypto`, for scrypt only (maintained by the Go team), and
  `github.com/BurntSushi/toml`, for reading `config.toml` (itself dependency-free). The HTTP server,
  HTML templating (contextual autoescaping), cookies and sessions are all Go's standard
  library. CI runs `go mod verify`, `go fix -diff` (modern idioms), staticcheck and govulncheck (pinned
  in `tools/go.mod`), on the toolchain pinned in `go.mod`. Dependabot watches both modules, and Trivy
  scans the image.
- **Container.** The image is `FROM scratch`: the binary and CA certificates, with no shell, package
  manager, interpreter or OS packages, so there is nothing else to patch or exploit. It runs as a non-root
  user and is meant to run with a read-only root filesystem, all capabilities dropped and
  `no-new-privileges`. Only `/config` (so the UI can save `config.toml`) and `/state` need to be writable.
  Release images are signed with cosign (keyless) and ship SBOM and provenance attestations.
- **Least-privilege mounts.** Mount only each app's `config.xml`, read-only, never its whole config
  directory, so the container cannot read or lock the apps' databases.

Out of scope: an attacker who already controls the host, the Docker daemon or one of the \*arr apps;
a malicious admin; and anyone with read access to `/state` (it holds the password hash and any keys
entered in the UI).

## Behind a reverse proxy with TLS

Plain http on a home LAN is the default. For https, put a reverse proxy *on your LAN* in front of
arr-trickle and tell arr-trickle to trust it:

```toml
[web]
cookie_secure = true                 # the session cookie is only sent over https
trusted_proxies = ["172.20.0.5"]     # the proxy's address as arr-trickle sees it
allowed_networks = ["192.168.1.0/24", "172.20.0.5/32"]   # your LAN (checked against the real client)
```

Only addresses in `trusted_proxies` may set `X-Forwarded-For`, `X-Forwarded-Proto` and
`X-Forwarded-Host`; from anyone else they are ignored. The proxy must pass the original `Host` header,
because form submissions are checked against it. Caddy does this by default
(`reverse_proxy arr-trickle:8790`); with nginx add `proxy_set_header Host $host;`,
`proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;` and
`proxy_set_header X-Forwarded-Proto $scheme;`. Don't publish arr-trickle's own port when it sits
behind a proxy, and keep the proxy itself off the internet.

With Docker's userland proxy, clients may appear to come from the Docker gateway (a 172.x address);
then the `ports:` binding (e.g. `"192.168.1.10:8790:8790"`) and your firewall decide who can connect.

## Verifying a release image

```sh
cosign verify ghcr.io/cstupi/arr-trickle:0.3.0 \
  --certificate-identity-regexp 'https://github.com/cstupi/arr-trickle/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
