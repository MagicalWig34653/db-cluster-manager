"""Static checks for deployment files (stdlib only; no Docker daemon needed).

Checks deploy/Dockerfile, deploy/Dockerfile.jobs, deploy/compose.yaml,
deploy/stack.yaml, deploy/manager.example.json and the build/release workflows. It is NOT a replacement for hadolint, `docker compose
config` or a real image build, and it does not prove the image builds.
PyYAML is used for a real parse only if importable; otherwise a simple
text-level structural check runs and says so.
"""
from pathlib import Path
import json
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
errors: list[str] = []
notes: list[str] = []


def err(msg: str) -> None:
    errors.append(msg)


def check_dockerfile(path: Path, required=("EXPOSE", "HEALTHCHECK")) -> list[tuple[str, str]]:
    """Generic checks; returns the logical instructions as (OP, rest)."""
    text = path.read_text()
    # Join line continuations, drop comments.
    logical = []
    buf = ""
    for raw in text.splitlines():
        line = raw.rstrip()
        if not buf and (not line.strip() or line.lstrip().startswith("#")):
            continue
        if line.endswith("\\"):
            buf += line[:-1] + " "
            continue
        logical.append((buf + line).strip())
        buf = ""
    args: dict[str, str] = {}
    stages: set[str] = set()
    user = None
    seen = {k: False for k in required}
    parsed: list[tuple[str, str]] = []
    for ins in logical:
        op, _, rest = ins.partition(" ")
        op = op.upper()
        parsed.append((op, rest))
        if op == "ARG":
            k, _, v = rest.partition("=")
            args[k.strip()] = v.strip()
        elif op == "FROM":
            m = re.match(r"(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?", rest, re.I)
            ref = m.group(1)
            ref = re.sub(r"\$\{(\w+)\}|\$(\w+)", lambda x: args.get(x.group(1) or x.group(2), x.group(0)), ref)
            if ref not in stages:
                if "$" in ref:
                    err(f"{path.name}: unresolved variable in FROM {ref}")
                name, _, digest = ref.partition("@")
                tag = name.rsplit(":", 1)[1] if ":" in name.rsplit("/", 1)[-1] else ""
                if not tag or tag == "latest":
                    err(f"{path.name}: base image without pinned tag: {ref}")
                if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                    err(f"{path.name}: base image not pinned by digest: {ref}")
            if m.group(2):
                stages.add(m.group(2))
        elif op == "USER":
            user = rest.split(":")[0].strip()
        elif op in seen:
            seen[op] = True
        elif op in ("COPY", "ADD"):
            parts = rest.split()
            if any(p.startswith("--from=") for p in parts):
                continue
            srcs = [p for p in parts if not p.startswith("--")][:-1]
            for s in srcs:
                if re.match(r"https?://", s):
                    err(f"{path.name}: remote ADD/COPY source {s}")
                elif not list(ROOT.glob(s.rstrip("/"))):
                    err(f"{path.name}: COPY source does not exist: {s}")
        if re.search(r"(password|secret|token|api[_-]?key)\s*=\s*\S+", ins, re.I) and op in ("ENV", "ARG", "RUN"):
            err(f"{path.name}: possible secret in {op}: {ins[:60]}")
    if user is None or user in ("root", "0"):
        err(f"{path.name}: final USER must be non-root (got {user!r})")
    for k, v in seen.items():
        if not v:
            err(f"{path.name}: missing {k}")
    if ":latest" in text:
        err(f"{path.name}: contains ':latest'")
    if not (ROOT / ".dockerignore").exists():
        err(".dockerignore is missing")
    return parsed


# Tools dbbackup.AllowedTools runs inside the job image.
JOB_TOOLS = ("mariabackup", "mbstream", "mariadb-dump", "mariadb")


