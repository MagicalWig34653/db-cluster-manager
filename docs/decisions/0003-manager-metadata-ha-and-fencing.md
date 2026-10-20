# ADR 0003: Manager metadata replication, coordination and fencing

Date: 2026-10-19
Status: Proposed; prototype evidence is in-process and loopback-process, plus one lab VM run (E3, see the end of this document). Not accepted for production.

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

## Prototype evidence (in-process and loopback processes, no VMs)

Library choice and comparison: [0003-evidence-library-evaluation.md](0003-evidence-library-evaluation.md). Chosen for the prototype: `github.com/hashicorp/raft` v1.8.0 (in-memory stores, in-memory transport wrapped with a switchable partition matrix). Executed on the development host: `go test -race -count=3 ./...` passed for `internal/fencing`, `internal/opjournal` and `internal/metastore`.

Persistence extension (branch `roadmap/metastore-persist`): `Config.DataDir` selects disk storage with `github.com/hashicorp/raft-boltdb/v2` v2.4.2 (MPL-2.0; bbolt v1.4.1, MIT) for log and stable state plus raft's file snapshot store; `Config.BindAddr` selects raft's plain TCP transport (no TLS). `cmd/metad` runs one node as a process with an unauthenticated loopback HTTP harness for VM experiments. Executed on the development host (go1.27.1 darwin/arm64): `go test -race -count=3 ./internal/... ./cmd/...` passed.

| Scenario | Test | Observed result |
| --- | --- | --- |
| Three TCP nodes on loopback commit, take file snapshots, commit more; all stop and restart from disk | `TestTCPClusterRestartsFromDisk` | Each node's term after restart (read before any new election) is not lower than before the stop; after `Barrier` the leader's journal is byte-identical to the pre-stop journal; all nodes converge; ownership fencing still applies |
| TCP leader stopped; two nodes continue; old leader restarted from disk | `TestTCPLeaderRestartConverges` | Higher term elected; commits continue; the restarted node converges to the identical journal and term; the old term is rejected with `ErrNotOwner` |
| New leader trusts its journal only after a barrier | `TestBarrierMakesNewLeaderJournalComplete`, `TestBarrierOnFollowerReturnsErrNotLeader` | After `Barrier` the journal holds all 25 committed operations without waiting. With the barrier call removed, the same check failed in 19 of 20 runs (20 of 20 with `-race`): the lag is real. Followers get `ErrNotLeader` |
| Three `metad` processes, SIGTERM, restart | `cmd/metad` `TestThreeProcessesRestartFromDisk` | Writes via HTTP on the leader succeed, followers answer 503 `not_leader`; after a full stop and restart the unfinished operation and its `running` step are restored and the term did not decrease |
| Snapshot size | `TestFSMSnapshotHoldsStateNotHistory`, `internal/opjournal` `TestSnapshotSizeTracksStateNotHistory` | 600 further commands on one operation do not grow the snapshot beyond the changed state fields |

| Scenario | Test | Observed result |
| --- | --- | --- |
| Three nodes elect one leader; committed operation commands reach all journals; followers refuse writes; a new leader keeps committed state after leader stop | `internal/metastore/store_test.go` | Passed |
| Leader lost mid-operation | `TestScenarioLeaderLossMidOperation` | New leader adopts; the interrupted step is `Unknown`, never `Succeeded`; it is retried (attempt 2) and the operation completes; the simulated agent executed the finished step exactly once |
| Partitioned old leader | `TestScenarioPartitionedOldLeader` | After the majority elects a higher term, the agent rejects the old term with `ErrStaleTerm`; the old leader's write does not commit and the majority journal stays unchanged |
| Majority loss | `TestScenarioMajorityLoss` | Survivor's write does not commit; existing journal remains readable |
| Heal | `TestScenarioHealConverges` | Old leader steps down and converges to the same journal as the others |

Findings that change the design:

