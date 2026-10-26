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

Prototype control-plane core (`internal/manager`, verified in process and on loopback only): the metastore's Raft state machine also holds a generic versioned document store (atomic multi-document compare-and-set on a global revision, snapshotted with the journal, backward compatible with journal-only snapshots). The manager keeps swarms, hosts with enrollment state, clusters, settings, operation definitions/views, events, accounts, passkeys, sessions, recovery codes, roles/bindings and audit records there; session digests, recovery-code hashes and passkey records are sealed with AES-256-GCM using a data key from a `KeyProvider`. Key provisioning across replicas is not solved. Only the leader serves the API and runs operations; on gaining leadership it runs a barrier, takes over unfinished operations with a fresh runner and starts reconcilers, and on losing it stops them and refuses agent commands locally. Not done: request forwarding from followers, membership changes, an authenticated metastore transport.

Manager-agent transport (ADR 0010, verified on loopback only): the manager dials each agent over mutually authenticated TLS 1.3 at the node's Swarm address; the agent runs in the host network namespace. Identities are URI SANs (`dbcm://<installation>/manager`, `dbcm://<installation>/agent/<node ID>`) issued by the manager's internal CA. Agents enroll with a one-time-per-host token from a Swarm secret and a pinned CA fingerprint (no trust on first use), submit a CSR for a locally generated key, renew over mTLS and are revoked through a serial deny list. The unauthenticated lab transport remains only behind `-insecure-lab`. The leader collects node health through the agents (`collect-health` over the node's socket or a host-published port, `read-grastate` for outage evidence), stores timestamped observations and reports stale ones as unknown; operation preconditions probe live through the same path. Secure Docker access from the manager to Swarm managers (P04) remains design work. Never assume that an unauthenticated network Docker socket is acceptable.

Command API (ADR 0011, verified with fakes and one in-memory replica only): hosts, Swarm connection/sync/agent deployment, cluster creation, node replacement, the read-only full-outage diagnosis plus position recovery (`--wsrep-recover`) and the approval-gated guided full-outage recovery (verified on lab VMs in E6), operation resume/abort/approve, backup policies, manual backups, inspection restores and notification settings are HTTP endpoints of the leader. The API authorizes and audits every call before the manager's command services (which check again) run. Restore operations wait before every destructive step for an approval that quotes the SHA-256 digest of the stored operation definition and needs the restore permission. A leader-only reconciler starts scheduled backups as the manager's own subject (bound to the `manager_system` role at leader start) and tears down expired inspection instances. Notification secrets set through the API are sealed with the data key and never returned.

## Routing and certificates

Redundant proxy instances use the replicated manager writer choice and an all-member freeze/acknowledgement barrier before a writer change ([ADR 0017](decisions/0017-coordinated-proxy-writer.md)); local latency remains useful for reads and local health can remove a writer but cannot choose a different one. The earlier independent policy selected different writers in E8. Optional read/write splitting must account for transactions and read-after-write behavior.

A domain alone does not solve connection failover. Test proxy loss, DNS caching, VIP ownership and client reconnection. Host address/port preservation during migration depends on actual port bindings and the chosen Swarm publication mode; staying on the same host is not sufficient proof.

TLS application-to-endpoint is the confirmed database default. Further database-link encryption is optional. ACME DNS-01, imported certificates and an internal CA need renewal, trust distribution and protected keys. Offline operation needs an alternative to public ACME. Passkeys also require an appropriate secure browser origin and a stable relying-party identity across recovery/address changes.

## Cross-cluster replication and backups

One source, optional selected-database replication, and read-only copies with database-specific access. No automatic promotion. Filtering, DDL, users/grants, replication loops and compatibility require an explicit contract; this is not automatically a single stretched Galera cluster.

Select physical/logical backup methods and their compatibility with individual database exports, PITR and isolated inspection (proposed in [ADR 0009](decisions/0009-cluster-backup-method.md)). Temporary restored databases must not join production Galera membership, restart upstream replication, or expose production credentials by default. Selective restore must handle schema/dependency conflicts and target write activity.

Manager backups include the configuration and secrets required for recovery, encrypted with an externally recoverable key. Restoring old metadata must first reconcile live state and fence obsolete controllers before resuming writes.

## Monitoring and permissions

Provide built-in metric collection/storage and native configurable dashboards. The metrics engine, archive format, index and aggregation semantics are undecided. On-demand S3/SFTP reads need time-range indexing, bounded download/cache space and visible missing/unavailable intervals.

Query telemetry coverage, overhead and sensitive SQL handling require a collection policy. Dashboard sharing does not grant access to underlying resources. Application-level roles must translate to real database privileges for automatic phpMyAdmin login; separate credentials require clear attribution and authorization rules.

## Extension boundaries

Separate database-engine capabilities, orchestration platform, backup transport, DNS provider and metric storage interfaces. Keep Galera recovery logic explicit. Future PostgreSQL, XtraDB and Kubernetes support must not inherit inappropriate Galera/Docker assumptions.

### Broadened scope (proposed, not implemented)

The owner broadened the product to a general cluster and service platform ([ADR 0012](decisions/0012-broadened-scope-platform.md), X01–X12). Everything in this subsection is a proposal; no component exists. R1 (Galera on Swarm) is unchanged and remains first.

```text
             UI (forms, wizards, plan preview, expert view, export as code)
                                   |
                              Manager API
                                   |
     +-------------+---------------+----------------+------------------+
     |             |               |                |                  |
 Service type   Planner /     Explainability     Build service     Provisioner
 registry       reconciler    (plan, YAML,       (recipes,          (SSH/agent
 (Galera, web,  (per type,    commands, diff,    BuildKit, SBOM,    host bootstrap:
 storage, gen.  durable ops,  audit link)        scan, sign)        Swarm, K8s)
 container,     fencing)                             |                  |
 Traefik,          |                                 v                  |
 registry)         |                          Registry abstraction      |
     |             |                          (managed registry type    |
     +------+------+                           or external e.g. ghcr;   |
            |                                  creds as secrets)        |
            v                                        |                  |
   Orchestrator interface  <-------- image refs -----+                  |
     |               |                                                  |
  Swarm back end   Kubernetes back end (later)  <-----------------------+
     |
  Agents (host health, local storage, engine updates)
```

Dependency rules:

- Service types depend on the orchestrator interface, never on a concrete back end. A type declares which platforms it supports.
- The planner produces an explicit, ordered plan; the explainability component renders exactly that plan (no separate "description" path that could diverge). Executed steps are linked from the audit log (U07).
- The build service depends on the registry abstraction only; service types reference images by digest.
- The managed registry is itself a service type deployed through the orchestrator, so bootstrapping it must not require the registry (use upstream images or a pre-seeded offline bundle).
- The provisioner is separate from orchestration: it creates or extends clusters; the orchestrator manages workloads on existing ones. It has its own credentials and threat model.
- Docker engine updates run through agents as host operations, coordinated with A08 maintenance checks and the per-type quorum rules; the boundary for restarts and approvals is open ([ENTSCHEIDUNGEN.md](ENTSCHEIDUNGEN.md#open-decisions-from-the-scope-extension)).
- Galera-specific safety (bootstrap, quorum, guided full recovery) stays explicit code behind the service-type interface; the generic reconciler must not restart or replace stateful nodes on its own.

Interfaces built during R1 should keep these seams cheap (engine capabilities, orchestrator, backup transport) but R1 does not implement them generically.

## Threat model for extended features (2026-10-25)

Trust boundaries are the browser/API, manager process and replicated state,
agent/Docker host, build worker/registry, external advisory or AI provider,
ACME CA/DNS provider, and user-supplied service configuration. The manager's
Docker socket and any CLI-login AI provider inherit broad manager privileges.
These controls are code-level and fake-tested unless another section records
real evidence; they are not deployment assurance.

| Surface and attacker-controlled input | Failure mode | Current control and remaining gap |
| --- | --- | --- |
| Login, recovery and first-run setup requests | Credential guessing, ceremony exhaustion, setup takeover | Per-peer-IP lockout covers setup-token guesses, recovery redemption and now login begin/finish; peer address ignores forwarded headers. Setup token is 256-bit random, held as a digest and invalidated after the first administrator. The token is printed once to the manager console, so console/log readers are trusted during setup. Limiter state is process-local and lost on restart or leader change; distributed rate limits and a trusted-proxy policy remain open. |
| Web composer, custom nginx snippet and build context | Directive injection, file disclosure, context traversal, compromised build host | Typed fields and a size/structure-limited snippet deny-list reject file roots, includes, dynamic script modules and request proxying; context paths are cleaned and reject traversal and backslashes. Tests cover a malicious corpus. The snippet validator is not a complete nginx parser, and the catalog and output image have no upstream or runtime verification. Build execution has manager-process privileges in the current design; isolate it before production use. |
| Traefik, SFTP and other service specs | Route injection, ambiguous encoded paths, directory escape, secret values in plans | Typed Traefik rules and generated YAML avoid free-form rules; canonical path validation rejects dot segments and percent encoding. SFTP user homes must be clean absolute paths and reject backslashes. Secret *names* may enter plans; secret values use Swarm secrets. Generated artifacts and container image contracts have only fake evidence. |
| Advisory feeds and AI providers | Malicious feed text, prompt injection, secret exfiltration, unsafe auto-apply | Feed bodies and prompt inputs are bounded and screened, and AI output is advisory to a deterministic planner; CLI providers require explicit opt-in. Admin review and digest-bound approval protect state changes. Provider isolation, feed authenticity beyond transport, legal login mechanics and safe automated policy are unresolved. No real provider was contacted in this review. |
| ACME and certificate/key rotation | Challenge abuse, wrong-domain issuance, stale replica keys, irreversible key loss | Listener exposure policy, sealed ACME state, challenge scoping and leader-scoped renewal are code paths; keyring rotation checks every configured recipient and recovery envelope before commit. Tests use fake CAs and keys. DNS-01 and real CA behavior, recovery with an external private key, revocation propagation and replica startup after rotation need separate evidence. |
| Logs, plans and audit | Secrets copied from request bodies or underlying library errors | API errors are coarse; authentication failure logs now omit verification errors that could echo credentials. Docker daemon error bodies are discarded because they can echo secret/config payloads; the HTTP status and typed error remain. Registry/build logs redact known values and patterns, and audit records describe changes rather than credential bodies. Redaction is best effort; log sinks, subprocess output, future handlers and exported plans need recurring review and regression tests. |

The security workflow contains optional, manually triggered `govulncheck` and
`gosec` jobs with the repository's existing SHA-pinned checkout/setup-go
actions and read-only permissions. Tool versions are pinned as Go module
versions, but their availability and findings were not checked offline. A
workflow run is required before treating either scan as evidence.
