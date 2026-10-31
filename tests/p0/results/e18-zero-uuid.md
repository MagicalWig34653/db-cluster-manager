# E18 part 2: guided zero-UUID recovery on real VMs, default and autoRecover (2026-10-31; ADR 0028/0029)

Harness: `tests/p0/e18-zero-uuid.sh` on the cluster of `tests/p0/e18-agent-blips.sh` (three Lima VMs, arm64, real manager, mTLS agents, 3-node Galera `e18db`). Raw log: `e18-zero-uuid.log` (the script ended with exit 1, see the harness faults below). Each phase hard-stops all three VMs (`limactl stop --force`), restarts them and the lab manager, and waits for the zero-UUID Primary. It occurred both times.

## Phase A: default, autoRecover off (human approval): PASS

| Check | Result |
| --- | --- |
| `autoRecover` default | `false` |
| Zero-UUID Primary after the power loss | present on all nodes; incident event `zero_cluster_uuid_primary` (critical) on n1/n2/n3 |
| 150 s hands off | no recovery operation started by the manager |
| Operator `POST /clusters/{id}/zero-uuid-recovery` | phase 1 `galera/zero-uuid-prepare` waits for approval; approval with a wrong digest refused; with the digest: stop all, snapshot (`pre-recovery-op-...`), succeeded |
| Phase 2 `galera/zero-uuid-recovery` | planned by the leader with candidate n3 (highest local seqno), `recovery.planned` critical notification, awaiting approval; approved with its digest; bootstrap, two SSTs, steady-state specs, state check: **succeeded** in about 70 s |
| After | three nodes Synced, new non-zero state UUID, sentinels 1,2 on every node |

## Phase B: autoRecover on: PASS, but later than the script waited

The script asserted failure (`a phase was not auto-approved`, recovery API `unavailable`). That was **harness faults, not a product fault**; checked afterwards on the same cluster:

1. After the second power loss the lab client's login was refused (`401 authentication_failed`): passkey clone detection, the client file had rolled back further than the +50 counter bump (E12 saw the same). With no session every status call returned nothing, which the script printed as `null`/`unavailable`. Fix in the harness idea: raise the counter by a large amount (done by hand afterwards: +100000, login worked).
2. autoRecover has a **one-hour cooldown after a run** (ADR 0029). Phase A's manual run started 10:05:23Z; the policy acted at 11:05:44Z, 59 minutes later, while the script gave up after about 20 minutes.

What the manager did by itself (events and notifications read afterwards, no human call):

| Time (UTC) | Event |
| --- | --- |
| 10:06:44 | admin set autoRecover to true (warning event, audited) |
| 10:07:34 | critical: cluster has no Primary component (full outage); 10:07:49 critical `zero_cluster_uuid_primary` |
| 11:05:44 | critical `recovery.planned`: "policy:autoRecover started the zero-UUID recovery ... phase 1 ... approved by the policy" |
| 11:05:56 | phase 1 succeeded, snapshot taken |
| 11:06:05 | `recovery.planned` (critical) phase 2: bootstrap n3 (local seqno 298) with a new state UUID, other nodes rejoin by SST; auto-approved because the candidate was unambiguous |
| 11:06:53-54 | phase 2 succeeded; `recovery.ended` (info): "all nodes Synced with a new state UUID"; node recovered notifications; alert resolved |

Final state: recovery status `autoRecover true`, prepare and recovery `succeeded`, both `auto true`, candidate n3; wsrep on all three nodes `Synced`, size 3, one non-zero state UUID; **sentinels 1,2,3,4 on all three nodes** (nothing committed before the power loss lost). Recorded after the fact by hand, not by the script.

## Findings

- The approval-gated default and the opt-in policy both work on real hosts. Notifications `recovery.planned` and `recovery.ended` and the incident events reach the UI inbox. No external channel (webhook, mail) was configured, so delivery to those was not checked.
- The cooldown means a second zero-UUID incident within an hour of any run waits for a human even with autoRecover on. It is by design but surprising; the UI/guide should say so (documented in the status entry, not changed).
- `recovery.needs_approval` was not seen (the candidate was unambiguous). The UI wizard for recovery was not clicked (API only).

## Limits

arm64, one cluster, tiny data (sentinels), two incidents, API only, no external notification channel, ambiguous or tie candidate not run, the 59-minute wait means the policy decision was only seen on a second run. The script itself needs the larger counter bump and a wait of more than one hour for phase B; it is kept as is, with this record.
