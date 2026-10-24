---
title: Services (stub)
---

# Services (stub)

[Guide index](index.md) | Previous: [Configuration reference](12-configuration-reference.md)

**Status: [Fakes] only.** The service-type framework (work package W07, requirements X03, X06 and X07 in [KONZEPT.md](../../KONZEPT.md)) exists as code and unit tests. It has never run against a Swarm, Docker or Traefik, and the web UI for it has only run against the mock server. This page is a stub that records what exists, so nobody mistakes it for a usable feature.

## What exists

Seven service types (generic container, Traefik, MariaDB Galera and four storage protocol servers), all planned by pure code (`internal/service`) and exposed by the manager API:

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

The storage types (SFTP, FTP/FTPS, S3-compatible object storage, NFS) are described in [Storage servers](#storage-servers-w09-fakes-only) below. Rules the types enforce: images must be pinned (no `latest`); environment variables that look like secrets (`*_PASSWORD`, `*_TOKEN`, ...) are refused and must use a Swarm secret with the `_FILE` convention; volumes are managed local volumes with an identity marker, never host paths, and need a pinned host and one replica. Rendered configuration never contains secret values.

## Flow

1. `POST /api/v1/services/plan` shows the plan (steps, generated artifacts, warnings, blockers) without changing anything.
2. `POST /api/v1/services` stores an operation and waits. Every state-changing step needs an approval bound to the digest of the planned operation.
3. `POST /api/v1/operations/{id}/approve` with that digest lets it run. Approving needs the `create` (for a deletion `delete`, for an update `update`) permission on the `service` resource in the Swarm.
4. `DELETE /api/v1/services/{id}` plans the removal the same way; volumes and their data are kept unless `removeVolumes` is set.

Permissions are deny by default. The viewer and operator presets can only read services; the administrator preset can do everything. Every decision is audited.

## Storage servers (W09, fakes only)

Four types share one pattern: you give the image, a pinned host and Swarm secret names; the manager plans a managed local volume (identity marker, one replica, pinned to the host), the Swarm service and review artifacts. The images named below are **proposals** for the first lab run, not dependencies; each type documents what the image has to read.

| Type | What it plans | Image contract (proposal) | Notes |
| --- | --- | --- | --- |
| `sftp` | SSH port 22, published in host mode (default), users with password and/or authorized-keys **secrets**, optional host key secret | `DBCM_SFTP_*` variables, users file `/etc/dbcm/sftp-users.json` from an existing Swarm config (`usersConfig`) | Candidate: `atmoz/sftp` with a thin wrapper |
| `ftp` | control port (21, or 990 for implicit TLS) plus a passive range published one to one in host mode, TLS certificate and key secrets | `DBCM_FTP_*` variables (mode, passive range and address, TLS files) | Plain FTP needs `allowPlaintext`; the passive address is required; at most 100 passive ports |
| `s3` | S3 API on 9000, optional console on 9001, root user and password **secrets**, optional Traefik routes (labels and a file-provider snippet) | MinIO conventions (`MINIO_ROOT_USER_FILE`, `MINIO_ROOT_PASSWORD_FILE`, `server /data`) | Single-drive mode: no erasure coding, no replication |
| `nfs` | NFSv4 on 2049, rendered exports file | kernel NFS server | **Always blocked**: needs `CAP_SYS_ADMIN` (privileged mode) and the `nfsd` module; Swarm services cannot be privileged and the agent has no capability field. Feasibility is an R8 lab question; the type may be deferred |

What every plan shows: the Swarm service JSON, the published ports (`published-ports.json`), a users or exports artifact (secret names and `/run/secrets/<name>` paths only, never values), warnings (tag-only image, no memory limit, no backup, no TLS, single drive, `no_root_squash`, console published directly) and blockers. `requireDigest: true` refuses an image that is not pinned by digest; without it a tag gives a warning.

Secrets are Swarm secrets that you created beforehand. The spec, plan, logs and operation records hold their names only. A literal `password` field does not exist, and environment variables that look like secrets are refused as for every container.

**Traefik routing for S3.** `apiRoute` and `consoleRoute` take hosts, entry points, TLS and a certificate resolver as typed values. The plan sets the `traefik.*` labels on the service (for a Traefik with the Swarm provider, which the Traefik type does not enable yet) and renders the same route as a file-provider snippet. The manager does not place that snippet yet (see limits).

**Traefik management metadata.** A Traefik plan now carries `traefik-management.json` (entry points, routers with hosts, backends, middlewares, resolvers, the dynamic file path, dashboard and insecure API off) for a UI to show what the instance holds.

## What the agent can do now

`create-service` accepts published ports (`ports`: target, published, protocol, mode ingress or host) and references to existing Swarm **configs** (`configs`: name or ID, absolute target path, mode). A referenced config must belong to the installation and be on the allow-list like networks and secrets, and is pinned to the checked ID. A spec without the new fields encodes and digests as before.

## Known limits

- **Swarm configs cannot be created yet**: the agent mounts an existing config but has no command to create or remove one. The dynamic Traefik configuration (routers, services, middlewares) therefore still blocks its plan, and the SFTP users file must be created by hand (`usersConfig`) or the plan warns.
- Published ports work in the agent and the Docker client against fakes only; nothing ran on a Swarm. No storage server image was run, so no client access smoke test, credential rotation or backup exists (all R8 exit gates are open).
- The storage UI form, CLI and any backup/restore of storage volumes do not exist. The web UI knows only the container and Traefik types.
- Health, updates, rollback, the galera type and the expert/export view exist with fakes only: no update, rollback or health observation has run against a real Swarm, and Swarm's automatic rollback behaviour is taken from the Docker documentation, not from a lab run.
- No canary rollout (the field only blocks), no manager-side automatic rollback (the rollback plan must be requested and approved), no backup of service volumes, no CLI.
- The web UI does not show updates, health, the expert view or the galera type yet.

## In the web UI (mock only)

The **Services** page lists managed services and has one form for both types. The three steps are always the same: describe the service, review the plan the manager returns (steps, generated files, warnings, blockers), then approve the operation with the digest it shows. Typing a password into an environment variable is refused: the form tells you to use a Swarm secret instead. A Traefik request with dynamic configuration is shown with its blocker (the agent cannot create Swarm configs yet) and cannot be requested; the mock data still shows the older port blocker. Deleting a service first shows a deletion plan; the data volumes are kept unless you tick the box.

![Services page: container request, plan and approval](../../assets/screenshots/en-light-services.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data), not from a real manager.*

![Traefik request with its plan and blockers](../../assets/screenshots/en-light-services-traefik-plan.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data), not from a real manager.*

The UI has only run against the mock server; no service was ever created on a Swarm [Mock only].
