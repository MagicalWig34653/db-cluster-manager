# ADR 0017: Docker Engine update plans and operator runbooks

Status: **Proposal**, 2026-10-25. X09/W07 safe default, fakes only. The update
boundary questions in [ENTSCHEIDUNGEN.md](../ENTSCHEIDUNGEN.md#open-decisions-from-the-scope-extension)
remain open. This ADR does not select an unattended upgrade or restart policy.

## Confirmed requirements

[X09](../KONZEPT.md#scope-extension) asks for Docker Engine updates on managed
hosts. [A08](../KONZEPT.md#placement-automation-and-host-protection) requires guided maintenance with Galera
safety checks. [ADR 0014](0014-manager-container-docker-access.md) records a
manager task pinned to a Swarm manager host; draining it can interrupt the UI.
The R6 [release gates](../RELEASES.md#r6--service-type-framework-generic-containers-and-docker-engine-updates)
remain open.

## Proposed mechanism implemented with fakes

`GET /hosts/{id}/engine-update-plan` reads the inventoried engine version,
queries an injected `EngineVersionSource` for installed and available exact
versions, requires its installed version to match Swarm inventory and
checks the target against the embedded compatibility matrix. The matrix's
`29.*` entry comes from evidence for only `29.9.1`, so export is limited to
that exact recommended version; unknown, untested and cross-major targets are
blocked. The shipping manager has no package-version
source configured, so it returns a blocked plan. A source must know the host's
installation method and offer versions actually obtainable from that method;
the current fake proves the interface without claiming package availability.

The planner checks fresh (at most five minutes old) active/ready host state, a majority of Swarm manager nodes
outside a manager target, no other host in maintenance in that Swarm and the
existing Galera maintenance preview. It marks every Swarm manager as potentially
pinned because the current host inventory cannot identify the exact manager
service placement. The operator must enter guided host maintenance and wait for
`ready` before export. This retains Galera's existing quorum, Synced and
identity checks; the planner does not implement an alternate path through them.

`POST /hosts/{id}/engine-update-runbook` requires the separate `engine_update`
`operate` permission, a freshly recomputed plan digest and an explicit flag
for a manager host. Only the administrator preset includes the new resource;
custom grants are possible. The route is audited. The exported shell script
**prints** instructions, including manual Swarm drain, package update, verify,
active and maintenance exit steps. No manager, agent, Docker or package-manager
execution path is connected to it. The exact package command remains an
operator choice because supported installation methods are undecided. The
digest changes with host and maintenance inventory, version-source output,
compatibility result and target; a stale digest is refused at export time.
There is no durable approval record or update job.

Downgrade is not promised. The runbook advises keeping a failed node drained,
checking Swarm/Galera and using guided recovery; it never bootstraps Galera.
The frontend has a DE/EN page and a development-only invented mock, not a
validated real-host flow.

## Alternatives and open product decisions

| Option | Benefit | Cost and decision needed |
| --- | --- | --- |
| Printable operator runbook (current proposal) | No unattended changes while boundaries are open | Manual package method, restart, drain and rollback; needs version-source integration |
| Manager executes approved engine commands | One UI workflow | Requires exact package methods, privileges, fencing, loss-of-contact recovery and an owner decision on approval scope |
| Agent executes the upgrade | Host-local package access | Agent privilege increase, safe agent restart/resume and verified package adapters required |
| External fleet tool | Existing patch infrastructure | Ownership, progress and post-update evidence integration required |

The owner still needs to decide who may restart hosts, which actions can be
automatic, whether OS packages beyond Docker are in scope and which installation
methods are supported. A real version source and an exact pinned-manager
placement signal are prerequisites before production export can be enabled.

## Evidence and limits

Go tests use an injected fake version source and in-memory host/cluster state.
They cover permission denial, unknown compatibility, Swarm manager quorum,
maintenance readiness, stale digest, manager confirmation and absence of an
upgrade command in the export. API tests cover route/schema matching and audit.
Frontend tests cover the export guard and download filename. No host, Docker
daemon, package repository or real infrastructure was contacted. Actual check
results are recorded in [STATUS.md](../STATUS.md#x09-docker-engine-update-plans-2026-10-25-fakes-and-mock-only).
