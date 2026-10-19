# Execution workplan

Status labels: READY, PENDING, IN PROGRESS, DONE, BLOCKED. Only mark DONE with recorded evidence. The product release roadmap remains in [RELEASES.md](RELEASES.md).

| ID | Status | Work package | Completion evidence |
| --- | --- | --- | --- |
| W00 | DONE | Prepare Claude instructions, bootstrap task, Go/monorepo ADR and initial docs workflows | Local documentation check passed; no Git/remote or infrastructure changes |
| W01 | DONE | Resolve publication coordinates/license detail, initialize Git, public push and enable Pages | Public repo https://github.com/MagicalWig34653/db-cluster-manager, commit 684c2ea pushed to `main`, docs-check and Pages runs succeeded, https://magicalwig34653.github.io/db-cluster-manager/ returns HTTP 200 |
| W02 | DONE | Scaffold Go manager/agent/CLI and SvelteKit UI, versioned API contract, reproducible build | `make build/test/check` pass locally and in the `Build and test` workflow; manager served `/api/v1/health`, `/api/v1/version` and the built UI over HTTP. Limitation: UI not rendered in a browser (none installed) |
| W03 | DONE | Design and prototype manager metadata HA/fencing and Swarm/local-storage ownership | ADR 0003 with in-process evidence (`go test -race -count=3 ./...` passes; 4 failure scenarios in `internal/metastore/scenarios_test.go`), library evaluation, ADR 0004 (proposal, unverified against Docker). Limits: in-memory, no VMs, no real network |
| W04 | DONE | Implement dedicated local lab creation/inventory/cleanup within resource budget | `bin/lab` (status/plan/apply/cleanup with ownership manifest, 47+16 unit tests with fake runner). Real cycle on 2026-10-19: one VM `dbcm-lab-m1` (2 CPU, 2 GiB, 10 GiB) created, measured (guest 2 vCPU, ~1.9 GiB RAM, Docker 29.9.1, 3.1 GiB on host disk), removed; the 3 unrelated resources (`colima:default`, 2 `thermoctl-*` VMs) were unchanged and reported intact. Limit: host free space dropped about 3.4 GB across the cycle and was not shown to be reclaimed (other host activity not ruled out) |
| W05 | IN PROGRESS | Execute P0 Galera/agent/routing experiments | P0 acceptance gates in release plan, compatibility limits and test artifacts. Done so far: E1, E2 (see ADR 0004 evidence). Still open: real agent, three-manager failover on VMs, proxy/VIP routing, full-cluster loss, resource measurements per gate |
| W06 | PENDING | Refine R1 work packages from evidence and implement incrementally | Traceable requirements, tests and bilingual user guides per package |

W03 experiments requiring VMs depend on the safe lab controls from W04. Design work can start first; do not run ad hoc experiments before ownership and cleanup are in place.

## Session handover fields

For the active task, maintain: objective, changed files/contracts, executed checks/results, unresolved issues, and next concrete action. Keep this concise and update `docs/STATUS.md`; avoid duplicating the full requirement catalog.

## Current next action

W05 continued: E3 three-manager failover on VMs using the real `internal/metastore` with persistent storage and a real transport (requires implementing both first), E4 proxy/VIP routing, E5 full-cluster outage. Before E3, decide whether `bin/lab` should own the user-v2 network and the rootful-Docker switch. Implement missing product pieces (agent, persistence) via small plans under `docs/superpowers/plans/`.
