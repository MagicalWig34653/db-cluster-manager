"""Check repository Markdown file links and release coverage without dependencies.

Does not validate external URLs, anchors, rendered Pages links or website builds.
"""
from pathlib import Path
import re
import sys
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
files = [ROOT / "README.md", ROOT / "AGENTS.md", ROOT / "CLAUDE.md"]
files += sorted((ROOT / "docs").rglob("*.md"))
errors = []
for file in files:
    content = re.sub(r"```.*?```", "", file.read_text(), flags=re.S)
    for target in re.findall(r"\]\(([^)]+)\)", content):
        target = target.strip().strip("<>")
        parsed = urlsplit(target)
        if parsed.scheme or parsed.netloc or not parsed.path:
            continue
        destination = (file.parent / unquote(parsed.path)).resolve()
        if not destination.is_relative_to(ROOT) or not destination.exists():
            errors.append(f"{file.relative_to(ROOT)}: invalid local target {target}")

requirements = re.findall(
    r"^\| ([PADNBUX]\d{2}) \|", (ROOT / "docs/KONZEPT.md").read_text(), re.M
)
coverage = (ROOT / "docs/RELEASES.md").read_text().split("## Requirement coverage", 1)[1]
covered = set()
for line in coverage.splitlines():
    if not line.startswith("| "):
        continue
    cell = line.split("|")[1]
    for first, last in re.findall(r"([PADNBUX]\d{2})–([PADNBUX]\d{2})", cell):
        if first[0] != last[0] or int(first[1:]) > int(last[1:]):
            errors.append(f"Invalid coverage range: {first}–{last}")
            continue
        covered.update(f"{first[0]}{i:02}" for i in range(int(first[1:]), int(last[1:]) + 1))
    covered.update(re.findall(r"[PADNBUX]\d{2}", cell))
if not requirements or len(requirements) != len(set(requirements)):
    errors.append("Requirement IDs are missing or duplicated")
if set(requirements) != covered:
    errors.append(f"Coverage mismatch: missing={sorted(set(requirements) - covered)}, unknown={sorted(covered - set(requirements))}")
for error in errors:
    print(error, file=sys.stderr)
print(f"Checked {len(files)} Markdown files and {len(requirements)} requirements; {len(errors)} errors.")
sys.exit(bool(errors))
