# E12: zero-UUID incident and recovery, proxy autoFailover, HA stack rolling update and certificate renewal

Dates: 2026-10-28 and 2026-10-29 (log times UTC; the second day continued the same kept VMs after an account limit). Script: [`tests/p0/e12-recovery-autofailover-rolling.sh`](../e12-recovery-autofailover-rolling.sh) on the E4/E6/E8/E9 harness; analyzer [`e9-analyze.py`](../e9-analyze.py) (new line: conflicts); samples in [`e12-samples/`](e12-samples/). Three arm64 Lima VMs from `bin/lab` (2 vCPU / 4 GiB / 20 GiB, lima:user-v2), all Swarm managers, HA stack from `deploy/stack-ha.yaml`, image built from the committed tree, MariaDB 11.4.13 Galera `e12db` created through the HA manager, `autoFailover: true`. Host load average during the run 28-150 (foreign), host free disk 12-30 GiB.

Log: [`e12-recovery-autofailover-rolling.log`](e12-recovery-autofailover-rolling.log) is the **concatenation of several stages on kept VMs**, not one uninterrupted run: stage 1 (all phases, 05:36-06:30 on 10-28; pwrrecover and pwrb3 failed on script defects), a hand-run recovery (marked "Stage 1 follow-up"), stage 2 (renew, B2, incident, pwrrecover; renew and pwrrecover failed on script defects), stage 3 (pwrrecover, B3), stage 4 (pwrrecover after B3) and a final renew stage. Failed assertions in the log are real events of earlier stages; each was fixed in the script and repeated, as listed below.

## Part 2: autoFailover (ADR 0017)

Layout: proxy-a on m1 and proxy-b on m2 (coordination through the socat TLS fronts), writer on the Galera node of **m3** (no proxy there, so a missing acknowledgement cannot block), four clients on m1 writing every 50 ms (two failover lists in opposite order, per-connection and persistent, 1 s timeout), proxy status sampled every 100 ms. m3 was `limactl stop --force` at 05:47:36.7.

| Check | Observed |
| --- | --- |
| Automatic switch | Both proxies `no writer, epoch 3` at +5 s, **writer n1, epoch 4 at +10 s** (first healthy candidate in sorted order), no operator request. Spans without a writer: proxy-a 5.25 s, proxy-b 5.96 s (05:47:38.5 to 05:47:43.8). |
| Clients | Longest gap without success **7.7 to 7.9 s** (from the power loss), 94 to 116 errors per client (`no usable writer`, `context deadline exceeded`, one `invalid connection`). |
| Two writers | 0 samples (100 ms resolution, 3022/3015 proxy samples) with two different non-empty writers; commit order: 22917 rows, 2 runs, 2 distinct nodes (n3 then n1), **0 interleavings**. Sub-100 ms overlap is not excluded. |
| Acknowledged writes | Acknowledged = committed for all four clients (5740/5740, 5743/5743, 5711/5711, 5723/5723); no acknowledged write lost; no unacknowledged-but-committed rows this time. |
| Return of m3 | VM back and Galera 3 Synced 24 s after the first ssh; the writer **stayed n1** (no switch back). |

Real bug found by this run and fixed test-first: the `autoFailover` route is configured by cluster **name** (`clusterId` in `manager.proxyWriters`, equal to the proxy `cluster_id`), but the manager looked clusters up by generated record ID (`clu-...`), so automatic failover could never fire on a real cluster. `ProxyWriterReconciler` now also matches the cluster name (`TestProxyWriterAutomaticFailoverMatchesClusterNameOfGeneratedID`, failing before the change). Not verified: autoFailover with a lost proxy (blocked switch, as E9) and with a minority partition instead of a power loss.

## Part 3: HA stack update and certificate renewal

| Scenario | Observed |
| --- | --- |
| A: `docker stack deploy` with a new image (all three services change at once, `update_config` parallelism 1 per service) | All three tasks Running 13 s after the deploy, leader replica 2, converged (all updates completed) after **47 s**. API sampled every 100 ms on each VM: longest gap between two 200 answers **1.9 s**, 13-15 x 503 and 2-3 connection failures per VM. Session, keyring (3/3 recipients) and cluster intact. Services of different replicas restart at the same time, there is no quorum safeguard (a majority could be down at once; here the 13 s were enough). |
| B: `docker service update --image` one replica at a time, followers first, leader last, gated on health | Order 1, 3, 2 (leader 2 last); each update completed in 40 s, **166 s** in total; longest gap between 200 answers **1.2 to 1.7 s**, 12-14 x 503, 2-3 connection failures per VM. Images of all three replicas new; keyring 3/3. |
| Certificate renewal (peer bundles renewed by the leader-independent per-replica check at start): new configs with `peerTLS.renew.beforeDays 365`, `validityDays 400`, replaced one replica at a time (followers first) | Serials changed on all three replicas (e.g. replica 2 `68C5AE5E...` to `56243F93...`), validity now to 2027-12-03 (400 days); the metastore port presents the new certificate (`openssl s_client`, `Verification: OK`, the client chain from the unchanged CA). 40 s per replica, API gaps **1.98 to 2.08 s**, 12-15 x 503 and 4-8 connection failures per VM; replicas hold the current key, quorum with renewed certificates. |

Findings (lab, not product defects): a configuration that fails validation (my first attempt used `beforeDays 400`; the maximum is 365) crash-looped the replica and **Swarm rolled the service back** by itself (`rollback_completed`, `failure_action: rollback`); the other replicas kept the quorum, the API on that VM was unreachable for 18.5 s, and the stack stayed healthy. The same mistake was repeated in stage 2 because the harness reused existing Swarm config names (`config exists`): the final stage used fresh names. **Not verified:** renewal of a running replica without restart (the 12-hourly check path) and renewal while a replica is down.

