# Galera Cluster Manager

An AGPLv3, self-hosted manager for clusters and services, starting with MariaDB Galera on existing Docker Swarm clusters. Working title; the name no longer fits the broadened scope and is under review. Early implementation; no release exists.

Scope (2026-10-22, [ADR 0012](docs/decisions/0012-broadened-scope-platform.md)): Galera on Swarm is priority 1 and the first release target. Planned later, as proposals: Kubernetes and provisioning of new Swarm/Kubernetes clusters, web servers with detailed composition (for example Nginx + PHP with chosen extensions), storage protocol servers (SFTP, FTP, S3-compatible, NFS), generic containers, Traefik management, image builds with a managed or external registry, and Docker engine updates — operable through the UI with every action shown as a reviewable plan.

Claude Code: read [the bootstrap handover](docs/handoffs/CLAUDE-START.md) and [execution workplan](docs/WORKPLAN.md). Git initialization and public publication by Claude are authorized.

Backend: Go for manager, agent and CLI. Monorepo with separate SvelteKit frontend source, distributed with the manager.

Start with the [documentation](docs/index.md), [current status](docs/STATUS.md), and [release plan](docs/RELEASES.md).

The repository is the source of truth; sessions are disposable. Technical documentation and code are English. The UI and user guides will support German and English.

The user deploys the manager; it deploys agents and database services. Swarm comes first, Kubernetes later. Local storage and replicated managers are required. Initial validation uses isolated Colima/Lima environments, not the user's existing Swarm.

License: `AGPL-3.0-only` (SPDX), see [LICENSE](LICENSE).
