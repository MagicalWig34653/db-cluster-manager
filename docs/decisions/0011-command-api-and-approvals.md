# ADR 0011: Command API, digest-bound approvals and leader-only backup scheduling

Date: 2026-10-23
Status: Proposed. Implemented in `internal/api` (control.go, control_handlers.go), `internal/manager` (control.go, approvals.go, backups.go, checker.go, notifysettings.go, statussource.go) and `cmd/manager` (managed.go). Tested with `httptest`, fakes and a single in-memory Raft replica; nothing ran against a real Swarm, agent, backup tool or MariaDB.

## Context

The second UI batch assumed endpoints for hosts, Swarm connection, cluster creation and node replacement, the full-outage diagnosis, operation control, backups, inspection restores and notification settings (docs/DESIGN.md). The manager had the commands (`Operations`, `Swarms`, `internal/dbbackup`) but no HTTP handlers, no approval mechanism for restore steps, no scheduler for backup policies and no construction of the monitoring service in `cmd/manager`.

## Decision

1. **Three command services behind the API.** `api.Control`, `api.Backups` and `api.Settings` are interfaces implemented by `internal/manager`. Every endpoint declares its permission; the API resolves the target resource (reading the body where the body decides the scope: `POST /clusters`, `POST /restores/inspect`), authorizes and audits before the service runs. The manager services check again with their own Authorizer (defence in depth), so a command produces two audit records. Error mapping is centralised (`manager.MapCommandError` to API sentinels to coarse codes); error text never leaves the server.
2. **Visibility on denial.** New object endpoints answer 403 when the caller may read the object but not perform the action, and 404 when the caller may not read it (`hideUnreadable`), so state-changing endpoints neither leak existence nor hide a plain lack of permission from a reader.
3. **Approvals bound to the definition digest.** A restore operation is planned with an approval ID; every destructive step carries `CondApproved(approval ID)` (dbbackup). The operation record stores the ID and `DefinitionDigest`, the SHA-256 of the stored operation definition. `POST /operations/{id}/approve` needs the restore permission on the cluster's backups (not in the operator preset), must quote the digest and is audited. The routing checker accepts the approval only while the digest recomputed from the stored definition equals the recorded one. The operation pauses before its first destructive step until approved and is resumed by the approval.
4. **One routing checker.** `manager.RoutingChecker` evaluates approvals from the replicated store, dbbackup gate conditions through the configured backend (unmet without one) and every other condition through the live agent probe (unmet without agents). Anything it cannot evaluate fails closed.
5. **Leader-only scheduling as the manager subject.** `manager.BackupScheduler` (a reconciler) evaluates policies with `dbbackup.SchedulePolicy`, advances the due time with compare-and-set before starting a run (two leaders in a row cannot start the same run twice) and starts backups as `collect.SystemSubject`. `cmd/manager` binds that subject to the `manager_system` role when the leader API starts; alert evaluation uses the same subject. Expired inspection instances are aborted and then torn down by the same loop.
6. **Notification secrets.** Webhook secrets and SMTP passwords set through the API are sealed with the data key in `notify-secrets`; the replicated settings record only that one is stored. `GET` reports `secretSet`/`passwordSet`; secrets are write-only.

## Alternatives

- **Let the services authorize alone (one audit record).** Rejected: the API's deny-by-default table is the single place that guarantees every route has a permission; a service that forgets a check would be open.
- **Approval by flag on the operation (no digest).** Rejected: an approval must cover exactly the plan that was shown; a later change of the stored definition must invalidate it.
- **Approval as a separate pre-plan step (plan only after approval).** Possible; rejected because the digest then covers a preview, not the executable definition, and a second planning pass could differ.
- **Scheduler on every replica with leader check per run.** Rejected: reconcilers already run only under a leadership with fenced agent access.

## Consequences and limits

- Inspection restores need an explicit host: free disk space is not observed, so automatic host selection answers `no_capacity`.
- The inspection init secret is created in the Swarm and not removed by the teardown (the Swarm connector has no secret removal yet).
- Restore verification is not wired; "last verified restore" stays unknown and inspection operations pause at their verification gate.
- The manager does not know the proxy's writer, so backup source selection cannot avoid it.
- The UI inbox read flag is shared by all users of the leader's inbox.
- Dashboards and metric queries keep their documented contract; the UI's assumed shapes for them still differ and need a UI change.
