---
title: Installation and packaging
---

# Installation and packaging

Technical guide for installing the manager. It describes only what exists today. **Not verified from these files** (marked below): managed mode ran on lab VMs only as a process (lab E4), never from the compose/stack files; manager configuration backup has no scheduler or API; there are no signed images. Verification status: files in `deploy/` and the release workflow are statically checked (`python3 scripts/check_deploy.py`), and Go tests load `deploy/manager.example.json` with the manager's config loader and start the managed runtime from it on loopback; **the image has not been built, and the workflow, compose and stack files have not been run on a real Swarm.**

## What the manager does today

Without a configuration the image serves only the web UI, `GET /api/v1/health` and `GET /api/v1/version`, stores no state and has no Docker access (prototype mode). `deploy/stack.yaml` and `deploy/compose.yaml` start **managed mode**: they require a configuration file (from `manager init`), mount a data volume and the local Docker socket, and pin the manager to one Swarm manager node ([ADR 0014](decisions/0014-manager-container-docker-access.md)). Configuration fields are documented in the header of `cmd/manager/main.go`; unknown fields are rejected.

## Artifacts

| Artifact | Source | Notes |
| --- | --- | --- |
| Release archives `db-cluster-manager_<version>_<os>_<arch>.tar.gz` | `.github/workflows/release.yml` on tag `v*` | linux/darwin, amd64/arm64, CGO disabled; contain `manager`, `agent`, `cli`, `deploy/`, per-binary `go version -m` listings |
| `SHA256SUMS` | same workflow | verify with `sha256sum -c SHA256SUMS` |
| Container image `ghcr.io/<owner>/db-cluster-manager:<version>` | same workflow, `deploy/Dockerfile` | linux/amd64 and linux/arm64; distroless, uid 65532, `/var/lib/dbcm` owned by 65532, healthcheck via `manager -healthcheck`; contains the agent binary, so it is also the agent image |
| Job image (backup/restore) | `deploy/Dockerfile.jobs` (not in the release workflow yet) | `mariadb:11.4.13` pinned by digest plus `dbcm-job` (`cmd/dbcm-job`); runs as uid/gid 999 (the image's `mysql`); **never built so far** ([ADR 0011](decisions/0011-galera-endpoints-and-bootstrap-sql.md)) |
| Example configuration `deploy/manager.example.json` | `manager init` (below) | single instance, no backups; a Go test checks that `manager init` reproduces it byte for byte |
| Offline bundle | `scripts/make-offline-bundle.sh` | binaries, compose/stack files, example configuration, checksums, manifest, optional `docker save` image |

`cmd/metad` and `cmd/lab` are test harnesses and are not shipped. `cmd/proxy` exists and ran as a lab container in E5; the manager does not deploy it and no release artifact contains it.

## Managed mode as a Swarm stack (single instance) [not run on a real Swarm]

Steps for the first Swarm trial. Per project rules, use a dedicated test Swarm and an explicit Docker context until then; never a production daemon.

1. **Images.** Build or pull the manager image and the job image and make both available on every node (registry, or `docker save | docker load` as in E4):
   `docker buildx build -f deploy/Dockerfile -t db-cluster-manager:local --build-arg VERSION=0.0.0-dev --load .`
   `docker buildx build -f deploy/Dockerfile.jobs -t db-cluster-manager-jobs:local --load .`
   Beyond a trial, pin both by digest in the configuration (`-agent-image`, `-job-image`).
2. **Configuration skeleton.** With the `manager` binary (or `docker run --rm db-cluster-manager:local init ...`):

   ```sh
   manager init -rp-id dbcm.example.com -manager-addr 192.0.2.10 -installation example -out manager.json
   ```

   `-rp-id` is the host name users open the UI on (passkey relying party; origin `https://<rp-id>` unless `-origin` is given). `-manager-addr` is the address of the pinned manager node that agents reach (enrollment endpoint `https://<addr>:7444`). Without `-installation` a random ID is chosen. The command writes the JSON (mode 0600, never overwriting a file; without `-out` to stdout) and prints first-run instructions to stderr: host directories, the Swarm secrets and networks to create, the deploy command and how to read the setup token. It prints no secret values; the configuration holds only Swarm secret **names** and container paths. `deploy/manager.example.json` is exactly the output of the command above.
   With `-backup -recovery-key-out <file>` it also configures backups: it generates a recovery key into the new file (0600; keep a copy outside the Swarm) and puts only its public half into `settings.backup.recipientKey`; `-backup -recipient-key <dbcmpk1:...>` uses an existing key instead. The manager then reads the backend configuration from `/run/secrets/dbcm-backend.json`: uncomment the `backend` secret in `deploy/stack.yaml` and set `DBCM_BACKEND_SECRET` to the backend secret's name.
3. **Prerequisites** listed by `init` (the manager creates or checks none of them): `/var/lib/dbcm-agent` and the volume base on every node; the root, monitoring and backup password secrets and the cluster network, labelled `dbcm.installation=<installation ID>`; for backups also the backup option file (`[client]` section with `user=dbcm_backup` and `password=`), the backend configuration (`dbbackup.BackendConfig` JSON) and the recovery key as secrets, and the inspection network. See also the E4 observations below.
4. **Store the configuration** as a Swarm config. Configs are immutable: use a new name per change and redeploy with it. `docker config create dbcm-manager-config-1 manager.json`
5. **Deploy** on a Swarm manager node:

   ```sh
   DBCM_MANAGER_NODE=<hostname of the pinned node> \
   DBCM_DOCKER_GID=$(stat -c %g /var/run/docker.sock) \
   DBCM_CONFIG_NAME=dbcm-manager-config-1 \
   docker stack deploy -c deploy/stack.yaml dbcm
   ```

   Optional: `DBCM_IMAGE` (default `db-cluster-manager:local`), `DBCM_PORT` (default 8080). Unset required variables fail the deploy.
6. **First administrator.** The setup token is printed to the service log only while no administrator exists: `docker service logs dbcm_manager 2>&1 | grep 'setup token'`. Put a TLS reverse proxy on the pinned node in front of `127.0.0.1:8080` for `https://<rp-id>` (passkeys need an https origin) and create the administrator in the UI.
7. **Register the Swarm** in the UI with the Docker endpoint `unix:///var/run/docker.sock`, then enroll hosts as in the [user guide](guides/en/02-hosts-and-agents.md).

What `deploy/stack.yaml` sets: one replica constrained to `node.role == manager` and `node.hostname == ${DBCM_MANAGER_NODE}`; host network; API and healthcheck on `127.0.0.1:${DBCM_PORT}`; metastore on `127.0.0.1:7445` (not 7946, Swarm's gossip port); the configuration as external Swarm config at `/etc/dbcm/manager.json` (`DCM_CONFIG`); the local volume `manager-data` at `/var/lib/dbcm` (metastore, data key `data.key`, monitoring store); the Docker socket; uid 65532 with the socket's group; read-only root filesystem; all capabilities dropped; stop-first updates with rollback; limits 1 CPU and 512 MiB.

**Docker socket.** Mounting `/var/run/docker.sock` is root-equivalent on the pinned node. It is needed because the manager's Docker client speaks only `unix://` and plain `tcp://` (no TLS); alternatives and limits are in [ADR 0014](decisions/0014-manager-container-docker-access.md). Do not mount it into anything else.

**Data volume.** The volume exists only on the pinned node. A task on another node would find an empty volume and start a **new, empty installation**; that is why the service is pinned. The image ships `/var/lib/dbcm` owned by uid 65532 so that a new volume is writable (Docker copies that ownership into a new named volume; not verified on the trial engine).

**Untested stack options.** Whether the trial engine's `docker stack deploy` honours `cap_drop`, the interpolated `user` group, the external `host` network and the config `uid`/`gid`/`mode` has not been checked.

### Single host with Docker Compose [not run]

`deploy/compose.yaml` is the same setup without Swarm scheduling, on one Swarm manager node: `DBCM_CONFIG_FILE=/path/to/manager.json DBCM_DOCKER_GID=$(stat -c %g /var/run/docker.sock) docker compose -f deploy/compose.yaml up -d`. The configuration file is bind-mounted read-only and must be readable by uid 65532 or the socket group; the backend file is an optional `secrets:` entry (commented out).

### Managed mode on a lab Swarm (what lab E4 ran)

Lab E4 ([results](../tests/p0/results/e4-manager-galera.md)) ran the manager as a plain process on a Swarm manager node with `unix:///var/run/docker.sock` and the `manager.settings` configuration section (installation ID, volume base, agent image, accounts, backup settings; see `cmd/manager/main.go`). Host and operator prerequisites observed there, none of which the manager creates or checks yet:

- On **every** host: the agent state directory `/var/lib/dbcm-agent` and the volume base (for example `/var/lib/dbcm/volumes`) must exist; Swarm rejects the agent task otherwise (`bind source path does not exist`).
- At least one enrolled **Swarm manager node** with an agent: service and backup-job commands are sent to its agent, because workers cannot change services.
- The cluster's attachable overlay network and the root, monitoring and backup password secrets (plus the backup option file, backend configuration and recovery key secrets) must exist with the label `dbcm.installation=<installation ID>`; the secrets the agent reads for `exec-sql` are listed in `manager.agentTransport.dbSecrets` before the agent service is created.
- Images are referenced by tag or digest; without a registry they must be present on every node (E4 used `docker save | docker load`).

## Three-replica variant (documented; ran on lab VMs as plain processes, never as Swarm services)

Not shipped as a file ([ADR 0013](decisions/0013-replica-keys-peer-tls-and-revocation.md)). Lab E4 re-run B2 ran three replicas with `manager.keyring` and `manager.peerTLS` as plain processes on three Swarm manager VMs, including two leader SIGKILLs ([results](../tests/p0/results/e4-rerun-B2.md)); the Swarm-service layout below has not run. Every replica's `manager.nodeId` must be one of the `manager.peers` IDs and its own peer address must equal its `advertise` address (the manager refuses to start otherwise). The intended layout, derived from the code:

1. Three Swarm manager nodes, each with one manager **service** (`manager-1`, `manager-2`, `manager-3`): copies of the `manager` service in `deploy/stack.yaml`, each with its own `node.hostname` constraint, volume and config. Do not raise `replicas`: Swarm replicas share one config and have no stable identity.
2. One configuration per replica, all with the same peer list:
   `manager init -rp-id ... -manager-addr <node N address> -installation <id> -node-id manager-N -peers manager-1=<addr1>:7445,manager-2=<addr2>:7445,manager-3=<addr3>:7445`
   This sets `bind`/`advertise` to the node's peer address, `keyring: true`, and `peerTLS` with `/run/secrets/dbcm-peer-ca.pem`, `dbcm-peer.pem` and `dbcm-peer-key.pem`. With backups, generate the recovery key once (`-recovery-key-out` for the first replica) and pass its public half with `-recipient-key` to the others; different keys per replica would conflict in the replicated settings.
3. Peer certificates: `DBCM_PEER_CA_PASSPHRASE=... manager -issue-peer-certs <dir> -installation <id> -peer-ids manager-1,manager-2,manager-3`; create per replica secrets from `<dir>/manager-N/ca.pem`, `peer.pem`, `peer-key.pem` and mount them with the targets above (uid 65532, mode 0400). Keep `ca.sealed` and the passphrase offline.
4. After the first leader is up, register the other replicas' data-key recipients (`manager -print-replica-key` on each, then `PUT /settings/replicas/{id}/data-key-recipient`).

Limits: only the metastore leader serves the API; other replicas answer 503 with `X-DCM-Leader`, and no shipped proxy follows the leader. Peers are a static bootstrap list (no membership changes).

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

The script's `-i` flag runs `docker save`; it never builds or pulls. Without `-i` the bundle holds binaries and files only. Transfer, then `sha256sum -c <bundle>.tar.gz.sha256`, extract, `sha256sum -c SHA256SUMS`, `docker load -i image/image.tar`, and continue with the steps above. The job image and database images are not part of this bundle yet. The non-docker parts of the script were tested; the `docker save` path was not.

## Upgrade

1. Read the release notes. 2. Pull or load the new image on the pinned node. 3. `docker service update --image <new> dbcm_manager` (or redeploy the stack with `DBCM_IMAGE=<new>`): stop-first, automatic rollback on a failed update. Compose: `DBCM_IMAGE=<new> docker compose -f deploy/compose.yaml up -d`. 4. Confirm `GET /api/v1/version` and health. The volume is kept. Managed mode keeps state in the volume, but no data migration mechanism or tested downgrade exists; roll back only to the previous image. A changed configuration needs a new config name (`DBCM_CONFIG_NAME`) and a redeploy. Updating the agent service to a new `settings.agentImage` is not part of this procedure [not implemented]. Major-version upgrades will need explicit approval once they exist.

## Backup and restore of manager configuration (not implemented)

Managed mode keeps its state (metastore, data key, monitoring store) in the `manager-data` volume on the pinned node; without `manager.keyring` the data key `data.key` is a plain file there. There is no backup or restore tool for it (requirement B08, R1 gate). Keep the `manager.json` you deployed and, with backups configured, the recovery key outside the Swarm; with `manager.keyring` and a recovery key the data keys are recoverable (ADR 0013), the rest of the state is not.

## Teardown and uninstall

Scoped to this installation only; none of these commands touches other projects:

```sh
docker stack rm dbcm                                   # stops the manager service; the volume remains
docker volume rm dbcm_manager-data                     # on the pinned node: deletes ALL manager state
docker config rm dbcm-manager-config-1
# Compose variant
docker compose -f deploy/compose.yaml down             # add -v to delete the manager-data volume
docker image rm db-cluster-manager:local               # remove the image you built/loaded
```

Objects the manager created stay in the Swarm: the agent service `dbcm-agent-<installation ID>`, database services and secrets labelled `dbcm.installation=<installation ID>` (list them with `docker service ls --filter label=dbcm.installation=<id>` and `docker secret ls --filter label=dbcm.installation=<id>`), node volumes below the volume base and the agent state in `/var/lib/dbcm-agent` on each host. Remove databases through the manager before removing it. Do not use `docker system prune`. Binary install: delete the extracted directory.

## Image details and reproducibility

- Base images are pinned by tag and digest in `deploy/Dockerfile` (resolved from the registries on 2026-10-20) and `deploy/Dockerfile.jobs` (`mariadb:11.4.13`, resolved 2026-10-22); refresh them deliberately. The job image's MariaDB version must match the clusters' server image.
- Go builds use `-trimpath`, CGO disabled, `-ldflags "-s -w -X ...Version -X ...Commit"`. Node dependencies come from `npm ci` and the lockfile.
- Bit-for-bit reproducibility has not been verified.
- Release workflow actions are pinned to commit SHAs. No secrets other than `GITHUB_TOKEN`; image signing/attestation and a formal SBOM format (SPDX/CycloneDX) are not done, only `go version -m` listings.
