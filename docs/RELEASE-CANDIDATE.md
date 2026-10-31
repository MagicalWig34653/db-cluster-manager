---
title: Release candidate (R1 slice) - state, limits and owner quickstart
---

# Release candidate (R1 slice)

Draft of 2026-10-30. **This is not a release.** No tag, image or release was created or pushed; the owner decides. The R1 exit gates in [RELEASES.md](RELEASES.md) are not met, and the release-candidate lab run (E13) first could not start (host resources, [e13-not-run.md](../tests/p0/results/e13-not-run.md)) and later ran only partly ([e13-rc-lab.md](../tests/p0/results/e13-rc-lab.md)). The honest state: the components were verified earlier on lab VMs and with fakes; the **assembled** candidate was walked through once (E16) by someone who knew the code, in a real browser with real backup data; an independent first-time reader has still not tried it.

## What was run for this candidate

| Item | Result | Evidence level |
| --- | --- | --- |
| Local release packaging (`scripts/rc-package.sh 0.1.0-rc1`): linux amd64/arm64 archives of manager/agent/cli, six CycloneDX SBOMs, SHA256SUMS, release notes | Ran; checksums verified | Host-only. Archives are bsdtar, not the workflow's reproducible GNU tar; SBOMs from a throwaway repo with a local tag |
| Container images (`deploy/Dockerfile`, `Dockerfile.jobs`) | Not built in this cycle | Earlier arm64 manager image ran in E7 only |
| `tests/p0/rc-demo.sh up` | Ran: three HA replicas up, token printed, forward works | Lab VMs (arm64); not the manual INSTALL.md path |
| Real-browser UI end-to-end (Playwright, virtual passkey): setup, login, connect, enroll, deploy agents, create Galera, rolling restart with approval; 360 px and tab order | Ran; three bugs fixed | Lab VMs. Real screenshots in `docs/assets/screenshots/real/`; the guides still use mock screenshots |
| Backup, inspection restore, scheduled verification, retention and copy with S3 (MinIO, TLS) and local target; failure cases | Ran (E14), five fixes | Lab VMs; [e14-backup-restore.md](../tests/p0/results/e14-backup-restore.md) |
| PITR through the real manager and UI (E16): binlog cluster, backup, archive, coverage check, approval, restore into an inspection instance, row/checksum comparison with the state at T | Ran, matched | Lab VMs; [e16-install-walkthrough.md](../tests/p0/results/e16-install-walkthrough.md) |
| INSTALL.md walkthrough on fresh VMs by a reader who had seen the code (E16): 14 gaps corrected in INSTALL, guides (EN, DE), stack file and `init` output | Ran | Lab VMs; not an independent novice |
| German UI: 11 pages checked, real German screenshots in the German guides | Ran | Lab VMs; three UI defects fixed |
| Archive schedule in the backup policy | Fakes and unit tests only | Not run on VMs |
| Executing a restore of a copy into a new cluster | **Not run** | The copy restore ends at the proposal |

## What is verified on VMs (earlier cycles, arm64, Docker 29.9.2, MariaDB 11.4.13)

E1/E2 Swarm Galera and host loss; E4 real manager and mTLS agents creating a three-node Galera cluster, node kill, physical backup to SFTP and an inspection restore; E7 single-instance stack from the manager image; E9 three-replica HA stack with election, forwarding, peer mTLS, leader kill, drain, and redundant proxy with coordinated writer; E10 patching; E11 restic and MinIO; E12 autoFailover, rolling redeploy, certificate renewal, full power loss with the zero-UUID incident and the manual recovery. Results are in `tests/p0/results/`.

## What exists only on fakes

Guided zero-UUID recovery automation (ADR 0029), Docker engine patch class, native manager TLS/ACME, restore into a new target through the UI, PITR, backup copies between targets, scheduled restore verification, scale/rolling operation approval through the UI against a real manager, host maintenance, cluster deletion, notifications to real receivers. The UI has been driven only against the mock server and in-process tests.

## Known limits and risks

- arm64 only; amd64 has no native evidence. Only Docker 29.x is in the compatibility matrix.
- Not production software. Full list: [known limits](guides/en/10-known-limits.md).
- Plain HTTP on the manager; passkeys need an https origin (or `http://localhost`) in front. The three-replica HA stack needs peer CA custody on the manager nodes and manual data-key recipient registration before the second and third replica can lead.
- The manager mounts the Docker socket (root-equivalent on its node).
- Lima exposes guest listeners on host loopback (lab hazard only).
- Host resource finding: the one development machine used for all lab evidence is frequently overloaded (load above 500, disk at 99 %, swap exhausted on 2026-10-30), so timing-sensitive results need a cleaner host.
- No signed artifacts: the release and cosign workflow has never run.

## Owner quickstart on a separate test Swarm

Use a dedicated test Swarm and an explicit Docker context. Never your production Swarm.

1. Build artifacts: `make build`, then `scripts/rc-package.sh 0.1.0-rc1 dist/rc` to get archives, SBOMs and checksums locally (nothing is published).
2. Build the two images on each node or one build host and `docker save | docker load` them to all nodes ([INSTALL.md](INSTALL.md) step 1).
3. Follow [INSTALL.md](INSTALL.md): single instance for a first look, or the three-replica HA stack. Create the administrator with the setup token from the service log.
4. Alternatively, on a machine with enough resources run `tests/p0/rc-demo.sh up` (three Lima VMs, about 12 GiB RAM, at least 15 GiB free disk), then `forward` and open `http://localhost:8080`; `down` removes only `dbcm-lab-` VMs. Untested.
5. Register the Swarm, enroll hosts ([guide 2](guides/en/02-hosts-and-agents.md)), create a three-node Galera cluster, and run a backup to a local or S3 target with a restore into a new target before trusting anything.
6. Teardown: [INSTALL.md](INSTALL.md) "Teardown and uninstall".

Report every deviation from these steps; they are the walkthrough gaps that this cycle could not collect.
