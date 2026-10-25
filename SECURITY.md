# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's **Report a vulnerability** button
(Security → Advisories) on this repository rather than in a public issue. You should get an
acknowledgement within a few days. Fixes are released as a new tagged version with an advisory.

## Threat model

arr-trickle holds the API keys of the \*arr apps it talks to, and those keys give full control of
the apps. The design goal is that nothing outside the container can obtain them.

- **No inbound network surface.** There is no HTTP server, web UI or listening socket. The only
  network activity is outbound requests to the configured app URLs.
- **No credential storage.** Keys are read at run time from the app's own `config.xml`, a secret file,
  or an environment variable. They are never written to disk, logged, included in error messages, or
  shown in `repr()`. Tests assert this.
- **Keys are only sent where you configured.** HTTP redirects are not followed, so a redirect cannot
  forward the `X-Api-Key` header elsewhere. Only `http`/`https` URLs are accepted, without embedded
  credentials. Plain `http` to a public host is refused unless `allow_insecure_http = true`.
- **Defensive parsing.** `config.xml` is read with a single anchored regex, never an XML parser, so
  there is no entity expansion. Responses over 16 MiB are refused. Error text from the apps is reduced
  to their validation messages and truncated.
- **Minimal supply chain.** There are no runtime dependencies. The image is a pinned Python base plus
  this package's source files, runs as a non-root user, and is meant to run with a read-only root
  filesystem and all capabilities dropped. Release images are signed with cosign (keyless) and ship
  SBOM and provenance attestations.
- **Least-privilege mounts.** Mount only each app's `config.xml`, read-only, never its whole config
  directory, so the container cannot read or lock the apps' databases.

Out of scope: an attacker who already controls the host, the Docker daemon, or one of the \*arr apps.

## Verifying a release image

```sh
cosign verify ghcr.io/OWNER/arr-trickle:0.2.0 \
  --certificate-identity-regexp 'https://github.com/OWNER/arr-trickle/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