def check_jobs_dockerfile(path: Path) -> None:
    """Job image: generic checks plus the job contract (ADR 0009/0011)."""
    parsed = check_dockerfile(path, required=("HEALTHCHECK",))
    text = path.read_text()
    args = {r.partition("=")[0].strip(): r.partition("=")[2].strip() for op, r in parsed if op == "ARG"}
    base = args.get("MARIADB_IMAGE", "")
    if not re.fullmatch(r"mariadb:\d+\.\d+\.\d+@sha256:[0-9a-f]{64}", base):
        err(f"{path.name}: MARIADB_IMAGE must be the official mariadb image pinned by full version tag and digest (got {base!r})")
    froms = [r for op, r in parsed if op == "FROM"]
    if not froms or "${MARIADB_IMAGE}" not in froms[-1]:
        err(f"{path.name}: the final stage must be built FROM ${{MARIADB_IMAGE}}")
    entry = [r for op, r in parsed if op == "ENTRYPOINT"]
    if not entry or "/usr/local/bin/dbcm-job" not in entry[-1]:
        err(f"{path.name}: ENTRYPOINT must be /usr/local/bin/dbcm-job (agent.JobCommand)")
    if "./cmd/dbcm-job" not in text or not (ROOT / "cmd/dbcm-job/main.go").exists():
        err(f"{path.name}: must build ./cmd/dbcm-job, which must exist")
    users = [r.strip() for op, r in parsed if op == "USER"]
    if not users or users[-1].split(":")[0] in ("root", "0", "mysql"):
        err(f"{path.name}: final USER must be numeric and non-root (got {users[-1] if users else None!r})")
    elif users[-1] != "999:999":
        err(f"{path.name}: final USER {users[-1]} differs from galera.DefaultDBUID/GID 999:999")
    runs = " ".join(r for op, r in parsed if op == "RUN")
    for t in JOB_TOOLS:
        if t not in runs:
            err(f"{path.name}: no build-time check that {t} exists")
    gofile = (ROOT / "internal/dbbackup/job.go").read_text()
    m = re.search(r"AllowedTools = \[\]string\{([^}]*)\}", gofile)
    if not m or sorted(re.findall(r'"([^"]+)"', m.group(1))) != sorted(JOB_TOOLS):
        err(f"{path.name}: JOB_TOOLS out of step with dbbackup.AllowedTools")
    spec = (ROOT / "internal/galera/spec.go").read_text()
    if "DefaultDBUID = 999" not in spec or "DefaultDBGID = 999" not in spec:
        err(f"{path.name}: galera default database owner is no longer 999:999; update USER")
    if re.search(r"(?mi)^\s*(EXPOSE|VOLUME)\b", text):
        err(f"{path.name}: a job image publishes no port and declares no volume")


def load_yaml(path: Path):
    try:
        import yaml  # type: ignore
    except ImportError:
        notes.append(f"{path.name}: PyYAML not installed; only text-level structural checks ran (no real YAML parse)")
        return None
    try:
        return yaml.safe_load(path.read_text())
    except Exception as e:  # noqa: BLE001
        err(f"{path.name}: YAML parse error: {e}")
        return None


def text_structure(path: Path) -> str:
    text = path.read_text()
    for n, line in enumerate(text.splitlines(), 1):
        if "\t" in line:
            err(f"{path.name}:{n}: tab character")
        indent = len(line) - len(line.lstrip(" "))
        if line.strip() and not line.lstrip().startswith("#") and indent % 2:
            err(f"{path.name}:{n}: odd indentation")
    if re.search(r":latest\b", text):
        err(f"{path.name}: ':latest' tag")
    if re.search(r"(?mi)^\s*(password|secret|token|passphrase)\w*\s*:\s*\S+", text):
        err(f"{path.name}: possible inline secret")
    if re.search(r"BEGIN [A-Z ]*PRIVATE KEY|dbcmrk1:", text):
        err(f"{path.name}: contains key material")
    return text


SOCKET = "/var/run/docker.sock"
CONFIG_TARGET = "/etc/dbcm/manager.json"
DATA_DIR = "/var/lib/dbcm"
BACKEND_FILE = "/run/secrets/dbcm-backend.json"


def uncommented(text: str) -> str:
    return re.sub(r"(?m)^\s*#.*$", "", text)


