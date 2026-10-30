#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Local dry run of the release workflow's archive, SBOM, checksum and release-note
# steps (.github/workflows/release.yml, jobs binaries/release). Pushes nothing,
# creates no tag, builds no image. Requires the web UI already built into
# internal/webui/assets (make web).
#
# Usage: scripts/rc-package.sh VERSION [OUTDIR]     e.g. scripts/rc-package.sh 0.1.0-rc1 dist/rc
set -eu -o pipefail
VERSION=${1:?usage: rc-package.sh VERSION [OUTDIR]}
OUT=${2:-dist/rc}
cd "$(dirname "$0")/.."
[ -f internal/webui/assets/index.html ] || { echo "web UI not built: run 'make web'" >&2; exit 1; }
COMMIT=$(git rev-parse --short=12 HEAD)
PKG=github.com/MagicalWig34653/db-cluster-manager/internal/version
SBOM=${CYCLONEDX_GOMOD:-$(go env GOPATH)/bin/cyclonedx-gomod}
[ -x "$SBOM" ] || go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.9.0
rm -rf "$OUT"
mkdir -p "$OUT"
# cyclonedx-gomod needs a tag on the main module (CI has one). Use a throwaway
# clone with a local tag so no tag is created in this repository.
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir "$TMP/src"
git archive HEAD | tar -x -C "$TMP/src"
git -C "$TMP/src" init -q
git -C "$TMP/src" add -A
git -C "$TMP/src" -c user.name=rc -c user.email=rc@localhost commit -q -m rc
git -C "$TMP/src" tag "v${VERSION}"
for GOARCH in amd64 arm64; do
	NAME="db-cluster-manager_${VERSION}_linux_${GOARCH}"
	mkdir -p "$OUT/$NAME"
	for c in manager agent cli; do
		CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH nice -n 15 go build -trimpath \
			-ldflags "-s -w -X ${PKG}.Version=${VERSION} -X ${PKG}.Commit=${COMMIT}" \
			-o "$OUT/$NAME/$c" "./cmd/$c"
		go version -m "$OUT/$NAME/$c" >"$OUT/$NAME/$c.buildinfo.txt"
		(cd "$TMP/src" && GOOS=linux GOARCH=$GOARCH "$SBOM" app -json -main "cmd/$c" -output "$TMP/$NAME-$c.bom.json" .)
		cp "$TMP/$NAME-$c.bom.json" "$OUT/$NAME/$c.bom.json"
		cp "$OUT/$NAME/$c.bom.json" "$OUT/$NAME-$c.bom.json"
	done
	cp LICENSE README.md "$OUT/$NAME/"
	cp -r deploy "$OUT/$NAME/deploy"
	# Same flags as the workflow (GNU tar); bsdtar on macOS lacks --sort.
	if tar --version 2>/dev/null | grep -q GNU; then
		tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2000-01-01 00:00:00 UTC' \
			-C "$OUT" -czf "$OUT/$NAME.tar.gz" "$NAME"
	else
		tar -C "$OUT" -czf "$OUT/$NAME.tar.gz" "$NAME"
		echo "note: bsdtar archive, not the reproducible GNU tar form" >&2
	fi
	rm -rf "${OUT:?}/$NAME"
done
python3 scripts/test_release_notes.py
# release_notes.py needs the tag in the git history of its working directory;
# use a throwaway repository holding HEAD's history and a local tag.
ROOT=$(pwd)
mkdir "$TMP/hist"
git -C "$TMP/hist" init -q
git -C "$TMP/hist" fetch -q "$ROOT" HEAD
git -C "$TMP/hist" tag "v${VERSION%%-*}" FETCH_HEAD
(cd "$TMP/hist" && python3 "$ROOT/scripts/release_notes.py" --tag "v${VERSION%%-*}" --output "$ROOT/$OUT/RELEASE_NOTES.md")
(
	cd "$OUT"
	test "$(find . -maxdepth 1 -name '*.tar.gz' | wc -l | tr -d ' ')" = 2
	test "$(find . -maxdepth 1 -name '*.bom.json' | wc -l | tr -d ' ')" = 6
	shasum -a 256 -- *.tar.gz *.bom.json >SHA256SUMS
	shasum -a 256 -c SHA256SUMS
	python3 -c 'import glob,json; [None if json.load(open(p))["bomFormat"] == "CycloneDX" else exit(1) for p in glob.glob("*.bom.json")]'
)
echo "release candidate artifacts in $OUT (nothing published)"
