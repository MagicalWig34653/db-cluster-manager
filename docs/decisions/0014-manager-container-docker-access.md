# ADR 0014: Docker API access and networking of the manager container

Date: 2026-10-23
Status: Proposed. Implemented in `deploy/stack.yaml`, `deploy/compose.yaml`, `deploy/Dockerfile` and `manager init` (`cmd/manager/init.go`). Verified only statically (`scripts/check_deploy.py`) and by Go tests that load the shipped example configuration with the manager's config loader and start the managed runtime from it on loopback. Run on a lab Swarm from an image built with `deploy/Dockerfile` (E7, 2026-10-24, three arm64 VMs, Docker 29.9.2): see [results](../../tests/p0/results/e7-stack-deploy.md). Not run on the user's Swarm. Reopen after the first Swarm trial.

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
- The image ships `/var/lib/dbcm` owned by 65532; E7 verified that a new named volume is writable by it.
- E7 verified that Docker 29.9.2 honours every option used (`cap_drop`, `user` with an interpolated group, external `host` network, config `uid`/`gid`/`mode`); other versions are untested.
- Draining the pinned node takes the manager down until the node is active again (E7).
- Only the metastore leader serves the API; with several replicas the reverse proxy has to find the leader (`X-DCM-Leader`), which no shipped component does.

## Amendment 2026-10-24: native manager HTTPS (fakes only)

The manager API/UI may now terminate TLS itself through `manager.tls`, instead
of requiring an external TLS proxy. The shipped stack/compose retain their
loopback/proxy layout. The image's prototype command now binds loopback too.
Plain HTTP on any non-numeric-loopback bind is refused unless explicitly
configured with `manager.tls.allowPlainHttp`; TLS configurations exclude that
opt-in. Static PEM certificates are an alternative to ACME.

`manager.tls.acme` selects an explicit RFC 8555 directory, contact email and
DNS-name allow-list. An omitted directory uses Let's Encrypt staging; production
is never selected implicitly. HTTP-01 has a separate challenge-only HTTP
listener, while TLS-ALPN-01 uses `acme-tls/1` on the public TLS listener. Unknown
SNI names, inactive/expired challenges and mixed application/challenge ALPN
offers fail closed. No challenge listener serves the API/UI.

The implementation uses the already-required `golang.org/x/crypto/acme` behind
`managertls.Client`. **Deviation from using `autocert.Manager`:** that wrapper
starts internal renewal timers and has no exported Stop/Close to end them on
leadership loss. Rather than leave renewal goroutines on former leaders or
attach CA requests to untrusted handshakes, a small lifecycle layer uses the
same library's RFC 8555 client. No custom JWS or RFC 8555 wire implementation,
new dependency or dependency version was added. Alternatives were autocert with
a transport fence (timers would still outlive leadership), a new minimal ACME
implementation (unnecessary protocol/security surface), or external-only TLS
(no automatic certificate management in the manager).

Account private keys, issued keypairs and active challenges are AES-GCM sealed
using the existing data-key/keyring path, bound to collection/key, before Raft
replication. Configuration identity includes CA URL, email, exact domain list
and challenge so configuration changes cannot silently reuse another CA's
certificate or account. Replicas must have identical ACME settings and keys.
Local follower reads permit challenges and TLS handshakes on any replica;
replication lag may temporarily prevent validation. Only the leader creates a
worker, bound to its cancellable Leadership context and term-specific storage
write/cleanup fence. The worker checks immediately on election and hourly,
renews within 30 days of expiry, and retries errors after five minutes. Old
certificates survive renewal failures. Client handshakes never issue; initial
handshakes fail until the first certificate is available. A successor reuses
the replicated account key but may create a new order. No cross-term order
resumption or guarantee of exactly one CA order during a failover is claimed.

Evidence: `internal/managertls` exercises the actual library against a local
fake RFC 8555 handler through `httptest.ResponseRecorder`, validates HTTP proof
and the critical RFC 8737 digest, tests due renewal, CA failure preservation,
cancellation, config/host/path scoping and a TLS handshake over `net.Pipe`.
`internal/manager` tests sealed replication, follower refusal and failover in a
three-node in-memory Raft group. Additional socket-based `httptest.Server`
variants skip when listeners are denied. No public CA, real DNS, browser,
external proxy, VM or container run is evidence for this amendment. See
[actual checks and limitations](../STATUS.md#manager-listener-acmetls-2026-10-24-fakes-only)
and the [operator guide](../guides/en/17-manager-tls.md).

External account binding, DNS-01 wiring, account-key rollover, revocation,
public HTTP redirects, live config reload, CA trust installation and automatic
port forwarding remain outside this increment. Ports 80/443 must reach the
appropriate listener; the non-root/capability-free image needs unprivileged
local ports plus operator-managed forwarding. The healthcheck follows the TLS
config and verifies trust/SNI; staging/private CAs need an installed root.
