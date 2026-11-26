#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
export GOWORK=off
export SKIP_INTEROP_TEST=1

# Upstream mihomo uses non-constant format strings in log/error helpers.
go vet -printf=false ./...
go vet -printf=false -tags= ./...

# Verify cross-platform compilation across all release OSes.
for target in linux/amd64 windows/amd64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -o /dev/null .
done

go test -vet=off -count=1 ./...