def check_socket_guard(path: Path, text: str, svc) -> None:
    """The Docker socket is root-equivalent (ADR 0014). It may be mounted only
    with uid 65532 whose group is the required DBCM_DOCKER_GID, a read-only
    root filesystem and all capabilities dropped."""
    body = uncommented(text)
    if SOCKET not in body:
        err(f"{path.name}: managed mode needs the Docker socket mount ({SOCKET})")
        return
    if "ADR 0014" not in text:
        err(f"{path.name}: the socket mount must reference ADR 0014")
    if not re.search(r'(?m)^\s*user:\s*"65532:\$\{DBCM_DOCKER_GID:\?', body):
        err(f"{path.name}: the socket mount needs user 65532 with the required DBCM_DOCKER_GID group")
    for needle in ("read_only: true", "cap_drop: [ALL]"):
        if needle not in body:
            err(f"{path.name}: the socket mount needs {needle!r}")
    if svc is not None:
        if str(svc.get("user", "")).split(":")[0] in ("", "0", "root"):
            err(f"{path.name}: the manager must not run as root")
        mounts = [v if isinstance(v, str) else f"{v.get('source')}:{v.get('target')}" for v in svc.get("volumes", [])]
        if not any(m.startswith(SOCKET + ":") for m in mounts):
            err(f"{path.name}: the socket is not a volume of the manager service")


def secret_file_refs(cfg) -> set:
    """Paths below /run/secrets that a configuration reads."""
    out = set()

    def walk(v):
        if isinstance(v, dict):
            for x in v.values():
                walk(x)
        elif isinstance(v, list):
            for x in v:
                walk(x)
        elif isinstance(v, str) and v.startswith("/run/secrets/"):
            out.add(v)
    walk(cfg)
    return out


def check_example_config(path: Path):
    """deploy/manager.example.json: JSON parse and secret-free here; the Go
    test TestShippedExampleConfig loads it with the manager's config loader
    and checks that `manager init` reproduces it."""
    text = path.read_text()
    try:
        cfg = json.loads(text)
    except ValueError as e:
        err(f"{path.name}: JSON parse error: {e}")
        return None
    if re.search(r'BEGIN [A-Z ]*PRIVATE KEY|dbcmrk1:|"password"', text):
        err(f"{path.name}: contains secret material")
    m = cfg.get("manager") or {}
    if m.get("dataDir") != DATA_DIR:
        err(f"{path.name}: manager.dataDir must be {DATA_DIR} (the volume target)")
    if m.get("labAgents"):
        err(f"{path.name}: labAgents must be off")
    if not cfg.get("auth", {}).get("enabled"):
        err(f"{path.name}: auth must be enabled")
    test = (ROOT / "cmd/manager/init_test.go").read_text()
    if "deploy/manager.example.json" not in test or "func TestShippedExampleConfig" not in test:
        err(f"{path.name}: the Go test that loads it is missing")
    return cfg


def check_manager_service(path: Path, text: str, svc, cfg) -> None:
    body = uncommented(text)
    for needle in ("DCM_CONFIG: " + CONFIG_TARGET, "target: " + CONFIG_TARGET, "manager-data:" + DATA_DIR,
                   "healthcheck:", "-healthcheck", "limits:", "memory:", "/tmp"):
        if needle not in body:
            err(f"{path.name}: missing {needle!r}")
    if not re.search(r'"-listen", "127\.0\.0\.1:\$\{DBCM_PORT:-8080\}"', body):
        err(f"{path.name}: the API must listen on 127.0.0.1 (host network)")
    if re.search(r"(?m)^\s*ports:", body):
        err(f"{path.name}: no published ports (host network, loopback API)")
    if svc is not None:
        hc = " ".join(svc.get("healthcheck", {}).get("test", []))
        if "127.0.0.1:${DBCM_PORT:-8080}" not in hc:
            err(f"{path.name}: the healthcheck must probe the loopback API port")
    if cfg is not None and secret_file_refs(cfg):
        err(f"{path.name}: the example config reads {sorted(secret_file_refs(cfg))}, which the default file does not mount")
    if "target: dbcm-backend.json" not in text:
        err(f"{path.name}: the optional backend secret (target dbcm-backend.json) is not documented")
    init = (ROOT / "cmd/manager/init.go").read_text()
    for const in (CONFIG_TARGET, DATA_DIR, BACKEND_FILE):
        if f'"{const}"' not in init:
            err(f"cmd/manager/init.go: path {const} out of step with {path.name}")


