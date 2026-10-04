#!/usr/bin/env bash
set -euo pipefail

readonly arches=(amd64 arm64)
readonly out=dist/licenses

log() { printf 'licenses: %s\n' "$*" >&2; }
die() {
  log "error: $*"
  exit 1
}

cd "$(dirname "${BASH_SOURCE[0]}")/.."

stamp=$(TZ=UTC git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd HEAD)

modules=$(
  for arch in "${arches[@]}"; do
    GOOS=linux GOARCH=$arch CGO_ENABLED=0 go list -mod=readonly -deps \
      -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/wawarden
  done | LC_ALL=C sort -u
)

rm -rf "$out"
mkdir -p "$out"
count=0
while read -r module dir; do
  [ -n "$module" ] || continue
  if [ -z "$dir" ] || [ ! -d "$dir" ]; then
    die "$module is not in the module cache"
  fi
  files=$(find "$dir" -mindepth 1 -maxdepth 1 -type f |
    awk -F/ '{ print $NF }' |
    grep -iE '^(licen[cs]e|copying|notice|patents|unlicense)([._-].*)?$' |
    LC_ALL=C sort || true)
  [ -n "$files" ] || die "$module ships no licence, copying, notice or patents file"
  mkdir -p "$out/$module"
  while read -r name; do
    cp "$dir/$name" "$out/$module/$name"
    chmod 0444 "$out/$module/$name"
  done <<< "$files"
  count=$((count + 1))
done <<< "$modules"
[ "$count" -gt 0 ] || die "no module is linked into ./cmd/wawarden"

find "$out" -type d -exec chmod 0755 {} +
find "$out" -exec touch -d "$stamp" {} +
log "collected the licence files of $count modules into $out"
