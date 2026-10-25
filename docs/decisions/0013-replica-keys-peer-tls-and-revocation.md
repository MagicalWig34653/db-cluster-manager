# ADR 0013: Data-key distribution, metastore peer TLS and certificate revocation across replicas

Date: 2026-10-23
Status: Proposed. Implemented in `internal/manager` (keyring, deny-list distribution, replica certificate revocation), `internal/metastore` (TLS stream layer), `internal/agent` (`set-deny-list`) and `cmd/manager`/`cmd/agent`. Verified only with unit and loopback tests (in-memory Raft, real TLS handshakes on 127.0.0.1, a real agent executor with a Docker stub). Nothing ran with several manager replicas on VMs. Reopen after the multi-replica lab run.

## Context

Three gaps blocked replicated managers (STATUS "What R1 still lacks", E4): every replica needed the same data key that seals sessions, passkeys and the agent CA, but it was a key file copied out of band and not recoverable outside the manager (B07); metastore peers talked plain TCP without authentication; and the agents could not learn about revoked certificates, so a manager client certificate of a removed or compromised replica stayed valid until it expired.

## Decision

1. **Replicated keyring.** The data key is stored only in wrapped form in the replicated document `keys/keyring`. Each replica keeps a private identity (a `backup.RecoveryKey`, `<dataDir>/replica.key`, 0600, never replicated); the keyring holds one envelope per data key and registered replica, sealed for the replica's X25519 public half with the existing backup stream format (ephemeral X25519, HKDF-SHA256, AES-256-GCM; the authenticated header binds key ID and recipient). The first leader bootstraps the keyring; an existing key file is adopted as key `file`, so documents sealed before stay readable.
2. **Registration is an administrator action.** A replica prints its public key (`manager -print-replica-key`); an administrator compares it out of band and registers it with `PUT /settings/replicas/{id}/data-key-recipient` (settings update, audited, event). Only current Raft voters are accepted. Nothing is wrapped automatically for a replica that merely joined the network.
3. **No leadership without the key.** A replica that becomes leader but cannot unwrap the current key does not start its leadership (it could not read sessions, passkeys or the CA) and calls Raft leadership transfer.
4. **Recovery envelope (B07).** When the installation has a backup recovery key configured (its public half `settings.backup.recipientKey`), every data key also gets an envelope for it (the leader refreshes this periodically). `RecoverDataKeys` unwraps the keys from a copy of the keyring with the externally retained recovery key alone; `GET /settings/replicas` reports `recoverable`.
5. **Metastore peer mTLS.** With `manager.peerTLS` the Raft transport is TLS 1.3 with mutual authentication: chains must verify against the peer CA, leaves must carry exactly one URI SAN `dbcm://<installation>/metastore/<replica ID>`, the dialed address must answer with the replica ID configured for it, and a revocation hook can deny serials. `manager -issue-peer-certs` creates the peer CA (its key sealed with a passphrase from `DBCM_PEER_CA_PASSPHRASE`, kept offline) and one certificate per replica; the certificate file is re-read when it changes.
6. **Deny-list distribution.** The replicated revocation list (superseded and revoked agent serials, and revoked manager certificates) is pushed by the leader to every enrolled agent with the fenced command `set-deny-list` whenever its digest differs from what the host last confirmed; the agent persists it and its mTLS listener rejects any denied serial in the presented chain. The revision is the newest revocation time; an older list never replaces a newer one.
7. **Replica certificate revocation.** Every manager certificate a replica is issued (agent-command client, enrollment server) is recorded with its replica ID. `POST /settings/replicas/{id}/revoke-certificates` (settings update, audited, event) denies all unexpired ones; a replica still in service gets fresh certificates on next use.

## Alternatives considered

- **Shared key file copied by the operator** (status quo): simple, but every replica's disk then holds the plaintext key, nothing proves which replicas have it, and recovery depends on a copy outside the product.
- **Automatic key transfer to any authenticated peer** (over the peer mTLS channel): convenient, but makes possession of a peer certificate equivalent to possession of every secret, and silently extends trust on membership changes. Rejected in favour of an explicit, audited registration.
- **Key derived from a passphrase entered on each replica:** needs an interactive unlock on every restart (unsuitable for Swarm services) and puts a guessable secret at the root.
- **Peer CA kept in the replicated store** (like the agent CA): the CA would be needed before the store is reachable; an offline peer CA avoids the chicken-and-egg problem at the cost of manual issuance.
- **CRL/OCSP endpoint for agents:** agents would need to reach the manager to check revocation; pushing the list over the existing fenced command channel needs no new endpoint and works while the manager is the only client anyway.