def check_compose(path: Path, cfg) -> None:
    text = text_structure(path)
    if "${DBCM_CONFIG_FILE:?" not in text:
        err(f"{path.name}: the config file must be required through DBCM_CONFIG_FILE")
    data = load_yaml(path)
    svc = None
    if data is not None:
        svc = data.get("services", {}).get("manager")
        if svc is None:
            err(f"{path.name}: no manager service")
        elif svc.get("network_mode") != "host":
            err(f"{path.name}: the manager must use the host network (as in lab E4)")
        if "file" not in (data.get("configs", {}).get("manager-config") or {}):
            err(f"{path.name}: manager-config must come from a file")
    check_socket_guard(path, text, svc)
    check_manager_service(path, text, svc, cfg)


def check_stack(path: Path, cfg) -> None:
    text = text_structure(path)
    body = uncommented(text)
    for needle in ("replicas: 1", "node.role == manager", "node.hostname == ${DBCM_MANAGER_NODE:?",
                   "order: stop-first", "name: host", "external: true", "${DBCM_CONFIG_NAME:?"):
        if needle not in body:
            err(f"{path.name}: missing {needle!r}")
    if "NOT" not in text or "metastore" not in text or "HA" not in text or "never run" not in text.lower():
        err(f"{path.name}: must state honestly that it is single-replica, never run, and how HA differs")
    data = load_yaml(path)
    svc = None
    if data is not None:
        svc = data["services"]["manager"]
        dep = svc["deploy"]
        if dep.get("replicas") != 1:
            err(f"{path.name}: replicas must be 1 (one metastore volume, one identity)")
        cons = dep.get("placement", {}).get("constraints", [])
        if "node.role == manager" not in cons or not any(c.startswith("node.hostname ==") for c in cons):
            err(f"{path.name}: must be pinned to one Swarm manager node")
        if dep.get("update_config", {}).get("order") != "stop-first":
            err(f"{path.name}: updates must be stop-first")
        if svc.get("networks") != ["hostnet"] or data.get("networks", {}).get("hostnet", {}).get("name") != "host":
            err(f"{path.name}: the manager must use the host network")
        if not data.get("configs", {}).get("manager-config", {}).get("external"):
            err(f"{path.name}: manager-config must be an external Swarm config")
        for name, s in (data.get("secrets") or {}).items():
            if not s.get("external"):
                err(f"{path.name}: secret {name} must be external")
    check_socket_guard(path, text, svc)
    check_manager_service(path, text, svc, cfg)


def check_workflow(path: Path) -> None:
    text = path.read_text()
    if "permissions:" not in text:
        err(f"{path.name}: no permissions block")
    if re.search(r"(?m)^permissions:\s*write-all", text):
        err(f"{path.name}: write-all permissions")
    for m in re.finditer(r"(?m)^\s*-?\s*uses:\s*(\S+)(.*)$", text):
        ref, tail = m.group(1), m.group(2)
        if ref.startswith("./"):
            continue
        if not re.fullmatch(r"[\w./-]+@[0-9a-f]{40}", ref):
            err(f"{path.name}: action not pinned to a commit SHA: {ref}")
        elif not re.search(r"#\s*v\d", tail):
            err(f"{path.name}: pinned action lacks '# vX' comment: {ref}")
    if re.search(r"secrets\.(?!GITHUB_TOKEN)", text):
        err(f"{path.name}: uses secrets other than GITHUB_TOKEN")
    if "\t" in text:
        err(f"{path.name}: tab character")
    data = load_yaml(path)
    if data is not None and not isinstance(data, dict):
        err(f"{path.name}: not a mapping")


def require_text(path: Path, text: str, needles) -> None:
    for needle in needles:
        if needle not in text:
            err(f"{path.name}: missing CI contract {needle!r}")


