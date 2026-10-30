---
title: Image updates
---

# Image updates [Fakes only; not run against a registry or Swarm]

The image update service ([ADR 0025](../../decisions/0025-image-update-service.md), requirement X16) finds newer images for your managed services and, only where you allow it, updates them: pull the newer tag or digest, update the Swarm service, wait for the health gate, roll back on failure. It works like Watchtower but only on objects the manager owns and always through the manager's own approval-gated service update. This module handles **container images only**. The Docker engine (docker-ce, containerd, buildx and compose plugins) is never updated this way; it is part of OS [patch management](23-patch-management.md) as the report-only `container-runtime` package class ([ADR 0023](../../decisions/0023-os-patch-management.md)).

## Enable it

It is off by default. Add to the manager configuration (needs `auth.enabled`):

```json
"imageUpdate": { "enabled": true, "pollIntervalSeconds": 300 }
```

Even enabled, every service starts in mode `manual`: nothing is checked on a schedule until an administrator sets a policy.

## What is in scope

Only services that carry your installation's ownership label. Foreign containers and services of other installations are invisible and answer 404. A foreign service can opt in with the label `dbcm.image-update=opt-in`; it is then only reported (no approval or update is possible).

## Policies per service

`PUT /api/v1/image-updates/services/{id}/policy` (administrator):

| Field | Meaning |
| --- | --- |
| `mode` | `manual` (default), `notify` (scheduled check and a reviewed plan, you approve), `auto` (eligible plans are approved by the policy and applied inside a window) |
| `track` | `digest` (same tag, new digest), `patch`, `minor`, `major` |
| `windows` | maintenance windows (days, `start` HH:MM, `durationMinutes`, `zone`); required for `auto`; also enforced when you apply by hand |
| `strategy` | Swarm update strategy: `order`, `parallelism`, `delaySeconds`, `monitorSeconds`, `maxFailureRatio`; failure always means roll back |
| `checkEveryMinutes` | default 360 |
| `waitMinutes` | how long an update may run before its outcome is "unknown" (default 15) |

Hard rules: databases never follow `major`; `auto` never crosses a major version, never applies a database minor jump, never touches Galera, built images, rollbacks or foreign services. Galera clusters get plans that only show the candidate; the update goes through the rolling or minor-upgrade planner.

## The flow

1. `POST .../services/{id}/check` (operator) or the scheduler reads the registry (credentials from the sealed registry store) and creates a plan: from image, to image `repo:tag@sha256:...`, kind, strategy, digest. The first check of an unpinned tag only records the digest baseline.
2. You review it and approve with the plan digest: `POST /image-updates/plans/{id}/approve`.
3. `POST .../apply` starts the service update operation with the Swarm `UpdateConfig`/`RollbackConfig`, approves it with your permission, and waits. Outcomes: `succeeded`, `rolled_back` (Swarm restored the previous spec; recorded with the reason), `failed` (including an unknown outcome after the wait limit).
4. `POST .../rollback` on a succeeded plan proposes the way back; it needs its own approval and is never automatic.

Plans record who approved, the executing agent runtime (`executedBy`) and the operation ID. Notifications: `imageupdate.candidate`, `applied`, `rolled_back`, `failed`, `check_failed`.

## Patched images from the image build service

`POST /image-updates/offers` with `{"jobId": "..."}` of a succeeded build creates `built` plans for services running the same repository. They always need a person.

## Limits

Fakes only; no UI yet; no registry webhooks; the automatic hand-off from vulnerability builds is not wired; the running digest is not read from Swarm (baseline from the first check). See the ADR.
