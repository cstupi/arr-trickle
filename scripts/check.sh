#!/bin/sh
# Run scripts/test.sh (the same checks as CI) in a throwaway Go container.
set -eu
cd "$(dirname "$0")/.."
exec docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOPATH=/tmp/go -e GOCACHE=/tmp/go-cache \
    -v "$PWD":/src -w /src golang:1.27.3 scripts/test.sh
