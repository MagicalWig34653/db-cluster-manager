# Local test strategy

## Confirmed environment and limits

User machine: M4 Pro Mac, 48 GB physical RAM. Colima and dedicated Lima VMs are available as the intended test approach; installation and existing environments have not been inspected.

Combined budget for all project test workloads:

- At most 16 GB RAM.
- At most 50% of host CPU capacity; detect the actual core count before assigning vCPUs. Do not assume an M4 Pro core count. vCPU allocation alone may not guarantee a strict utilization cap.
- Approximately 100 GB SSD, including VM disks, images, database data, backups, metrics and caches.

All resources must be selectively removable. Existing Colima profiles, Lima VMs, Docker contexts and unrelated workloads must remain untouched. No tests on the user's existing Swarm until the first working version is ready.

## Proposed lab layout

Use dedicated named profiles/VMs and an explicit ownership manifest. An initial experiment could use three VMs at 4 GB RAM each, leaving 4 GB within the project budget for auxiliary workload overhead. Actual allocations need measurement; this is not a guaranteed sufficient topology.

Run more demanding backup, HA, spare-node and cross-cluster experiments in separate scenarios. Do not infer support from a topology that omits quorum or replica requirements just to fit the budget. Each VM disk allocation must fit within an aggregate budget that leaves room for host-side artifacts and image caches; thin provisioning is not a capacity guarantee.

Set explicit Docker contexts in commands. Keep a project inventory of VM/profile names, owned resource IDs, directories and test-only ports. Use synthetic data and short-lived test credentials.

## Required experiments

- Agent deployment and service ownership; scheduler/manager disagreement.
- Galera local-volume identity, node loss, host return, delayed messages and partition.
- Three-manager coordination, failover and operation fencing without shared storage.
- Proxy/endpoint failure and node-specific latency measurement; VIP feasibility.
- Backup/restore integrity, isolated backup inspection, PITR as introduced.
- Standalone migration with retained address/port and rejected incompatible sources.
- Restart during cleanup, resource exhaustion and capacity denial.
- Unrelated sentinel containers retain configuration/data and continue serving.

ARM64 runs can provide native local evidence. AMD64 emulation, if used, must be identified as emulation and cannot substantiate native performance. A strategy for native AMD64 evidence remains open. Candidates, none decided: an amd64 host or cloud VM that the owner provides, for the multi-VM experiments, or GitHub-hosted amd64 runners for single-host functional checks such as image builds, agent and manager start, and a single-node MariaDB backup/restore.

Start lab experiments only when the foreign host load is low. Record `uptime` per phase. On 2026-10-23 a planned E4 re-run did not start because the 1-minute load stayed at 30-970 for about 4.5 h ([record](../tests/p0/results/e4-rerun-not-run.md)).

## Cleanup and retained evidence

Provide a cleanup preview and scoped teardown based on the ownership manifest. Remove owned VMs/disks, containers, networks, volumes, backup copies and temporary caches. Do not use global Docker prune or delete all Colima/Lima instances.

Retain sanitized test commands, versions, results and resource measurements in the repository. Test data and secrets are disposable. Verify removal and reclaimed disk space, and compare unrelated resource inventories before/after. Teardown must be repeatable after interrupted runs.

## Lab commands

`make build` produces `bin/lab` (source `cmd/lab`). All commands read the ownership manifest at `.lab/manifest.json` (override with `-manifest PATH`; `.lab/` is git-ignored). VM names are `dbcm-lab-m1` … `dbcm-lab-mN`.

- `bin/lab status` — read-only: host capacity (logical CPUs from the Go runtime, RAM from `sysctl -n hw.memsize`), the lab budget (CPU limit = half the logical CPUs, 16 GiB RAM, 100 GiB disk), the manifest entries, the Lima/Colima inventory (`limactl list --json`, `colima list --json`) and the unrelated entries that lab never touches.
- `bin/lab plan [-vms 3] [-cpus 2] [-ram 4] [-disk 20] [-network lima:user-v2]` — dry run: checks the request plus VMs already in the manifest against the budget and prints the `limactl create`/`limactl start` commands. RAM and disk are GiB per VM. `-network lima:user-v2` (the only accepted value) adds `--network=lima:user-v2` to `limactl create` for VM-to-VM connectivity; the shared user-v2 network is not a manifest resource and is never removed by cleanup. Fails (exit 1) when the budget is exceeded or the host capacity cannot be detected.
- `bin/lab apply [same flags]` — without `--apply`, identical to `plan`. With `--apply` it refuses to run when host capacity or the pre-snapshot cannot be determined; otherwise it prints and records the pre-snapshot (`.lab/pre-apply-snapshot.json` next to the manifest), prints and executes the actions (recording each created VM in the manifest), then prints the post-snapshot diff. Exit 1 if an action failed or the diff contains anything other than additions owned by the manifest.
- `bin/lab cleanup` — preview of `limactl stop --force`, `limactl delete --force` for manifest VMs and `rm -rf -- .lab/<name>` for manifest directories. Only names matching `dbcm-lab-[a-z0-9-]` are considered.
- `bin/lab cleanup --apply` — refuses to run without detected host capacity and a pre-snapshot; prints the pre-snapshot, the unrelated entries, the actions and the post-snapshot diff. Exit 1 if an action failed (a failed delete keeps the VM in the manifest) or if any pre-existing unrelated entry is missing afterwards.

