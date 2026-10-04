#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
toolchain=$(awk '$1 == "toolchain" { print $2 }' go.mod)
if [ -z "$toolchain" ]; then
  echo "::error::go.mod has no toolchain line"
  exit 1
fi
goversion=$(go env GOVERSION)
if [ "$goversion" != "$toolchain" ]; then
  echo "::error::go env GOVERSION reports $goversion, but the toolchain line of go.mod names $toolchain"
  exit 1
fi
echo "go env GOVERSION reports $goversion, as the toolchain line of go.mod names."
