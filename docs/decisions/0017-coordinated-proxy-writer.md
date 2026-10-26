# ADR 0017: coordinated writer choice for redundant proxies

Date: 2026-10-25. Status: Implemented; VM evidence from E9 (2026-10-26) in [the results](../../tests/p0/results/e9-ha-proxy-writer.md): both proxies agree, a controlled switch freezes writes about 1 s, a proxy that is unreachable blocks the switch (writes down 26-28 s), running proxies keep the choice through a manager outage, a restarted proxy has no writer until a manager answers, loss of the writer's VM leaves no writer for 62 s without an operator or `autoFailover` (not exercised). Sampling 100 ms.

## Context and decision

[E8](../../tests/p0/results/e8-proxy-redundancy-powerloss.md) observed two independent `single-writer-stable` proxies selecting n2 and n3. The previous mode retained each proxy's first healthy, lowest-latency writer. Galera permits both to accept writes, so a VIP or client failover list does not make that choice single-writer.

A redundant proxy group now uses one replicated manager choice per cluster. `manager.proxyWriters` names the initial writer, the allowed writer candidates and **every** proxy member. Each proxy has a distinct ID and a per-proxy token and reads the choice from the manager leader through a consistent Raft barrier. The manager stores the choice and monotonically increasing epoch in its document store. An unavailable manager leaves a running proxy on its last observed choice, subject to its local Galera health checks. A newly started proxy has no writer until it reads the leader. If its selected node becomes unhealthy, the proxy has no writer; it never picks another from local latency.

A writer change uses a two-phase freeze. The administrator requests `POST /api/v1/proxy-writers/{clusterId}/switch` with a separate token. The leader first commits a choice with no writer and a pending candidate. Each proxy that sees that epoch refuses new writes, **closes existing write connections immediately** and acknowledges the epoch. Only when every configured member acknowledged does the leader atomically publish the new writer. Coordinated mode requires Galera sync probes. The proxy's optional read endpoint is disabled: it is an L4 path and cannot prevent SQL writes to a nominal reader. A proxy that cannot reach the manager blocks the switch, which remains frozen. There is no timeout that silently removes a member. A stale epoch or stale freeze response cannot roll an active proxy back.

The safety claim assumes all write traffic uses registered proxies, their per-proxy tokens are protected, every running proxy uses this protocol and the configured member list is complete. A leaked token can forge an acknowledgement. Direct database connections and independent proxy configurations are outside this guarantee. Planned removal of a permanently lost member requires a separate fencing procedure and is not implemented; operators must not delete a member merely because it is unreachable.

## Alternatives and trade-offs

| Option | Outcome |
| --- | --- |
| Sort each proxy's locally healthy nodes and choose the smallest ID | Deterministic only when all proxies see the same health set; a network partition can split the set and produce two writers. Rejected. |
| Common `prefer-node` in every proxy file | Worked in E8 while the preferred node was healthy, but independent fallback after its loss can diverge. Rejected for redundant groups. |
| Time-limited writer lease | Could recover after a lost proxy, but needs proven clock-skew and lease-expiry bounds to avoid overlap; not adopted here. |
| Replicated choice with all-member revocation acknowledgement | Chosen for a clear safety boundary without synchronized clocks. Availability is intentionally sacrificed when a member cannot acknowledge. |

## Evidence, operation and limits

Unit tests use an in-memory three-member Raft group, fake proxy writer sources and an HTTP handler with no listener. They cover different local latencies, health flapping, manager partition, a pending switch across leader change, stale epochs, unregistered acknowledgements and closure of existing write connections before ACK. E8 was run before this change and does **not** validate this protocol on VMs.

Manager URLs must use HTTPS, except loopback HTTP for tests; the manager's listener or a trusted front proxy must protect the path. Proxy tokens and the administrator token are different files on each manager replica; do not put them in tracked config. The switch request is audit recorded before the freeze under the automation actor `proxy-writer-admin`; the endpoint does not identify a human user. All manager replicas must use the same proxy member/candidate IDs and token distribution. A member or candidate mismatch makes choice reads fail closed. The initial writer and later targets must be valid writer-eligible node IDs in each proxy; unknown or unhealthy targets yield no writer. A proxy's `/status` shows cluster, proxy ID, epoch and pending target.

Optional `autoFailover` uses the leader’s replicated health observations: only fresh Synced members of a Primary component with a majority can be chosen, in sorted candidate-ID order. It starts the same freeze and does not switch back while the current writer remains healthy. The default is `false`; with it the operator first confirms Galera quorum and a healthy candidate, then requests the switch. An offline proxy blocks completion in either mode. This is deliberate until a safe member-fencing mechanism exists. Manager deployment of proxy instances, node TLS and an E8 rerun with this code remain open.
