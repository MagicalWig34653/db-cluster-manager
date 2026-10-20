# Current status

Updated: 2026-10-18.

## Phase

Concept documented; Claude Code handover prepared. Go and the monorepo are confirmed. The user explicitly authorized Claude to initialize Git and push a public GitHub repository.

W01 done: public repository https://github.com/MagicalWig34653/db-cluster-manager (`main`), license `AGPL-3.0-only` (LICENSE from gnu.org, SHA-256 0d96a4ff…bcb0), workflow actions pinned to commit SHAs, Pages deployed at https://magicalwig34653.github.io/db-cluster-manager/. No application code or test infrastructure exists. No production Swarm access has occurred. Copyright identity is still open.

## Completed

- 56 confirmed requirements, release allocation proposal and local test budget documented.
- Go/monorepo decision recorded in ADR 0002.
- `CLAUDE.md` imports shared `AGENTS.md` instructions.
- Bootstrap handover covers authentication, publication coordinates, licensing detail, initial commit/public push, Pages and subsequent development.
- Workplan W00–W06, `.gitignore`, a dependency-free documentation checker, documentation CI and Pages workflow prepared.

## W02 result

Go module with `cmd/manager`, `cmd/agent`, `cmd/cli`; `internal/api` (`/api/v1/health`, `/api/v1/version`, contract in `api/openapi.yaml`); `internal/webui` (embedded SPA with index fallback); SvelteKit 3 / Svelte 5 / Tailwind 4 / shadcn-svelte (button, card, badge; hand-written `components.json` and zinc theme CSS because `shadcn-svelte init` needs an interactive preset choice) in `web/`, dependencies pinned in `web/package-lock.json`. German/English UI strings with browser-language detection and a language switch (`web/src/lib/i18n.ts`). Executed: `make build`, `make test` (Go and 4 vitest tests), `make check`, and the `Build and test` workflow on GitHub. A built manager served the API and UI over loopback.

Limits: the UI was never rendered in a browser (none installed), so layout, dark mode and the language switch are unverified visually. Frontend imports use Node subpath imports (`#lib/...`, `web/package.json`). Agent and CLI only report their version. `npm` reports pending install-script approvals (`npm approve-scripts`) that have not been reviewed.

## W03 result

ADR 0003 (metadata replication, leader-only operations, term fencing) now has in-process prototype evidence: `internal/fencing` (agent term guard), `internal/opjournal` (operation journal), `internal/metastore` (hashicorp/raft v1.8.0 wrapper, in-memory) and four failure scenarios. `go test -race -count=3 ./...` passed. Library evaluation: [evidence document](decisions/0003-evidence-library-evaluation.md). ADR 0004 proposes pinned per-node Swarm services with a storage-identity marker; it is unverified against Docker.

Known gaps: no persistence, real transport, mTLS/enrollment, secrets, read barrier or snapshot export; MPL-2.0 vs AGPL read from licence text only. The work used subagents (plan: `docs/superpowers/plans/2026-10-19-w03-consensus-and-fencing.md`); a final whole-branch review has not been run.

## W04 result

`bin/lab` (cmd/lab, internal/lab): budget from detected CPUs (14 logical -> 7 allowed), RAM 16 GiB, disk 100 GiB; ownership manifest `.lab/manifest.json`; names restricted to `dbcm-lab-` plus a strict pattern; dry-run by default; cleanup acts only on manifest entries and fails if an unrelated Lima/Colima entry disappears. A real one-VM create/measure/cleanup cycle ran; unrelated resources were unchanged. Not verified: reclaimed disk space (free space was 3.4 GB lower afterwards), Ctrl-C handling, CPU utilisation cap (vCPU count only), Docker contexts and Colima profiles as lab resources (none created yet).

## W05 progress

E1 and E2 ran on three Lima VMs (Swarm, pinned Galera services, sentinel). Confirmed: pinned restart, no rescheduling on host loss, safe rejoin, minority refuses writes. Refuted: Swarm alone protects against an empty volume (a marker guard is required), and a datadir marker survives SST. Details in ADR 0004. Not yet covered: real agent, multi-manager failover on VMs, proxy/VIP, full-cluster outage, AMD64. P0 exit gates are therefore NOT met.

