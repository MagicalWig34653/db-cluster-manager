# ADR 0001: Swarm-first product direction

Date: 2026-10-18
Status: Accepted product direction, based on explicit user answers. Concrete implementation mechanisms remain open.

## Context

The initial sketch suggested external Docker database hosts serving Swarm/Kubernetes clients. The user clarified that databases must run directly on an existing Swarm and that the manager should deploy all supporting agents itself.

## Decision

Use Swarm-native deployment first. The user installs the manager via Compose or a service, inside or outside the target Swarm. Manage multiple independent Swarms; deploy one agent per managed host as a service. Support local storage and manager HA with at least three instances; offer single-instance testing.

Start with MariaDB Galera and small installations. Deliver in stages. Validate prototypes in isolated local Colima/Lima environments before testing the first working version on the user's Swarm. Preserve the full requirement catalog across releases.

The project uses AGPLv3. Code and technical documentation are English; UI and user guides support German and English. The repository is the source of truth, with disposable sessions and a Claude Code handover at concept completion.

## Consequences

Swarm service ownership, local-volume identity and replicated manager metadata are early feasibility gates. External database hosts are not the initial deployment model. Kubernetes and other engines need later platform/engine adapters. Release allocation is proposed separately and is not implicitly accepted by this ADR.

See [requirements](../KONZEPT.md), [open decisions](../ENTSCHEIDUNGEN.md) and [release plan](../RELEASES.md).
