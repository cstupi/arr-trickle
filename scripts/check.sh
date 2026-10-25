#!/bin/sh
# Format check, vet, staticcheck, govulncheck and unit tests (race detector, coverage >= 90%)
# in a throwaway Go container (same steps as CI).
set -eu
cd "$(dirname "$0")/.."
exec docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOPATH=/tmp/go -e GOCACHE=/tmp/go-cache \
    -v "$PWD":/src -w /src golang:1.26 sh -euc '
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then echo "not gofmt-formatted:"; echo "$unformatted"; exit 1; fi
go mod verify
go vet ./...
go vet -tags integration ./integration/
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
pkgs="$(go list ./internal/... | grep -v /testutil | paste -sd, -)"
go test -race -count=1 -coverpkg="$pkgs" -coverprofile=/tmp/cover.out ./...
total="$(go tool cover -func=/tmp/cover.out | awk "/^total:/ {sub(\"%\", \"\", \$3); print \$3}")"
echo "total coverage: ${total}%"
awk -v t="$total" "BEGIN { exit (t >= 90) ? 0 : 1 }" || { echo "coverage below 90%"; exit 1; }
'
