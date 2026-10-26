"""Tests for release notes; all Git activity stays in a temporary repository."""
# SPDX-License-Identifier: AGPL-3.0-only

from pathlib import Path
import subprocess
import tempfile
import unittest

from release_notes import render, version

SCRIPT = Path(__file__).with_name("release_notes.py")


class ReleaseNotesTests(unittest.TestCase):
    def test_version_rejects_noncanonical_tags(self):
        for tag in ("v1.2", "v01.2.3", "v1.2.3-rc1", "v1.2.3; echo bad"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                version(tag)

    def test_grouping_and_markdown_escaping(self):
        notes = render("v1.2.3", "v1.2.2", ["feat(api)!: remove *old*", "fix(ui): show [state]", "unclassified subject"])
        self.assertIn("### Breaking changes\n\n- **api:** remove \\*old\\*", notes)
        self.assertIn("### Fixes\n\n- **ui:** show \\[state\\]", notes)
        self.assertIn("### Maintenance\n\n- unclassified subject", notes)

    def test_git_range_ignores_older_commits(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)

            def git(*args):
                subprocess.run(["git", *args], cwd=root, check=True, capture_output=True)

            git("init", "-q")
            git("config", "user.name", "Release Test")
            git("config", "user.email", "release@example.invalid")
            (root / "file").write_text("first")
            git("add", "file")
            git("commit", "-qm", "feat: old")
            git("tag", "v1.0.0")
            (root / "file").write_text("second")
            git("commit", "-qam", "fix(api): new")
            git("tag", "v1.0.1")
            output = root / "notes.md"
            subprocess.run(["python3", str(SCRIPT), "--tag", "v1.0.1", "--output", str(output)], cwd=root, check=True)
            notes = output.read_text()
            self.assertIn("new", notes)
            self.assertNotIn("old", notes)
            self.assertIn("Changes since v1.0.0", notes)


if __name__ == "__main__":
    unittest.main()
