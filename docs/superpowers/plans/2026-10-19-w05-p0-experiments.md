# W05 P0 Experiments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Confirm or refute the assumptions of ADR 0004 (pinned per-node Swarm services, storage-identity marker, replacement, returning host) with reproducible experiments on dedicated lab VMs, and record the evidence.

**Architecture:** Each experiment is a script under `tests/p0/` (POSIX shell, explicit `docker --host`/`limactl shell` targets, no ambient Docker context) that creates its own `dbcm-lab-` VMs through `bin/lab apply`, runs, writes sanitized results to `tests/p0/results/<experiment>.md`, and always ends with `bin/lab cleanup -apply` plus an unrelated-resource comparison.

**Tech Stack:** Lima VMs created by `bin/lab`, Docker Swarm inside the VMs, MariaDB official image with Galera, shell.

## Global Constraints

- Budget (docs/TESTING.md): combined at most 16 GB RAM, 50% of host CPU capacity (7 of 14 logical CPUs on this host), about 100 GB disk, including images and caches.
- Existing Colima profile `default`, Lima VMs `thermoctl-*`, Docker contexts and unrelated workloads must stay untouched. VMs are created and removed only via `bin/lab`. No global prune, no host restart, no production credentials, synthetic data and short-lived test credentials only.
- A sentinel service (plain `nginx` or `busybox httpd`, replicas 1) is deployed in each Swarm before the experiment; its container ID, image digest and response must be identical after the experiment.
- Never claim a result that was not observed. Record the exact commands, versions and outputs (sanitized: no private paths, no tokens). Failures and surprises are results.
- AMD64 is not tested natively (host is arm64); say so.
- Commit messages without attribution lines. English.

---

### Task 1: Swarm on three lab VMs, sentinel, pinned Galera services

**Files:** Create `tests/p0/common.sh` (helpers: `vm_exec NAME CMD...` via `limactl shell`, wait-for-condition with deadline, teardown trap), `tests/p0/e1-swarm-galera.sh`, `tests/p0/results/e1-swarm-galera.md`.

- [ ] **Step 1:** `bin/lab apply -apply -vms 3 -cpus 2 -ram 4 -disk 20`; record `limactl list`, guest `nproc`/memory, Docker version, host free disk before and after.
- [ ] **Step 2:** `docker swarm init` on m1 (advertise the VM's reachable address, found with `limactl shell`), join m2 and m3 as workers (one manager only is acceptable for this experiment; state that), record `docker node ls`. Verify VM-to-VM connectivity first; if Lima's default user-mode network does not allow it, document and use the documented Lima mechanism for a shared network (read the Lima docs; do not modify host networking).
- [ ] **Step 3:** Deploy the sentinel service; record its task ID, container ID and image digest.
- [ ] **Step 4:** Pull a MariaDB image (record exact tag and digest; arm64). Create three services `galera-1..3`, each replicas=1, `--constraint node.id==<id>`, local bind-mounted data directory at `/var/lib/dbcm/galera-N` on its host, with an identity marker file written by a one-shot container before start. Bootstrap galera-1 with `--wsrep-new-cluster`, join the others. Record the Galera options used.
- [ ] **Step 5:** Verify `wsrep_cluster_size = 3`, `wsrep_cluster_status = Primary`, write on node 1 and read the row on nodes 2 and 3 (synthetic table), record outputs.
- [ ] **Step 6:** Kill the container of galera-2 (`docker kill`): record that Swarm restarts it on the same host and the node rejoins (IST/SST observed in logs), cluster size returns to 3. Record time to recovery.
- [ ] **Step 7:** Compare sentinel container ID/digest/response with step 3. Run `bin/lab cleanup -apply`, verify the unrelated comparison output, record host free disk.
- [ ] **Step 8:** Write results (what worked, what did not, deviations from ADR 0004 assumptions, open items) to `tests/p0/results/e1-swarm-galera.md`. Commit scripts and results.

---

### Task 2: Host loss, returning host and partition

**Files:** Create `tests/p0/e2-host-loss.sh`, `tests/p0/results/e2-host-loss.md`.

- [ ] **Step 1:** Recreate the E1 setup using the E1 scripts (functions in `common.sh`), including a cluster with three Galera services and the sentinel.
- [ ] **Step 2 (host loss):** `limactl stop --force` the VM hosting galera-3 (only a `dbcm-lab-` VM). Record: Swarm node state (`Down`), whether the pinned service stays pending and is NOT rescheduled elsewhere, cluster size 2 and writes still possible on the two-node majority.
- [ ] **Step 3 (returning host):** start the VM again, record the task restart, marker verification outcome (the identity marker file must still match), rejoin via IST or SST, cluster size 3, data consistent (row counts and a checksum query on all nodes).
- [ ] **Step 4 (empty volume trap):** stop galera-3's VM, delete its data directory contents but leave the marker file absent, start it: record what the stock service does (it will start with an empty volume). Then demonstrate the ADR 0004 guard as a script: a pre-start check that refuses to start when the marker is missing on a non-empty Swarm/cluster identity; record that the plain Swarm service does NOT protect against this by itself.
- [ ] **Step 5 (partition):** isolate m3 from m1/m2 using `iptables` rules inside the VM (only in lab VMs, removed afterwards): record that the isolated node goes non-Primary and refuses writes, the majority keeps serving; heal and record rejoin.
- [ ] **Step 6:** Sentinel unchanged; scoped teardown and unrelated comparison; write results and commit.

---

### Task 3: Evidence into the ADRs and status

**Files:** Modify `docs/decisions/0004-swarm-ownership-and-local-storage-identity.md`, `docs/TESTING.md`.

- [ ] **Step 1:** Add an "Evidence (lab VMs)" section to ADR 0004 summarising E1/E2 per assumption: confirmed, refuted or untested, with links to the result files; update the status line.
- [ ] **Step 2:** In docs/TESTING.md list the experiments now covered and those still open (three-manager coordination on VMs, proxy/VIP, backups, migration, resource exhaustion, native AMD64).
- [ ] **Step 3:** Run `python3 scripts/check_docs.py`; commit.
