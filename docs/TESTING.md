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

ARM64 runs can provide native local evidence. AMD64 emulation, if used, must be identified as emulation and cannot substantiate native performance. A strategy for native AMD64 evidence remains open.

## Cleanup and retained evidence

Provide a cleanup preview and scoped teardown based on the ownership manifest. Remove owned VMs/disks, containers, networks, volumes, backup copies and temporary caches. Do not use global Docker prune or delete all Colima/Lima instances.

Retain sanitized test commands, versions, results and resource measurements in the repository. Test data and secrets are disposable. Verify removal and reclaimed disk space, and compare unrelated resource inventories before/after. Teardown must be repeatable after interrupted runs.

No VM has been created and no infrastructure test has run yet.
