---
title: Known limits
---

# Known limits

[Guide index](index.md) | Previous: [Troubleshooting](09-troubleshooting.md) | Next: [Operator runbook](11-operator-runbook.md)

Taken from [STATUS.md](../../STATUS.md) of 2026-10-23. The R1 gates in [RELEASES.md](../../RELEASES.md) are **not** met, and the P0 exit gates are not met either. This is not production software and there has been no Swarm trial.

## Evidence base

Real-component evidence exists only from Lima VMs on one Apple-silicon host (arm64, Docker 29.9.2, MariaDB 11.4.13): E1-E6. Most features were exercised once. Everything else was tested only with fakes. Timings from E4 were measured under foreign host load.

## Not proven or missing

- **Platforms:** amd64 has no evidence. Only Docker 29.x is in the compatibility matrix. Kubernetes is out of scope for R1.
- **Manager HA:** a multi-replica manager has not run on VMs. The metastore transport has no TLS, the data key must be copied to replicas by hand, and the agent deny list is not distributed. `deploy/stack.yaml` is not an HA deployment.
- **Packaging:** the container image and the job image were never built by the project; the release workflow never ran; the managed-mode stack and compose files (one replica pinned to a Swarm manager node, Docker socket mounted) were only checked statically and never deployed.
- **Database access:** the manager does not deploy proxies; no redundant proxies, no VIP, no node TLS or proxy-to-node TLS. Client-facing TLS exists only in the proxy, without certificate issuance (no ACME).
- **Operations:** no drift handling, maintenance mode, spare-host workflow, update workflow, migration, phpMyAdmin, or PITR. Joiners always use SST.
- **Backups:** no verified full restore of production data; restore into a new cluster, logical backup and the scheduler were not run on VMs; manager-config backup is a library with no scheduler or API. Manifests carry plaintext chunk digests. Restore verification does not compare table contents.
- **Monitoring:** no event or notification for a full outage; many default alert series have no real collector; notification channels untested against real receivers.
- **Authorization:** outage-recovery approval uses the restore permission. Real hardware authenticators, Safari/Firefox and screen readers untested.
- **Deletion:** cluster deletion keeps host directories of local volumes and backend backups by design, and ran only against fakes.
- **Resilience:** resource exhaustion and capacity denial, restart during cleanup, VM power loss and graceful host drain were not tested.
- **Lab hazard:** Lima exposes guest listeners (including unauthenticated lab harness ports) on host loopback.

## Rules that will not change

No automatic promotion of cross-cluster copies; no node replacement or bootstrap that bypasses quorum or the recovery rules; major database upgrades and full-cluster recovery require explicit approval.
