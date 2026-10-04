#!/usr/bin/env bash
set -euo pipefail

readonly module=github.com/dortort/wawarden
readonly source_url=https://github.com/dortort/wawarden
readonly arches=(amd64 arm64)
readonly platforms=linux/amd64,linux/arm64

log() { printf 'repro-build: %s\n' "$*" >&2; }
die() {
  log "error: $*"
  exit 1
}

mode=${1:-oci}
case $mode in
  binaries | oci | push) ;;
  *) die "usage: VERSION=<version> [IMAGE=<image>] $0 [binaries|oci|push]" ;;
esac

VERSION=${VERSION:-}
IMAGE=${IMAGE:-ghcr.io/dortort/wawarden}
BUILDKIT_IMAGE=${BUILDKIT_IMAGE:-moby/buildkit:v0.33.1@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea}
[[ $VERSION =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "VERSION must be a valid image tag, got '$VERSION'"
[[ $IMAGE =~ ^[a-z0-9][a-z0-9._/:-]*$ ]] || die "IMAGE is not a valid image name: '$IMAGE'"

required=(git go)
if [ "$mode" != binaries ]; then
  required+=(docker jq tar)
fi
for tool in "${required[@]}"; do
  command -v "$tool" > /dev/null || die "$tool is not installed"
done

cd "$(dirname "${BASH_SOURCE[0]}")/.."
root=$(git rev-parse --show-toplevel)
cd "$root"

changes=$(git status --porcelain --untracked-files=normal --ignore-submodules=none)
if [ -n "$changes" ]; then
  git status --short >&2
  die "the working tree is not clean; go build would stamp vcs.modified=true"
fi
tags=$(git tag --merged HEAD)
if [ -n "$tags" ]; then
  die "tags reachable from HEAD change the module version go build stamps; fetch the commit with --no-tags"
fi

export GOENV=off GOFLAGS='' GOWORK=off GOTOOLCHAIN=local GOEXPERIMENT='' GOFIPS140=off CGO_ENABLED=0 GOOS=linux GOAMD64=v1 GOARM64=v8.0

toolchain=$(awk '$1 == "toolchain" { print $2 }' go.mod)
[ -n "$toolchain" ] || die "go.mod has no toolchain directive"
goversion=$(go env GOVERSION)
[ "$goversion" = "$toolchain" ] || die "go.mod pins $toolchain but the local Go is $goversion"
hostos=$(go env GOHOSTOS)
hostarch=$(go env GOHOSTARCH)

modflag=-mod=readonly
if [ -f vendor/modules.txt ]; then
  modflag=-mod=vendor
fi

marker=$(sed -nE 's/.*devMarker[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' internal/buildinfo/dev.go)
[ -n "$marker" ] || die "cannot read devMarker from internal/buildinfo/dev.go"

revision=$(git rev-parse HEAD)
SOURCE_DATE_EPOCH=$(git log -1 --format=%ct HEAD)
export SOURCE_DATE_EPOCH
stamp=$(TZ=UTC git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd HEAD)

log "commit $revision, SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH ($stamp), $goversion, $modflag"

build_setting() {
  awk -v key="$1" '$1 == "build" { i = index($2, "="); if (substr($2, 1, i - 1) == key) print substr($2, i + 1) }' <<< "$2"
}

check_binary() {
  local bin=$1 arch=$2 meta tags
  meta=$(go version -m "$bin")
  tags=$(build_setting -tags "$meta")
  if [[ ,$tags, == *,dev,* ]]; then
    die "$bin was built with the dev tag"
  fi
  [ "$(build_setting vcs.revision "$meta")" = "$revision" ] || die "$bin does not record vcs.revision=$revision"
  [ "$(build_setting vcs.modified "$meta")" = false ] || die "$bin does not record vcs.modified=false"
  [ "$(build_setting CGO_ENABLED "$meta")" = 0 ] || die "$bin does not record CGO_ENABLED=0"
  [ "$(build_setting GOARCH "$meta")" = "$arch" ] || die "$bin does not record GOARCH=$arch"
  if LC_ALL=C grep -qaF -- "$marker" "$bin"; then
    die "$bin contains the dev build marker"
  fi
  go tool nm "$bin" | grep -E "[[:space:]]$module/internal/buildinfo\.Version\$" > /dev/null ||
    die "$bin lacks $module/internal/buildinfo.Version, the -X target symbol"
  if [ "$mode" = push ]; then
    log "$bin not run: push mode runs no binary it builds"
    return
  fi
  if [ "$hostos" != linux ] || [ "$arch" != "$hostarch" ]; then
    log "$bin not run: the host is $hostos/$hostarch"
    return
  fi
  local reported expected
  reported=$(env -i "$bin" version) || die "$bin version failed"
  expected=$(printf 'wawarden %s\nrevision %s\ndev build false' "$VERSION" "$revision")
  [ "$reported" = "$expected" ] || die "$bin version does not report version $VERSION, revision $revision and a release build"
  log "$bin version reports $VERSION, revision $revision and a release build"
}

sha256() {
  if command -v sha256sum > /dev/null; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

rm -rf dist
mkdir dist
names=()
for arch in "${arches[@]}"; do
  name=wawarden_${VERSION}_linux_$arch
  GOARCH=$arch go build "$modflag" -trimpath -buildvcs=true \
    -ldflags="-buildid= -X $module/internal/buildinfo.Version=$VERSION" \
    -o "dist/$name" ./cmd/wawarden
  check_binary "dist/$name" "$arch"
  touch -d "$stamp" "dist/$name"
  names+=("$name")
done
(cd dist && sha256 "${names[@]}" > SHA256SUMS)
cat dist/SHA256SUMS >&2

if [ "$mode" = binaries ]; then
  exit 0
fi

buildx_version=$(docker buildx version | awk '{ print $2 }')
log "buildx $buildx_version, BuildKit $BUILDKIT_IMAGE"
if [ -n "${BUILDX_VERSION:-}" ] && [ "$buildx_version" != "$BUILDX_VERSION" ]; then
  die "buildx $BUILDX_VERSION is required, found $buildx_version"
fi

builder=wawarden-repro-$$
cleanup() { docker buildx rm --force "$builder" > /dev/null 2>&1 || true; }
trap cleanup EXIT
docker buildx create --name "$builder" --driver docker-container \
  --driver-opt "image=$BUILDKIT_IMAGE" --bootstrap > /dev/null

build_args=(
  --builder "$builder"
  --platform "$platforms"
  --provenance=false
  --sbom=false
  --no-cache
  --pull
  --progress=plain
  --build-arg "VERSION=$VERSION"
  --build-arg "REVISION=$revision"
  --annotation "index:org.opencontainers.image.source=$source_url"
  --annotation "index:org.opencontainers.image.licenses=MIT"
  --annotation "index:org.opencontainers.image.version=$VERSION"
  --annotation "index:org.opencontainers.image.revision=$revision"
  --metadata-file dist/metadata.json
)

if [ "$mode" = oci ]; then
  docker buildx build "${build_args[@]}" \
    --output "type=oci,dest=dist/wawarden.oci.tar,rewrite-timestamp=true" . >&2
  digest=$(tar -xOf dist/wawarden.oci.tar index.json |
    jq -r 'if (.manifests | length) == 1 then .manifests[0].digest else error("expected exactly one manifest in index.json") end')
  [ "$digest" = "$(jq -r '."containerimage.digest"' dist/metadata.json)" ] ||
    die "index.json and the build metadata disagree on the image digest"
else
  docker buildx build "${build_args[@]}" \
    --output "type=image,name=$IMAGE,push-by-digest=true,push=true,oci-mediatypes=true,rewrite-timestamp=true" . >&2
  digest=$(jq -r '."containerimage.digest"' dist/metadata.json)
fi

[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || die "unexpected image digest '$digest'"
printf '%s\n' "$digest" > dist/image-digest
log "image index digest $digest"
printf '%s\n' "$digest"