The stop uses `--force` because a graceful `limactl stop` fails on an instance that is not running, which would otherwise abort cleanup before the delete. Exit codes: 0 success, 1 failure, 2 usage error.

`tests/p0/e12-recovery-autofailover-rolling.sh` (E12: three VMs with the HA stack; autoFailover with a hard-stopped writer VM, stack redeploy and per-replica rolling update, peer certificate renewal, full power loss with zero-UUID incident, manual recovery and staggered loss; phases selectable with `E12_PHASES`, VMs kept with `E12_KEEP_VMS=1`, `E9_MIN_FREE_GIB` host disk guard) writes `tests/p0/results/e12-recovery-autofailover-rolling.{log,md}`; the log is the concatenation of several stages on kept VMs. About 2.5 hours with retries.

`tests/p0/rc-demo.sh up|info|forward|down` (release-candidate demo: E12 phases up to the HA stack, stops before the first administrator and prints URL and setup token; **never executed**) and `scripts/rc-package.sh VERSION` (local dry run of the release workflow's archive, SBOM, checksum and release-notes steps; host-only, ran 2026-10-30). See [E13 not-run record](../tests/p0/results/e13-not-run.md).

`tests/p0/e15-pitr.sh` (E15, point-in-time recovery, [ADR 0031](decisions/0031-point-in-time-recovery.md): three VMs, a Galera cluster with `binlog: true`, the cross-built `cmd/dbcm-job` run with `docker run` for the physical backup, the binlog archive, the physical restore and the binlog replay, `cmd/pitrlab` for the library calls the script must not copy; guards: 1-minute host load below 40 and at least 40 GiB free disk; `E15_KEEP_VMS=1` keeps the VMs) writes `tests/p0/results/e15-pitr.{log,md}`. It does not start the manager, the agent or the UI.

E18 (2026-10-31): `tests/p0/e18-agent-blips.sh` (builds the E4 layout and runs backups under per-VM samplers; `E18_KEEP_VMS=1` keeps the VMs; [results](../tests/p0/results/e18-agent-blips.md)) and `tests/p0/e18-zero-uuid.sh` (needs the kept VMs, `E4_REUSE_VMS=1`; phase A autoRecover off, phase B on; phase B needs more than an hour because of the autoRecover cooldown and a larger passkey counter bump than the script uses; [results](../tests/p0/results/e18-zero-uuid.md)).

E17 (2026-10-31, [bootstrap](../tests/p0/results/e17-bootstrap.md), [composer](../tests/p0/results/e17-composer.md), [patch](../tests/p0/results/e17-patch.md), [restic](../tests/p0/results/e17-restic.md), [modules](../tests/p0/results/e17-modules.md)) has no single script: it was driven by the host-side harnesses `cmd/bootlab` (fresh-server bootstrap service against a lab VM's forwarded SSH port on loopback, with lab-own credentials; `-run` executes one command for verification), `cmd/composelab` (`audit` prints every package spec of the catalog per base release, `gen` renders a recipe; the specs were simulated in the real base images inside a lab VM) and the real `bin/manager`/`agent` with `tests/p0/e4client`. The recipes are `tests/p0/e17/*.json`, the raw package audit `tests/p0/results/e17/pkgaudit-*.tsv`. The VMs were `bin/lab apply -templates debian-12,almalinux-9` and `bin/lab cleanup --apply`.

`tests/p0/e11-restic.sh` (restic lab, two Debian 12 VMs, pinned downloads from GitHub, MinIO cross-built with `go install` for linux/arm64) writes `tests/p0/results/e11-restic.{log,md}`; the in-VM harness is `cmd/resticlab`, the external-host case is the env-gated test `TestE11ExternalHostSSHPlanReal` in `internal/manager`. It needs outbound network access and about 10 minutes.

The `cmd/lab` tests use a fake runner only. `lab apply --apply` and `lab cleanup --apply` have not been executed against a real host.

## Covered and open experiments

Run on 2026-10-19 on dedicated `dbcm-lab-` Lima VMs (arm64): E1 Swarm with a sentinel and three pinned Galera services, E2 host loss, returning host, empty-volume guard and minority partition; on 2026-10-20 E3 three-`metad` failover, fencing, restart and majority loss (scripts `tests/p0/`, results `tests/p0/results/`). On 2026-10-23 E4 ran the real manager and mTLS agents creating a 3-node Galera cluster through the API, with a node kill, a physical backup to SFTP and an inspection restore ([results](../tests/p0/results/e4-manager-galera.md)). Also on 2026-10-23 E5 put `cmd/proxy` in front of that cluster (writer selection, failover, hysteresis, drain, TLS termination) and E6 ran a clean and a crash full outage with diagnosis, position recovery and the approval-gated guided recovery (`tests/p0/e5-e6-proxy-outage.sh`, [results](../tests/p0/results/e5-e6-proxy-outage.md)). On 2026-10-24 E7 deployed `deploy/stack.yaml` with a `manager init` configuration from an image built with `deploy/Dockerfile` ([results](../tests/p0/results/e7-stack-deploy.md)). On 2026-10-24/25 E8 ran two proxies with a keepalived VIP and client-side failover (proxy kill and VM hard-stop under load, no acknowledged write lost, 2.4-7.2 s write gaps) and VM power loss of one and of all nodes (no automatic bootstrap; Galera self-recovered) ([results](../tests/p0/results/e8-proxy-redundancy-powerloss.md)). On 2026-10-26 E9 ran `deploy/stack-ha.yaml` (three manager services, leader election, forwarding, peer mTLS and wrong-certificate refusal, leader kill, drain), the ADR 0017 coordinated writer with two proxies (agreement, controlled switch, blocked switch, manager outage, 100 ms sampling) and the B2/B3 power-loss diagnosis ([results](../tests/p0/results/e9-ha-proxy-writer.md); `tests/p0/e9-ha-proxy-writer.sh`, analysis `tests/p0/e9-analyze.py`; 11 stages on kept VMs; B3 and the guided recovery after a VM power loss not obtained). Open: `autoFailover` on VMs, backup/restore and PITR, standalone migration, resource exhaustion and capacity denial, restart during cleanup, native AMD64, graceful reboot/drain, and the real agent.

Host free disk showed a drop of about 1 GiB and of about 3.4 GB across lab cycles that was not shown to be reclaimed; do not treat teardown as proof of reclaimed space until measured with a controlled method.

## Native amd64 P0 evidence plan

Prepared 2026-10-24; **not executed**. The owner decision between an owner-provided amd64 host and a cloud VM remains open. Native GitHub runner build/test results, cross-compilation and QEMU image builds do not close the multi-node P0 gate. This task uses fakes/unit tests only and contacts no infrastructure.

1. After the owner selects the environment, record `uname -m`, CPU capacity, OS/kernel, Go/Node/Docker/Buildx versions and image/platform digests. Require native x86_64; label any emulation explicitly. Produce a budget and ownership inventory before provisioning; preserve the combined 16 GiB RAM, 50% detected host CPU and approximately 100 GiB disk limits, or obtain an explicit revised budget.
2. Build both images and binaries on that host. Verify the selected amd64 manifests, tool presence, SBOM/provenance contents and manager/agent/job startup. Use synthetic data and disposable credentials; no production Swarm or backups.
3. Adapt the existing E1–E7 lab harnesses only after reviewing their Lima/arm64 assumptions. Rerun the relevant multi-node gates: Galera create/replication, node loss/return and identity guard, minority isolation, three-manager fencing/failover/restart, physical and logical backup with verified isolated restore, proxy failover, approval-gated full outage and stack deployment. Record resource measurements, unrelated sentinel integrity, cleanup interruption and remaining proxy/VIP/power-loss gaps separately. Do not claim untouched gates passed.
4. Retain sanitized commands, commit and image digests, logs, assertions, timings and resource measurements in `tests/p0/results/`; compare owned/unrelated inventories before and after scoped teardown and measure reclaimed disk. Update the compatibility claims and release gates only from that evidence.

The native CI matrix in [build.yml](../.github/workflows/build.yml) checks Go (including `-race`), web and both native image builds on each architecture without push. The tag-only [release workflow](../.github/workflows/release.yml) gates publication on this matrix and requests SBOM/provenance for both multi-platform images. Neither workflow runs P0 infrastructure experiments. Run `python3 scripts/check_deploy.py` for static workflow guards and `python3 scripts/test_check_deploy.py` for their regression checks; these do not execute Actions or Docker.

The build workflow also offers a `security_checks` checkbox for a manual
`workflow_dispatch`. It starts separate `govulncheck` and `gosec` jobs with
read-only repository permissions and the existing SHA-pinned setup actions.
The Go tool versions are fixed in the workflow. These jobs are optional and
have not been executed in this worktree; an offline static check does not
verify tool download, findings or compatibility with the current Go version.
