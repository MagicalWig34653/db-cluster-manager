# E3: Three-manager metadata failover on lab VMs

Date: 2026-10-20. Script: [`tests/p0/e3-manager-failover.sh`](../e3-manager-failover.sh). Lab-only simulated agent: [`tests/p0/e3agent`](../e3agent/main.go). Shared helpers: [`tests/p0/common.sh`](../common.sh). Full sanitized log of the recorded run: [`e3-manager-failover.log`](e3-manager-failover.log).

All figures come from one complete run from scratch: `tests/p0/e3-manager-failover.sh`, no environment overrides, exit 0, 19:24Z to 19:27Z (about 3 minutes including VM creation and cleanup). Two runs came before it, and their logs were overwritten:

1. A development run with `E3_KEEP_VMS=1` passed every scenario. It revealed a watcher bug (see "Failures and surprises", item 1), which was fixed before the recorded run. Its VMs were removed afterwards with `bin/lab cleanup -apply`.
2. A run from scratch failed in step (b) while the host was heavily loaded by unrelated processes (item 2). The `EXIT` trap ran the scoped cleanup successfully.

After run 3, one line was added to the script: it removes `/var/folders/...` temp paths from the log. That same `sed` was applied by hand to the recorded log.

All results are native arm64 on an Apple Silicon host with `vz` VMs.

Environment:

