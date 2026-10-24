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

The `cmd/lab` tests use a fake runner only. `lab apply --apply` and `lab cleanup --apply` have not been executed against a real host.

## Covered and open experiments

Run on 2026-10-19 on dedicated `dbcm-lab-` Lima VMs (arm64): E1 Swarm with a sentinel and three pinned Galera services, E2 host loss, returning host, empty-volume guard and minority partition; on 2026-10-20 E3 three-`metad` failover, fencing, restart and majority loss (scripts `tests/p0/`, results `tests/p0/results/`). On 2026-10-23 E4 ran the real manager and mTLS agents creating a 3-node Galera cluster through the API, with a node kill, a physical backup to SFTP and an inspection restore ([results](../tests/p0/results/e4-manager-galera.md)). Also on 2026-10-23 E5 put `cmd/proxy` in front of that cluster (writer selection, failover, hysteresis, drain, TLS termination) and E6 ran a clean and a crash full outage with diagnosis, position recovery and the approval-gated guided recovery (`tests/p0/e5-e6-proxy-outage.sh`, [results](../tests/p0/results/e5-e6-proxy-outage.md)). On 2026-10-24 E7 deployed `deploy/stack.yaml` with a `manager init` configuration from an image built with `deploy/Dockerfile` ([results](../tests/p0/results/e7-stack-deploy.md)). Open: a multi-replica manager as Swarm services, redundant proxies and VIP, backup/restore and PITR, standalone migration, resource exhaustion and capacity denial, restart during cleanup, native AMD64, graceful reboot/drain, and the real agent.

Host free disk showed a drop of about 1 GiB and of about 3.4 GB across lab cycles that was not shown to be reclaimed; do not treat teardown as proof of reclaimed space until measured with a controlled method.
