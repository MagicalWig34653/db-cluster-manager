---
title: Known limits
---

# Known limits

[Guide index](index.md) | Previous: [Troubleshooting](09-troubleshooting.md) | Next: [Operator runbook](11-operator-runbook.md)

Baseline limits were taken from [STATUS.md](../../STATUS.md) on 2026-10-23; the E9 power-loss finding below is newer. The R1 gates in [RELEASES.md](../../RELEASES.md) and the P0 exit gates are **not** met. This is not production software; see STATUS for later lab evidence.

## Evidence base

Real-component evidence exists from E1-E9 on Lima VMs on one Apple-silicon host (arm64, Docker 29.9.2, MariaDB 11.4.13); [E9 results](../../../tests/p0/results/e9-ha-proxy-writer.md) include the zero-UUID finding. Most features were exercised once. The new incident and 409 contract have fake-test evidence only.

## Not proven or missing

- **Platforms:** amd64 has no evidence. Only Docker 29.x is in the compatibility matrix. Kubernetes is out of scope for R1.
- **Manager HA:** three manager replicas ran as plain processes on lab VMs (E4 B2), including leader kills; the three-service Swarm layout, deny-list distribution and new HA operations have only static/fake evidence. Key rotation/re-encryption and membership transitions have fake evidence only; membership endpoints are 503 in the default runtime. Peer certificate renewal, expiry-based deny-list compaction and mTLS forwarding are implemented with fake-only evidence. Real binlog replay is not done. Every replica needs a local Swarm-manager Docker socket. `deploy/stack.yaml` remains single-instance; `deploy/stack-ha.yaml` is a statically checked, unrun three-service layout.
- **Packaging:** the manager image and single-instance stack ran on arm64 lab VMs (E7); the job image, three-service HA stack, Compose and release workflow remain unverified.
- **Database access:** the manager does not deploy proxies. E8 manually ran redundant proxies and a VIP on VMs; their original local policy selected different writers. The coordinated writer choice is now implemented with fakes only and needs an E8 rerun. It blocks a switch while any registered proxy is unreachable. No node TLS or proxy-to-node TLS; client-facing TLS exists only in the proxy, without certificate issuance.
- **Operations:** host maintenance, drift detection and cluster deletion exist [Fakes]; the product never reboots or drains a host and drift only proposes manual actions. Approval-gated scale-out/in, rolling restarts and same-series patch upgrades exist through the cluster API [Fakes]; see [the runbook](11-operator-runbook.md#7-scale-and-roll-a-galera-cluster). No spare-host workflow, cross-series upgrade path, migration, phpMyAdmin, or real PITR (archive/planning and fake replay exist). Joiners always use SST.
- **Backups:** no verified full restore of production data; restore into a new cluster, logical backup, retention and the scheduler were not run on VMs; the manager's own metastore backup, backup copies between targets, scheduled restore verification and client-side encrypted targets exist with fakes only ([guide 21](21-disaster-recovery.md)); the metastore import was never run on a real manager, restoring from a copy through the API is not wired, transfer records verify only with the keyring of the installation that wrote them, and scheduled verification needs an explicit inspection host. Format 2 manifests use keyed digests, older ones plaintext digests. Restore verification does not compare table contents.
- **Monitoring:** the full-outage event and notification exist only with fakes; many default alert series have no real collector; notification channels untested against real receivers.
- **Authorization:** outage-recovery approval needs the `recover` permission (administrator preset only) [Fakes]. Real hardware authenticators, Safari/Firefox and screen readers untested.
- **Deletion:** cluster deletion keeps host directories of local volumes and backend backups by design, and ran only against fakes.
- **Resilience:** resource exhaustion and capacity denial, restart during cleanup and graceful host drain were not tested. E8/E9 exercised VM power loss, but recovery from the E9 state remains open.
- **Full power loss:** E9 reproduced a two-node Primary component with a zero cluster UUID and a third node crash-looping during SST/IST. E12 verified the critical incident and the 409 reason on the real cluster [Lab VMs]; the manual bootstrap procedure worked in three runs and B3 reproduces the zero UUID. The guided zero-UUID recovery with a mandatory snapshot and the opt-in `autoRecover` policy (ADR 0029) replaces that manual path but ran only against fakes; it compares no table contents and its snapshot is a same-volume copy, not a backup. See [full-outage recovery](07-full-outage-recovery.md#primary-component-with-a-zero-state-uuid-e9).
- **Lab hazard:** Lima exposes guest listeners (including unauthenticated lab harness ports) on host loopback.

## Rules that will not change

No automatic promotion of cross-cluster copies; no node replacement or bootstrap that bypasses quorum or the recovery rules; major database upgrades and full-cluster recovery require explicit approval.