## Evidence

- Lab E4 re-run B2 (2026-10-24, [record](../../tests/p0/results/e4-rerun-B2.md)): three replicas with `manager.keyring` and `manager.peerTLS` (certificates from `manager -issue-peer-certs`) formed a quorum; the first leader bootstrapped the keyring; after `PUT /settings/replicas/{id}/data-key-recipient` for the two others all three held the current key, and both later leaders (after SIGKILLs) served the replicated sessions. Not captured: the TLS handshake itself and a refused wrong certificate on VMs. The chicken-and-egg concern (fresh replicas without recipients could not lead) does not apply to a fresh installation: the first leader creates the key and registers itself; until the others are registered, only it can lead.

## Not done / limits

- Rotation and membership groundwork now exist with fakes; see the amendment below. Revoking a replica's registration does not take back keys it already unwrapped.
- Default runtime membership administration remains unavailable; an injected fake backend exercises add/remove safely.
- Peer certificates are not renewed by the product (reissue with the sealed peer CA by hand); no peer deny-list distribution beyond the revocation hook.
- The agent's enrollment renewal client does not consult the deny list for the enrollment server certificate.
- Deny-list entries are never pruned (expired certificates stay listed).

## Amendment (2026-10-24): resumable key rotation and membership groundwork

All evidence below is from in-process fakes; no VM or infrastructure was touched.
The earlier "no rotation" and "no membership changes" limitations are superseded
at library/API-contract level only.

### Data-key rotation

`POST /api/v1/settings/data-key/rotate` with `{}` returns a digest tied to the
current keyring revision. Submitting that digest explicitly approves starting a
new random, versioned key. Both the HTTP layer and backend require the global
administrator preset (a custom wildcard role does not suffice); audit failure
prevents authorization. An active rotation refuses a second start.

The leader creates all replica envelopes and the configured external recovery
envelope **before** committing the new current key and rotation record in one
CAS write. New seals use that key. Every sealed document write carries an atomic
keyring-version guard, so an old seal prepared before activation cannot commit
later. `KeyringReconciler` resumes bounded batches (64 documents per interval,
default 30 seconds). It scans all collections for top-level sealed envelopes,
including registry/AI credentials, sessions, passkeys, PKI and future sealed
collections; it does not use a fragile list of secret collection names. Each
reseal and progress increment commit atomically with document/keyring CAS. A
successor can continue after a crash without a process-local cursor. Corruption
halts progress and leaves old envelopes intact. Status exposes `rotation` with
from/to IDs, count and completion; start/completion events record the result.

Old keys remain wrapped **after** completion as well: historical Raft snapshots
and exported configuration backups may still need them. Automatic key retirement,
log erasure, revoking keys already cached by removed replicas, and rotation of
external database passwords or the backup recovery key are not implied. A new
configured recovery public key receives envelopes through the existing refresher.

Alternative rejected: rewriting everything in one huge transaction would increase
log/latency pressure and need a full restart on conflict. A cursor without write
fencing could miss a late old-key write. Small atomic batches plus a keyring CAS
guard preserve resumability and close that race.

### Membership changes

Optional `api.Config.Membership` exposes CSR provisioning and digest-approved
single-replica changes under `/settings/replicas/{id}/peer-certificate` and
`/settings/replicas/{id}/membership`. Global administrator and audit are required.
The **default manager returns 503**: production peer-CA access and authenticated
readiness/catchup RPCs have not been designed or enabled. The offline peer CA
policy remains; tests inject a temporary CA. The joining replica keeps its CSR
private key. Certificate SANs come from the installation and approved replica ID,
never from CSR requests. Provisioning alone gives neither voting membership nor
data-key envelopes. Admission explicitly approves the data-key recipient as part
of membership, before it can lead. Existing voter-key registration stays valid.

An approved intent and a global change lock are replicated before effects.
Changes run only on the leader. Add: `AddNonvoter`, commit/barrier, authenticated
readiness observation at least at the leader's applied index, then `AddVoter`
and barrier. Remove: check old and proposed reachable majorities, refuse removal
of the leader (transfer separately) or last voter, then `RemoveServer` and barrier.
After removal, atomically tombstone the peer, deny its recorded peer/manager
certificate serials and remove its key recipient/envelopes. A successor retries
the persisted intent against actual membership, including a crash after Raft
committed and before metadata cleanup. Another change cannot interleave while an
intent is unfinished. Removed IDs cannot be silently reused.

