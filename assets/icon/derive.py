#!/usr/bin/env python3
"""Turn raw ictool exports (16-bit PNG) into the committed icon files.

Called by assets/icon/export.sh with the directory of raw exports. Needs
Pillow. Writes 8-bit PNGs and .ico files to assets/icon/png, web/static and
docs/assets/icon. Run from the repository root.
"""
import shutil
import sys
from pathlib import Path

from PIL import Image

RAW = Path(sys.argv[1])
ROOT = Path.cwd()
PNG = ROOT / "assets/icon/png"
WEB = ROOT / "web/static"
DOCS = ROOT / "docs/assets/icon"
FAVICON_SVG = ROOT / "assets/icon/favicon.svg"


def load(name: str) -> Image.Image:
    return Image.open(RAW / name).convert("RGBA")


def save(img: Image.Image, dest: Path) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    img.save(dest, optimize=True)
    if dest.stat().st_size > 400_000:  # repository limit per PNG
        img.quantize(256, method=Image.Quantize.FASTOCTREE).save(dest, optimize=True)


for size in (1024, 512, 256, 128, 64, 32, 16):
    save(load(f"icon-{size}.png"), PNG / f"icon-{size}.png")
save(load("icon-dark-1024.png"), PNG / "icon-dark-1024.png")

ico_sizes = [(16, 16), (32, 32), (48, 48)]
ico = load("icon-48.png")
for target in (WEB / "favicon.ico", DOCS / "favicon.ico"):
    target.parent.mkdir(parents=True, exist_ok=True)
    ico.save(target, sizes=ico_sizes)

for target in (WEB, DOCS):
    save(load("icon-180.png"), target / "apple-touch-icon.png")
    shutil.copyfile(FAVICON_SVG, target / "favicon.svg")
save(load("icon-256.png"), DOCS / "logo-256.png")
