---
title: Security model
---

# Security model

[Guide index](index.md) | Previous: [Full-outage recovery](07-full-outage-recovery.md) | Next: [Troubleshooting](09-troubleshooting.md)

This describes the design and what was verified. No external security review has been done, and none of this has been tested against a hostile network.

## Authentication and tokens

- **Passkeys only.** There are no passwords for the UI. Sessions are server-side with an idle timeout of 30 minutes and an absolute timeout of 12 hours (defaults); logout and recovery end them. Strict `Origin` checking protects state-changing requests (CSRF). [Fakes] with a software authenticator and a virtual browser authenticator; [Lab VMs] only for the API flow.
- **Setup token:** printed once at start while no administrator exists; rate limited per client IP; setup is disabled for good after the first administrator.
- **Recovery codes:** ten single-use codes issued at setup, shown once, stored as hashes. Redeeming one removes the account's passkeys, ends all sessions and opens a short recovery session (10 minutes idle, 15 absolute) that only allows registering a new passkey. Attempts are limited per IP and per account (five failures in 15 minutes lock for 15 minutes, doubling up to 24 hours); every failure returns the same body. [Fakes].
- **Recovery URL tokens** (`/auth/recovery/token`): the issuing command line does not exist yet, so this path cannot be used [Untested].

## Authorization and audit

Deny by default. Preset roles: viewer, operator, database administrator, administrator (plus a manager system role); custom roles are possible. Permissions are per resource type and action on a scope (Swarm, host, cluster, backup, and so on). Resources a caller may not read answer 404. Destructive operations (cluster delete, inspection restore, full-outage recovery) wait for a **digest-bound approval**: the digest is the SHA-256 of the stored plan, so you approve exactly what was shown. Cluster deletion needs the delete permission, which only the administrator preset holds. Every decision is audited. The audit goes to a persistent replicated store in managed mode (and to the log in prototype mode). [Fakes]; the approval digest check and `409 approval_mismatch` also ran on lab VMs for the inspection restore [Lab VMs].

## Manager-agent channel (mTLS)

Agents enroll with a one-time token (delivered as a Swarm secret), pin the CA fingerprint, submit a CSR and are checked by source address. Afterwards both sides use TLS 1.3 with client certificates; agent commands are fenced by a term, idempotent and checked against an ownership allow-list, so an agent refuses objects the installation did not create. Enrollment and the mTLS transport ran on lab VMs [Lab VMs]. Deny-list distribution to agents and revoking a manager replica's certificates are implemented and tested on loopback [Fakes]; revocation and renewal with real agents on VMs were not exercised [Untested]. See [Hosts and agents](02-hosts-and-agents.md).

## Secrets and data at rest

Database passwords live in Swarm secrets, never in the API or tracked files. Notification secrets are write-only and stored encrypted (AES-GCM). The manager's data key is created in the data directory on first start. For several replicas set `manager.keyring`: the key is then stored only wrapped for each registered replica's identity (`<dataDir>/replica.key`) and for the backup recovery key; an administrator registers each replica by comparing the public key printed by `manager -print-replica-key` (`PUT /settings/replicas/{id}/data-key-recipient`, voters only, audited), and a replica without the key hands leadership over ([ADR 0013](../../decisions/0013-replica-keys-peer-tls-and-revocation.md)) [Fakes]. Without `keyring` the key file must still be copied to every replica by hand. Data-key rotation and resumable re-encryption are implemented with fakes (see below). Withdrawing a registration does not take back keys a replica already unwrapped. Backups are encrypted with a recovery key that you must keep outside the cluster; losing it makes backups unreadable.

## Known weaknesses

- The metastore transport between manager replicas is plain TCP unless `manager.peerTLS` is configured (TLS 1.3, mutual, peer CA from `manager -issue-peer-certs`) [Fakes]. Peer certificates are not renewed by the product, and the peer CA is kept offline by you.
- Backup manifests of format 1 contain plaintext chunk digests; format 2 uses keyed HMACs [Fakes]. Old backups keep the weaker format.
- The UI/API supports native ACME or imported TLS certificates [Fakes]; see [Manager HTTPS](17-manager-tls.md). Plain HTTP beyond numeric loopback requires explicit `manager.tls.allowPlainHttp` for an operator-managed proxy.
- Lima forwards guest listeners to host loopback: in the lab, the unauthenticated metastore harness and agent ports were reachable on the host. Lab-only, but do not run lab harnesses on shared machines.
- Client-facing database TLS exists only in the proxy; node-to-node and proxy-to-node TLS do not exist. The manager configures no TLS between Galera nodes; encrypting the operator-provided overlay network (`--opt encrypted`) is your decision and was not tested.

## Data-key rotation and replica membership [Fakes]

With `manager.keyring`, a global administrator calls
`POST /api/v1/settings/data-key/rotate` with `{}` to preview, then submits
`{"digest":"<returned digest>"}` to approve that exact keyring revision. A stale
digest needs a fresh preview; a running rotation refuses another start. Read
`GET /api/v1/settings/replicas` for `rotation.processed` and `rotation.complete`.
The leader resumes batches after failover; corrupt ciphertext pauses progress
without discarding the old key. New seals use the new ID and recovery envelope.
Old envelopes remain available for historical configuration backups even after
completion. This does not rotate database passwords or the external recovery key.
There is no rotation UI and no real multi-host rotation evidence yet.

Replica CSR provisioning and add/remove have an optional backend and HTTP
contracts, but the default manager returns 503. The fake workflow provisions a
peer certificate, previews the membership change and submits its digest. Joining
peers first catch up as nonvoters; removal checks both quorums, requires a separate
leadership transfer for the current leader, revokes recorded certificates and
removes data-key envelopes. Removed peer identities are denied on new TLS
handshakes. Existing streams are not closed automatically. Peer-CA delivery,
authenticated readiness RPCs and certificate renewal remain unimplemented; do not
edit the bootstrap peers list as a membership-change procedure.
