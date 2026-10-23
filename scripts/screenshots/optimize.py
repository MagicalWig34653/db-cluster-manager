"""Quantize raw screenshots to 256-color PNGs (needs Pillow).

Usage: optimize.py <raw-dir> <output-dir>; see capture.mjs.
"""
import sys
from pathlib import Path
from PIL import Image

src, dst = Path(sys.argv[1]), Path(sys.argv[2])
dst.mkdir(parents=True, exist_ok=True)
total = 0
for p in sorted(src.glob("*.png")):
    im = Image.open(p).convert("RGB")
    q = im.quantize(256, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE)
    out = dst / p.name
    q.save(out, optimize=True)
    size = out.stat().st_size
    total += size
    print(f"{size // 1024:5d} KB {p.name}")
print(f"total {total // 1024} KB")
