#!/usr/bin/env bash
set -euo pipefail

readonly default_image=golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c

log() { printf 'offline-test: %s\n' "$*" >&2; }
die() {
  log "error: $*"
  exit 1
}

command -v docker > /dev/null || die "docker is not installed"
cd "$(dirname "${BASH_SOURCE[0]}")/.."
root=$(pwd)

if [ -e vendor ] || [ -L vendor ]; then
  die "vendor exists at the repository root; modules are pinned by go.sum and never vendored, so remove it"
fi

image=${OFFLINE_GO_IMAGE:-$default_image}
toolchain=$(awk '$1 == "toolchain" { print $2 }' go.mod)
[ -n "$toolchain" ] || die "go.mod has no toolchain directive"

cache=$(mktemp -d "${TMPDIR:-/tmp}/wawarden-modcache.XXXXXX")
cleanup() {
  chmod -R u+w "$cache" 2> /dev/null || true
  rm -rf "$cache"
}
trap cleanup EXIT

as_user=(--user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOTOOLCHAIN=local -e GOFLAGS=-mod=readonly
  --tmpfs "/tmp:exec,size=6g" -v "$root:/src:ro" -w /src)

version=$(docker run --rm --network none "${as_user[@]}" "$image" go env GOVERSION)
[ "$version" = "$toolchain" ] || die "go.mod pins $toolchain but $image has $version"

log "downloading the modules that go.sum pins into a fresh cache, with network access"
docker run --rm "${as_user[@]}" -v "$cache:/modcache" -e GOMODCACHE=/modcache "$image" go mod download

log "running the suite in a container without a network, $version"
docker run --rm --network none "${as_user[@]}" -v "$cache:/modcache:ro" -e GOMODCACHE=/modcache -e GOPROXY=off "$image" sh -ec '
    for iface in /sys/class/net/*/; do
      iface=${iface%/}
      if [ "${iface##*/}" != lo ] && [ "$(cat "$iface/operstate")" != down ]; then
        echo "offline-test: ${iface##*/} is up, so the container has a network" >&2
        exit 1
      fi
    done
    if [ "$(wc -l < /proc/net/route)" -gt 1 ]; then
      echo "offline-test: the container has a route" >&2
      exit 1
    fi
    go test -race -count=1 -timeout 45m ./...
    go test -race -count=1 -timeout 45m -tags dev ./...
  '
log "the whole suite passed without a network, release and dev builds"
