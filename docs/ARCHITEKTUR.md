# Architecture direction and unresolved design

Confirmed product constraints are in [KONZEPT.md](KONZEPT.md). The implementation below is a proposal, not a validated design.

## Deployment and ownership

```text
Manager UI/API replicas (inside or outside target Swarms)
           |
           +-- durable management state / coordination (design pending)
           |
           +-- authenticated Swarm management connection
                 |
                 +-- agent service: one agent per managed Docker host
                 +-- Galera services with host-local persistent data
                 +-- proxy services on multiple hosts
                 +-- isolated restore / backup / monitoring workloads

Applications -- domain / optional VIP --> proxies --> database nodes
Applications ---------------------------> direct database node (optional)
Independent target cluster <------------- selected/all database replication
S3 / SFTP <----------------------------- backups and metric archives
```

The earlier proposal to run databases outside Swarm is superseded. The user installs only the manager. It discovers the existing Swarm and creates/manages agent and database services. Define ownership between the manager, local agents and Swarm's scheduler before implementation: no competing controllers independently recreate the same node.

An agent is one per managed host, not one per database. Agents should expose narrowly defined operations. Installation, cluster and node identities plus recorded resource IDs constrain modifications. Labels alone are not a security boundary for Docker API access.

Database traffic must continue without the manager. Manager loss pauses unsafe new operations rather than taking down serving databases or proxies.

## Local storage and recovery

Swarm scheduling must preserve each database node's storage identity. Moving a task to another host must not silently treat a new empty volume as the old node. Automatic replacement creates a new identity and synchronizes from a verified healthy source when capacity and policy permit.

A returning old host/node must be reconciled before serving traffic. Host loss, temporary disconnection, minority partitions and total cluster failure require different decisions. Automatic replacement cannot manufacture quorum or bypass guided full-cluster recovery.

Continuously synchronized standby nodes participate in cluster behavior even when excluded from application traffic. Periodically refreshed reserves require a feasibility experiment: cache retention, missed writes, storage cost and fallback to full transfer. Do not promise that periodic refresh always avoids full synchronization.

## Replicated managers

HA is required with at least three manager instances, including without distributed storage. Go is selected for manager, agent and CLI; the metadata store is not finalized.

Design durable metadata replication, election/coordination, secret availability, job ownership, fencing and recovery together. Simply increasing service replicas does not establish HA. Management metadata must not depend exclusively on the database cluster being managed.

Operations need durable IDs, step state, timeouts, bounded retries, idempotency and fencing against stale owners. Network loss means unknown completion, not necessarily failure. Pause/abort at safe boundaries; database changes are not universally reversible.

Prototype control-plane core (`internal/manager`, verified in process and on loopback only): the metastore's Raft state machine also holds a generic versioned document store (atomic multi-document compare-and-set on a global revision, snapshotted with the journal, backward compatible with journal-only snapshots). The manager keeps swarms, hosts with enrollment state, clusters, settings, operation definitions/views, events, accounts, passkeys, sessions, recovery codes, roles/bindings and audit records there; session digests, recovery-code hashes and passkey records are sealed with AES-256-GCM using a data key from a `KeyProvider`. Key provisioning across replicas is not solved. Only the leader serves the API and runs operations; on gaining leadership it runs a barrier, takes over unfinished operations with a fresh runner and starts reconcilers, and on losing it stops them and refuses agent commands locally. Not done: request forwarding from followers, membership changes, authenticated metastore/agent transports, health collection from agents.

Secure Docker access, manager/agent identity, enrollment, rotation and revocation remain design work. Outbound agent connections with mTLS are a candidate. Never assume that an unauthenticated network Docker socket is acceptable.

## Routing and certificates

Multiple proxy instances route according to configured writer policy, health and locally measured latency. Specify latency smoothing, hysteresis, stale health handling and connection draining. Optional read/write splitting must account for transactions and read-after-write behavior.

A domain alone does not solve connection failover. Test proxy loss, DNS caching, VIP ownership and client reconnection. Host address/port preservation during migration depends on actual port bindings and the chosen Swarm publication mode; staying on the same host is not sufficient proof.

TLS application-to-endpoint is the confirmed database default. Further database-link encryption is optional. ACME DNS-01, imported certificates and an internal CA need renewal, trust distribution and protected keys. Offline operation needs an alternative to public ACME. Passkeys also require an appropriate secure browser origin and a stable relying-party identity across recovery/address changes.

## Cross-cluster replication and backups

One source, optional selected-database replication, and read-only copies with database-specific access. No automatic promotion. Filtering, DDL, users/grants, replication loops and compatibility require an explicit contract; this is not automatically a single stretched Galera cluster.

Select physical/logical backup methods and their compatibility with individual database exports, PITR and isolated inspection. Temporary restored databases must not join production Galera membership, restart upstream replication, or expose production credentials by default. Selective restore must handle schema/dependency conflicts and target write activity.

Manager backups include the configuration and secrets required for recovery, encrypted with an externally recoverable key. Restoring old metadata must first reconcile live state and fence obsolete controllers before resuming writes.

## Monitoring and permissions

Provide built-in metric collection/storage and native configurable dashboards. The metrics engine, archive format, index and aggregation semantics are undecided. On-demand S3/SFTP reads need time-range indexing, bounded download/cache space and visible missing/unavailable intervals.

Query telemetry coverage, overhead and sensitive SQL handling require a collection policy. Dashboard sharing does not grant access to underlying resources. Application-level roles must translate to real database privileges for automatic phpMyAdmin login; separate credentials require clear attribution and authorization rules.

## Extension boundaries

Separate database-engine capabilities, orchestration platform, backup transport, DNS provider and metric storage interfaces. Keep Galera recovery logic explicit. Future PostgreSQL, XtraDB and Kubernetes support must not inherit inappropriate Galera/Docker assumptions.
