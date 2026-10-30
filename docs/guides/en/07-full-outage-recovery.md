---
title: Full-outage recovery with approval
---

# Full-outage recovery with approval

[Guide index](index.md) | Previous: [Node replacement](06-node-replacement.md) | Next: [Security model](08-security-model.md)

A full outage means no node is in a Primary component. The manager **never** bootstraps on its own: it diagnoses read-only, proposes a candidate, and acts only after an explicit approval of exactly the plan you saw.

## Procedure

1. **Diagnose (read-only):** `GET /api/v1/clusters/{id}/outage-diagnosis`. It reads each node's `grastate.dat` through its agent and combines it with the last health observation. It starts nothing and answers `409` while a Primary component exists. The result shows a proposed candidate, or explains why there is none (for example a tie at equal seqno after a clean shutdown, or `seqno: -1` on every node after a crash).
2. **Recover crashed positions (changes crash-recovery state only):** `POST /api/v1/clusters/{id}/outage-diagnosis` additionally runs `mariadbd --wsrep-recover` in one-off containers without network for nodes whose `grastate.dat` has no saved seqno. This performs InnoDB crash recovery on stopped data like any start would; it starts no database service and never bootstraps.
3. **Request the plan:** `POST /api/v1/clusters/{id}/outage-recovery` with `{"candidate": "<slot>"}`. The manager re-collects evidence and refuses with `409` if the fresh diagnosis proposes no candidate or a different one, if a Primary component exists, or if another operation is unfinished. Otherwise it stores the plan and returns an **approval digest**. Nothing is stopped or started yet.
4. **Approve:** `POST /api/v1/operations/{id}/approve` with the digest (`409 approval_mismatch` otherwise).
5. The plan: stop every node, wait until no task runs, verify every identity marker, re-check the candidate position and set `safe_to_bootstrap`, bootstrap the candidate, start the others one at a time, drop the bootstrap flag.

Requesting the plan needs operate permission on the cluster. **Approval needs the separate `recover` action on the cluster, which only the administrator preset holds**; the operator and restore presets no longer cover it [Fakes]. When a cluster becomes unavailable the manager records a critical event and sends the notification `health.cluster_outage` (and `health.cluster_outage_ended` afterwards) [Fakes]; it plans nothing by itself.

![Full-outage diagnosis and planned recovery waiting for approval](../../assets/screenshots/en-light-outage-recovery.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data), not from a real manager.*

## Choosing the candidate

Follow the diagnosis. If it names none, do not force one. If you suspect divergence or data loss, restore from a backup instead ([Backups and restore](05-backup-restore.md)). The manager never promotes a cross-cluster backup copy automatically.

## Primary component with a zero state UUID (E9)

After a full VM power loss in E9, two nodes re-formed a Primary component from `gvwstate.dat` with `wsrep_cluster_state_uuid = 00000000-0000-0000-0000-000000000000`. A third node repeatedly failed SST/IST with `Failed to parse uuid:seqno pair` and `IST started with wrong seqno`. The manager now marks this observation unhealthy and emits a critical `health.zero_cluster_uuid_primary` incident. It does not treat the component as safe donor or quorum evidence, and it does not label it a full outage while Primary is still reported.

The outage diagnosis and recovery endpoints return `409` with `{"error":"conflict","reason":"zero_cluster_uuid_primary"}` while this Primary component is observed. Preserve all node data and logs, inspect the component's state and joiner transfer errors, and plan recovery with an operator after the Primary component is gone. Do not force a bootstrap, replace a node automatically, or bypass quorum rules. A `409` with `reason:"no_candidate"` means the stopped nodes' evidence does not support a bootstrap candidate; inspect positions before any approved recovery. E9 diagnosed the cause; E12 verified the incident and the `409` reason on the real cluster (all three nodes were in the zero-UUID Primary after the power loss, API nodes `unavailable`).

### Guided zero-UUID recovery ([ADR 0029](../../decisions/0029-zero-uuid-recovery.md)) [Fakes]

The `409 zero_cluster_uuid_primary` answer now carries `"offer":"zero-uuid-recovery"`. The recovery is the E12 manual procedure as two approval-gated operations (the procedure itself was lab-verified; this automation ran only against fakes):

1. `POST /api/v1/clusters/{id}/zero-uuid-recovery` (operate permission) returns the prepare operation, its **snapshot name** and an approval digest. Approve it (`POST /operations/{id}/approve`, `recover` permission, administrators). It stops every node, waits until no task runs, verifies the identity markers and **copies every data directory** to `snapshots/<name>/` on the same volume. Without enough free space the copy is refused and nothing is changed: free space or copy the data elsewhere, then start again.
2. With the nodes stopped the manager reads each position (`wsrep-recover`) and plans the second operation (`GET .../zero-uuid-recovery` shows its diagnosis, candidate and digest). The candidate is the node with the **unique highest** local seqno. Approve it: `safe_to_bootstrap` is set on that node, it bootstraps with a **new state UUID** (the old group UUID is gone), the others are started one at a time and take SST, the normal arguments are restored, and a state check requires every node Synced in one Primary component with one common non-zero UUID. The data check is that state check; table contents are not compared.
3. **Ambiguity always needs a human:** equal top seqno, an unreadable or missing position, or fewer than a majority of readable positions. The diagnosis lists them as risks (`auto_eligible: false`); without a candidate the nodes stay stopped and the snapshot is kept.

**Optional policy `autoRecover`** (per cluster, default off, `PUT /api/v1/clusters/{id}/auto-recover`, administrators only): when a zero-UUID Primary is observed, the manager starts the run and approves step 1 (stop and snapshot) itself; it approves step 2 itself **only** when every position is readable and the highest seqno is unique. It notifies before and after every run (`recovery.planned` / `recovery.needs_approval`, `recovery.ended`), keeps a one-hour pause after a run and never touches images or upgrades. Note that step 1 stops a cluster that still serves from the zero-UUID Primary; leave the policy off if you do not want that without your approval. The snapshot is a copy on the same volume, not a backup, and is never deleted automatically.

For reference, the manual procedure this replaces [Lab VMs]: (1) scale all database services to 0; (2) read each node's position with a one-off container on its volume (`mariadbd --wsrep-recover` with the cluster's binlog options); (3) pick the node with the highest seqno (lowest slot on a tie; never a node that was down while the others kept writing) and set `safe_to_bootstrap: 1` in its `grastate.dat`; (4) update that service with `--wsrep_cluster_address=gcomm://` and scale it to 1; (5) scale the other services to 1 so they take SST; (6) put the bootstrap node's normal arguments back. The cluster gets a **new** state UUID; in E12 all data committed before the power loss was present afterwards. Keep copies of the volumes first; see [E12 results](../../../tests/p0/results/e12-recovery-autofailover-rolling.md).

## Verification

| Part | Level |
| --- | --- |
| Clean-shutdown outage and crash outage on a 3-node MariaDB 11.4 cluster: correct read-only diagnosis (tie; no candidate until `--wsrep-recover`), no change for 120 s, recovery after approval in 40 s and 61 s, data and state UUID kept | [Lab VMs] (E6, final run exit 0) |
| A recovery that silently started a new history from `seqno: -1` was found and fixed | [Lab VMs] (E6, with regression test) |
| 5-node clusters, non-default datadir layouts, a VM power loss | [Untested] |
| Guided zero-UUID recovery (two operations, snapshot, `autoRecover` policy, permissions): the manual procedure ran on VMs in E12, the automation only against fakes | [Fakes] |
| Event and notification for a full outage; the `recover` permission for approval | [Fakes]: not run on VMs |
