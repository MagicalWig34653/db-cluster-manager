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

The operation needs operate permission on the cluster; approval currently uses the **restore** permission on the cluster's backups (a known mismatch, see [Known limits](10-known-limits.md)).

## Choosing the candidate

Follow the diagnosis. If it names none, do not force one. If you suspect divergence or data loss, restore from a backup instead ([Backups and restore](05-backup-restore.md)). The manager never promotes a cross-cluster backup copy automatically.

## Verification

| Part | Level |
| --- | --- |
| Clean-shutdown outage and crash outage on a 3-node MariaDB 11.4 cluster: correct read-only diagnosis (tie; no candidate until `--wsrep-recover`), no change for 120 s, recovery after approval in 40 s and 61 s, data and state UUID kept | [Lab VMs] (E6, final run exit 0) |
| A recovery that silently started a new history from `seqno: -1` was found and fixed | [Lab VMs] (E6, with regression test) |
| 5-node clusters, non-default datadir layouts, a VM power loss | [Untested] |
| Event or notification for a full outage | [Untested]: not implemented |
