# Technical references

Initial research: 2026-10-18. These references support constraints and possibilities, not validation of our proposed architecture. Recheck relevant version-specific documentation during implementation.

- [MariaDB: quorum and bootstrap](https://mariadb.com/docs/galera-cluster/high-availability/resetting-the-quorum-cluster-bootstrap) — recovery and `safe_to_bootstrap`.
- [MariaDB: deployment variants](https://mariadb.com/docs/galera-cluster/galera-architecture/galera-cluster-deployment-variants/) — typical three/five-node topologies, not a five-node maximum claim.
- [MariaDB: minor-version rolling upgrades](https://mariadb.com/kb/en/upgrading-between-minor-versions-with-galera-cluster/) — supported upgrade-path planning.
- [MariaDB Backup](https://mariadb.com/docs/server/server-usage/backup-and-restore/mariadb-backup) — backup implementation research starting point.
- [Docker Engine security](https://docs.docker.com/engine/security/) — Docker daemon trust boundary.
- [Protect the Docker socket](https://docs.docker.com/engine/security/protect-access/) — access protection and authorization.
- [Kubernetes Services](https://kubernetes.io/docs/concepts/services-networking/service/) — external backends; retained as future reference, not the initial deployment architecture.
- [Claude Code project memory](https://code.claude.com/docs/en/memory) — durable project instructions.
- [GitHub Pages and Jekyll](https://docs.github.com/en/pages/setting-up-a-github-pages-site-with-jekyll/about-github-pages-and-jekyll) — Markdown documentation publishing.

Additional primary-source research is needed for Swarm local-volume placement, manager consensus, proxy/VIP behavior, passkey recovery, periodic reserves, filtered replication and PITR before implementation decisions.
