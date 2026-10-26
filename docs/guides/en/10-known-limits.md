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
- **Database access:** the manager does not deploy proxies. E8 manually ran redundant proxies and a VIP on VMs; their original local policy selected different writers. The coordinated writer choice is now implemented with fakes only and needs an E8 rerun. It blocks a switch while any registered proxy is unreachable. No node TLS or proxy-to-node TLS; client-facing TLS exists only in the proxy, without certificate issuance.

- **Manager HA:** three manager replicas ran as plain processes on lab VMs (E4 B2), including leader kills; the three-service Swarm layout, deny-list distribution and new HA operations have only static/fake evidence. Key rotation/re-encryption and membership transitions have fake evidence only; membership endpoints are 503 in the default runtime. Peer certificate renewal, expiry-based deny-list compaction and mTLS forwarding are implemented with fake-only evidence. Real binlog replay is not done. Every replica needs a local Swarm-manager Docker socket. `deploy/stack.yaml` remains single-instance; `deploy/stack-ha.yaml` is a statically checked, unrun three-service layout.
- **Packaging:** the manager image and single-instance stack ran on arm64 lab VMs (E7); the job image, three-service HA stack, Compose and release workflow remain unverified.
- **Operations:** host maintenance, drift detection and cluster deletion exist [Fakes]; the product never reboots or drains a host and drift only proposes manual actions. Approval-gated scale-out/in, rolling restarts and same-series patch upgrades exist through the cluster API [Fakes]; see [the runbook](11-operator-runbook.md#7-scale-and-roll-a-galera-cluster). No spare-host workflow, cross-series upgrade path, migration, phpMyAdmin, or real PITR (archive/planning and fake replay exist). Joiners always use SST.
- **Backups:** no verified full restore of production data; restore into a new cluster, logical backup, retention and the scheduler were not run on VMs; manager-config backup is a library with no scheduler or API. Format 2 manifests use keyed digests, older ones plaintext digests. Restore verification does not compare table contents.
- **Monitoring:** the full-outage event and notification exist only with fakes; many default alert series have no real collector; notification channels untested against real receivers.
- **Authorization:** outage-recovery approval needs the `recover` permission (administrator preset only) [Fakes]. Real hardware authenticators, Safari/Firefox and screen readers untested.
- **Deletion:** cluster deletion keeps host directories of local volumes and backend backups by design, and ran only against fakes.
- **Resilience:** resource exhaustion and capacity denial, restart during cleanup, VM power loss and graceful host drain were not tested.
- **Lab hazard:** Lima exposes guest listeners (including unauthenticated lab harness ports) on host loopback.

## Rules that will not change

No automatic promotion of cross-cluster copies; no node replacement or bootstrap that bypasses quorum or the recovery rules; major database upgrades and full-cluster recovery require explicit approval.
