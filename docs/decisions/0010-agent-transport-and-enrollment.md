# ADR 0010: Authenticated manager-agent transport and agent enrollment

Date: 2026-10-22
Status: Proposed. Implemented in `internal/certs` (CSR signing), `internal/enroll`, `internal/agent` (mTLS transport, read-only evidence commands), `internal/manager` (agent PKI, directory, health collection) and `cmd/agent`/`cmd/manager`. Tested only on loopback with real TLS handshakes and fakes for Docker, Swarm and MariaDB. Reopen after the first run on a real Swarm.

## Context

The agent served an unauthenticated HTTP lab transport on a loopback address inside its task. A manager on another host could not reach it, and any local process could send commands. R1 needs authenticated, cross-host manager-agent communication with identity, enrollment, rotation and revocation (ARCHITEKTUR "Secure Docker access, manager/agent identity ...").

## Decision

1. **Direction and network.** The manager dials the agent. The agent runs as a global service in the host network namespace (`Networks: [host]`) and listens on a host port (default 7443); the manager resolves a host to its Swarm node address (`Status.Addr`, falling back to `ManagerStatus.Addr` when a manager reports `0.0.0.0`).
2. **mTLS, TLS 1.3 only.** The agent requires a client certificate that chains to the manager's internal CA, carries clientAuth and the URI SAN `dbcm://<installation>/manager`. The manager verifies the agent's certificate against the same CA, serverAuth and the URI SAN `dbcm://<installation>/agent/<node ID>` of the host it meant to reach (host names and IPs are not the identity). Revoked serials are rejected on every connection by the manager.
3. **Enrollment without trust on first use.** The manager creates an enrollment token (`dbcm-et1.<id>.<secret>`), stores only its SHA-256 digest with the installation, the allowed host IDs and an expiry (default 30 days), and puts the plain token into a Swarm secret mounted by the agent service. The service arguments pin the CA's SHA-256 fingerprint. The agent generates its ECDSA P-256 key locally and posts a CSR with the token and its node ID (from `{{.Node.ID}}` in `DBCM_HOST_ID`) to the manager's enrollment endpoint. It trusts that endpoint only if the presented chain contains a CA with the pinned fingerprint, the server certificate chains to it and carries the manager URI SAN. The manager accepts a token once per host (replays are refused), only for enrolled hosts it covers, and by default only from the host's Swarm address. Identities in the issued certificate come from the manager's host record, never from the CSR.
4. **Identity record.** The issued serial is stored in the host record (`agentSerial`) as the agent identity.
5. **Rotation.** The agent renews over mTLS with its current certificate when 30% (at least 24 h) of the lifetime (default 30 days) remain; the manager accepts only the currently recorded serial, records the new serial and denies the previous one.
6. **Revocation.** Revoking a host's enrollment, or `AgentPKI.RevokeAgent`, adds the serial to the deny list (`revocations` collection) and clears the identity; the agent must enroll again with a new token.
7. **CA storage.** The CA (certificate plus passphrase-encrypted key, `internal/certs`) is stored in the replicated store sealed with the manager data key; created once by the leader with compare-and-set. The manager's client and enrollment certificates are issued from it in memory and reissued before expiry.
8. **Lab transport.** Kept only behind `agent -insecure-lab` (loopback only); the manager's `labAgents` and `agentTransport` settings exclude each other.

## Alternatives

- **Agent dials the manager (outbound connections, long-lived stream).** Works through NAT and needs no listening port on hosts. Rejected for R1: needs a multiplexed command stream with reconnection and leader redirection across manager replicas; the request/response model with fencing and idempotent steps already exists and maps directly onto HTTPS requests. Reconsider if host ports are unacceptable.
- **Published port with `mode: host` instead of host networking.** Possible; rejected because the agent then cannot reach a database port published on the same host through loopback, which health collection uses.
- **Token per host (one secret each).** Stronger binding, but a global service mounts the same secret on every node; per-host services would replace the global service. The source-address check and the per-host single use limit the shared token instead.
- **TOFU (trust the first manager certificate).** Rejected: a network attacker at first start would become the manager.
- **External PKI/ACME.** Not suitable for internal machine identities; the internal CA already exists (`internal/certs`).

## Consequences and limits

- The deny list is enforced by the manager (agent certificates) and the renewal endpoint; it is **not distributed to agents**, so a stolen manager client certificate is valid on agents until it expires (7 days). Distribution is open work.
- The shared token lets any enrolled host that is compromised claim another enrolled host that has not enrolled yet, unless the source-address check is active (default). Behind NAT the check must be disabled (`allowAnySource`).
- An expired agent certificate cannot renew; the host needs a new token (redeploying the agent service). Token rotation for an existing agent service is not implemented: hosts enrolled after the token expired cannot join.
- If the agent fails to persist a renewed certificate after the manager recorded it, it keeps the old (now denied) certificate and must enroll again.
- The CA key's availability depends on the unsolved data-key provisioning across replicas.
- Nothing here has run on a real Swarm: the `host` network attachment by name, the `{{.Node.ID}}` env template and secret mounting are taken from the Docker documentation, not observed.
