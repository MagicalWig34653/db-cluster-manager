# Galera Cluster Manager

An AGPLv3, self-hosted database manager for existing Docker Swarm clusters. Working title; currently a concept, with no application implemented.

Claude Code: read [the bootstrap handover](docs/handoffs/CLAUDE-START.md) and [execution workplan](docs/WORKPLAN.md). Git initialization and public publication by Claude are authorized.

Backend: Go for manager, agent and CLI. Monorepo with separate SvelteKit frontend source, distributed with the manager.

Start with the [documentation](docs/index.md), [current status](docs/STATUS.md), and [release plan](docs/RELEASES.md).

The repository is the source of truth; sessions are disposable. Technical documentation and code are English. The UI and user guides will support German and English.

The user deploys the manager; it deploys agents and database services. Swarm comes first, Kubernetes later. Local storage and replicated managers are required. Initial validation uses isolated Colima/Lima environments, not the user's existing Swarm.

License: `AGPL-3.0-only` (SPDX), see [LICENSE](LICENSE).
