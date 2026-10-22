---
title: Installation and packaging
---

# Installation and packaging

Technical guide for installing the manager. It describes only what exists today. **Not implemented yet** (marked below): persistent manager state, Docker/Swarm connection, agent deployment, manager configuration backup, replicated metastore inside the manager, signed images. Verification status: files in `deploy/` and the release workflow are statically checked (`python3 scripts/check_deploy.py`); **the image has not been built, and the workflow, compose and stack files have not been run.**

## What the manager does today

It serves the web UI and `GET /api/v1/health` and `GET /api/v1/version`. It stores no state and has no Docker access. Installing it validates packaging, not cluster management.

## Artifacts

| Artifact | Source | Notes |
| --- | --- | --- |
| Release archives `db-cluster-manager_<version>_<os>_<arch>.tar.gz` | `.github/workflows/release.yml` on tag `v*` | linux/darwin, amd64/arm64, CGO disabled; contain `manager`, `agent`, `cli`, `deploy/`, per-binary `go version -m` listings |
| `SHA256SUMS` | same workflow | verify with `sha256sum -c SHA256SUMS` |
| Container image `ghcr.io/<owner>/db-cluster-manager:<version>` | same workflow, `deploy/Dockerfile` | linux/amd64 and linux/arm64; distroless, uid 65532, healthcheck via `manager -healthcheck` |
| Job image (backup/restore) | `deploy/Dockerfile.jobs` (not in the release workflow yet) | `mariadb:11.4.13` pinned by digest plus `dbcm-job` (`cmd/dbcm-job`); runs as uid/gid 999 (the image's `mysql`); **never built so far** ([ADR 0011](decisions/0011-galera-endpoints-and-bootstrap-sql.md)) |
| Offline bundle | `scripts/make-offline-bundle.sh` | binaries, compose/stack files, checksums, manifest, optional `docker save` image |

`cmd/metad` and `cmd/lab` are test harnesses and are not shipped. There is no proxy binary yet.

## Fresh install (single instance, test installations)

1. Obtain an image: pull `ghcr.io/<owner>/db-cluster-manager:<version>` (after a release exists), or build it locally from the repository root:
   `docker buildx build -f deploy/Dockerfile -t db-cluster-manager:local --build-arg VERSION=0.0.0-dev --load .`
   (Per project rules, use a dedicated local test environment and an explicit Docker context until the first Swarm trial; never the production daemon.)
2. `DBCM_IMAGE=db-cluster-manager:local docker compose -f deploy/compose.yaml up -d`
3. Open `http://127.0.0.1:8080/`. Check `curl http://127.0.0.1:8080/api/v1/health` and `docker compose -f deploy/compose.yaml ps` (should become `healthy`).

Defaults: port bound to loopback only (`DBCM_BIND`, `DBCM_PORT` override), read-only root filesystem, all capabilities dropped, named volume `manager-data`, **no Docker socket mounted**. To reach the UI from other machines, put a TLS reverse proxy in front rather than changing `DBCM_BIND`; the manager has no TLS or authentication yet.

### Connecting to a Docker API (not implemented)

External-Docker-API connection details are an open design item (requirement P04). When implemented the intended approach is a remote, mutually authenticated endpoint rather than mounting `/var/run/docker.sock`. Do not mount the socket into the manager container; it grants root on the host.

## Three-replica Swarm service (not highly available yet)

`deploy/stack.yaml` defines three replicas with `max_replicas_per_node: 1`, spread by node, an encrypted overlay network, a healthcheck, resource limits and a local volume per node. The manager does not yet embed the replicated metastore, so there is no peers configuration to set, and three replicas are three independent stateless instances. The metastore lab daemon takes `-id`, `-bind`, `-advertise`, `-data`, `-peers id=host:port,...` and `-bootstrap`; the manager will need an equivalent, with stable per-instance identities that `replicas: 3` alone does not provide. Deploy only to exercise the packaging: `docker stack deploy -c deploy/stack.yaml dbcm`.

## Binary install (without containers)

Download the archive for your platform, verify, extract, run:

```sh
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf db-cluster-manager_<version>_linux_amd64.tar.gz
./db-cluster-manager_<version>_linux_amd64/manager -listen 127.0.0.1:8080
```

No service unit is provided yet.

## Offline installation

Internet egress is the default; offline use is optional (P10). On a connected machine:

```sh
make build                                    # binaries in bin/
docker save db-cluster-manager:<version> -o image.tar   # optional, needs a built image
scripts/make-offline-bundle.sh -v <version> -i db-cluster-manager:<version>
```

The script's `-i` flag runs `docker save`; it never builds or pulls. Without `-i` the bundle holds binaries and files only. Transfer, then `sha256sum -c <bundle>.tar.gz.sha256`, extract, `sha256sum -c SHA256SUMS`, `docker load -i image/image.tar`, and continue with the compose steps above. Database images and agent images that future releases need are not part of this bundle yet. The non-docker parts of the script were tested; the `docker save` path was not.

## Upgrade

1. Read the release notes. 2. Pull or load the new image. 3. `DBCM_IMAGE=<new> docker compose -f deploy/compose.yaml up -d` (recreates the container; the volume is kept). 4. Confirm `GET /api/v1/version` and health. Rollback: set `DBCM_IMAGE` to the previous tag. For the stack: `docker service update --image <new> dbcm_manager` (rolling, one at a time, automatic rollback on failure). Because the manager holds no state today, there are no data migrations; this section must be revisited when state is introduced. Major-version upgrades will need explicit approval once they exist.

## Backup and restore of manager configuration (not implemented)

There is no manager configuration to back up yet; the volume is empty. Manager configuration backup and restore from setup (requirement B08, R1 gate) are planned. Until then the only artifact worth keeping is your `DBCM_*` environment and the compose/stack files you used.

## Teardown and uninstall

Scoped to this stack only; none of these commands touches other projects:

```sh
docker compose -f deploy/compose.yaml down            # stop and remove the container
docker compose -f deploy/compose.yaml down -v         # also delete the manager-data volume
docker image rm db-cluster-manager:local               # remove the image you built/loaded
# Swarm variant
docker stack rm dbcm                                   # stops services; volumes remain
docker volume rm dbcm_manager-data                     # on every node that ran a replica
```

Do not use `docker system prune`. Binary install: delete the extracted directory. Because the manager does not yet create agents or databases, there is nothing else to clean up; once it does, the teardown section must list agent services, database cluster services and their volumes explicitly.

## Image details and reproducibility

- Base images are pinned by tag and digest in `deploy/Dockerfile` (resolved from the registries on 2026-10-20) and `deploy/Dockerfile.jobs` (`mariadb:11.4.13`, resolved 2026-10-22); refresh them deliberately. The job image's MariaDB version must match the clusters' server image.
- Go builds use `-trimpath`, CGO disabled, `-ldflags "-s -w -X ...Version -X ...Commit"`. Node dependencies come from `npm ci` and the lockfile.
- Bit-for-bit reproducibility has not been verified.
- Release workflow actions are pinned to commit SHAs. No secrets other than `GITHUB_TOKEN`; image signing/attestation and a formal SBOM format (SPDX/CycloneDX) are not done, only `go version -m` listings.
