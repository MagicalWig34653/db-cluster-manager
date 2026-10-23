---
title: Node replacement
---

# Node replacement

[Guide index](index.md) | Previous: [Backups and restore](05-backup-restore.md) | Next: [Full-outage recovery](07-full-outage-recovery.md)

## When to replace

A node whose host is gone, or whose data you decide to discard. A node that merely restarted needs no replacement: Swarm restarts the task and Galera rejoins (observed in E4 within 40 s [Lab VMs]).

## How

`POST /api/v1/clusters/{id}/nodes/{slot}/replace` (cluster operate permission, audited). The plan is built under the quorum rules in `galera.PlanReplacement`: a verified donor and a Primary component must be observed on **fresh** data. Otherwise the request is refused with `409 requires_guided_recovery`, nothing starts, and **there is no override**. Another unfinished operation on the cluster gives `409 busy`. With `autoReplace` at create time the manager may replace automatically only within those same rules; it never bypasses quorum.

The operation chooses a target host by the placement rules, creates a new volume with an identity marker and lets the new node join by state transfer (SST). The exact step list is defined in `internal/galera` and `internal/runner` and has run only against fakes; read the plan in the operation view before relying on it.

## Verification

| Part | Level |
| --- | --- |
| Host loss, returning host, empty-volume guard, minority partition at service level (scripted) | [Lab VMs] (E1/E2) |
| Replacement through the manager API with the real agent | [Untested]: E4 did not run node replacement |
| Plan and refusal logic (`requires_guided_recovery`, `busy`), end-to-end with fake agents | [Fakes] |
| Joiners always take SST (no IST optimization) | by design, observed in E4/E5 |
| A VM power loss and graceful host drain | [Untested] |

Do not use this to recover from a full outage: see [Full-outage recovery](07-full-outage-recovery.md).
