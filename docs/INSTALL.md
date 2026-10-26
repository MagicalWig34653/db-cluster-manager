---
title: Installation and packaging
---

# Installation and packaging

Technical guide for installing the manager. The arm64 manager image and single-instance stack ran on lab VMs in [E7](../tests/p0/results/e7-stack-deploy.md). Native amd64 deployment, Compose, the new three-service HA stack and the updated CI/release workflows remain unverified. Static deployment checks do not prove image builds or runtime compatibility. No release image or signature exists yet; manager configuration backup has no scheduler or API.

## What the manager does today

Without a configuration the image serves only the web UI, `GET /api/v1/health` and `GET /api/v1/version`, stores no state and has no Docker access (prototype mode). `deploy/stack.yaml` and `deploy/compose.yaml` start **managed mode**: they require a configuration file (from `manager init`), mount a data volume and the local Docker socket, and pin the manager to one Swarm manager node ([ADR 0014](decisions/0014-manager-container-docker-access.md)). Configuration fields are documented in the header of `cmd/manager/main.go`; unknown fields are rejected.

## Artifacts

| Artifact | Source | Notes |
| --- | --- | --- |
| Release archives `db-cluster-manager_<version>_<os>_<arch>.tar.gz` | `.github/workflows/release.yml` on tag `v*` | linux/darwin, amd64/arm64, CGO disabled; contain `manager`, `agent`, `cli`, `deploy/`, per-binary `go version -m` listings |
| `SHA256SUMS` | same workflow | verify with `sha256sum -c SHA256SUMS` |
| Container image `ghcr.io/<owner>/db-cluster-manager:<version>` | same workflow, `deploy/Dockerfile` | linux/amd64 and linux/arm64; distroless, uid 65532, `/var/lib/dbcm` owned by 65532, healthcheck via `manager -healthcheck`; contains the agent binary, so it is also the agent image |
| Job image (backup/restore) | same workflow, `deploy/Dockerfile.jobs` | `mariadb:11.4.13` pinned by digest plus `dbcm-job` (`cmd/dbcm-job`); runs as uid/gid 999 (the image's `mysql`); linux/amd64 and linux/arm64; updated release path not yet run ([ADR 0011](decisions/0011-galera-endpoints-and-bootstrap-sql.md)) |
| Example configuration `deploy/manager.example.json` | `manager init` (below) | single instance, no backups; a Go test checks that `manager init` reproduces it byte for byte |
| Offline bundle | `scripts/make-offline-bundle.sh` | binaries, compose/stack files, example configuration, checksums, manifest, optional `docker save` image |

`cmd/metad` and `cmd/lab` are test harnesses and are not shipped. `cmd/proxy` exists and ran as a lab container in E5; the manager does not deploy it and no release artifact contains it.

## Managed mode as a Swarm stack (single instance) [ran on a lab Swarm, E7]

Lab E7 ([results](../tests/p0/results/e7-stack-deploy.md)) ran exactly these steps on three arm64 lab VMs (Docker 29.9.2): image built from `deploy/Dockerfile`, `manager init` from the image, `docker stack deploy`, administrator setup, agent enrollment, Galera create through the API, container kill, forced update and drain of the pinned node. Steps for the first Swarm trial. Per project rules, use a dedicated test Swarm and an explicit Docker context until then; never a production daemon.

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

**Stack options.** On Docker 29.9.2 (E7) `docker stack deploy` honoured `cap_drop`, the interpolated `user` group, the external `host` network and the config `uid`/`gid`/`mode`, and a new named volume was writable by uid 65532 (ownership copied from the image). Other engine versions are untested.

**Behaviour observed in E7.** A killed manager container is replaced by Swarm in about 6 s and leads again about 8 s after the kill; sessions and clusters survive (data volume). `docker service update --force` is stop-first (about 14 s). Draining the pinned node leaves the manager `Pending` (no other node satisfies the constraint) and the API down until the node is `active` again; the agent and any database slots on that node leave too, so drain only a node whose clusters keep a majority, and expect about a minute until the manager leads again after reactivation. Replicas above 1 are not supported by this file.

### Single host with Docker Compose [not run]

`deploy/compose.yaml` is the same setup without Swarm scheduling, on one Swarm manager node: `DBCM_CONFIG_FILE=/path/to/manager.json DBCM_DOCKER_GID=$(stat -c %g /var/run/docker.sock) docker compose -f deploy/compose.yaml up -d`. The configuration file is bind-mounted read-only and must be readable by uid 65532 or the socket group; the backend file is an optional `secrets:` entry (commented out).

### Managed mode on a lab Swarm (what lab E4 ran)

Lab E4 ([results](../tests/p0/results/e4-manager-galera.md)) ran the manager as a plain process on a Swarm manager node with `unix:///var/run/docker.sock` and the `manager.settings` configuration section (installation ID, volume base, agent image, accounts, backup settings; see `cmd/manager/main.go`). Host and operator prerequisites observed there, none of which the manager creates or checks yet:

- On **every** host: the agent state directory `/var/lib/dbcm-agent` and the volume base (for example `/var/lib/dbcm/volumes`) must exist; Swarm rejects the agent task otherwise (`bind source path does not exist`).
- At least one enrolled **Swarm manager node** with an agent: service and backup-job commands are sent to its agent, because workers cannot change services.
- The cluster's attachable overlay network and the root, monitoring and backup password secrets (plus the backup option file, backend configuration and recovery key secrets) must exist with the label `dbcm.installation=<installation ID>`; the secrets the agent reads for `exec-sql` are listed in `manager.agentTransport.dbSecrets` before the agent service is created.
- Images are referenced by tag or digest; without a registry they must be present on every node (E4 used `docker save | docker load`).

## Three-replica HA stack (static checks and in-process tests only)

[`deploy/stack-ha.yaml`](../deploy/stack-ha.yaml) contains three services, one per Swarm manager node, with separate configs and local volumes, pinned hostnames, stop-first updates, host networking and the socket guard from [ADR 0014](decisions/0014-manager-container-docker-access.md). Lab E4 re-run B2 exercised three replicas as **plain processes**, including leader SIGKILLs ([results](../tests/p0/results/e4-rerun-B2.md)); this stack ran on a three-manager lab Swarm in E9 (2026-10-26: election, forwarding, peer mTLS, leader kill, node drain; [results](../tests/p0/results/e9-ha-proxy-writer.md)). Not covered there: rolling update, loss of two replicas, certificate renewal, the backend secret. The impostor test needed the peer bundle placed in each volume by hand (step 2) and a TLS front for the loopback API when proxies coordinate over HTTPS (E9 used socat on :8443). Use only an owned lab Swarm after the fake-only checks have passed. Each host must be a Swarm manager and must have the same numeric Docker socket group ID; otherwise set the task group correctly per host rather than weakening socket permissions.

1. Generate one config per replica with the same installation ID, RP ID, origin, peer list and backup recovery public key. For replica N: `manager init -rp-id <ui-host> -manager-addr <node-N-address> -installation <id> -node-id manager-N -peers manager-1=<addr1>:7445,manager-2=<addr2>:7445,manager-3=<addr3>:7445 -out manager-N.json`. The HA output sets `keyring: true`, peer mTLS, `apiForwardBind` on `<addrN>:7447`, each `peers[].apiAddress`, and certificate renewal. Ports 7445 and 7447 must be reachable among the three manager nodes; 7444 is the agent enrollment endpoint. The public API remains loopback on 8080 and needs a TLS proxy on each node or a load balancer. Requests sent to a follower are forwarded once over peer mTLS. On a leader change or an uncertain write outcome, the follower returns 503; the client must inspect operation state before retrying.
2. Run `DBCM_PEER_CA_PASSPHRASE=... manager -issue-peer-certs <dir> -installation <id> -peer-ids manager-1,manager-2,manager-3`. Keep the original sealed CA and passphrase outside the cluster as recovery copies. On **each** node, initialize its local manager volume's `/var/lib/dbcm/peer/peer-bundle.pem` from `<dir>/manager-N/peer-bundle.pem`, owned by uid 65532 with mode 0600. The bundle contains that replica's leaf chain and private key. Never copy one replica's bundle to another node.
3. Create external Swarm secrets for `<dir>/ca.pem`, `<dir>/ca.sealed` and a file containing the CA passphrase, and set `DBCM_PEER_CA_NAME`, `DBCM_PEER_CA_SEALED_NAME`, `DBCM_PEER_CA_PASSPHRASE_NAME`. The stack mounts them read-only at `/run/secrets/dbcm-peer-ca.pem`, `/run/secrets/dbcm-peer-ca-sealed`, `/run/secrets/dbcm-peer-ca-passphrase`. **This opts into online custody of the peer CA on every manager node:** compromise of one node and its mounted passphrase can issue peer identities. Restrict access to the three manager nodes, rotate the CA after compromise, and keep the external recovery copy separate. If this risk is unacceptable, omit `peerTLS.renew` and renew manually before expiry; the stack's default HA configuration expects the three secrets.
4. Create three external configs from `manager-N.json`. With `manager init -backup`, also add the backend secret to the shared secret list in `stack-ha.yaml` (target `dbcm-backend.json`, uid/gid 65532, mode 0400) and its external declaration; the unmodified HA stack serves configurations without a backend. Set `DBCM_CONFIG_NAME_1..3`, three distinct `DBCM_MANAGER_NODE_1..3`, `DBCM_DOCKER_GID`, and `DBCM_IMAGE`; then deploy with `docker stack deploy -c deploy/stack-ha.yaml dbcm`. Check all three service task locations, then register the other replicas' data-key recipients with `manager -print-replica-key` and `PUT /api/v1/settings/replicas/{id}/data-key-recipient` as in the [operator runbook](guides/en/11-operator-runbook.md#5-replicas-key-ring-and-peer-certificates). Until registration, those replicas cannot lead.

The renewal loop checks on start and every 12 hours, renews within 30 days of expiry, writes a new bundle atomically and uses it on new metastore/API-forwarding handshakes without restart. A failed renewal is logged and retried on the next interval; if the CA or secret is unavailable past expiry, peer communication fails closed. A cert revoked for a removed replica remains denied. Expired serials with recorded `not_after` are compacted by the leader and redistributed; legacy entries without expiry remain until separately resolved. The default runtime still does not expose membership-changing endpoints. Do not turn a single-instance metastore volume into a three-member installation by editing its peer list.

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
- The [release policy](RELEASING.md) documents version tags, artifacts and verification. The workflow is configured to publish per-binary CycloneDX JSON SBOMs, SHA-256 checksums, BuildKit image SBOM/provenance attestations and keyless cosign signatures for both GHCR image indexes. It has **not run**; no published artifact, signature or attestation is verified. Binary archives have no independent signed provenance attestation. Existing Actions are pinned to commit SHAs and the workflow uses no secret besides `GITHUB_TOKEN`.

## Native manager HTTPS (fakes only)

An external TLS proxy is optional when `manager.tls.acme` or an imported
`manager.tls.certFile`/`keyFile` pair terminates HTTPS in the manager. See the
[English ACME guide](guides/en/17-manager-tls.md),
[German guide](guides/de/17-manager-tls.md) and
[configuration reference](guides/en/12-configuration-reference.md#managertls-and-managertlsacme).
The default ACME CA is staging and is not publicly trusted. Plain HTTP outside
numeric loopback now requires explicit `manager.tls.allowPlainHttp`; the image
prototype default binds loopback. The shipped stack/compose remain on loopback
HTTP; native TLS requires configuring listener ports, external forwarding,
trust roots and startup grace period. No real deployment of this new mode was
performed.

- Build and release Actions reuse the repository's existing commit-SHA pins (not refreshed or independently verified in this task). The release image builds request BuildKit SBOM (`sbom: true`) and provenance (`mode=max`), then keyless cosign signing of the pushed index digest. Their presence and contents must be inspected after a real tag build. No vulnerability scan runs in the release workflow. Only the image publication job has `packages: write` and `id-token: write`; it uses `GITHUB_TOKEN` for GHCR on version-tag pushes after native CI passes.

## Architecture validation and publication

[Build and test](../.github/workflows/build.yml) runs Go builds, vet, race tests and frontend build/check/tests on native `ubuntu-24.04` (amd64) and `ubuntu-24.04-arm` (arm64) runners. Each runner builds both Dockerfiles for its own architecture with Buildx into temporary OCI archives with SBOM/provenance, without login or push. The archives are validation outputs discarded with the runner, not downloadable release artifacts.

[Release](../.github/workflows/release.yml) calls the same checks before publishing archives or images. Only exact stable `vMAJOR.MINOR.PATCH` tags pass the tag guard. The workflow would publish two Linux archives, six per-binary CycloneDX SBOMs, checksums and the multi-platform manager/agent and `ghcr.io/<owner>/db-cluster-manager-jobs:<version>` images. Both images target `linux/amd64,linux/arm64`; the tag publication build uses QEMU for foreign-platform build steps. A multi-platform build is not native P0 evidence. No workflow has been run for this change and no registry publication is claimed; see [release policy](RELEASING.md).

See the [deployment architecture plan](../deploy/README.md) and [native amd64 P0 plan](TESTING.md#native-amd64-p0-evidence-plan). The owner must still choose an amd64 host or cloud VM for multi-node P0; no resource is provisioned by CI.
