---
title: Hand-off for the next session
---

# Hand-off for the next session

State of 2026-11-01. Read this first, then `docs/STATUS.md` (per-topic sections, newest first), `docs/WORKPLAN.md`, `docs/ENTSCHEIDUNGEN.md` and `docs/RELEASE-CANDIDATE.md`. Nothing is tagged or published; a tag (for example `v0.1.0-rc1`) and any publication need the owner's approval.

## Where the project stands

Self-hosted service and cluster platform ("cluster-manager", provisional name; folder, repository and module path stay `db-cluster-manager`). Priority 1 is MariaDB Galera on Docker Swarm with a web UI, CLI, replicated managers and agents.

Verified on real lab VMs (Lima, arm64, Docker 29.9.2, MariaDB 11.4.13), with the result files under `tests/p0/results/`:

| Area | Evidence |
| --- | --- |
| Swarm Galera, host loss, manager failover, real manager and mTLS agents | E1-E8 |
| Zero-UUID incident, guided recovery (approval-gated), autoFailover, rolling update, certificate renewal | E12, E18 (autoRecover default off and policy-approved run) |
| Real-browser UI: setup, passkey, connect, enroll, create 3-node Galera, rolling restart, abort | E13 |
| Backup (S3/MinIO TLS and local), inspection restore, scheduled verify, retention, copy to a second target, failure cases | E14 |
| Point-in-time recovery: binlog archive, coverage check, approval, restore into an isolated instance | E15 (job helper), E16 (through manager and UI) |
| `INSTALL.md` walkthrough, German UI with real screenshots | E16 (by a reader who knew the code, not a novice) |
| Fresh-server bootstrap (Debian 12, AlmaLinux 9.8), image composer catalog and four builds, patch management with native agent, native restic, module registry | E17 |
| Agent port 7443 blips (not reproduced) | E18 |

Image composer catalog: 2026.10.30-2, includes a Minecraft server (vanilla and Paper, EULA acceptance mandatory) and an optional Geyser/Floodgate crossplay add-on for Paper 26.3. Bedrock Dedicated Server is deliberately not included (see ADR 0026 addendum).

## Not verified (do not report these as working)

- Restore of a backup copy into an isolated instance (code and fakes only, `targetSecrets`), archive schedule on VMs, multi-file archives and gaps. A lab run was started on 2026-11-01 (`tests/p0/results/e19-*.md`); check whether it landed before repeating it.
- Fresh-server bootstrap through the HTTP API or UI wizard (only through the lab harness so far); Ubuntu, Rocky, Alpine; native-agent and Swarm-join steps.
- Release workflow (`docs/RELEASING.md`) has never run. Container images were not built in the release-candidate cycle.
- amd64 on real hosts (GitHub runners test amd64 in CI only), load tests, Kubernetes real runs.
- Real Bedrock client against the crossplay image; Geyser `transport` setting is not managed by the recipe.
- Web UI views for patch management, restic, image updater and runtime are missing or mock-only.
- Hypothesis for the short "node became unknown" events right after agents are deployed (settling) is unproven.

## Open owner decisions

- Enrollment token for a not-yet-enrolled bootstrap host (guide 27): the token exists only for an already registered host.
- `autoRecover` is opt-in per cluster, default off; Phase 1 auto-approval deviates from "only if all positions are readable" (ADR 0029). Recommendation: leave it off.
- Whether and when to cut a release candidate for a separate test Swarm (needs a clean independent `INSTALL.md` run first).

## Next steps, in this order

1. Merge the E19 lab result if the agent finished (branch `worktree-agent-ae7f803e7f86b152b` or whatever `git branch --list 'worktree-agent-*'` shows as unmerged), then run the verify script.
2. Independent first-time-reader walkthrough of `INSTALL.md` (an agent that has not seen the code), including HTTPS backend and the HA stack.
3. Bootstrap through the API/UI on a fresh VM; decide the enrollment-token gap with the owner.
4. Missing UI views (patch, restic, image updater, runtime) behind the module switches.
5. Release dry run in a fork or with a throwaway tag only after owner approval; build and test the container images.
6. Galera update/resize service type, canary executor, SBOM and rescan, real Kubernetes after the Swarm gates.

## Working rules that matter

- Communicate with the owner in German; code and technical docs in English; UI and guides in German and English.
- Quality over quantity: small diffs, no speculative abstractions, remove dead code. Optional features are switchable modules and default to off (`internal/modules`, ADR 0030).
- Lab limits: combined 16 GB RAM, 50% host CPU, about 100 GB SSD; at least 40 GiB free disk and load below 40 before a lab run. Only `dbcm-lab-` VMs through `bin/lab`; end with `bin/lab cleanup --apply`. Never touch unrelated Lima/Colima profiles, never prune Docker globally, no production credentials in tests or tracked files.
- Never report an unexecuted check as passing; update `STATUS.md` and `WORKPLAN.md` with actual results.
- Git: worktree per agent, integrate on branch `integration`, run the full verify (gofmt, `go vet`, `go test -race -count=1 -p 2 ./...`, `scripts/check_docs.py`, `npm run check`, vitest, `npm run build`), then `git push origin integration:main`. No force pushes, no attribution lines, no secrets. `npm run build` deletes `internal/webui/assets/.gitkeep`; `touch` it afterwards. `go test` can be flaky under host load above about 25; wait for low load.
- Host disk: a Time Machine local snapshot can hold freed space for hours. Big non-project consumers (Xcode DerivedData, `~/Library/Caches`, CoreSimulator, loose folders in `/private/tmp/claude-501`) are not ours to delete without the owner's say.
- Codex was unavailable during this phase (usage limit); Claude agents were used instead.