The pinned hashicorp/raft v1.8.0 source (`api.go:GetConfiguration`,
`configuration.go:nextConfiguration`) exposes **single-server overlapping
configurations**, not a joint-configuration API. This amendment uses that library
safety mechanism with serialized steps and old/new quorum checks; it does not
claim an implementation of explicit joint-consensus log entries. Its
`GetConfiguration().Index()` is always zero because `latestIndex` is omitted in
that future. Passing zero would disable `prevIndex` checking. The wrapper instead
finds the matching configuration's nonzero index in its durable Raft log or
snapshot, and refuses ambiguity. Editing bootstrap peers or FSM documents to
change voting membership was rejected.

Managed peer TLS now reads replicated serial revocations and peer tombstones on
new handshakes (including offline-issued certificates identified by replica URI).
It fails closed before the store is attached. Already-established transport
streams are not forcibly closed; deny-list observation follows local replication,
and active-connection revocation still needs a runtime design/test. A replica
that already learned data keys or a replicated CA key cannot be made to forget
those keys by removal; further key/CA compromise handling is outside this work.

Evidence: race-tested in-memory Raft removal/re-addition, old/new quorum refusal,
stale configuration refusal, four-replica CSR/admission, envelopes, approved
removal across leader loss, serial/identity denial and idempotent retry. HTTP
contract/authorization tests run without sockets, as does the TLS verifier test.
Not verified: real peer readiness, mTLS membership change, certificate delivery or
renewal, membership restart from a disk snapshot, partitions during transition,
and production peer-CA provisioning. A failed/expired staged candidate may need
operator repair; no cancellation or peer-renewal endpoint is provided yet.

## Amendment (2026-10-25): HA operations, fake-only

`manager init -peers` now emits a separate peer-mTLS API forwarding listener
(`apiForwardBind`, port 7447) and `peers[].apiAddress`. A follower forwards an
API request to the leader ID reported by Raft, using the existing peer CA and
replica URI SAN checks. The leader handles authentication, CSRF, authorization
and approvals in its normal API stack. The follower sends each request only
once: if the leader changes during a write or the connection fails after the
request was sent, it returns 503 and the caller must inspect the operation
before retrying. A forwarded marker prevents loops. The handler checks Raft
leadership before serving a formerly active leader's API. Alternative: HTTP
redirects would expose internal leader addresses and require clients to
replay authenticated writes; external load-balancer-only routing would leave
followers unable to accept approvals. Before dispatching a mutating request,
an active leader completes a Raft barrier and rechecks its term. This closes
the interval where a partitioned replica still reports `Leader` locally until
its lease check steps it down. An in-process test isolates the leader from
both followers and confirms the barrier refuses the write. The cost is one
quorum round trip per mutating HTTP request; operation-level fencing and
conditional commits remain necessary because leadership may change after it.

Peer certificates previously relied on an offline CA and manual reissuance.
Automatic renewal cannot happen with the issuer kept strictly offline. The
HA configuration therefore **opts in** to online custody: the sealed CA and
passphrase are separate Swarm secrets on every manager node. Each node issues
only its own URI SAN from its existing certificate identity and atomically
replaces a single PEM bundle in its local volume before expiry. It checks on
startup and every 12 hours, with a 30-day window and 365-day validity by
default. No metastore restart is needed because new handshakes reload the
bundle. A compromised manager node with both secrets can issue other peer
identities, so operators must restrict these nodes and keep offline recovery
copies; CA rotation after compromise is not implemented. Alternative: a
leader signing service would centralize custody but would need a separate
authenticated CSR/authorization protocol before quorum was available and
would make renewal depend on leader availability. This design is not a proof
against a compromised manager node.

The leader now compacts revocation records only when their certificate
`NotAfter` is known and past. A replicated monotonic revision fence advances
in the same conditional transaction as deletions, so agents can accept the
shorter deny list after the latest revocation was removed. Unknown-expiry
legacy entries are retained. No certificate is made valid again: x509 expiry
is independently verified. Alternative: time-based retention without the
certificate expiry would risk unblocking still-valid credentials.

`deploy/stack-ha.yaml` pins three distinct single-replica services to three
Swarm manager hostnames, each with a different config and local volume. The
static checker guards identity, placement and socket constraints. Evidence:
in-process three-node Raft forwarding/leader-change test, fake-clock renewal
test, in-process three-node compaction/failover test, static deployment tests.
The new stack has not run on Swarm. The forwarding test injects an in-process
transport and exercises the real authenticated recovery-approval route before
and after leader change; an actual manager API forwarding TLS handshake and
Swarm-service deployment still require an owned lab run. Existing Raft
streams are not immediately reauthenticated after certificate replacement or
revocation.