- `Apply` on a leader that lost its quorum returns "apply outcome unknown" (`metastore.ErrOutcomeUnknown`), not "not leader", because the entry might still commit. Callers must treat this as unknown completion and reconcile.
- The state machine does not compare `Command.Term` with the term at commit time. Fencing relies on the agent-side guard and on the issuer passing the current term. Whether the state machine should enforce this too is open.
- Journal reads are local and may lag; a new leader must call `Node.Barrier` before it trusts its journal. Implemented and tested (see above); without it the lag was observed in most runs.
- The snapshot is the journal's own state export (`opjournal.Journal.Snapshot`, versioned JSON of all operations); restore replaces the journal object. Its size follows the journal state, not the command history. Operations are never removed from the journal, so it still grows with the number of operations ever begun; compaction of finished operations is open.
- Bootstrapping a running node (`Raft.BootstrapCluster`) raced with AppendEntries from peers that had already elected a leader ("bootstrap only works on new clusters" in one of three TCP runs). The node now bootstraps its stores before Raft starts (`raft.BootstrapCluster`) and skips bootstrap when state exists, so restarted nodes may keep the flag.

Limitations: persistence is now on local disk, but fsync loss, disk-full and corrupted files were not tested (a second node on the same data directory fails on the bbolt lock: `TestSecondNodeOnSameDataDirFails`, same process only); the TCP transport has no TLS or peer authentication; the `metad` HTTP endpoint is an unauthenticated test harness; no membership change after bootstrap; only loopback, no real network between hosts; no agent mTLS, enrollment, secret handling or restore-from-backup flow was exercised; resource use was not measured; AGPL-3.0-only/MPL-2.0 compatibility was read from licence text only, without legal review. The VM-based confirmation belongs to W05 after the lab controls of W04.

### Evidence (lab VMs, 2026-10-20)

Source: [E3 results](../../tests/p0/results/e3-manager-failover.md), script `tests/p0/e3-manager-failover.sh`.

Setup:

- Three `dbcm-lab-` Lima VMs (arm64, 2 vCPU / 2 GiB each) created through `bin/lab apply -network lima:user-v2`.
- One `metad` process per VM (linux/arm64 cross-compiled; raft library defaults: 1 s heartbeat and election timeout, pre-vote on) forming a Raft group over the user-v2 network.
- The test script acts as the controller.
- A lab-only simulated agent (`tests/p0/e3agent`, using `internal/fencing.Guard`) and an unrelated nginx sentinel run on the third VM.
- One recorded run; every figure is a single sample.

| Claim | Result on VMs |
| --- | --- |
| Leader lost mid-operation: new leader adopts, interrupted step becomes `unknown`, is retried, no step runs twice unsafely | **Confirmed** for leader process `SIGKILL` (new leader known after 2.2 s) and leader VM power-off (1.6 s). In the power-off case the step had already executed; the retry hit the agent's idempotency record and did not execute again |
| Partitioned old leader cannot commit and its old term is rejected by the agent | **Confirmed**: isolated leader stepped down after 0.57 s; its write returned `504 outcome_unknown`, then `503`; nothing committed; agent rejected term 5 after admitting 6 |
| Majority continues; healed old leader converges | **Confirmed**: new leader after 2.4 s, 150 further commits; healed node converged to the same journal and term within 2 s |
| Pre-vote keeps an isolated node from inflating the term | **Confirmed** (observed, not a prior claim): the isolated node and the quorum-less survivor kept their terms |
| Restart from data directories restores journal and terms | **Confirmed** for all three stopped with SIGTERM, one node after `SIGKILL`, and one node after a VM power cut |
| Majority loss: metadata read-only, no new operations, served workloads unaffected | **Confirmed**: writes and barrier reads `503`, local reads work, sentinel unchanged |
| Resource use within budget | **Measured**: 9-13 MiB RSS and under 3 % of one vCPU per replica; commit latency p50 1.4 ms / p99 4.3 ms (sequential, loopback HTTP) |
| Behavior under host CPU starvation | **Refuted as stable with default timeouts**: one run under unrelated host load (load average about 800) had unprovoked leader changes, and a controller holding a 30 s old leader got `503`. Timeout tuning and leader re-resolution are needed |
| Restored old metadata is fenced (ADR item 5) | **Untested** |
| Snapshot restore on VMs, membership change, TLS/authenticated transports, asymmetric partitions, real manager controller and real agent | **Untested** (no snapshot was taken: about 480 commits are below the default threshold) |

Findings for the design:

- Lima forwards guest loopback listeners to host loopback by default, so the unauthenticated harness was reachable on host `127.0.0.1:18101-18103`.
- A VM clock was off by about 240 ms right after a power cut and restart.
