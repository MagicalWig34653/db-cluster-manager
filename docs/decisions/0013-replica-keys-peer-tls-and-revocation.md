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

## Not done / limits

- No key rotation (the format and the key provider support several key IDs; nothing creates a second one) and no re-encryption; revoking a replica's registration does not take back keys it already unwrapped.
- No membership changes (add/remove voter) through the product; peers remain the static bootstrap list.
- Peer certificates are not renewed by the product (reissue with the sealed peer CA by hand); no peer deny-list distribution beyond the revocation hook.
- The agent's enrollment renewal client does not consult the deny list for the enrollment server certificate.
- Deny-list entries are never pruned (expired certificates stay listed).
