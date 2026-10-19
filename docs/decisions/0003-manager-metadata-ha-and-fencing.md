# ADR 0003: Manager metadata replication, coordination and fencing

Date: 2026-10-19
Status: Proposed. Nothing here is validated. No candidate library was run or version-checked yet; the properties below are design expectations to be confirmed by the W03 prototype and recorded as evidence.

## Context

Confirmed requirements (see [KONZEPT.md](../KONZEPT.md)): P07 manager HA with at least three instances and a single instance for small tests; local storage without distributed storage; manager metadata must not depend on the managed database cluster; B07/B09 configuration backup and restore that first inspects live state; databases and proxies keep serving when the manager is gone ([ARCHITEKTUR.md](../ARCHITEKTUR.md)).

Problems to solve together: durable metadata replication, leader election, job ownership, stale-owner fencing, secret availability and restore of old metadata.

## Proposed direction

1. **Replicated log inside the manager.** Each manager replica embeds a Raft-style consensus module and keeps its own local copy of the state on local disk. Three replicas tolerate one failure; a lost majority makes the metadata read-only and pauses all unsafe operations (databases keep serving). A single replica runs the same code with a one-member group.
2. **Single writer of operations.** Only the current leader schedules and executes operations. An operation has a durable ID, step state, timeout, bounded retries and idempotent steps; the journal is part of the replicated state.
3. **Fencing token = consensus term.** Every command to an agent or to the Docker API carries the leader's term (and operation ID). Agents persist the highest term seen and reject commands with a lower term. Network loss during a step means "unknown completion": the new leader reconciles against live state before retrying.
4. **No competing controllers.** Swarm's scheduler stays the only component placing tasks; agents never recreate nodes on their own. Manager actions on Swarm services are recorded with resource IDs; the ownership inventory constrains what may be modified.
5. **Restore.** A manager restored from backup starts in inspection mode, advances the term beyond any value previously issued (stored in the backup and additionally negotiated with agents) and requires approval before resuming changes.
6. **Secrets.** Secrets are encrypted in the replicated state with a key that is available to all replicas and recoverable externally (B07). Key distribution is not designed yet.

## Alternatives considered

| Option | For | Against / unknown |
| --- | --- | --- |
| Embedded Raft library in the manager (for example the HashiCorp or etcd Raft libraries) | One binary, no extra service, term available for fencing | Own storage, snapshotting and membership code to build and test |
| Embedded etcd | Mature consensus, leases and watches | Large dependency, operational surface, unclear fit for single-binary single-node mode |
| Replicated SQLite service (for example rqlite) | SQL access to state | Extra service; interplay with fencing and secrets unclear |
| Managed Galera cluster as metadata store | Existing technology | Rejected by requirement: metadata must not depend on the managed cluster |
| Swarm's internal state or Swarm leader as lock | No extra store | Not a general datastore; Swarm manager semantics are not a fencing mechanism; manager may run outside the Swarm |
| Active/passive with a lease on shared storage | Simple | Requires shared storage, which is not assumed |

Do not build custom consensus. Three replicas alone are not proof of HA; the experiments below are the proof.

## Open questions

- Which consensus implementation, storage engine and snapshot format; supported upgrade path of the state schema.
- Member bootstrap, enrollment and removal; relation to agent mTLS identities and enrollment.
- Behavior with two of three replicas on one host or in one failure domain; two-replica degraded operation.
- How an agent learns the current term after a manager restore without trusting a stale leader.
- Whether agents also need local state for safe autonomous behavior during manager loss (for example refusing destructive commands).

## Required evidence (W03 prototype, then W05 on VMs)

- Leader loss during a multi-step operation: no step runs twice unsafely; the new leader resumes or aborts at a safe boundary.
- Partitioned old leader: its commands are rejected by agents after a new term is established.
- Restored old metadata: obsolete controller is fenced; live-state differences are reported before changes.
- Majority loss: metadata read-only, no new operations, no effect on served databases.
- Measured resource use of three replicas within the budget in [TESTING.md](../TESTING.md).

First prototype step (no VMs, no Docker): three in-process or loopback-process replicas with injected partitions, a simulated agent that enforces term checks, and tests for the scenarios above. VM-based runs wait for the lab controls of W04.
