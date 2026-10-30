# ADR 0030: Optional features are modules that an administrator switches

Status: **Implemented; verified with fakes and an in-process manager only (2026-10-30), no VM.** Implements the owner requirement of 2026-10-30 that patch management (X15), host backups (X14), image updates (X16), image composer (X17), fresh-server bootstrap (X18), vulnerability and AI response (X13), Kubernetes/provisioning and backup copies can each be enabled and disabled.

## Problem

Each optional feature had its own switch in the configuration file (`manager.patch.enabled`, `imageUpdate`, `imageComposer`, `serverBootstrap`, `experimentalProvisioning`, presence of `imageBuild` or `vuln`) or none at all (host sources, backup copies). A change needed a file edit and a manager restart on every replica, the UI could not tell which pages made sense, and "off" answered differently per feature (401, 503, 404).

## Decision

| Question | Chosen mechanism | Alternative and reason |
| --- | --- | --- |
| Registry | Package `internal/modules`: a static catalog (id, DE/EN title and description, required modules, owned API routes) and a `Registry` that answers `Enabled(id)`. | Plain config flags: need a restart, no dependency checks, no UI. |
| State | One document per module in the replicated metastore (`modules/<id>`: enabled, who, when). The configuration file only supplies the default of a module that was never switched (`managerConfig.moduleDefaults`); a stored state always wins. Existing files keep working unchanged. | Config as the only truth: cannot be changed at runtime. |
| Effective state | `Enabled` = switched on, configuration prerequisites present, every required module effective. It fails closed: a stored "on" whose prerequisite vanished does not run. Prerequisites are facts of the process (agent transport, `imageBuild`, `vuln` section, reviewed agent release) and are listed as `missingConfig`. | Trusting the stored flag: would run a feature without its credentials or transport. |
| Runtime | Services are built on every leadership whenever their configuration allows it; the registry decides whether they act. API routes of a disabled module answer `404 module_disabled` before any handler or backend runs (a single check in the endpoint wrapper, routes mapped by `modules.ForRoute`); pollers (patch, image update, vulnerability feeds) skip while off. No manager restart, no mounting and unmounting of handlers. | Mount/unmount handlers: more state to keep consistent across leader changes. Build tags or separate binaries: cannot be switched at runtime, multiply the release artifacts and test matrix, and a module such as backup copies is entangled with core backups, so a compile-time split would force interfaces nobody else needs. |
| Control | `GET /modules` (any session; the UI needs it for navigation) and `PUT /modules/{id}` (administrator, audited like every state change). Enabling is refused with `409 module_blocked` when configuration or a required module is missing; disabling when an enabled module needs it. `GET /modules` carries the precise reasons (`missingConfig`, `missingModules`, `dependents`). | Reason text in the error body: error codes stay coarse by design. |
| Disabling | Stops new work and hides the pages. It deletes nothing: records, sealed credentials, plans, history and operation journals stay and reappear when the module is enabled again. Operations already running are not cancelled. | Cancelling work on disable: unsafe for host and cluster operations. |
| Default | Off for a module the file does not enable. Where a feature had no switch before (host backups, backup copies) it defaults to on when `manager.backup` is configured, so an upgrade does not silently remove a working function. | Everything off: breaks existing installations. |

Not modules: core clusters and Swarm management, core backups and restores, monitoring and alerts, the registries, Docker engine update plans, auth and settings. The image build service is a module because the composer needs it.

## Known limits

- An operation that is already running when its module is switched off continues to its end; only new work is refused. The UI does not show running operations on the module page.
- The Kubernetes module covers the metadata-only registration and the k3s plan preview of SSH provisioning; SSH execution of Kubernetes stays disabled as before.
- The `PUT` is not forwarded between replicas beyond the existing leader forwarding; a non-leader answers `not_leader` like any other write.
- The routes `/settings/*`, `/clusters/*` and the registries are not mapped to a module; the backup-copy routes are.
- Verified with fakes and an in-process single-node manager; no multi-replica or restart test of a switched state beyond the registry unit test.