## Building blocks (library level, tested, not yet integrated into the manager)

`go vet ./...` and `go test -race ./...` pass for every package on this branch. None of the packages below is wired into `cmd/manager`, which still serves only health, version and the prototype UI. Everything marked "fake" or "canned" has not run against the real thing.

- `internal/metastore` + `cmd/metad`: disk-backed Raft (raft-boltdb), TCP without TLS, read barrier, bounded snapshot; `bin/metad` is an unauthenticated lab harness.
- `internal/agent`: fenced, idempotent executor, identity-marker guard, ownership allow-list (including referenced networks/secrets), lab HTTP transport (loopback, unauthenticated).
- `internal/dockerclient`: Docker Engine API client (v1.47 field names checked against the reference); tested against canned JSON only. `cmd/agent` runs the agent with it.
- `internal/galera`: service specs, placement with stated precedence, create/replacement plans that never bypass quorum, full-outage diagnosis, health model; `internal/orchestrate`: conversion to agent specs. Only the lab-proven parts come from E1/E2; `--wsrep-recover`, non-default datadir layout and 5-node clusters are untested.
- `internal/routing`: writer selection with hysteresis and drain plans (no proxy/VIP).
- `internal/authz`, `internal/recovery`, `internal/passkey` (go-webauthn v0.18.2, BSD-3-Clause, ADR 0005): permissions, recovery codes/tokens, sessions, passkey ceremonies tested with a software authenticator; no real browser or hardware authenticator; all in memory.
- `internal/backup`: encrypted manager-config backups, local and in-memory backends, retention, restore inspection; SFTP/S3 backends are stubs (ErrNotImplemented).
- `internal/certs`: internal CA, renewal logic, imported-certificate validation, deny list; ACME DNS-01 is only an interface.
- `internal/notify`: event log, rules, UI/webhook (SSRF-protected)/email channels; in memory.

Dependencies added overall: hashicorp/raft and raft-boltdb (MPL-2.0), bbolt (MIT), go-webauthn and transitive modules (BSD/MIT/Apache); no legal review.

## Second batch of building blocks

- `internal/runner`: operation executor with leader checks, barrier, unknown-outcome handling, per-resource locks, conversion of Galera plans; runs a 3-node create plan against real `agent.Executor` instances with a fake Docker and an in-process Raft cluster. Operation definitions and pause state are not replicated; there is no real Galera health checker.
- `internal/agent` gained `create-volume` and `update-service` (and `internal/dockerclient` the matching calls); `start-db` still expects a path-style mount source and would refuse a volume-name mount from the Galera builder.
- `internal/api`: setup, passkey login, sessions, recovery endpoints, CSRF via strict Origin check, deny-by-default authorization with audit, resource endpoints behind interfaces, OpenAPI checked against routes. Mounted by `cmd/manager` only with `-config` enabling auth. State is in memory (lost on restart); no real inventory store; passkeys tested with a software authenticator only.
- `internal/proxy` + `cmd/proxy`: L4 MariaDB proxy with routing, draining, TLS termination (MySQL in-protocol upgrade), hot cert reload. The Galera status probe is a stub, so only `assume-synced` mode works. Never tested against a real MariaDB or client connector.
- `internal/dbadmin`: SQL generation, grants, planning and argv builders for dump/load, tested against a recording fake only.
- Packaging: `deploy/Dockerfile`, `compose.yaml`, `stack.yaml`, `release.yml`, offline bundle script, `docs/INSTALL.md`. The image was never built, the release workflow never run; `scripts/check_deploy.py` is static only.

## Next action

Continue W05 (see WORKPLAN). The first working version must be validated locally before the user's Swarm trial. Most R1 scope (cluster creation through the manager, backups, TLS, passkeys, UI flows, notifications, restore, packaging) is not started.

## Verification and limits

`python3 scripts/check_docs.py` checks local Markdown file links and the mapping of 56 requirements to releases. It does not validate external URLs, anchors or rendered website links.

No application tests, VM tests or native architecture checks have run. The docs check, Jekyll build and Pages deployment succeeded on GitHub Actions. The Claude CLI itself has not been launched or configured globally.
