#!/bin/sh
# Export the Icon Composer document to PNGs and derive the web favicons.
#
# Requirements (macOS only):
#   - Xcode with Icon Composer; ICTOOL points at the ictool bundled inside
#     Icon Composer.app (Contents/Executables/ictool). /usr/bin/ictool is the
#     Xcode asset-catalog shim and does not accept --export-image.
#   - PYTHON points at a Python 3 interpreter with Pillow (for 8-bit PNG
#     conversion and the .ico file), for example a throwaway virtualenv.
#
# Usage from the repository root:
#   ICTOOL=".../Icon Composer.app/Contents/Executables/ictool" PYTHON=.../python3 assets/icon/export.sh
set -eu

ICTOOL="${ICTOOL:-/Applications/Xcode.app/Contents/Applications/Icon Composer.app/Contents/Executables/ictool}"
PYTHON="${PYTHON:-python3}"
DOC="assets/icon/AppIcon.icon"
RAW="$(mktemp -d)"
trap 'rm -rf "$RAW"' EXIT

python3 assets/icon/generate_layers.py

render() { # platform rendition size output
	"$ICTOOL" "$DOC" --export-image --output-file "$RAW/$4" --platform "$1" --rendition "$2" \
		--width "$3" --height "$3" --scale 1 >/dev/null
}

for size in 1024 512 256 180 128 64 48 32 16; do
	render macOS Default "$size" "icon-$size.png"
done
render macOS Dark 1024 icon-dark-1024.png

"$PYTHON" assets/icon/derive.py "$RAW"
