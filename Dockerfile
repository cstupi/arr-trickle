# syntax=docker/dockerfile:1
# Build: a static Go binary. Pinned toolchain; Dependabot proposes updates (see .github/dependabot.yml).
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine3.24 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/arr-trickle ./cmd/arr-trickle

# Runtime: nothing but the binary and CA certificates (for https to the apps). No shell, no package
# manager, no interpreter, no OS packages to patch.
FROM scratch

LABEL org.opencontainers.image.title="arr-trickle" \
      org.opencontainers.image.description="Work through the Sonarr/Radarr/Lidarr backlog a few searches at a time" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/cstupi/arr-trickle"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/arr-trickle /arr-trickle

# Runs as nobody unless overridden (e.g. `user: "1000:1000"` to read your *arr config.xml).
USER 65534:65534
VOLUME ["/state"]
# The web UI (see [web] in the settings). Publish it to your LAN only; never to the internet.
EXPOSE 8790

HEALTHCHECK --interval=5m --timeout=10s --start-period=2m CMD ["/arr-trickle", "healthcheck"]
ENTRYPOINT ["/arr-trickle"]
CMD ["run"]
