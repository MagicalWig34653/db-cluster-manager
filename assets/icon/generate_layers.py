#!/usr/bin/env python3
"""Generate the SVG layers of the Icon Composer document (AppIcon.icon).

The icon is product-neutral: three database cylinders (cluster members)
connected as a quorum triangle around a hub. Run from the repository root:

    python3 assets/icon/generate_layers.py
"""
from pathlib import Path

HERE = Path(__file__).resolve().parent
ASSETS = HERE / "AppIcon.icon" / "Assets"
NODES = [(512, 318), (292, 712), (732, 712)]
HUB = (512, 581)
RX, RY, HALF, GAP = 105, 32, 80, 7


def cylinder(cx: int, cy: int) -> str:
    """Two stacked segments with a curved gap, read as a database disk."""
    upper = (f"M{cx-RX} {cy-HALF} A{RX} {RY} 0 0 1 {cx+RX} {cy-HALF} "
             f"V{cy-GAP} A{RX} {RY} 0 0 1 {cx-RX} {cy-GAP} Z")
    lower = (f"M{cx-RX} {cy+GAP} A{RX} {RY} 0 0 0 {cx+RX} {cy+GAP} "
             f"V{cy+HALF} A{RX} {RY} 0 0 1 {cx-RX} {cy+HALF} Z")
    return f"{upper} {lower}"


def nodes_svg() -> str:
    body = " ".join(cylinder(x, y) for x, y in NODES)
    tops = "".join(f'<ellipse cx="{x}" cy="{y-HALF}" rx="{RX}" ry="{RY}"/>' for x, y in NODES)
    return (
        '<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" viewBox="0 0 1024 1024">\n'
        f'<path fill="#FFFFFF" d="{body}"/>\n'
        f'<g fill="#A8D8FF">{tops}</g>\n'
        "</svg>\n"
    )


def links_svg() -> str:
    a, b, c = NODES
    edges = "".join(f'<line x1="{p[0]}" y1="{p[1]}" x2="{q[0]}" y2="{q[1]}"/>' for p, q in [(a, b), (a, c), (b, c)])
    spokes = "".join(f'<line x1="{HUB[0]}" y1="{HUB[1]}" x2="{x}" y2="{y}"/>' for x, y in NODES)
    return (
        '<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" viewBox="0 0 1024 1024">\n'
        f'<g stroke="#FFFFFF" stroke-width="30" stroke-linecap="round" fill="none">{edges}</g>\n'
        f'<g stroke="#FFFFFF" stroke-width="16" stroke-linecap="round" fill="none" opacity="0.7">{spokes}</g>\n'
        f'<circle cx="{HUB[0]}" cy="{HUB[1]}" r="44" fill="#FFFFFF"/>\n'
        "</svg>\n"
    )


def favicon_svg() -> str:
    """Flat composite of both layers for browsers (no Liquid Glass effects)."""
    def inner(svg: str) -> str:
        return "\n".join(svg.splitlines()[1:-1])
    return (
        '<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" viewBox="0 0 1024 1024">\n'
        '<defs><linearGradient id="bg" x1="0" y1="0" x2="0" y2="1">'
        '<stop offset="0" stop-color="#3D85FF"/><stop offset="1" stop-color="#1A2B85"/>'
        "</linearGradient></defs>\n"
        '<rect width="1024" height="1024" rx="230" fill="url(#bg)"/>\n'
        f'<g opacity="0.55">\n{inner(links_svg())}\n</g>\n'
        f"{inner(nodes_svg())}\n"
        "</svg>\n"
    )


if __name__ == "__main__":
    ASSETS.mkdir(parents=True, exist_ok=True)
    (ASSETS / "nodes.svg").write_text(nodes_svg())
    (ASSETS / "links.svg").write_text(links_svg())
    (HERE / "favicon.svg").write_text(favicon_svg())
