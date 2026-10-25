<p align="center">
  <img src="docs/assets/icon/logo-256.png" alt="Application icon: three database nodes connected as a cluster" width="128" height="128">
</p>

# cluster-manager

An AGPLv3, self-hosted manager for clusters and services, starting with MariaDB Galera on existing Docker Swarm clusters. The name is **cluster-manager** for now (owner decision 2026-10-25); the repository and folder stay `db-cluster-manager`. **Early implementation; no release exists.**

[![Build](https://github.com/MagicalWig34653/db-cluster-manager/actions/workflows/build.yml/badge.svg)](https://github.com/MagicalWig34653/db-cluster-manager/actions/workflows/build.yml)
[![Docs](https://github.com/MagicalWig34653/db-cluster-manager/actions/workflows/docs-check.yml/badge.svg)](https://github.com/MagicalWig34653/db-cluster-manager/actions/workflows/docs-check.yml)
[Documentation site](https://magicalwig34653.github.io/db-cluster-manager/)

![Cluster detail page of the web UI](docs/assets/screenshots/en-light-cluster-detail.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data). The UI has not been run against a real manager yet.*

## Features and verification status

Nothing has run on a production Swarm; the first Swarm trial has not started. Levels: **Lab VMs** = ran with the real manager, agents and MariaDB 11.4 on dedicated Lima VMs (arm64), a few times; **Fakes** = unit tests against fakes only; **Mock only** = UI against invented data; **Planned** = not implemented. Details: [current status](docs/STATUS.md). Setup and options: [user guides](docs/guides/en/index.md) and the [configuration reference](docs/guides/en/12-configuration-reference.md) ([Deutsch](docs/guides/de/12-configuration-reference.md)).

| Feature | Level |
| --- | --- |
| Create a 3-node Galera cluster through the manager API with manager-deployed agents | Lab VMs (E4) |
| Agent enrollment and mTLS 1.3 transport | Lab VMs (E4); revocation, deny-list distribution to agents and revoking manager certificates: Fakes |
| Health collection and node-outage detection | Lab VMs (E4); node-outage and full-outage events and notifications: Fakes |
| Full-outage diagnosis, position recovery and approved guided recovery | Lab VMs (E6); separate `recover` permission and outage notification: Fakes |
| Physical backup to SFTP and inspection restore with digest-bound approval | Lab VMs (E4) |
| Logical backups, schedules, retention and keyed (HMAC) manifest digests | Fakes |
| SQL proxy: writer selection, read routing, TLS termination | Lab VMs (E5, lab container); deployment by the manager: Planned |
| Cluster deletion as an approved plan, host maintenance, drift detection | Fakes |
| Replicated manager (Raft metastore, peer mTLS with `-issue-peer-certs`, wrapped data keyring with `-print-replica-key`) | Fakes; no multi-replica run on VMs |
| Web UI in German and English, light and dark; pages for services, image builds, registries, host maintenance, drift, replica keyring, logical backup and retention | Mock only |
| Kubernetes, provisioning, web/storage servers, Traefik, image builds, engine updates | Planned ([ADR 0012](docs/decisions/0012-broadened-scope-platform.md)) |
| AI-assisted vulnerability response (advisory feeds, patched rebuilds or mitigations, selectable AI providers, approval by default) | Wired and UI built, fakes and mock only; proposed ([ADR 0015](docs/decisions/0015-ai-assisted-vulnerability-response.md)) |

## Screenshots

All from the development mock server with invented data; they show the interface, not verified behaviour.

| Operations with approval and progress | Full-outage recovery plan |
| --- | --- |
| ![Operations list](docs/assets/screenshots/en-light-operations.png) | ![Full-outage diagnosis and recovery plan](docs/assets/screenshots/en-light-outage-recovery.png) |
| **Cluster wizard** | **Dark mode** |
| ![Cluster creation wizard](docs/assets/screenshots/en-light-create-wizard.png) | ![Cluster detail in dark mode](docs/assets/screenshots/en-dark-cluster-detail.png) |

| **Services (earlier mock capture: container request, plan, approval)** | **Image composer with Dockerfile and plan** |
| --- | --- |
| ![Services page](docs/assets/screenshots/en-light-services.png) | ![Image composer](docs/assets/screenshots/en-light-images.png) |
| Extended service forms, health and updates: [EN guide](docs/guides/en/13-services.md), [DE-Anleitung](docs/guides/de/13-services.md). New mock captures pending: sandbox blocks local ports and Chromium. | [DE/EN mock capture script](scripts/screenshots/services.mjs) (browser flow not executed) |
| **Host maintenance plan** | **Drift check** |
| ![Host maintenance plan](docs/assets/screenshots/en-light-host-maintenance.png) | ![Drift check](docs/assets/screenshots/en-light-drift.png) |
| **Replicas and keyring** | **Logical backup and retention** |
| ![Replicas and keyring](docs/assets/screenshots/en-light-keyring.png) | ![Backup schedule form](docs/assets/screenshots/en-light-backup-policy.png) |

| **Vulnerabilities: findings** | **Finding detail with proposal and plan** |
| --- | --- |
| ![Vulnerability findings](docs/assets/screenshots/en-light-vulnerabilities.png) | ![Finding detail with a proposal](docs/assets/screenshots/en-light-vulnerability-detail.png) |
| **Policy with the auto-approval warning** | **AI providers and the consent step** |
| ![Vulnerability policy](docs/assets/screenshots/en-light-vulnerability-policy.png) | ![AI providers and consent](docs/assets/screenshots/en-light-vulnerability-providers.png) |

More views, also in German, are in [docs/assets/screenshots](docs/assets/screenshots/) and in the user guides ([English](docs/guides/en/index.md), [Deutsch](docs/guides/de/index.md)).

## Scope

Scope (2026-10-22, [ADR 0012](docs/decisions/0012-broadened-scope-platform.md)): Galera on Swarm is priority 1 and the first release target. Planned later, as proposals: Kubernetes and provisioning of new Swarm/Kubernetes clusters, web servers with detailed composition (for example Nginx + PHP with chosen extensions), storage protocol servers (SFTP, FTP, S3-compatible, NFS), generic containers, Traefik management, image builds with a managed or external registry, and Docker engine updates — operable through the UI with every action shown as a reviewable plan.

The user deploys the manager; it deploys agents and database services. Swarm comes first, Kubernetes later. Local storage and replicated managers are required. Initial validation uses isolated Colima/Lima environments, not the user's existing Swarm.

## Development

Backend: Go for manager, agent and CLI. Monorepo with separate SvelteKit frontend source, distributed with the manager. The application icon is an Icon Composer document in [assets/icon](assets/icon/AppIcon.icon/icon.json); see [docs/DESIGN.md](docs/DESIGN.md#icon-and-screenshots) for how icon files and screenshots are produced.

Start with the [documentation](docs/index.md), [current status](docs/STATUS.md), and [release plan](docs/RELEASES.md). Claude Code: read [the bootstrap handover](docs/handoffs/CLAUDE-START.md) and [execution workplan](docs/WORKPLAN.md). Git initialization and public publication by Claude are authorized.

The repository is the source of truth; sessions are disposable. Technical documentation and code are English. The UI and user guides support German and English.

License: `AGPL-3.0-only` (SPDX), see [LICENSE](LICENSE).
