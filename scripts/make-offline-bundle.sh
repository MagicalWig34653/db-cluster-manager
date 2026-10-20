#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# Create an offline installation bundle (tar.gz) containing the manager
# binaries, deployment files, checksums and a manifest.
#
# Usage: scripts/make-offline-bundle.sh [-o OUTDIR] [-v VERSION] [-i IMAGE] [-b BINDIR]
#   -o OUTDIR   output directory (default: ./dist)
#   -v VERSION  bundle version (default: 0.0.0-dev)
#   -b BINDIR   directory with built binaries manager/agent/cli (default: ./bin)
#   -i IMAGE    OPTIONAL: also export this local image with `docker save`
#               (requires Docker and the image already built/pulled; this
#               script never builds or pulls images). Without -i, no image is
#               included and the manifest says so.
#
# Build the binaries first (make build). Cross-platform bundles: build the
# binaries with GOOS/GOARCH and pass the matching -b directory.
set -eu

OUT=dist
VERSION=0.0.0-dev
BIN=bin
IMAGE=

while getopts o:v:b:i: opt; do
  case $opt in
    o) OUT=$OPTARG ;;
    v) VERSION=$OPTARG ;;
    b) BIN=$OPTARG ;;
    i) IMAGE=$OPTARG ;;
    *) echo "usage: $0 [-o OUTDIR] [-v VERSION] [-b BINDIR] [-i IMAGE]" >&2; exit 2 ;;
  esac
done

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
case $BIN in /*) ;; *) BIN=$PWD/$BIN ;; esac
case $OUT in /*) ;; *) OUT=$PWD/$OUT ;; esac

for f in manager agent cli; do
  [ -x "$BIN/$f" ] || { echo "missing binary: $BIN/$f (run make build)" >&2; exit 1; }
done
for f in deploy/compose.yaml deploy/stack.yaml docs/INSTALL.md LICENSE; do
  [ -f "$ROOT/$f" ] || { echo "missing file: $ROOT/$f" >&2; exit 1; }
done

if command -v sha256sum >/dev/null 2>&1; then
  sum() { sha256sum "$@"; }
elif command -v shasum >/dev/null 2>&1; then
  sum() { shasum -a 256 "$@"; }
else
  echo "need sha256sum or shasum" >&2; exit 1
fi

NAME=db-cluster-manager-offline_$VERSION
STAGE=$(mktemp -d "${TMPDIR:-/tmp}/dbcm-bundle.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT INT TERM
D=$STAGE/$NAME
mkdir -p "$D/bin" "$D/deploy" "$D/docs" "$OUT"

for f in manager agent cli; do cp "$BIN/$f" "$D/bin/$f"; done
cp "$ROOT/deploy/compose.yaml" "$ROOT/deploy/stack.yaml" "$D/deploy/"
cp "$ROOT/docs/INSTALL.md" "$D/docs/"
cp "$ROOT/LICENSE" "$D/"

IMAGE_NOTE="none (not requested; see docs/INSTALL.md for building and loading an image)"
if [ -n "$IMAGE" ]; then
  command -v docker >/dev/null 2>&1 || { echo "docker not found but -i given" >&2; exit 1; }
  mkdir -p "$D/image"
  docker save "$IMAGE" -o "$D/image/image.tar"
  IMAGE_NOTE="image/image.tar ($IMAGE); load with: docker load -i image/image.tar"
fi

{
  echo "name: db-cluster-manager offline bundle"
  echo "version: $VERSION"
  echo "image: $IMAGE_NOTE"
  echo "files:"
  (cd "$D" && find . -type f ! -name MANIFEST.txt ! -name SHA256SUMS | LC_ALL=C sort | sed 's/^\.\//  /')
} > "$D/MANIFEST.txt"

(cd "$D" && find . -type f ! -name SHA256SUMS | LC_ALL=C sort | while IFS= read -r f; do sum "$f"; done) > "$D/SHA256SUMS"

TARBALL=$OUT/$NAME.tar.gz
tar -C "$STAGE" -czf "$TARBALL" "$NAME"
(cd "$OUT" && sum "$NAME.tar.gz" > "$NAME.tar.gz.sha256")
echo "created $TARBALL"
echo "verify after transfer: sha256sum -c $NAME.tar.gz.sha256, then extract and run sha256sum -c SHA256SUMS inside"
