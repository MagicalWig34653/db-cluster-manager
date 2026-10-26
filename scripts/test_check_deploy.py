"""Regression checks for CI publication boundaries; no network or Docker."""
import contextlib
import io
from pathlib import Path
import runpy
import tempfile
import unittest

with contextlib.redirect_stdout(io.StringIO()):
    checker = runpy.run_path(str(Path(__file__).with_name("check_deploy.py")))
ROOT = checker["ROOT"]


class WorkflowGuards(unittest.TestCase):
    def check_mutation(self, workflow, old, new):
        text = (ROOT / f".github/workflows/{workflow}.yml").read_text()
        self.assertIn(old, text)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / f"{workflow}.yml"
            path.write_text(text.replace(old, new))
            checker["errors"].clear()
            checker[f"check_{workflow}_workflow"](path)
            self.assertTrue(checker["errors"], f"accepted mutation: {old} -> {new}")

    def test_native_runner_required(self):
        self.check_mutation("build", "ubuntu-24.04-arm", "ubuntu-24.04")

    def test_race_tests_required(self):
        self.check_mutation("build", "go test -race", "go test")

    def test_no_ci_push(self):
        self.check_mutation("build", "push: false", "push: true")

    def test_no_ci_write_permission(self):
        self.check_mutation("build", "contents: read", "contents: write")

    def test_no_other_ci_write_permission(self):
        self.check_mutation("build", "contents: read", "contents: read\n  issues: write")

    def test_no_release_branch_trigger(self):
        self.check_mutation("release", 'tags: ["v*"]', 'tags: ["v*"]\n    branches: [main]')

    def test_attestations_required(self):
        self.check_mutation("build", "sbom: true", "sbom: false")
        self.check_mutation("release", "provenance: mode=max", "provenance: false")

    def test_release_supply_chain_required(self):
        self.check_mutation("release", "sha256sum -c SHA256SUMS", "true")
        self.check_mutation("release", "cyclonedx-gomod@v1.9.0", "cyclonedx-gomod@latest")
        self.check_mutation("release", "cosign@v3.0.2", "cosign@latest")
        self.check_mutation("release", 'cosign" sign --yes "${IMAGE}@${DIGEST}"', "true")
        self.check_mutation("release", "needs: [binaries, image]", "needs: binaries")
        self.check_mutation("release", "goarch: [amd64, arm64]", "goarch: [amd64]")

    def test_release_oidc_scoped_to_image(self):
        self.check_mutation("release", "contents: write", "contents: write\n      id-token: write")

    def test_tag_guard_required(self):
        self.check_mutation("release", "startsWith(github.ref, 'refs/tags/v')", "true")

    def test_validation_required_before_publication(self):
        self.check_mutation("release", "needs: verify", "needs: []")

    def test_job_image_required(self):
        self.check_mutation("release", "dockerfile: deploy/Dockerfile.jobs", "dockerfile: deploy/Dockerfile")

    def test_no_extra_registry_permissions(self):
        self.check_mutation("release", "packages: write", "packages: write\n      id-token: write")

    def test_immutable_action_pin_required(self):
        self.check_mutation("build", "actions/checkout@11d5960a326750d5838078e36cf38b85af677262", "actions/checkout@v4")


class HAStackGuards(unittest.TestCase):
    def test_unique_replica_identity_and_stop_first(self):
        src = (ROOT / "deploy/stack-ha.yaml").read_text()
        for old, new in (("manager-data-2:/var/lib/dbcm", "manager-data-1:/var/lib/dbcm"),
                         ("source: manager-config-3", "source: manager-config-2"),
                         ("order: stop-first", "order: start-first")):
            with tempfile.TemporaryDirectory() as tmp:
                path = Path(tmp) / "stack-ha.yaml"
                path.write_text(src.replace(old, new))
                checker["errors"].clear()
                checker["check_stack_ha"](path)
                self.assertTrue(checker["errors"], f"accepted mutation: {old} -> {new}")


if __name__ == "__main__":
    unittest.main()
