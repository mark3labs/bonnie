#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "$tmp/helper" ./cmd/bonnie-skillfs
  gzip -n -c "$tmp/helper" > "sandbox/discoveryhelpers/linux-$arch.gz"
done
