#!/bin/sh
# Integration tests against real, throwaway Sonarr/Radarr/Lidarr containers.
#
# Starts fresh instances on a private Docker network (no host ports), waits for them,
# runs the Go integration tests (integration/, build tag `integration`) in a Go container on the same network, then removes
# everything. Used locally and by CI. Nothing touches an existing *arr install.
#
#   scripts/integration.sh              # latest images
#   ARR_TAG=4.0.16 scripts/integration.sh   # pin a tag for every app (or SONARR_IMAGE=..., etc.)
#   ARR_IT_SHELL='go run ./cmd/arr-trickle check' scripts/integration.sh   # run a command against the apps instead
set -eu
cd "$(dirname "$0")/.."

NET="arr-trickle-it-$$"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/arr-trickle-it.XXXXXX")"
TAG="${ARR_TAG:-latest}"
APPS="sonarr radarr lidarr"

cleanup() {
    for app in $APPS; do docker rm -f "$NET-$app" >/dev/null 2>&1 || true; done
    docker network rm "$NET" >/dev/null 2>&1 || true
    # The apps write as our uid, so this needs no privileges.
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

docker network create "$NET" >/dev/null
for app in $APPS; do
    image_var="$(echo "$app" | tr '[:lower:]' '[:upper:]')_IMAGE"
    image="$(eval "echo \${$image_var:-lscr.io/linuxserver/$app:$TAG}")"
    mkdir -p "$WORK/$app/media"
    docker run -d --name "$NET-$app" --network "$NET" \
        -e PUID="$(id -u)" -e PGID="$(id -g)" -e TZ=UTC \
        -v "$WORK/$app:/config" "$image" >/dev/null
    echo "started $app ($image)"
done

port() { case "$1" in sonarr) echo 8989 ;; radarr) echo 7878 ;; lidarr) echo 8686 ;; esac; }

ENV_ARGS=""
for app in $APPS; do
    up=""
    for _ in $(seq 1 90); do
        if grep -q "<ApiKey>" "$WORK/$app/config.xml" 2>/dev/null &&
            docker exec "$NET-$app" sh -c "wget -q -O /dev/null http://localhost:$(port "$app")/ping || curl -fs http://localhost:$(port "$app")/ping >/dev/null"; then
            up=1
            break
        fi
        sleep 2
    done
    [ -n "$up" ] || { echo "$app did not come up"; docker logs --tail 50 "$NET-$app"; exit 1; }
    echo "$app is up"
    APP_UPPER="$(echo "$app" | tr '[:lower:]' '[:upper:]')"
    ENV_ARGS="$ENV_ARGS -e ARR_IT_${APP_UPPER}_URL=http://$NET-$app:$(port "$app") -e ARR_IT_${APP_UPPER}_CONFIG=$WORK/$app/config.xml"
done

# shellcheck disable=SC2086  # ENV_ARGS is a deliberate word-split list of -e flags
docker run --rm --network "$NET" --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOPATH=/tmp/go -e GOCACHE=/tmp/go-cache \
    -e ARR_IT_SHELL="${ARR_IT_SHELL:-}" $ENV_ARGS -v "$PWD":/src -v "$WORK":"$WORK" -w /src golang:1.26 sh -euc '
if [ -n "${ARR_IT_SHELL:-}" ]; then sh -c "$ARR_IT_SHELL"; else
go test -count=1 -tags integration -v -timeout 15m ./integration/ "$@"; fi
' sh "$@"
