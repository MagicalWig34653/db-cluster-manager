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

**The guided recovery cannot start from this state.** After the zero-UUID re-formation `mariadbd --wsrep-recover` returns positions with the zero UUID on every node, so the diagnosis answers `no_candidate`. E12 repaired the cluster by hand [Lab VMs], an operator procedure that is not a product feature and needs your decision: (1) scale all database services to 0; (2) read each node's position with a one-off container on its volume (`mariadbd --wsrep-recover` with the cluster's binlog options); (3) pick the node with the highest seqno (lowest slot on a tie; never a node that was down while the others kept writing) and set `safe_to_bootstrap: 1` in its `grastate.dat`; (4) update that service with `--wsrep_cluster_address=gcomm://` and scale it to 1; (5) scale the other services to 1 so they take SST; (6) put the bootstrap node's normal arguments back. The cluster gets a **new** state UUID; in E12 all data committed before the power loss was present afterwards. Keep copies of the volumes first; see [E12 results](../../../tests/p0/results/e12-recovery-autofailover-rolling.md).

## Verification

| Part | Level |
| --- | --- |
| Clean-shutdown outage and crash outage on a 3-node MariaDB 11.4 cluster: correct read-only diagnosis (tie; no candidate until `--wsrep-recover`), no change for 120 s, recovery after approval in 40 s and 61 s, data and state UUID kept | [Lab VMs] (E6, final run exit 0) |
| A recovery that silently started a new history from `seqno: -1` was found and fixed | [Lab VMs] (E6, with regression test) |
| 5-node clusters, non-default datadir layouts, a VM power loss | [Untested] |
| Event and notification for a full outage; the `recover` permission for approval | [Fakes]: not run on VMs |
