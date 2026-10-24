---
title: Services (stub)
---

# Services (stub)

[Guide index](index.md) | Previous: [Configuration reference](12-configuration-reference.md)

**Status: [Fakes] only.** The service-type framework (work package W07, requirements X03, X06 and X07 in [KONZEPT.md](../../KONZEPT.md)) exists as code and unit tests. It has never run against a Swarm, Docker or Traefik, and the web UI for it has only run against the mock server. This page is a stub that records what exists, so nobody mistakes it for a usable feature.

## What exists

Three service types, all planned by pure code (`internal/service`) and exposed by the manager API:

- **container**: a generic Docker service with a pinned image, environment, Swarm secrets, networks, managed local volumes, resources, placement and a restart policy.
- **traefik**: a Traefik service with a typed model of entry points, ACME certificate resolvers, routers, services (backends) and middlewares. The manager renders static and dynamic configuration as YAML for review.
- **galera**: a MariaDB Galera cluster. The spec has the fields of the cluster creation request (`size`, `image`, `network`, `rootPasswordSecret`, `gcacheSize`, `resources`, `placement`, `autoReplace`). The type changes no safety rule: every plan comes unchanged from the Galera planner, and every request runs the existing cluster flow (see below).

## Galera as a service type

A galera service *is* a cluster: `GET /api/v1/services` lists every cluster as a galera service, and the same ID works on `/api/v1/clusters`.

- **Create** through `POST /api/v1/services` runs exactly the cluster creation of `POST /api/v1/clusters`: the same plan, the same operation (tests compare them step by step) and the same permission (`create` on the cluster resource, not on the service resource). Like the direct path it starts without an approval step; the response has an empty approval digest.
- **Delete** through `DELETE /api/v1/services/{id}` is the cluster deletion with its approval (`delete` on the cluster).
- **Replace a node, guided recovery, maintenance and backups** stay on the cluster pages and routes. The quorum and verified-donor checks of a replacement, the approval of a full-outage recovery, the deny-list and the ownership rules are unchanged; the service API offers no way around them.
- **Not supported**: updating a galera service with the generic update strategy (409; Galera nodes are updated node by node behind sync gates) and changing the cluster size (there is no planner for it).
- A role that may manage services but has no cluster permission can neither see nor create nor delete a galera service.

## Updates

`POST /api/v1/services/{id}/update-plan` previews and `POST /api/v1/services/{id}/update` requests an update of a container or traefik service. The body carries the new `spec` and a `strategy`:

| Field | Default | Meaning |
| --- | --- | --- |
| `order` | `stop-first` | `stop-first` stops the old task before starting the new one; `start-first` starts the new task first. `start-first` is refused for services with local volumes (two tasks must not share one volume). |
| `parallelism` | 1 | Tasks replaced at once. |
| `delaySeconds` | 0 | Pause between batches. |
| `monitorSeconds` | 30 | How long Swarm watches each new task. |
| `maxFailureRatio` | 0 | Tolerated share of failed tasks. |
| `onFailure` | `rollback` | `rollback`: Swarm restores the previous spec when a new task fails (exits or fails its container health check) within the monitor period. `pause`: Swarm stops the update. |
| `canary` | none | Accepted but not executable yet: a plan with a canary has a blocker. |

The plan has three steps: `update-service` (the agent's existing command, with the strategy as Swarm update and rollback configuration), `await-service` (fails when Swarm rolled the update back or paused it) and a health gate (every desired task must run). If the gate does not hold, the operation pauses; it never reports success on unknown health. The update waits for an approval like create and delete (`update` permission on the service).

The stored spec changes only after the update succeeded; the old spec is then kept, and `{"rollback": true}` (without a spec) plans the update back to it. Each update plan also shows its prepared rollback plan. The agent's `update-service` cannot change the replica count, the placement or the volumes: such a change is a blocker (delete and recreate the service instead).

## Health

The manager derives the health of a container or traefik service from the Swarm task state of its owned service (running against desired tasks, and the Swarm update state): `healthy`, `degraded` (some tasks run, or a rolled-back or paused update), `unhealthy` (no task runs) or `unknown` (not observed, or older than two minutes; never shown as healthy). `GET /api/v1/services` shows it. A change between healthy and not healthy is recorded as an event and sent to the notification channels (`health.service_unhealthy`, `health.service_recovered`); no notification is sent while the service is being created, updated or deleted. For galera services the health is the cluster's wsrep-based state.

## Plan preview, export and expert view

Every plan endpoint (`/services/plan`, `/services/{id}/delete-plan`, `/services/{id}/update-plan`) returns, besides the readable steps, the **expert view** `agentSteps`: the exact agent commands of the operation, with the target host, the arguments as the agent receives them and the conditions checked first. In a preview the operation, token, approval (and for galera the cluster and node) IDs are provisional (`provisionalIds`); a start issues new ones, everything else is the same. The response of a started request shows the steps of the stored operation itself. Add `?format=yaml` to export the same document as YAML (`application/yaml`) instead of JSON. Arguments never contain secret values.

Rules the types enforce: images must be pinned (no `latest`); environment variables that look like secrets (`*_PASSWORD`, `*_TOKEN`, ...) are refused and must use a Swarm secret with the `_FILE` convention; volumes are managed local volumes with an identity marker, never host paths, and need a pinned host and one replica. Rendered configuration never contains secret values.

## Flow

1. `POST /api/v1/services/plan` shows the plan (steps, generated artifacts, warnings, blockers) without changing anything.
2. `POST /api/v1/services` stores an operation and waits. Every state-changing step needs an approval bound to the digest of the planned operation.
3. `POST /api/v1/operations/{id}/approve` with that digest lets it run. Approving needs the `create` (for a deletion `delete`, for an update `update`) permission on the `service` resource in the Swarm.
4. `DELETE /api/v1/services/{id}` plans the removal the same way; volumes and their data are kept unless `removeVolumes` is set.

Permissions are deny by default. The viewer and operator presets can only read services; the administrator preset can do everything. Every decision is audited.

## Known limits

- **Published ports cannot be created yet**: the agent's service spec has no port field. A plan with a published port is shown with a blocker and cannot be started. This includes the entry points of Traefik.
- **Configuration files cannot be delivered yet**: the agent has no command for it, so the dynamic Traefik configuration (routers, services, middlewares) is only a preview and also blocks the plan.
- Health, updates, rollback, the galera type and the expert/export view exist with fakes only: no update, rollback or health observation has run against a real Swarm, and Swarm's automatic rollback behaviour is taken from the Docker documentation, not from a lab run.
- No canary rollout (the field only blocks), no manager-side automatic rollback (the rollback plan must be requested and approved), no backup of service volumes, no CLI.
- The web UI does not show updates, health, the expert view or the galera type yet.

## In the web UI (mock only)

The **Services** page lists managed services and has one form for both types. The three steps are always the same: describe the service, review the plan the manager returns (steps, generated files, warnings, blockers), then approve the operation with the digest it shows. Typing a password into an environment variable is refused: the form tells you to use a Swarm secret instead. A Traefik request is shown with its blockers (published ports and configuration delivery are not supported by the agent yet) and cannot be requested. Deleting a service first shows a deletion plan; the data volumes are kept unless you tick the box.

![Services page: container request, plan and approval](../../assets/screenshots/en-light-services.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data), not from a real manager.*

![Traefik request with its plan and blockers](../../assets/screenshots/en-light-services-traefik-plan.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data), not from a real manager.*

The UI has only run against the mock server; no service was ever created on a Swarm [Mock only].
