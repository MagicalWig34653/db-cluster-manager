---
title: Creating a Galera cluster
---

# Creating a Galera cluster

[Guide index](index.md) | Previous: [Enrolling hosts](02-hosts-and-agents.md) | Next: [Health and alerts](04-health-alerts.md)

## Request

`POST /api/v1/clusters` (or the UI cluster wizard). Required fields: `swarm`, `name` (lowercase letters, digits, hyphens, at most 48 characters), `size` (**3 or 5**), `image` (MariaDB image, pinned by digest), `network` (existing attachable overlay), `rootPasswordSecret` (name of an existing Swarm secret, never the password), `resources.limits` and `resources.reservations`, and `placement` (allow list, exclusion, preference, `reserve`). Optional: `gcacheSize`, `autoReplace`.

Placement precedence is allow list, exclusion, preference, then automatic scoring; allow-listing and excluding the same host is `policy_conflict`. Other coarse errors: `no_capacity`, `settings_incomplete`, `cluster_exists`. The response is an operation (`202`); watch it with `GET /api/v1/operations` (progress shows the current step, attempts and the latest unmet precondition).

## What the operation does

One pinned Swarm service per node, each with its own local volume and an identity marker; the bootstrap node starts first, the others join one at a time; the monitoring account (`dbcm_monitor`) and backup account (`dbcm_backup`) are created and replicate. A node never starts on an empty volume that lacks the marker (the E2 "empty-volume trap" guard).

## Verification

| Part | Level |
| --- | --- |
| 3-node MariaDB 11.4 cluster through the manager API: operation `succeeded` in about 80 s, all nodes `Synced`, size 3, same state UUID, writes on n1/n3 readable everywhere, SST by mariabackup over the relocated socket | [Lab VMs] (E4, once from scratch; host carried foreign load, timings only conditional) |
| Pinned services, host loss and return, minority partition, marker guard | [Lab VMs] (E1/E2, scripts, not through the manager) |
| 5-node clusters | [Untested] |
| Capacity planning, `no_capacity`, resource exhaustion | [Fakes] |
| Wizard UI | [Fakes] (headless browser, fake API) |
| amd64 | [Untested] |

## Failed create and deletion

A failed create keeps the cluster name and planned capacity until you delete it. `DELETE /api/v1/clusters/{id}` plans a `cluster/delete` operation and returns an approval digest; nothing is removed before `POST /api/v1/operations/{id}/approve` with that digest. It needs the **delete** permission (administrator preset only). Deletion removes only objects the installation created (database services, unfinished job services, inspection instances, node volumes after the identity check). Operator networks and secrets, host directories of local volumes and backups in the backend are kept. This path ran only against fakes [Fakes]; the E4 failed-create cleanup was done by hand.

## After creation

- Endpoints are shown as `<cluster>-nN:3306` inside the overlay network. A client-facing endpoint requires the database proxy, which the manager does not deploy yet; see [Known limits](10-known-limits.md). The `cmd/proxy` binary ran in front of the lab cluster (writer selection, failover, TLS termination) [Lab VMs] (E5), started as a lab container by hand.
- Database, user and grant management exist as SQL generation only [Fakes].
