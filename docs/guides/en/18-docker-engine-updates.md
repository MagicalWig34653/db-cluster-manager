# Docker Engine update plans

> **Execution path (owner decision 2026-10-30).** This page only previews and exports a runbook. The Docker engine is actually updated by [patch management](23-patch-management.md) as the report-only `container-runtime` package class, behind an engine guard (drained host or acknowledged tasks, manager quorum, Galera coordination, post-update health check). Image updates ([guide 25](25-image-updates.md)) never touch the engine. A daemon restart affects all containers on the host.

The **Docker Engine updates** page provides a read-only plan for one Swarm host.
Choose a host and a target version, then review compatibility, blockers, the
Galera maintenance plan, operator steps and rollback limits. The plan's digest
is the value used for runbook export. A target not present in the configured
version source or not tested in the compatibility matrix is blocked.

The shipping manager currently has **no package-version source configured**.
It reports a blocker and cannot export a runbook. The development mock shows
invented versions and an exportable sample; it is not evidence of real package
availability or of an update. A later version source must match the host's
actual installation method.

When an exportable version source is configured, use this order:

1. Check the version, Swarm manager quorum, affected Galera nodes and rollback
   material. Only one host in a Swarm may be in maintenance for this plan.
2. Open **Hosts**, enter guided maintenance for the target and wait until it
   reports **ready**. The manager stops affected Galera nodes after its safety
   checks; it does not drain or reboot the host.
3. Refresh the update plan. For a Swarm manager host, confirm that a separate
   manager endpoint remains available and explicitly acknowledge the pinned
   manager warning. Approve the current digest to download the runbook.
4. Run the downloaded shell script locally to **print** the procedure. Perform
   its Docker drain and exact package installation steps manually using the
   host's approved package method. Verify Swarm quorum, Docker version, node
   readiness and Galera health before returning the node to active availability.
5. End guided host maintenance in **Hosts** and verify every affected Galera
   node is Synced. Keep a failed node drained and use guided recovery when
   necessary; do not bootstrap Galera manually.

The manager never executes the upgrade, drain, reboot or rollback. Package
downgrades can be unsupported. A change to inventory or maintenance invalidates
the previous digest. Update boundaries remain an owner decision in
[ADR 0019](../../decisions/0019-docker-engine-update-plans.md).
