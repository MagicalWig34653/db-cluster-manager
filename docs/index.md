---
title: cluster-manager
layout: landing
---

<section class="hero" markdown="1">

![Application icon: three database nodes connected as a cluster](assets/icon/logo-256.png){: .logo}

# cluster-manager

<p class="lead">A self-hosted, AGPLv3 manager for MariaDB Galera clusters on existing Docker Swarm clusters, with risky actions shown as plans that wait for your approval.</p>

<div class="badges" markdown="1">
<span class="status-badge">Early implementation · no release yet</span>
[![Build](https://github.com/MagicalWig34653/db-cluster-manager/actions/workflows/build.yml/badge.svg)](https://github.com/MagicalWig34653/db-cluster-manager/actions/workflows/build.yml)
[![Docs](https://github.com/MagicalWig34653/db-cluster-manager/actions/workflows/docs-check.yml/badge.svg)](https://github.com/MagicalWig34653/db-cluster-manager/actions/workflows/docs-check.yml)
</div>

<div class="cta" markdown="1">
[User guides (English)](guides/en/index.md)
[Benutzerhandbuch (Deutsch)](guides/de/index.md)
[Current status](STATUS.md)
[Release policy](RELEASING.md)
</div>

Keyboard navigation, focus and narrow-screen behavior are described in the [UI design note](DESIGN.md#static-accessibility-and-narrow-screen-review-2026-10-26) and the bilingual getting-started guides. This review is static; no new browser or screen-reader result is claimed.

<div class="shot" markdown="1">
![Cluster detail page of the web UI with node table and state](assets/screenshots/real/real-cluster-detail.png)

*Real screenshot: the UI against a real manager on lab VMs (E13, 2026-10-30, English). Cluster detail, operations, hosts and the wizard are real captures; all other screenshots are from the development mock server (invented sample data) and the German ones are mock.*
</div>

</section>

**Nothing has run on a production Swarm and the first Swarm trial has not started.** The manager, agents and MariaDB have run together only on dedicated lab VMs (arm64); many features are verified only with fakes. Each feature below carries its verification level, taken from [STATUS.md](STATUS.md).

## Features and verification

<div class="grid" markdown="1">

<div class="card" markdown="1">
### Galera on Swarm <span class="tag lab">Lab VMs</span>

The manager creates a 3-node MariaDB Galera cluster through its API, using agents it deploys as Swarm services (E4).
</div>

<div class="card" markdown="1">
### Authenticated agents <span class="tag lab">Lab VMs</span>

Agents enroll with one-time tokens and talk mTLS 1.3 to the manager; revocation and deny-list distribution remain <span class="tag fakes">Fakes</span> only.
</div>

<div class="card" markdown="1">
### Health and node outages <span class="tag lab">Lab VMs</span>

Node loss detected within seconds on the lab. Outage events and notifications exist; their delivery is <span class="tag fakes">Fakes</span>.
</div>

<div class="card" markdown="1">
### Full-outage recovery <span class="tag lab">Lab VMs</span>

Read-only diagnosis of all nodes, optional position recovery, then a guided bootstrap that runs only after a digest-bound approval (E6). The approval needs its own `recover` permission (administrators only) and an outage raises a notification: <span class="tag fakes">Fakes</span>.
</div>

<div class="card" markdown="1">
### Backups and inspection restore <span class="tag lab">Lab VMs</span>

Physical backup to SFTP and an isolated inspection restore with approval (E4). Logical backups, schedules, retention and keyed manifest digests: <span class="tag fakes">Fakes</span>.
</div>

<div class="card" markdown="1">
### SQL proxy <span class="tag lab">Lab VMs</span>

Writer selection, read routing and TLS termination in front of the real cluster as a lab container (E5). Deployment by the manager: <span class="tag planned">Planned</span>.
</div>

<div class="card" markdown="1">
### Cluster deletion, maintenance, drift <span class="tag fakes">Fakes</span>

Deletion as an approved plan, guided host maintenance that never reboots or drains a host, and drift detection that only proposes manual actions.
</div>

<div class="card" markdown="1">
### Replicated manager <span class="tag fakes">Fakes</span>

Raft metastore with peer mTLS and a per-replica data keyring. Three manager processes survived leader kills on lab VMs (E4 B2). Follower API forwarding, automatic peer certificate renewal and deny-list compaction have in-process evidence; the three-service Swarm stack has static checks only.
</div>

<div class="card" markdown="1">
### Web UI, German and English <span class="tag mock">Mock only</span>

SvelteKit UI with light and dark mode. Checked in a browser only against the development mock server, never against a real manager.
</div>

<div class="card" markdown="1">
### Beyond Galera <span class="tag planned">Planned</span>

Kubernetes, web and storage servers, Traefik, image builds and Docker engine updates are proposals ([ADR 0012](decisions/0012-broadened-scope-platform.md)). Cluster provisioning has experimental Swarm plan and SSH code plus a flag-gated Kubernetes script preview, tested only with fakes ([guide](guides/en/19-provisioning.md), [ADR 0018](decisions/0018-ssh-cluster-provisioning.md)); no host was contacted. AI-assisted vulnerability response (advisory feeds, patched rebuilds or mitigations, selectable AI providers, notify-only by default, admin approval before any change) is a proposal too ([ADR 0015](decisions/0015-ai-assisted-vulnerability-response.md)); its backend groundwork exists and is tested with fakes only ([guide](guides/en/15-vulnerability-response.md)).
</div>

</div>

Labels: **Lab VMs** ran with the real manager, agents and MariaDB 11.4 on dedicated Lima VMs, a few times; **Fakes** means unit tests against fakes only; **Mock only** means the UI against invented data; **Planned** is not implemented.

## A look at the UI

All screenshots come from the development mock server with invented sample data (the yellow banner says so). They show the interface, not verified behaviour.

<div class="gallery" markdown="1">

<div markdown="1">
![Operations list with an approval waiting and step progress](assets/screenshots/real/real-operations.png)

Operations: approval bound to the plan digest, step progress.
</div>

<div markdown="1">
![Full-outage diagnosis with a planned recovery waiting for approval](assets/screenshots/en-light-outage-recovery.png)

Full-outage diagnosis and a recovery plan waiting for approval.
</div>

<div markdown="1">
![Cluster creation wizard](assets/screenshots/real/real-wizard.png)

Cluster wizard: basics, placement, resources, preflight, review.
</div>

<div markdown="1">
![Backups page with schedule and inspection restore](assets/screenshots/en-light-backups.png)

Backups: start, schedule and inspection restore.
</div>

<div markdown="1">
![Cluster detail in dark mode](assets/screenshots/en-dark-cluster-detail.png)

Dark mode.
</div>

<div markdown="1">
![Hosts page in German](assets/screenshots/de-light-hosts.png)

German interface: hosts and agent enrollment.
</div>

<div markdown="1">
![Galera operations card with a stored plan and its approval digest (mock capture)](assets/screenshots/en-light-cluster-operation-plan-mock.png)

Galera scale, rolling restart and patch-upgrade planner: the plan is stored first, approval needs the displayed digest (mock capture).
</div>

<div markdown="1">
![Data-key rotation preview (mock capture)](assets/screenshots/de-light-key-rotation-mock.png)

German interface: data-key rotation preview and explicit confirmation (mock capture). Metastore membership and proxy writer switch work the same way.
</div>

</div>

## User guides

- [User guides (English)](guides/en/index.md): installation, hosts and agents, clusters, health and alerts, backup and restore, node replacement, full-outage recovery, security model, troubleshooting, known limits, operator runbook, [configuration reference](guides/en/12-configuration-reference.md).
- [Benutzerhandbuch (Deutsch)](guides/de/index.md): dieselben Themen auf Deutsch, einschließlich der [Konfigurationsreferenz](guides/de/12-configuration-reference.md).
- Operator CLI: [English](guides/en/20-cli.md) · [Deutsch](guides/de/20-cli.md) (fakes only; existing session cookie required).
- OS patch management (agent-based reports, approval-gated patching, Galera guard): [English](guides/en/23-patch-management.md) · [Deutsch](guides/de/23-patch-management.md) ([ADR 0023](decisions/0023-os-patch-management.md); fakes plus a two-VM lab run; no UI).
- Agent runtimes and capabilities (native vs container agent, routing table, restic on the native agent): [English](guides/en/24-agent-runtimes.md) · [Deutsch](guides/de/24-agent-runtimes.md) ([ADR 0024](decisions/0024-agent-runtimes.md); fakes only; UI badge only).
- Image updates of managed services (Watchtower-style: policies, tag rules, health gate, rollback): [English](guides/en/25-image-updates.md) · [Deutsch](guides/de/25-image-updates.md) ([ADR 0025](decisions/0025-image-update-service.md); fakes only; no UI).
- Image composer (assemble a service image from components: distribution, nginx, PHP with extensions and settings; reviewed Dockerfile preview, variants per distribution): [English](guides/en/26-image-composer.md) · [Deutsch](guides/de/26-image-composer.md) ([ADR 0026](decisions/0026-image-composer.md); fakes and mock server only; catalog unverified, no image built yet).
- Fresh-server bootstrap (onboard a new Linux server: OS check, preflight, hardening, Docker from the pinned repository, agents, Swarm, restic and patch capabilities, host-key pinning, separate lockdown approval): [English](guides/en/27-fresh-server-bootstrap.md) · [Deutsch](guides/de/27-fresh-server-bootstrap.md) ([ADR 0027](decisions/0027-fresh-server-bootstrap.md); simulated host only, nothing ran against a real host).
- Optional modules (switch patch management, host backups, image build/composer/updates, vulnerability response, bootstrap, provisioning, Kubernetes and backup copies at runtime; data is never deleted): [English](guides/en/28-modules.md) · [Deutsch](guides/de/28-modules.md) ([ADR 0030](decisions/0030-optional-modules.md); fakes only).
- Docker Engine update plans: [English guide](guides/en/18-docker-engine-updates.md) · [Deutsche Anleitung](guides/de/18-docker-engine-updates.md) (fakes and mock only; no package source in the shipping manager).

## Project documentation

<div class="docs-list" markdown="1">

- [Current status](STATUS.md)
- [Confirmed product requirements](KONZEPT.md)
- [Architecture and feasibility questions](ARCHITEKTUR.md)
- [UI and user experience](DESIGN.md)
- [Decisions and remaining questions](ENTSCHEIDUNGEN.md)
- [Proposed release plan and acceptance gates](RELEASES.md)
- [Local test environment](TESTING.md)
- [Development, documentation and Claude Code handover](ENTWICKLUNG.md)
- [Claude Code bootstrap](handoffs/CLAUDE-START.md)
- [Execution workplan](WORKPLAN.md)
- [Installation and packaging](INSTALL.md)
- [Go and monorepo decision](decisions/0002-go-monorepo.md)
- [Decision records](decisions/0001-product-direction.md)
- [Broadened scope decision](decisions/0012-broadened-scope-platform.md)
- [Replica keys, peer TLS and revocation](decisions/0013-replica-keys-peer-tls-and-revocation.md)
- [Manager container Docker access](decisions/0014-manager-container-docker-access.md)
- [AI-assisted vulnerability response (proposed)](decisions/0015-ai-assisted-vulnerability-response.md)
- [Technical references](QUELLEN.md)

</div>

Requirements reflect the user's answers through 2026-10-18 and the scope extension of 2026-10-22 ([scope extension requirements](KONZEPT.md#scope-extension)). Release assignments and implementation details remain proposals unless identified as confirmed. Technical documentation is English; the user guides exist in German and English.
