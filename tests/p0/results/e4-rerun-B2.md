# E4 re-run, Part B (attempt 2, three manager replicas): all steps passed, exit 0

Date: 2026-10-24, log UTC 04:47Z-04:52:56Z. Branch `worktree-agent-af405ad109074ceb8`, product code at `deeae3c`. Raw log: [`e4-rerun-B2.log`](e4-rerun-B2.log). Script: [`tests/p0/e4-rerun.sh`](../e4-rerun.sh).

## Cause of the B failure (attempt 1) and changes

- **Root cause (script):** the replica config was written by a remote `sh -c` snippet with `"nodeId": "manager-$11"`. In `sh`, `$11` is `$1` followed by `1`, so the node IDs became `manager-192.168.104.11` etc. (visible in `e4-rerun-B.log` line 998) while the peer list named `manager-1..3`. No replica was a member of its own bootstrap configuration, so none could be elected, and nothing reported an error. Fixed with `${11}`.
- **Product:** `manager` now refuses such a configuration (`manager.nodeId ... is not in manager.peers`, duplicate IDs or addresses, own peer address different from `advertise`/`bind`), and the metastore refuses to bootstrap a node whose ID is not in its peers (`cmd/manager/managed.go` `validatePeers`, `internal/metastore/store.go` `checkBootstrapPeers`, tests `TestManagerPeersValidation`, `TestBootstrapRefusesNodeOutsideItsPeers`).
- **Keyring hypothesis checked:** "a replica without a data-key recipient hands leadership over, so with three fresh replicas nobody may lead" does not hold. `Keyring.ensureLeader` bootstraps the keyring on the first leader when none exists (it creates the key and registers itself); only replicas that find an existing keyring without their envelope hand leadership over. Covered by `TestKeyringDistributesDataKeyToRegisteredReplicas` (fakes) and now by this run. The remaining limit: until the other replicas are registered, only the first leader can lead.
- **Script, documented variant:** Part B now uses `manager.keyring: true` (no shared data key file) and `manager.peerTLS` with certificates from `manager -issue-peer-certs` (random CA passphrase, discarded), registers manager-2/3 as data-key recipients in `hasetup` (`-print-replica-key`, `PUT /settings/replicas/{id}/data-key-recipient`) and asserts that all three hold the current key. On any failure after `hamanager` started, the exit trap writes every replica's `systemctl status`, file listing and last 300 journal lines to `tests/p0/results/<log>-replicas.log` (setup tokens redacted) before the cleanup. No failure occurred, so no replica log file was written in this run.

## Invocation

- Fresh VMs; `R_PHASES` = setup steps `build vms swarm images prereqs rprep backend manager setup swarmapi` plus `hareset hamanager hasetup haswarm hacreate hafailover hadelete`.
- `bin/lab status` before: manifest empty; unrelated `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`.
- Host 1-minute load 124-128 during Part B (`host uptime` lines). Timings are single samples under foreign load.

## Per-step outcomes

| Step | Outcome | Evidence (log line) |
|---|---|---|
| setup steps | completed | lines 2-832 |
| hareset | completed | line 840; m2/m3 promoted |
| hamanager | **passed** (attempt 1: timeout) | `wait ok after 2s: a replica leads` (04:50:35Z); leader m1 `leadership started term=2` (line 1174); m2 and m3 answer `503` with `X-Dcm-Leader: manager-1` (lines 1189-1200); metastore listeners on `192.168.104.1/.3/.4:18490` (section "metastore listeners" after line 1200) |
| hasetup | passed | admin setup on the leader; manager-2 and manager-3 registered (`hasCurrentKey: true`, lines 1247, 1257); keyring status `{"currentKey":"dk-7728447cc8b6b562","recoverable":true,"replicas":[manager-1 leader, manager-2, manager-3 all hasCurrentKey true]}` (line 1265) |
| haswarm | completed | `all 3 agents enrolled` |
| hacreate | passed | leader m1 SIGKILLed at `7/23 05-exec-sql-n1` (line 1401); `new leader dbcm-lab-m3 answers the API 3 s after the SIGKILL` (line 1426); session valid on the new leader; operation `adopted by term 3; resume pending`, then resumed and `succeeded` 23/23 (line 1455); exactly 3 services and one volume per slot; one create operation only; old leader rejoined as follower (`X-Dcm-Leader: manager-3`) |
| hafailover | passed | leader m3 SIGKILLed; `new leader dbcm-lab-m2 after 2 s`; `writes during the leader loss: 30 ok, 0 failed over 8 s` (line 1563); 30 rows on n3; the backup started just before the kill ended `succeeded` (line 1575; it ran 3 s in all, so whether a step ran on the new leader is not established from the log) |
| hadelete | passed | `deletion of e4ha ended: succeeded` (line 1642); no e4ha services, volumes or containers left; sentinels unchanged (line 1672) |

## Observations and limits

- The first leader logged `the data key has no recovery envelope` at bootstrap (06:50:34 local); the later keyring status reports `recoverable: true`, so the recovery envelope was added afterwards (when the configured settings with the recovery key were applied, or by a later distribution). The exact moment is not in the log.
- Peer mTLS was configured on all three replicas and they formed a quorum; the run does not capture the TLS handshake itself (no packet capture, no negative test with a wrong certificate on VMs).
- Only one run; each figure is a single sample.

## Scoped cleanup and proof

- Exit trap `bin/lab cleanup -apply` at 04:52:56Z: post-snapshot diff `added: (none)`, `removed: lima:dbcm-lab-m1, lima:dbcm-lab-m2, lima:dbcm-lab-m3`, `unrelated resources intact (3)` (line 1728 f.). `bin/lab status` afterwards: manifest empty, inventory identical to the baseline.
