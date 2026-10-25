#!/bin/sh
# Every static check plus the unit tests (race detector, coverage >= 90%). CI runs this directly;
# scripts/check.sh runs it in a throwaway Go container.
set -eu
cd "$(dirname "$0")/.."
test -z "$(gofmt -l . | tee /dev/stderr)"
test -z "$(go fix -diff ./... | tee /dev/stderr)"  # modernizers: code uses current Go idioms
go mod verify
go vet ./...
go vet -tags integration ./integration/
go tool -modfile=tools/go.mod staticcheck ./...
go tool -modfile=tools/go.mod govulncheck ./...
cover="${TMPDIR:-/tmp}/arr-trickle-cover.out"
go test -race -count=1 -coverpkg="$(go list ./internal/... | grep -v /testutil | paste -sd, -)" -coverprofile="$cover" ./...
go tool cover -func="$cover" | awk '/^total:/ { sub("%", "", $3); print "total coverage: " $3 "%"; exit ($3 < 90) }'
