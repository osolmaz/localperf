#!/usr/bin/env bash
# Builds the release archives of one version into a folder, with SHA256SUMS.
# Usage: scripts/build-release.sh v0.2.0 dist
set -euo pipefail
cd "$(dirname "$0")/.."

version="${1:?usage: build-release.sh <version> <out dir>}"
out="${2:?usage: build-release.sh <version> <out dir>}"

targets=(linux/amd64 linux/arm64 darwin/arm64 darwin/amd64)

rm -rf "$out"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  name="localperf-${version}-${goos}-${goarch}"
  mkdir -p "$work/$name"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -o "$work/$name/localperf" ./cmd/localperf
  cp README.md "$work/$name/"
  tar --no-xattrs -C "$work" -czf "$out/$name.tar.gz" "$name"
done

(cd "$out" && sha256sum -- *.tar.gz > SHA256SUMS)
cat "$out/SHA256SUMS"
