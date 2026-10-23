# ADR 0014: Docker API access and networking of the manager container

Date: 2026-10-23
Status: Proposed. Implemented in `deploy/stack.yaml`, `deploy/compose.yaml`, `deploy/Dockerfile` and `manager init` (`cmd/manager/init.go`). Verified only statically (`scripts/check_deploy.py`) and by Go tests that load the shipped example configuration with the manager's config loader and start the managed runtime from it on loopback. **Never run on a real Swarm or from a built image.** Reopen after the first Swarm trial.

## Context

Managed mode needs the Docker API of a Swarm manager node: the Swarm connector reads nodes, and the manager creates the agent service and its secrets (`internal/manager/swarm`). `internal/dockerclient` speaks `unix://` or plain `tcp://` only; TLS is not implemented. Lab E4 ran the manager as a root process on a Swarm manager node with `unix:///var/run/docker.sock`, `-listen 127.0.0.1:<port>` and host addresses for the enrollment endpoint. The earlier compose/stack files mounted no socket, so they could only run prototype mode.

## Decision

1. **Mount the local Docker socket** into the manager container, and only there: the stack pins the single task to one named Swarm manager node (`node.role == manager`, `node.hostname == ${DBCM_MANAGER_NODE}`).
2. **Least privilege around it:** uid 65532 with the socket's group as primary group (`${DBCM_DOCKER_GID}`, required), read-only root filesystem, all capabilities dropped, no published ports. The socket remains root-equivalent on that node; the files and the docs say so.
3. **Host network** (as in E4): the UI/API listens on `127.0.0.1` only (a TLS reverse proxy on the node is required for passkeys anyway), the metastore on `127.0.0.1:7445` (not 7946, which dockerd's Swarm gossip uses), and the TLS enrollment endpoint on the node address that agents reach. Agents see their real source address, which the enrollment source check needs.
4. **One replica, stop-first updates, pinned local volume.** Two tasks must never open the same metastore, and a task on another node would start an empty installation. HA is three separately pinned services with their own configs, keyring and peer TLS (ADR 0013), documented in `docs/INSTALL.md` but not shipped as a file.
5. **Configuration through `manager init`:** a non-secret JSON skeleton (Swarm secret *names*, container paths below `/run/secrets`) stored as a Swarm config; secret values are created by the operator. The only secret `init` can create is a recovery key, written to a new 0600 file; only its public half enters the configuration.

## Alternatives considered

- **Remote Docker endpoint with mutual TLS** (the earlier intention in `docs/INSTALL.md`): keeps the socket out of the container, but the client has no TLS and dockerd's TLS mode grants the same root-equivalent API to the certificate holder; deferred until the client supports it.
- **Socket proxy with an endpoint allow-list** (for example a filtering proxy container): narrows the API, but adds a third-party image, the allow-list would have to cover service, secret, node and task calls (most of the dangerous surface), and nothing of it ran in the lab.
- **Manager as a host process** (E4 layout): verified, but outside the Swarm packaging the requirements ask for (P07) and without Swarm restarts.
- **Overlay network with published ports:** Swarm cannot publish on loopback only (ADR 0011, alternatives), the routing mesh would expose the UI on every node, and NAT would hide agent source addresses from the enrollment check.

## Consequences and limits

- Anyone who controls the manager container controls the node. The read-only, capability-free, non-root process limits what a compromised manager *process* can do without the socket, nothing more.
- The image now ships `/var/lib/dbcm` owned by 65532, assuming Docker copies that ownership to a new named volume (not verified).
- Whether `docker stack deploy` honours every option used (`cap_drop`, `user` with an interpolated group, external `host` network, config `uid`/`gid`/`mode`) on the Docker version of the trial Swarm is untested.
- Only the metastore leader serves the API; with several replicas the reverse proxy has to find the leader (`X-DCM-Leader`), which no shipped component does.