## Part 1: power loss, zero-UUID incident, recovery, B3

B2 (all three VMs `limactl stop --force` with a healthy cluster, three Synced, UUID non-zero) ran **three times** (stage 1 B2, stage 2 B2, stage 3 B3 as the staggered variant); every time the cluster re-formed a Primary component with `wsrep_cluster_state_uuid = 00000000-...` and `grastate.dat` `uuid 0, seqno -1, safe_to_bootstrap 0` on all nodes. Differences to E9: in stage 1 and 2 **all three nodes** joined that Primary (size 3, Synced, `wsrep_last_committed` 0/1; E9 had two nodes and a crash-looping third), in B3 the stale node n3 failed with the E9 signature (`IST started with wrong seqno: 65, expected <= 0`, `Failed to JOIN the cluster after SST gcs_join(0000...:-1)`), exit 1 every ~25 s, while n1/n2 served as a size-2 zero-UUID Primary. Swarm and the manager stack were back about 60-90 s after the first VM start (leader election within seconds); the manager did nothing automatically for 150 s (no spec change, no bootstrap flag, no operation).

| Check | Observed |
| --- | --- |
| Incident detection (real cluster) | **Verified.** Event log (stage 1): `06:11:53 critical node n1/n2/n3 of cluster e12db reports zero_cluster_uuid_primary: Primary component reports the zero cluster UUID; preserve all node data and logs...` about 30 s after the manager leader started; the API listed all three nodes `unavailable` and the cluster `degraded` although the database served writes (the incident makes the node unhealthy by design). No notification channel was configured, so only the event/inbox was checked. |
| Outage diagnosis 409 reason | **Verified** (stage 1 and 2): `GET` and `POST .../outage-diagnosis` answered `409 {"error":"conflict","reason":"zero_cluster_uuid_primary"}` while the Primary existed. After the operator scaled all three services to 0 the POST kept the 409 for several seconds (stale Primary observation) and then answered 200 with `positions` all `00000000...:-1`, source `unknown`, risks `no-data ... zero cluster UUID`, reasons `no candidate: positions known for 0 of 3 nodes, need 2`. A stage-2 repetition of the check answered 401 because of a lab-client artifact (see below), so the second verification is partial. |
| Guided recovery through the product | **Not possible.** After a zero-UUID re-formation every `mariadbd --wsrep-recover` returns `00000000-...:N` (stage 1: n1 1, n2 1, n3 2; B3 state: n1 1303, n2 1304, n3 1302); the diagnosis needs a real UUID and answers "no candidate". The product has no path from this state; the operator must bootstrap a node by hand. |
| Operator recovery (documented, manual) | Services scaled to 0; `mariadbd --wsrep-recover` per node on its volume (one-off container, `--wsrep_cluster_address=gcomm://` and the cluster's binlog options); the node with the highest seqno (lowest slot on a tie) gets `safe_to_bootstrap: 1` in `grastate.dat` (without it the bootstrap start aborts: the first attempt of stage 2 crash-looped on `safe_to_bootstrap: 0`); its service is updated with `--wsrep_cluster_address=gcomm://` and scaled to 1; the other nodes are scaled to 1 and take SST; the bootstrap node's arguments are put back and it restarts as a member. Result every time: three Synced, **new non-zero UUID** (b8e7d239..., e64aa5d9..., 1d575f9b...; the old group UUID is lost), identical data on all nodes: sentinel ids 1,2,3,900 and 22917 load rows after the first recovery; after B3 sentinels 1,2,3,5,6,900 on all three, i.e. nothing committed before the power loss was lost. The scripted version (`pwrrecover`) took about 40 s to a Primary and succeeded in stages 3 and 4. |
| B3 (staggered loss, valid: started on a healthy cluster, three Synced) | The first-lost node n3 (stale, seqno 1302) was **not chosen**: the highest seqno (n2, 1304) bootstrapped; sentinel 6, written while n3 was down, is on all nodes afterwards. The stale node never joined the zero-UUID Primary by itself (same root cause as E9). |

Lab-client defect (not product): the harness client's passkey signature counter is a file that the power loss rolled back behind the server's stored counter; from the second power loss on logins were refused (`authentication_failed`, server log `api: login failed`) until the counter was raised. `e8_login` of the E12 script now raises it by 50 before every login.

## What this does not show

- The 409 reason and the incident were seen on the real cluster, but the guided recovery of the product was **not** exercised end to end (it cannot select a candidate); the manual recovery above is an operator procedure, not an approved product behavior, and a major decision on it is open.
- The cause of the zero UUID after a total power loss is unchanged from E9 (diagnosed, not repaired): the Primary re-forms from `gvwstate.dat` without the group UUID. Whether another MariaDB/Galera configuration (for example durable `gcache.recover` or `wsrep_provider_options` page settings) avoids it was not tested.
- autoFailover covered one power loss of the writer VM with two proxies on other VMs; sampling 100 ms; the proxies' clock bounds and VIP were not part of this run.
- The rolling update used derived images (a label on the same binaries); the rollback of a failing image was seen only through the invalid configuration.
- Foreign host load (28-150) and 20-30 GiB free host disk influenced timings; reclaimed disk after teardown was not measured.

## Script defects found

`node_vm` returned an empty VM name with the services at 0 (fixed by an override); the harness reused existing Swarm config names (`config exists`) and so tested the old configuration again; `e8_login` and the signature counter (above); `docker service logs` hangs as in E9 (container logs are read per container).

## Cleanup

`bin/lab cleanup --apply` at the end: removed `lima:dbcm-lab-m1..m3`, `unrelated resources intact (3)`; `limactl list` afterwards shows only the two foreign stopped Lima profiles and the Colima profile was untouched. No global prune, no host restart.
