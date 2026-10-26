#!/usr/bin/env python3
"""Generate release notes from conventional commit subjects in Git history."""
# SPDX-License-Identifier: AGPL-3.0-only

import argparse
from collections import defaultdict
from pathlib import Path
import re
import subprocess

TAG = re.compile(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)\Z")
SUBJECT = re.compile(r"(?P<type>[a-z]+)(?:\((?P<scope>[^()]+)\))?(?P<breaking>!)?: (?P<text>.+)\Z")
SECTIONS = (("Breaking changes", "breaking"), ("Features", "feat"),
            ("Fixes", "fix"), ("Performance", "perf"),
            ("Documentation", "docs"), ("Maintenance", "other"))
TEMPLATE = Path(__file__).resolve().parents[1] / "docs/RELEASE_NOTES_TEMPLATE.md"


def git(*args: str) -> str:
    return subprocess.check_output(["git", *args], text=True).strip()


def version(tag: str) -> tuple[int, int, int]:
    match = TAG.fullmatch(tag)
    if not match:
        raise ValueError(f"invalid release tag {tag!r}; expected vMAJOR.MINOR.PATCH")
    return tuple(map(int, match.groups()))


def previous_tag(tag: str) -> str | None:
    current = version(tag)
    candidates = []
    for candidate in git("tag", "--list", "v*").splitlines():
        if TAG.fullmatch(candidate) and version(candidate) < current:
            if subprocess.run(["git", "merge-base", "--is-ancestor", candidate, tag],
                              check=False).returncode == 0:
                candidates.append(candidate)
    return max(candidates, key=version) if candidates else None


def escape(value: str) -> str:
    return re.sub(r"([\\`*_{}\[\]()<>!|])", r"\\\1", value)


def render(tag: str, prior: str | None, subjects: list[str]) -> str:
    groups: dict[str, list[str]] = defaultdict(list)
    for subject in subjects:
        match = SUBJECT.fullmatch(subject)
        if match:
            kind = match["type"]
            group = "breaking" if match["breaking"] else kind if kind in {"feat", "fix", "perf", "docs"} else "other"
            scope = f"**{escape(match['scope'])}:** " if match["scope"] else ""
            line = f"- {scope}{escape(match['text'])}"
        else:
            group, line = "other", f"- {escape(subject)}"
        groups[group].append(line)
    lines = []
    for heading, key in SECTIONS:
        if groups[key]:
            lines.extend((f"### {heading}", "", *groups[key], ""))
    if not subjects:
        lines.extend(("No commit subjects in this range.", ""))
    changes = "\n".join(lines)
    return (TEMPLATE.read_text().replace("{tag}", tag)
            .replace("{changes}", changes)
            .replace("{range}", f"Changes since {prior}." if prior else "Initial release range."))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    version(args.tag)
    git("rev-parse", "--verify", f"refs/tags/{args.tag}^{{commit}}")
    prior = previous_tag(args.tag)
    rev_range = f"{prior}..{args.tag}" if prior else args.tag
    subjects = git("log", "--format=%s", rev_range).splitlines()
    args.output.write_text(render(args.tag, prior, subjects))


if __name__ == "__main__":
    main()