- Host: 14 logical CPUs, 48 GiB RAM, `go1.27.1 darwin/arm64`, `limactl version 2.2.0`.
- Host load average during the recorded run: 9-14 (1 min) and 24-29 (5 min), recorded at the start of every step.
- VMs: three `dbcm-lab-m1..m3`, each with 2 vCPUs, 2 GiB RAM and 10 GiB disk (6 CPUs, 6 GiB, 30 GiB in total, within the 7 CPU / 16 GiB / 100 GiB budget). Ubuntu 26.04 LTS, kernel 7.0.0-28-generic, iptables v1.8.11 (nf_tables), curl 8.18.0. NTP was synchronized (systemd-timesyncd).
- `metad`: cross-compiled with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath ./cmd/metad`. Static aarch64 ELF, 11.8 MB, sha256 `786d6a5b…7024a`. It uses hashicorp/raft v1.8.0 with library defaults: heartbeat and election timeout 1 s, leader lease 0.5 s, pre-vote enabled, and raft-boltdb on local disk.
- Sentinel: `nginx:1.27-alpine@sha256:65645c7b…2a10` (arm64).

## Setup

- **`bin/lab apply -apply -vms 3 -cpus 2 -ram 2 -disk 10 -network lima:user-v2`.** The new `-network` option adds `--network=lima:user-v2` to `limactl create` and changes nothing else. It accepts only `lima:user-v2`.
  - The VM itself is the only manifest resource. The shared user-v2 network is not recorded and never removed: its daemon and `~/.lima/_networks/user-v2` are shared Lima state, and they existed before the run.
  - The E1/E2 stop / `limactl edit` / start cycle is no longer needed.
  - The user-v2 interface came up as `eth0`: m1 `192.168.104.1`, m2 `.3`, m3 `.4`. VM-to-VM ping worked.
- **Binaries.** `metad`, `e3agent` and three small in-VM helper scripts (state watcher, latency loop, resource sampler) were copied with `limactl copy` and installed as `/usr/local/bin/dbcm-e3-*`.
- **One `metad` per VM** as the transient systemd unit `dbcm-e3-metad`:
  - command line: `dbcm-e3-metad -id mN -bind <user-v2 IP>:7101 -data /var/lib/dbcm-e3/metad -http 127.0.0.1:1810N -peers m1=192.168.104.1:7101,m2=192.168.104.3:7101,m3=192.168.104.4:7101 -bootstrap`
  - unit options: `-p Restart=no --collect`, so systemd never restarts it.
  - The HTTP harness listens on VM loopback only. The script reaches it with `limactl shell … curl`.
- **Controller.** The script plays the manager controller. It reads the leader's term from `/state` and commits `begin`/`start`/`finish`/`adopt` commands with that term. It sends each step to the agent with the same term.
- **Agent.** `e3agent` runs on m3: `internal/fencing.Guard` with a file persister, plus an idempotent step record (one execution per op/step, fsynced). It is invoked through `limactl shell`. The delivery path therefore does not cross the lab network, which models an agent that a stale controller can still reach (the worst case for fencing).
- **Sentinel.** m3 also runs `dbcm-e3-sentinel` (nginx on rootful Docker, enabled with the E1 recipe inside m3 only).
  - m3 is never force-stopped.
  - Before scenarios (b) and (c), the script moves leadership off m3 if needed, by a graceful restart of m3's metad.

## Results

| Scenario | Observed |
| --- | --- |
| Formation | Leader m2 at term 2 after 3 s (2 s polling, includes process start). A follower answered `503 not_leader` with the leader's ID and Raft address. |
| (a) leader process `SIGKILL` mid-operation | `op-a` [s1,s2,s3,hold]: s1 done, `start s2` committed, then the leader m2 was killed before the agent call. New leader m3 at term 3 was **known on a survivor 2183 ms after the kill**. After the barrier and `adopt` (term 3): `s1=succeeded/1 s2=unknown/1 s3=pending/0`. The agent query showed `NOT-EXECUTED`. The new leader retried s2 (attempt 2), executed it and continued with s3. An old-term journal command on the new leader got `409 not_owner`. The agent rejected term 2 (`fencing: stale term: got 2, highest 3`). The restarted m2 converged after 2 s (same leader, term and unfinished journal). |
| (b) leader VM `limactl stop --force` mid-operation | Leadership was first moved off m3, which gave leader m2 at term 4. `op-b`: s1 done, `start s2` committed, **the agent executed s2**, then m2 was powered off before `finish` committed. New leader m1 at term 5 was **known 1593 ms after the stop command was issued** (host clock). After `adopt`: `s2=unknown/1`. The agent query showed `EXECUTED term=4`. The retry (`start s2`, attempt 2) went to the agent, which answered `ALREADY … executed under term=4; not executed again`. The step then finished: `s2=succeeded/2`. The agent rejected term 4. After `limactl start`, m2 kept its address. The transient unit was not running after boot, as expected. metad restarted from the data directory left by the power cut and converged after 3 s. |
| (c) leader VM partitioned (iptables in m1) | Chain `DBCM-E3` inside m1 dropped traffic from and to m2/m3 (`limactl shell` uses vsock: `SSH_CONNECTION=UNKNOWN …`). A 600 s heal timer was armed. The old leader's controller tried `start s2` with term 5 right away: `504 outcome_unknown` ("leadership lost while committing log"). **m1 stopped reporting leader after 572 ms** (failed to contact quorum, stepped down). The majority **elected m3 at term 6 after 2385 ms**. A retry on m1 got `503 not_leader`. The majority adopted `op-c` and ran s2. **The isolated old leader's command (term 5) to the agent was rejected** (`highest 6`). The majority committed 150 more commands. m1's term stayed **5** while partitioned: its log shows `entering candidate state … term=7` and pre-vote requests, but its stored term did not change. After the heal, m1 converged to leader m3 at term 6 within 2 s. The isolated write never committed (`s2=succeeded/1`: attempts 1, run by the majority). |
| (d) all three stopped (SIGTERM) and restarted from their data directories | Terms before the stop: 6/6/6. Read right after the restart: 6/6/6 (none lower). A new leader m1 at term 7 was elected within 2 s. The leader's unfinished operations after the barrier were **identical** to those before the stop (3 ops). |
| (e) majority loss (m1, m2 `limactl stop --force`) | The survivor m3 refused writes: `503 not_leader` 5 s and 10 s after the loss. Local `GET /state` still worked (`leader false, leader_id "", term 7, 3 ops`). `GET /state?barrier=1` got 503. The agent state was readable. Pre-vote kept the survivor's term at 7. |
| Sentinel | Container ID, image digest, start time `2026-10-20T19:24:44.614418746Z`, restart count 0 and response hash were **identical** before and after all scenarios. |

### Timing and resources

Election times are measured from the disruption to the first survivor that knows a new leader. The new leader is detected by an in-VM watcher that polls `/state` every 50 ms.

- Kill and partition instants are VM-clock timestamps taken right before the action.
- The forced stop is timed on the host clock before `limactl stop --force`.
- Survivor timestamps are converted with per-VM clock offsets. The offsets were measured over a persistent `limactl shell` session; the best of 40 samples had a round trip of about 1.3-1.5 ms.
- Offsets were -4 to +56 ms, except m2 right after its forced stop and restart: **+240 ms**. The VM clock is off after a power cut until NTP corrects it.

| Measurement | Value |
| --- | --- |
| Election after leader process kill (a) | 2183 ms |
| Election after leader VM power-off (b) | 1593 ms (from the host issuing the stop) |
| Election after leader partition (c) | 2385 ms; isolated leader stepped down after 572 ms |
| Commit latency, 3 nodes, sequential, loopback HTTP on the leader | 300 commits: p50 1.41 ms, p90 2.29 ms, p99 4.32 ms, max 8.15 ms, 223 commits/s |
| Commit latency, 2 of 3 nodes (one partitioned) | 150 commits: p50 1.38 ms, p99 3.85 ms, max 5.13 ms, 216 commits/s |
| metad idle, 30 s window | 0.5-1.9 % of one vCPU; RSS 9.0-12.1 MiB; 7-8 threads |
| metad during the 300-commit run | RSS 13.1-13.3 MiB. The run took 1.35 s of the 10 s window, which gives 1.4-2.8 % of one vCPU over the window. The leader used 0.28 s CPU for 300 commits. |
| metad after the full restart; survivor without quorum | RSS 11.8-12.5 MiB; 0.1-2.2 % CPU |
| Data directory after all scenarios | 236 KiB per node (`raft.db` 256 KiB file); no snapshot file was written (see Limitations) |
| VM memory | about 0.5 GiB used of 1.9 GiB per VM (OS, Docker, metad) |

The development run on a quiet host measured the same order of magnitude: elections about 1.6-1.8 s, partition step-down 560 ms and comparable RSS. Its 3-node latency was p50 2.8 ms / p99 73 ms, worse than in the recorded run.

## Failures and surprises (all observed)

1. **Watcher bug in the development run.** The first watcher printed an empty `leader_id` as an empty field, and `set --` dropped it. A survivor that had lost the leader ("no leader known") was therefore counted as "new leader known", so some election times in that run were too short. The watcher now prints `id=<value>`. The recorded run's watcher output (in the log) shows the leaderless phase explicitly, for example `false id= 2` before `false id=m3 3`.
2. **The run from scratch failed under unrelated host load.** Unrelated iOS Simulator processes raised the host load average to about 800; the lab did not touch them.
   - `limactl shell` calls took up to 12 s.
   - The 3-node latency degraded to p50 48 ms / p99 192 ms (3-node, 300 commits).
   - metad used 6-15 % CPU at idle.
   - The Raft group changed leader on its own: term 6 by step (b), with only two intended disruptions.
   - The step-(b) `begin` was sent to the leader the script had found 30 s earlier and got `503 not_leader` (leader m2). The script stopped, and cleanup ran.

   With the library defaults (1 s heartbeat and election timeout), the group is sensitive to host or CPU starvation. The product needs either tuned timeouts or a controller that re-resolves the leader and retries on `not_leader`. The recorded run was started once the load average dropped below about 20.
3. **Lima forwards the unauthenticated metad HTTP harness to the host.** While metad ran, the host had `limactl` listeners on `127.0.0.1:18101-18103`: Lima's default port forwarding exposes guest loopback listeners on host loopback. The harness was therefore reachable from the host's loopback, not only inside the VM. It stays loopback-only and lab-only, but this matters for any future agent or manager lab endpoint.
4. **Pre-existing host listener on the sentinel port.** Host port 18080 was already in use before the run: `*:18080`, the ssh mux of the unrelated Colima `default` instance. It had the same PID before and after the run and was not touched. The sentinel was published on guest `127.0.0.1:18080` and checked only from inside m3, so its comparison was unaffected. Lima's forward to the host was not possible for that port.
5. **The isolated leader answered `504 outcome_unknown`, not `503`, for the first write after the partition.** This was predicted by the in-process tests (`ErrOutcomeUnknown`). A controller must treat it as unknown completion. The entry did not commit.
6. **Pre-vote prevented term inflation.** The isolated node logged repeated candidate attempts for term 7, but its stored term stayed at 5. The quorum-less survivor in (e) behaved the same way. After the heal, the majority did not have to step down because of a higher term.
7. **The VM clock was off by about 240 ms after the forced stop and restart.** It did not affect Raft, but it would affect any timestamp-based reasoning (leases, audit timestamps) right after host recovery.
8. **The bbolt/Raft state survived a power cut.** The forced VM stop in (b) came right after commits, and the node restarted from that data and converged. Only one power cut was tested; this is not proof of fsync correctness.

## ADR 0003 claims

| Claim (in-process evidence) | VM result |
| --- | --- |
| Leader lost mid-operation: new leader adopts, interrupted step becomes `unknown`, retried, no unsafe double execution | **Confirmed** for a process kill (a) and a VM power-off (b). In (b) the step had already executed, and the idempotent agent did not execute it again on retry. |
| Partitioned old leader cannot commit; agent rejects its old term after a new term is established | **Confirmed** (c): `504` then `503` on the isolated node, no commit, agent `REJECT` for term 5 after admitting 6 |
| Majority continues during the partition | **Confirmed** (c): 150 commits at p50 1.38 ms |
| Healed old leader converges | **Confirmed** (c): same leader, term and unfinished journal 2 s after the heal |
| Restart from disk: journal and terms restored, terms never decrease | **Confirmed** (d) for a graceful stop of all three, and (a)/(b) for a single node after `SIGKILL` and after a power cut |
| Majority loss: no new operations, reads still work, served databases unaffected | **Confirmed** (e): `503` on writes and barrier, local reads work, sentinel unchanged |
| Resource use within the lab budget | **Measured**: about 9-13 MiB RSS and under 3 % of one vCPU per replica at this load |

## Limitations

- **The controller is the test script, not a manager.** Leader discovery, `adopt` after a barrier, reconcile-then-retry and fencing are script logic, applied as ADR 0003 describes. A product controller does not exist yet.
- **The agent is a lab-only stand-in.** `tests/p0/e3agent` uses the real `internal/fencing.Guard`, but not `internal/agent` or Docker. Its commands travel over `limactl shell`, not over an authenticated network transport.
- **Partition scope.** One partitioned VM was tested. Not tested: asymmetric or partial partitions, packet loss and delay, and a partition of a follower.
- **No Raft snapshot was taken.** About 480 commits stay below the default snapshot threshold of 8192, so the VM restarts replayed the log only. Snapshot restore on VMs remains untested (it is covered only in-process).
- **Not exercised on VMs:**
  - membership change
  - restore from backup (ADR 0003 item 5)
  - TLS or authentication on the Raft and HTTP transports
  - two replicas in one failure domain
  - timeout tuning
  - more than one run per scenario: the election figures are single samples, and their spread was not measured
- **Load sensitivity.** Behavior under host CPU starvation was observed once, by accident (item 2), and not measured.

## Cleanup and unrelated resources

`bin/lab cleanup -apply`, run by the `EXIT` trap, stopped and deleted `dbcm-lab-m1..m3`. Post-snapshot diff: `removed: lima:dbcm-lab-m1, lima:dbcm-lab-m2, lima:dbcm-lab-m3`, `unrelated resources intact (3)`, exit 0. The manifest is empty afterwards. The unrelated list was identical before and after:

```
unrelated (never touched):
  colima:default
  lima:thermoctl-e2e-basestation
  lima:thermoctl-test-vm
```

- `limactl list` after cleanup shows only the two thermoctl VMs, both still `Stopped`.
- The lab's user-v2 usernet daemon exited with the VMs. Only Colima's pre-existing usernet daemon is still running, untouched.
- `~/.lima/_networks/user-v2` still exists, with the same entries before and after the run (`leases.json` and two usernet log files). It existed before the run and is shared Lima state, so it was neither edited nor deleted.
- Host free disk: 91 GiB before apply, 89 GiB with the VMs, 91 GiB after cleanup (`df -h`, 1 GiB resolution).
