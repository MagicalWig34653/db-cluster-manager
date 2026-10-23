---
title: Health and alerts
---

# Health and alerts

[Guide index](index.md) | Previous: [Creating a cluster](03-create-cluster.md) | Next: [Backups and restore](05-backup-restore.md)

## Health

A leader-only collector asks each node's agent for the Galera status (`collect-health`) through the monitoring account over the node socket. `GET /api/v1/clusters/{id}` shows the cluster as `healthy` or `degraded` and each node with its state (`synced`, `donor`, `unavailable`, and so on). Absent values mean unknown, not zero.

Observed after a `docker kill` of one container: `degraded` and the node `unavailable` after 10 s, donor `n1` during state transfer, all `healthy/synced` after 40 s (Swarm restarted the task) [Lab VMs] (E4).

## Events and notifications

- A node going unhealthy or healthy records an event and a UI notification once per transition (suppressed while the cluster is being deleted). [Fakes]: added after E4 and **not** re-run on VMs; in E4 itself no event existed for the outage.
- There is **no** event or notification for a full-cluster outage yet.
- Notification channels: UI, webhook (SSRF-protected) and email, configured through `GET/PUT /api/v1/settings/notifications` (secrets are write-only, stored encrypted). `POST /api/v1/settings/notifications/test` sends a test event. A real webhook receiver or SMTP server was **not** tested [Fakes]. Acknowledge and read state: `POST /api/v1/notifications/{id}/ack` and `/read`.

## Alert rules and metrics

Default rules exist as a proposed set: Galera size deficit (1 min), no Primary component (30 s), flow control paused above 0.1 for 2 min, host disk above 90%, backup age past the schedule, certificate expiry below 14 and 3 days, target down, proxy without writer. Each has a severity and a cooldown. [Fakes] only: the engine, own time-series store and dashboards API are unit-tested; the collectors for most of these series are not wired to real hosts, so several rules cannot fire in a real installation yet. Metric retention limits and a long-term archive to S3/SFTP are library code [Fakes].

Dashboards (`/api/v1/dashboards`, `/api/v1/metrics/query`) render SVG charts in the UI. Rendered once in headless Chromium against fakes [Fakes].

## What to do about alerts

For a degraded cluster, follow [Troubleshooting](09-troubleshooting.md). Do not replace nodes while quorum is unclear: see [Node replacement](06-node-replacement.md).