def check_build_workflow(path: Path) -> None:
    """Fail closed on changes to the read-only native build contract.

    These text checks also run without PyYAML; they deliberately require the
    reviewed job layout. They are static guards, not an Actions interpreter.
    """
    check_workflow(path)
    text = uncommented(path.read_text())
    require_text(path, text, (
        "  workflow_call:", "    runs-on: ${{ matrix.runner }}",
        "- runner: ubuntu-24.04\n            arch: amd64",
        "- runner: ubuntu-24.04-arm\n            arch: arm64",
        'test "$(go env GOHOSTARCH)" = "$EXPECTED_ARCH"',
        "persist-credentials: false", "make build", "make check",
        "python3 scripts/check_deploy.py", "go test -race -count=1 -p 4 ./...",
        "working-directory: web\n        run: npm test",
    ))
    if re.search(r"(?m)^\s*[\w-]+:\s*write|write-all|secrets\.", text):
        err(f"{path.name}: native build must not have write permissions or secrets")
    builds = re.findall(r"(?ms)^      - name: Build [^\n]+image\n(.*?)(?=^      - |\Z)", text)
    if len(builds) != 2:
        err(f"{path.name}: expected exactly two container builds")
    for build, dockerfile in zip(builds, ("deploy/Dockerfile", "deploy/Dockerfile.jobs")):
        require_text(path, build, (f"file: {dockerfile}", "platforms: linux/${{ matrix.arch }}",
                                  "push: false", "outputs: type=oci,dest=", "sbom: true",
                                  "provenance: mode=max"))
    if re.search(r"(?m)^\s*push: true|docker/login-action|--push", text):
        err(f"{path.name}: native build may not log in or push")


def check_release_workflow(path: Path) -> None:
    check_workflow(path)
    text = uncommented(path.read_text())
    require_text(path, text, (
        '    tags: ["v*"]', "permissions: {}",
        "  verify:\n    uses: ./.github/workflows/build.yml",
        "  web:\n    needs: verify",
        "  image:\n    needs: verify",
        "    if: github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')",
        '- dockerfile: deploy/Dockerfile\n            suffix: ""',
        '- dockerfile: deploy/Dockerfile.jobs\n            suffix: "-jobs"',
        "images: ghcr.io/${{ github.repository }}${{ matrix.suffix }}",
        "registry: ghcr.io", "file: ${{ matrix.dockerfile }}",
        "platforms: linux/amd64,linux/arm64", "push: true",
        "sbom: true", "provenance: mode=max",
    ))
    if not re.search(r'(?m)^on:\n  push:\n    tags: \["v\*"\]\npermissions: \{\}', text):
        err(f"{path.name}: expected only the version-tag push trigger")
    # Only the image job needs registry writes; release creation needs contents.
    jobs = re.split(r"(?m)^  ([\w-]+):\n", text.split("jobs:\n", 1)[-1])
    for name, body in zip(jobs[1::2], jobs[2::2]):
        writes = re.findall(r"(?m)^\s+([\w-]+): write\s*$", body)
        expected = {"image": ["packages"], "release": ["contents"]}.get(name, [])
        if writes != expected:
            err(f"{path.name}: unexpected write permissions in {name}: {writes}")
    if re.search(r"(?m)^  (?:pull_request|pull_request_target|workflow_dispatch|workflow_call|schedule):", text):
        err(f"{path.name}: publication must only run on version tag pushes")


def check_sh(path: Path) -> None:
    r = subprocess.run(["sh", "-n", str(path)], capture_output=True, text=True)
    if r.returncode:
        err(f"{path.name}: sh -n failed: {r.stderr.strip()}")


check_dockerfile(ROOT / "deploy/Dockerfile")
check_jobs_dockerfile(ROOT / "deploy/Dockerfile.jobs")
example = check_example_config(ROOT / "deploy/manager.example.json")
check_compose(ROOT / "deploy/compose.yaml", example)
check_stack(ROOT / "deploy/stack.yaml", example)
check_build_workflow(ROOT / ".github/workflows/build.yml")
check_release_workflow(ROOT / ".github/workflows/release.yml")
check_sh(ROOT / "scripts/make-offline-bundle.sh")

for n in notes:
    print("note:", n)
if errors:
    print("\n".join(errors))
    sys.exit(1)
print("deploy checks passed (static only; no image was built)")
