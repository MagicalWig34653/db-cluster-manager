"""Static checks for deployment files (stdlib only; no Docker daemon needed).

Checks deploy/Dockerfile, deploy/Dockerfile.jobs, deploy/compose.yaml,
deploy/stack.yaml and the release workflow. It is NOT a replacement for hadolint, `docker compose
config` or a real image build, and it does not prove the image builds.
PyYAML is used for a real parse only if importable; otherwise a simple
text-level structural check runs and says so.
"""
from pathlib import Path
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
    if "docker.sock" in re.sub(r"(?m)^\s*#.*$", "", text):
        err(f"{path.name}: docker socket must not be mounted")
    if re.search(r"(?mi)^\s*(password|secret|token)\w*\s*:\s*\S+", text):
        err(f"{path.name}: possible inline secret")
    return text


def check_compose(path: Path) -> None:
    text = text_structure(path)
    for needle in ("services:", "manager:", "volumes:", "read_only: true", "image:"):
        if needle not in text:
            err(f"{path.name}: missing {needle!r}")
    if not re.search(r'"\$\{DBCM_BIND:-127\.0\.0\.1\}:', text):
        err(f"{path.name}: published port must default to 127.0.0.1")
    data = load_yaml(path)
    if data is not None and "manager" not in data.get("services", {}):
        err(f"{path.name}: no manager service")


def check_stack(path: Path) -> None:
    text = text_structure(path)
    for needle in ("replicas: 3", "healthcheck:", "driver: overlay", "resources:", "limits:", "spread:", "read_only: true"):
        if needle not in text:
            err(f"{path.name}: missing {needle!r}")
    if "NOT" not in text or "metastore" not in text:
        err(f"{path.name}: must state honestly that HA needs the metastore peers configuration")
    data = load_yaml(path)
    if data is not None:
        dep = data["services"]["manager"]["deploy"]
        if dep.get("replicas") != 3:
            err(f"{path.name}: replicas must be 3")


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


def check_sh(path: Path) -> None:
    r = subprocess.run(["sh", "-n", str(path)], capture_output=True, text=True)
    if r.returncode:
        err(f"{path.name}: sh -n failed: {r.stderr.strip()}")


check_dockerfile(ROOT / "deploy/Dockerfile")
check_jobs_dockerfile(ROOT / "deploy/Dockerfile.jobs")
check_compose(ROOT / "deploy/compose.yaml")
check_stack(ROOT / "deploy/stack.yaml")
check_workflow(ROOT / ".github/workflows/release.yml")
check_sh(ROOT / "scripts/make-offline-bundle.sh")

for n in notes:
    print("note:", n)
if errors:
    print("\n".join(errors))
    sys.exit(1)
print("deploy checks passed (static only; no image was built)")
