# Current status

Updated: 2026-10-26.

## Operator CLI increment (2026-10-26, cx-cli, fakes only)

`cmd/cli` now uses the existing REST API for session validation, context selection, cluster list/get/create, stored Galera operation plans, deletion plans and digest approvals, operation list/watch/abort, backup list/run/verification status and inspection-restore initiation, host list, service preview/create, vulnerability finding list/proposal approval and JSON/YAML service plan export. It prints table or JSON, emits bash/zsh/fish completion, and maps API failures to documented exit codes. Request bodies come from JSON files; session tokens enter only through stdin, environment or files and are kept outside the context config. Inspection responses with their one-time password go to a 0600 result file and the console summary omits that password. Redirects are not followed. [ADR 0020](decisions/0020-operator-cli.md) records the API constraints and alternatives. [English](guides/en/20-cli.md) and [German](guides/de/20-cli.md) man-style guides and Pages links are current.

The manager has no general operator Bearer API token endpoint: it authenticates these routes through a passkey-created session cookie. This CLI validates an existing session token through `/auth/session`; it cannot itself perform a passkey ceremony or issue an API token. `clusters create` immediately starts the server's create flow because there is no separate create-preview endpoint. `backups verify CLUSTER` is a read-only check of the last passed restore-verification timestamp; adding `--result-file` starts an approval-gated inspection restore. No real manager, cluster, backup backend or infrastructure was contacted.

Actual final-tree checks (writable temporary `GOCACHE`, `GOMAXPROCS=4` for Go vet and full race test): `gofmt -l .` was empty; `go vet ./...` exited 0 without output; `go test -race -count=1 -p 4 ./...` exited 1 (26 packages passed, 20 failed, three had no tests). This sandbox refuses local TCP and Unix listeners (`bind: operation not permitted`), including the CLI's `httptest.NewTLSServer` test. The complete CLI flow using `httptest.ResponseRecorder` passed with `go test -race -count=1 -run 'TestCLICommandsAgainstAPI|TestCLIWithoutListener' ./cmd/cli`; the listener-backed tests remain unverified. `python3 scripts/check_docs.py` reported 84 Markdown files, 69 requirements, zero errors; `python3 scripts/check_deploy.py` reported static checks passed, no image built. `git diff --check` passed. No frontend code changed, so frontend checks were not run. Next: run the full race suite where local listeners are permitted, then exercise the CLI with a real manager session in a dedicated environment before claiming end-to-end readiness.

## Static web accessibility and UX review (2026-10-26, cx-a11y, fakes/unit only)

Reviewed `web/src` statically for keyboard use, focus, accessible names, light/dark contrast, validation announcements, 360 px layout and DE/EN completeness. The shell now caps and scrolls the mobile navigation drawer, gives it a dialog description, keeps the main target focusable, wraps page actions and stacks definition lists below 400 px. Tables have a labelled, keyboard-focusable horizontal scroll region. The inline abort confirmation is correctly a named group; opening focuses Cancel and closing or completing returns focus to Abort. Loading, empty and error states share `StateView`; major list empties and API failure states use it. The cluster wizard, service form, dashboard editor and notification settings announce validation summaries and move focus to the first invalid control when present. Light/dark ring and border tokens were strengthened; Vitest computes contrast for base and accent tokens. DE/EN labels and technical examples moved into the message catalog. `npm run check:i18n` parses both catalogs and Svelte templates for missing keys, placeholder drift and fixed visible strings; Vitest also runs it. [DESIGN.md](DESIGN.md#static-accessibility-and-narrow-screen-review-2026-10-26), both getting-started guides and the Pages index describe the behavior and limits.

Actual checks: `gofmt -l .` exit 0, no output. Plain `go vet ./...` and `go test -race -count=1 -p 4 ./...` first exited 1 because the default Go cache under `~/Library/Caches/go-build` is sandbox-denied (`operation not permitted`). With `GOCACHE=/private/tmp/dbcm-cx-a11y-go-cache`, `go vet ./...` exited 0 without output. The same full race command with that cache exited 1: existing tests in multiple packages failed to bind local TCP/Unix listeners (`bind: operation not permitted`), including `internal/enroll` and `internal/galera` tests; no full-suite pass is claimed. `python3 scripts/check_deploy.py` passed its static checks (no image built). Web: `npm run check` completed with 0 errors and 0 warnings; `npm run check:i18n` passed; `npm test` passed 22 files / 212 tests; `npm run build` succeeded and `internal/webui/assets/.gitkeep` was touched afterward. `python3 scripts/check_docs.py`: 81 Markdown files, 69 requirements, 0 errors.

Limits: no browser render, 360 px measurement, screen reader, real manager, network or infrastructure test was performed. The i18n checker covers visible Svelte template text and catalogs, not arbitrary strings assembled in TypeScript or API data. Specialized forms outside the four named flows still need a manual keyboard/screen-reader pass. Next: exercise the shell, confirmation, forms, tables and both themes/locales at 360 px and zoom with synthetic data and a screen reader, then rerun the Go race gate where local listeners are permitted. W05/R1 gates remain open.

## UI coverage round 2 (2026-10-26, w06-ui2, mock only)

Compared `api/openapi.yaml` with `web/src/lib/api.ts` and the pages. Backend features without UI were: the Galera operation planner (`/clusters/{id}/operation-plan`: scale-out/in, rolling restart, patch upgrade), data-key rotation, metastore membership change and the proxy writer switch. Added, with DE/EN texts, dev mock handlers and vitest:

- Cluster page card "Galera operations": local slot/image validation, scale-in-below-three warning, stored plan with steps and warnings, then the operation card with the plan digest and an explicit confirmation before approval (`approveOperation` sends the displayed digest).
- Settings, replicas: data-key rotation (preview, digest, confirmation naming the digest, approval) and membership change (preview add/remove, same confirmation). A 409 clears the stale preview.
- Cluster page card "Proxy writer switch": sends the separate writer administrator token once as a Bearer header and clears it afterwards; there is no plan digest because the API has none, so the card requires an explicit confirmation of the write freeze.
- Not added: peer-certificate issuance (needs an offline CSR and key and is a fakes-only backend), proxy `GET`/`ack` (machine endpoints with proxy tokens), and TLS/ACME status, PITR/binlog planning, provisioning wizard and security/audit views, which already had UI or no HTTP route on `integration`.

Mock screenshots (DE/EN, Chromium via a scratch Playwright install, `scripts/screenshots/ops.mjs`) are in `docs/assets/screenshots/*-mock.png`, labelled as mock captures in the README, `docs/index.md` and guides 08 and 11. They are element captures, so the MOCK DATA banner is not inside the image; captions say so. Nothing was run against a real manager, so the panels are unverified against the real API.

Actual checks: `npm run check` 0 errors/0 warnings; `npm test` 21 files / 207 tests passed; `npm run build` succeeded and `internal/webui/assets/.gitkeep` was touched; `go test -race -count=1 -p 4 ./...` exit 0 (45 packages ok); `gofmt -l .` empty; `python3 scripts/check_docs.py` 81 Markdown files, 69 requirements, 0 errors. Limits: the membership and rotation backends are opt-in (default manager 503); screenshots use invented data; screenshots are not optimized (no Pillow installed). Next: exercise these panels against a real manager with the fakes-enabled backends and in a lab.

## Abort view ordering fix (2026-10-25, cx-abortfix)

`TestAbortCreate` intermittently timed out under host load because an abort
request saved an older `running` runner snapshot after the execution goroutine
had saved `aborted`. The persisted operation then had `EndedAt` set but still
reported `running`; the runner and journal had already reached `aborted` and
`unknown` for the in-flight exec-sql step. The manager now discards superseded
views by leader term and runner timeline length, and never replaces a stored
`aborted` or `succeeded` status with another status. The runner reads its
journal and state together so a terminal view cannot carry an earlier
`running` step. Failed operations remain resumable and abortable.
A deterministic test saves the two snapshots in reverse order and checks that
the API remains aborted and the step remains unknown. It failed before the fix
and passed after it.

The suspected regression interval was not a valid monotonic bisect range in
this environment: a detached worktree at `536154d` also timed out once during
`go test -race -count=15 -run TestAbortCreate ./internal/manager` while another
race run loaded the host. The view-saving code responsible for the overwrite
was already present at that commit. The current commit before the fix failed
in a five-count race run; the diagnostic run captured the persisted/runner
disagreement above. No infrastructure was used.

After the fix, three separate `go test -race -count=30 -run TestAbortCreate
./internal/manager` runs passed (90 repetitions of each matching test overall).
`gofmt -l .` was empty, `go vet ./...` passed, and
`python3 scripts/check_docs.py` reported 73 Markdown files, 69 requirements,
zero errors. `go test -race -count=1 -p 4 ./...` was executed but exited 1:
existing tests across packages could not bind local TCP/Unix listeners in this
sandbox (`operation not permitted`); `internal/manager` aborted at an
`httptest` listener before its full test set ran. The full race gate therefore
remains unverified. Next: rerun that gate where local listeners are permitted.

## W10 SSH cluster provisioning increment (2026-10-25, fakes only)

Implemented on `cx-provision`: `internal/provision` creates deterministic Swarm host plans and flag-gated k3s script previews. Read-only SSH preflight checks Linux, requested ports, time synchronization, at least 10 GiB free disk and existing Swarm/k3s membership. The SSH `CommandRunner` uses a private key and pinned OpenSSH `known_hosts`; unit tests use a fake runner and contact no hosts. Per-host scripts are exportable, carry no key or join token and check a per-plan marker before treating existing membership as their own. `internal/manager` seals drafts and SSH credentials in the replicated store, compares the plan digest on execute, then uses the existing term-fenced operation runner with a second definition-digest approval gate. It records observed Swarm cluster IDs and confirmed hosts in an ownership manifest for later scoped cleanup. A command with uncertain completion pauses for reconciliation. The shipping manager wires this only with `manager.experimentalProvisioning`; `manager.experimentalKubernetesProvisioning` allows k3s preview but not execution. The API (`/provision/plans`, `/provision/plans/{id}/execute`), OpenAPI contract, global settings `operate` authz, DE/EN UI wizard and mock, guides and [ADR 0018](decisions/0018-ssh-cluster-provisioning.md) are current.

Swarm preflight requires an already installed, responding Docker daemon; no engine installation or update is performed.

Actual checks: `gofmt -l .` initially listed `internal/provision/plan.go`; after `gofmt -w`, rerun was empty (exit 0). `go vet ./...` exited 0 without output using writable `GOCACHE`. Focused `go test -race -count=1 -p 4` for provision, API contract and manager ownership passed. Final full `go test -race -count=1 -p 4 ./...` exited 1: 26 packages passed, 19 failed, four had no tests. Existing TCP/Unix listener tests failed with `bind: operation not permitted` in this sandbox. In an earlier full run, `internal/manager/TestAbortCreateWithExecSQLInFlight` also timed out; it passed alone with the race detector and did not fail in the final full run. No all-suite pass is claimed. `python3 scripts/check_docs.py`: 76 Markdown files, 69 requirements, 0 errors. `python3 scripts/check_deploy.py`: static checks passed; no image built. `web`: `npm run check` reported 0 errors and 0 warnings, `npm test` passed 19 files / 198 tests, `npm run build` succeeded and `internal/webui/assets/.gitkeep` was touched afterward. Dependencies were restored with `npm ci --offline --ignore-scripts` because this worktree had no `node_modules`.

Limits: no real SSH, Docker, k3s, host-key handshake, browser render or deployment test; the SSH execution path remains experimental and should not be enabled on production hosts. Preflight does not reserve capacity, install Docker, configure firewalls or NTP. A crash between a successful remote command and manifest write leaves a host for manual reconciliation. No cleanup operation, automatic Swarm registration, Kubernetes execution, key rotation or complete k3s security design exists. The exported k3s scripts have not been run or schema-validated. The R9 gates remain open. Next: review the SSH/token threat model, test deliberate disconnect/restart cases in an owned lab within `docs/TESTING.md` limits, and implement a manifest-scoped cleanup plan before a real host trial.

Git limitation: `git add internal/provision` failed because the sandbox cannot create the shared `.git/worktrees/cx-provision/index.lock` outside this writable worktree (`Operation not permitted`). No commit or push was made; the changes remain in this worktree. Once Git metadata is writable, review and commit the core, API/manager, and UI/docs groups separately.

## Security hardening round 2 (2026-10-25, cx-security2, fakes only)

- Reviewed the nginx snippet deny-list and composer context paths. The
  snippet now rejects file-root, proxy/FastCGI, authentication and write
  directives; `WriteContext` rejects backslashes, control characters and a
  second Dockerfile. Traefik route/health paths reject ambiguous dot segments
  and percent encoding; SFTP homes reject ambiguous separators, encoding and
  control characters. Table and fuzz seed corpora cover injection and
  traversal. A deny-list still is not a complete nginx parser or runtime
  sandbox; no real image or service was run.
- Reviewed the nginx/PHP catalog against what is available offline. No
  upstream documentation snapshot or Alpine package index is in this
  worktree, so **every selectable entry remains `unverified`**, including
  version availability and the PHP 8.2/8.4 exclusions. The exact entries and
  evidence needed to promote one to `verified-by-doc` are in
  [IMAGEBUILD-CATALOG.md](IMAGEBUILD-CATALOG.md). No network request or build
  was made. The browser catalog remains a separate copy.
- Added manually triggered `govulncheck` and `gosec` jobs to `build.yml` with
  the existing SHA-pinned checkout/setup-go actions, read-only permissions
  and fixed Go tool versions. No Actions job or tool download ran; version
  availability, Go compatibility and findings are unknown.
- Reviewed secret-shaped formatting and request-body error paths across
  `internal/`, including API, manager, imagebuild, service and Docker client.
  Authentication failure logs no longer
  include verifier errors that could echo a supplied credential; Docker
  builder errors no longer echo a runner error that could carry a registry
  password. Docker daemon error bodies are discarded because they can echo
  the base64 contents of secret/config requests; typed HTTP status mapping
  remains. Synthetic-secret tests check responses, logs, audit and Docker
  errors. Other
  generic errors and plan exports still need ongoing review as code changes.
- Login begin and finish now have separate per-peer-IP lockout keys, using the
  existing five-attempt/15-minute policy; setup-token guessing already used
  that policy. Tests cover lockout, `Retry-After`, expiry and token secrecy.
  OpenAPI and both security-model guides describe the new `429` responses.
  The limiter is process-local and reset on restart/leader change; it is not
  a distributed anti-abuse control. Setup token is a one-time 256-bit value
  stored only as a digest, but whoever can read the initial manager console
  can use it before setup finishes.
- Added the extension threat model to [ARCHITEKTUR.md](ARCHITEKTUR.md#threat-model-for-extended-features-2026-10-25)
  and updated both image-build guides, both security guides and TESTING. No
  Swarm, Docker daemon,
  registry, CA, AI provider or other infrastructure was contacted.

Actual final-tree checks (Go used `GOCACHE=/private/tmp/dbcm-cx-security2-cache`
and `GOMAXPROCS=4`):

- `gofmt -l .`: exit 0, no output.
- `go vet ./...`: exit 0, no output.
- `go test -race -count=1 -p 4 ./...`: exit 1; 25 packages passed, 19 failed,
  four had no tests. Every reported test failure has immediate TCP/Unix
  `bind: operation not permitted` or `httptest` listener evidence in this
  sandbox. The full suite is **not passing** here.
- Focused `go test -race -count=1 -p 4` for the new security tests and fuzz
  seed corpus in `internal/imagebuild`, `internal/service`, `internal/api`
  and `internal/dockerclient`: all four packages passed (1.159 s, 1.243 s,
  1.281 s, 1.135 s). The complete
  `internal/imagebuild` and `internal/service` package suites also passed
  after the last code edit (1.931 s, 1.413 s); complete `internal/api`
  requires a local listener.
- `internal/dockerclient`'s fake-transport test for daemon error-body
  reflection passed with `-race` (1.260 s). Its full package suite still
  needs local listeners; the new test used none.
- The OpenAPI route/parser contract tests passed in `internal/api` with
  `-race` (1.176 s).
- `python3 scripts/check_docs.py`: `Checked 74 Markdown files and 69 requirements; 0 errors.`
- `python3 scripts/check_deploy.py`: `deploy checks passed (static only; no image was built)`.
- `python3 scripts/test_check_deploy.py`: 12 tests passed, `OK`.
- `git diff --check`: exit 0, no output.
- No frontend code changed; frontend check/test/build were not run.

Small commits were attempted, but `git add` could not create the shared
`.git/worktrees/cx-security2/index.lock` (`Operation not permitted`). No
commit or push was made; the reviewed changes remain in this worktree.

Next: commit the reviewed changes where Git metadata is writable; check
catalog names against exact upstream image and Alpine release
documentation, run the optional CI scans and the complete race suite in an
environment permitting local listeners, then review any findings before
claiming security or release readiness. W05/R1 gates remain open.

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

E1, E2 and E3 ran on Lima VMs (arm64 only). E1/E2: Swarm with pinned Galera services, host loss/return, empty-volume trap (a marker guard is required), minority partition. E3: three `bin/metad` processes over the lab network - leader process kill, leader VM force-stop, partition via iptables, restart of all nodes from disk, majority loss. Observed once each: election 1.6-2.4 s, commit latency p50 1.4 ms / p99 4.3 ms (3 nodes, ~300 commits), old-term writes rejected by the term guard, no committed write from the isolated leader, no term decreased after full restart. Details: `tests/p0/results/`, ADR 0003/0004.

Findings that change the design: under heavy host load (load average ~800) elections and `limactl shell` calls slowed down and a script hit a stale leader, so the product needs tuned timeouts and a controller that re-resolves the leader and retries. Lima forwards guest loopback ports to the host loopback, so unauthenticated lab harness endpoints were reachable on the host. Not tested: Raft snapshot restore on VMs, membership changes, TLS, asymmetric partitions, backup restore, the real `internal/agent` over a network, proxy/VIP, full-cluster outage, AMD64, resource exhaustion. The P0 exit gates are NOT met.

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

## Third batch (2026-10-22)

- `internal/manager` + `cmd/manager`: replicated document store (Raft, CAS versions) in the metastore; embedded metastore node (single-instance = one-member group); leader-only loop with barrier and runner takeover; persistent accounts/sessions/passkeys/roles/audit (AES-GCM at rest; key provisioning across replicas unsolved); Swarm connector with a compatibility matrix seeded only with Docker 29.x; idempotent agent deployment as a global service; cluster creation/replacement/diagnosis operations. Only with a `manager` config section; the default prototype mode is unchanged. Everything runs against fakes: no real Docker/Swarm, no health collection from agents, agent endpoints are unauthenticated lab HTTP and unreachable across hosts.
- `internal/galeraprobe` (go-sql-driver/mysql v1.10.1, MPL-2.0): wsrep status prober and runner checker; the proxy's `sync: galera` mode now works against a fake MySQL server only.
- `internal/backup`: real S3 (own SigV4, stdlib) and SFTP (pkg/sftp) backends tested against in-process fakes only.
- `internal/metrics`: own time-series engine (WAL, blocks, rollups, archive interface, permission-filtered queries); no collectors, no HTTP API, single node.
- `web/`: R1 UI (shell, setup/login/recovery with passkeys, swarms/clusters/operations/events pages, German/English). Rendered once in headless Chromium with a virtual authenticator (35 scripted checks); no real authenticator, no Safari/Firefox, no screen reader. The API exposes no cluster node roles or last verified restore, so these show as unknown.
- E3 on VMs (see W05 progress). A full verification on this tree: `go vet`, `gofmt`, `go test -race ./...` (30 packages), `npm run check/test/build`, `check_docs.py`, `check_deploy.py` all pass.

### What R1 still lacks (so no Swarm trial yet)

Update 2026-10-24: data-key rotation, binlog/PITR planning and membership changes now have fake-only groundwork; see "Key rotation, PITR and membership groundwork" below. This does not provide continuous archiving, real PITR or default-runtime membership administration.

- No end-to-end run of the manager creating a Galera cluster on a real Swarm through its own API; the agent is not authenticated and not reachable across hosts; no health collection; no TLS/mTLS between manager, agents and metastore peers.
- Manager-config backup is a library; nothing schedules it, no cluster data backup/restore (physical/logical), no PITR, no verified full restore.
- No ACME, client-facing database TLS is only a proxy feature without certificate issuance wiring; no email/webhook wiring of `internal/notify` into the manager; no metrics collection; no drift handling, maintenance mode, spare-host workflow, update workflow, migration, or phpMyAdmin deployment beyond data structures.
- P0 gates not met: proxy/VIP on VMs, full-cluster outage, AMD64, resource exhaustion, restart during cleanup, real agent on VMs.
- Update 2026-10-23: several items of this list now have code verified with fakes only (agent mTLS, metastore peer mTLS, data-key distribution, deny-list distribution, logical backups, retention, drift detection, host maintenance); see "R1 backend gaps closed" below. None of them has VM evidence yet.

## Fourth batch (2026-10-22/23)

Merged: authenticated agent transport (mTLS 1.3 with CSR enrollment, one-time tokens, CA fingerprint pinning, revocation, renewal; ADR 0010), host-network agent service and address resolution, `collect-health`/`read-grastate` agent commands and a leader-only `HealthCollector`, `start-db` volume-name fix; cluster backup/restore engine `internal/dbbackup` (ADR 0009: mariabackup physical backup plus per-database logical export, new-cluster restore, isolated inspection restore, selective-restore preview, verification record, cron-like schedule, encrypted streaming format; backup jobs as one-shot owned Swarm services); monitoring pipeline (`internal/collect`, `internal/alerts`, dashboards/metrics/notifications API); second UI batch (hosts, cluster wizard, cluster detail, backups, dashboards with SVG charts, notifications, settings). A SFTP retry bug that could upload a truncated object was found and fixed during this work.

Test stability: three tests that flaked under parallel load were root-caused as test defects (shared random source across concurrent probes; Raft timeouts of 100 ms with fsync under the race detector) and fixed in the tests; production defaults unchanged. Evidence: 10 consecutive full-suite rounds under artificial load. Note: that stress run temporarily exceeded the 50% CPU ceiling of AGENTS.md; later stress runs must respect it.

**Still true: nothing has run against a real Swarm with the manager, the real agent over mTLS, real mariabackup or real MariaDB behind the proxy.** All new subsystems were exercised only with fakes and loopback. Open items that block the Swarm trial: E4 (manager + agent create a Galera cluster on lab VMs), health endpoint/monitoring account inside the Galera spec (the collector needs a socket or published port that the spec does not provide yet), data-key distribution across replicas, metastore peer encryption, agent deny-list distribution, API endpoints the UI assumes (clusters create/replace/outage-diagnosis, hosts, backups, notification settings) mostly lack HTTP handlers, `cmd/manager` does not construct the monitoring service, no job image for backup tools, PITR, ACME, update workflow, drift handling, maintenance mode, migration, AMD64 evidence, bilingual user guides.

### E4 result (2026-10-23, lab VMs, arm64)

The real manager and three real mTLS agents built a 3-node Galera cluster on a lab Swarm (1 manager, 2 workers) through the manager API; the last from-scratch run ended with exit 0. Observed: operation `succeeded` in about 80 s; all nodes Synced, cluster size 3, same state UUID; writes on n1/n3 read on all nodes; monitor and backup accounts created and replicated; SST via mariabackup over the relocated socket; node kill detected within 10 s and healthy again after 40 s; physical backup to SFTP (about 15 s); inspection restore with approval digest (wrong digest gave `409 approval_mismatch`) and a readable restored instance. 14 product defects were found and fixed on the way (F1-F13, details in `tests/p0/results/e4-manager-galera.md`). Timings are only conditionally reliable: the host carried foreign load (1-minute load 26-62 in the final run). VMs were removed with `bin/lab cleanup -apply`; unrelated resources were intact.

Still broken or untested after E4: no API to delete or discard a cluster (a failed create keeps name and capacity); restore and inspection jobs on worker hosts would hit F12 (only backup jobs are delegated to a manager-node agent); restore verification is not wired; the Galera position is missing from the backup manifest; backup manifests carry plaintext chunk hashes (open security question for ADR 0009); no event or notification for a node outage; the operations API shows no progress while a step polls; Lima exposes guest listeners (including the unauthenticated metastore harness port) on host loopback. Not run: node replacement, outage diagnosis, backup scheduler, logical backup, restore into a new cluster, certificate renewal/revocation, several manager replicas, the UI, amd64.

### E4 gaps closed (2026-10-23, fakes and unit tests only)

Implemented, each verified **only with fakes** (fake agents, in-memory Docker fake, `httptest`, one in-memory Raft replica, synthetic xbstreams); nothing ran on the lab VMs or against real Docker, mariabackup or MariaDB:

- **Cluster delete/discard**: `DELETE /clusters/{id}` plans a `cluster/delete` operation whose removals wait for a digest-bound approval; starting and approving it need the **delete** permission on the cluster (only the administrator preset; denied by default for operator, database admin and a custom restore role; every decision audited). The agents remove, under the ownership allow-list, each node's database service by name and cluster label (`remove-cluster-service`), jobs of unfinished backups, untorn inspection instances with their init secrets, and node volumes after the identity check (`remove-volume` with `missing_ok`/`allow_unmarked` for failed creates; `volume-in-use` retried through the new `runner.Step.RetryCodes`). Failed operations of the cluster are aborted first; an unfinished one answers 409 busy. On success the record and backup policy are deleted, releasing name and capacity (tested: the same name can be created again). Operator objects, host data directories of local volumes and backend backups are kept.
- **F12 for restores and inspections**: restore jobs, their removal and the inspection instance service run on the Swarm manager node's agent; the job is pinned to the target host (`RunRestoreArgs.NodeID`) after a read-only `check-restore-target` there (identity marker, empty datadir). Also in `ClusterRestorePlan`.
- **Galera position**: when `--extra-lsndir` has no `galera_info` (MariaDB 11.4 in E4), the job reads it from the plaintext xbstream and records it with `source: stream`. Assumption to confirm on the lab: 11.4 puts the file into the stream.
- **Node outage events**: the health collector records an event and calls `Monitoring.Notify` (wired in `cmd/manager`) when a node stops or starts being healthy, once per transition; suppressed while a deletion of the cluster runs.
- **Operation progress**: `GET /operations` carries `progress` (current step, target, attempts, latest unmet precondition or retry reason, poll count), refreshed while a step polls.
- **Restore verification and teardown**: the `restore-verified` gate probes the inspection instance through its host's agent and records an `instance`-level report (stream digests, instance answers, not in a Galera component; table contents not compared); a passed report sets `lastVerifiedRestoreAt`. The init file now leaves the restored `dbcm_monitor@localhost` unlocked for that probe. Teardown removes the init secret (`remove-secret`) after the instance, on the manager node's agent.

Checks run on the branch: `gofmt -l .` (no output), `go vet ./...`, `go test -race -count=1 -p 4 ./...` (35 packages ok), `python3 scripts/check_docs.py`. Remaining limits: none of this ran on VMs; a deletion keeps the host directories of local volumes; an operation that succeeds in a resumed run still skips its `start` callback (create service IDs, replacement desired state; only the deletion moved its completion into `saveView`); verification does not compare tables; the manifest still carries plaintext chunk digests (ADR 0009 question).

### E5 and E6 result (2026-10-23, lab VMs, arm64)

On the E4 lab (real manager, mTLS agents, 3-node MariaDB 11.4 Galera), final run from scratch with exit 0 ([results](../tests/p0/results/e5-e6-proxy-outage.md)). Foreign host load was low (1-minute 5-10).

- **E5, proxy:** `cmd/proxy` as a lab container in front of the real cluster, Galera sync state through a monitoring account. Writer selection, round-robin reads that never hit the writer, hysteresis (no flap back after the old writer returned), planned switch by reload with the 10 s drain, and TLS termination (go-sql-driver and the MariaDB 11.4 CLI with verification; plaintext refused) worked. A killed writer: the proxy switched within 0.2 s, but no write succeeded for 6.2 s (Galera's failure detection); when the returning node took SST the new writer became donor and the proxy switched a second time, closing long-lived connections at the drain deadline. Probes are aborted handshakes on the nodes (about two per second and node, one log line each).
- **E6, full outage:** clean shutdown (all `safe_to_bootstrap: 0`, equal seqno) and crash (all seqno -1). The read-only diagnosis was correct in both cases (tie at equal seqno; no candidate after the crash with "run --wsrep-recover"), and the manager changed nothing for 120 s. New: `POST /clusters/{id}/outage-diagnosis` recovers positions with `mariadbd --wsrep-recover` in one-off agent containers, and `POST /clusters/{id}/outage-recovery` runs the guided plan only after a digest-bound approval. Both recoveries succeeded (40 s and 61 s after approval) with data and cluster state UUID kept.
- Fixed with tests: no executable recovery (F-E6-1), no position recovery (F-E6-2), the `--wsrep-recover` command line (F-E6-3), a crash recovery that silently started a new cluster history from `seqno: -1` (F-E6-4), explicit `skip_name_resolve` (F-E5-2).

Still open after E5/E6 (the first two closed with fakes on 2026-10-23, see below): no event/notification for a full outage; recovery approval uses the restore permission; joiners always take SST; proxy deployment by the manager, node TLS and proxy-to-node TLS; probe log noise; amd64. Redundant proxies/VIP and VM power loss: see E8 below.

### E8 result (2026-10-24/25, lab VMs, arm64): redundant proxies with a VIP, VM power loss

Run with `tests/p0/e8-proxy-redundancy-powerloss.sh` ([results](../tests/p0/results/e8-proxy-redundancy-powerloss.md)). Run 1 completed every phase; run 2 (B1-B3 only) hung after the B2 observation and was killed, so it is partial evidence.

- **Gate A (two proxies on m2/m3, keepalived VIP, client-side failover list):** the VIP moved in 2-3 s; clients lost writes for 2.4-3.9 s when a proxy container was killed and 6.4-7.2 s when the holder VM was hard-stopped (dominated by Galera failure detection, same for VIP and failover-list clients). No acknowledged write was lost; a few writes were committed although the client saw a timeout. **Finding:** with `single-writer-stable` two independent proxies chose different writers (n2 and n3); only a common `prefer-node` gave one writer. The coordinated follow-up below has not been rerun on VMs. `docker kill` does not trigger the Docker restart policy.
- **Gate B (`limactl stop --force`):** one node VM: rejoined 21-60 s after the VM start, sentinels intact, no manager action. All three VMs (also staggered): the manager did nothing in 150 s (no bootstrap, no spec change, no operation); Galera restored the primary component by itself from `gvwstate.dat`, so the guided recovery was **not exercised** after a VM power loss (it is covered for container SIGKILL in E6). Open: in run 1 the third node did not rejoin within 600 s in B2 and B3 (repeated exit-1 task failures, cause not diagnosed), and the later diagnosis answered "no Galera state".
- Resource measurements per gate are in the log and the results file. Limits: arm64 only, coarse VIP sampling (2-3 s), one environment (Lima user-v2).

### E8 coordinated writer follow-up (2026-10-25, fakes only)

[ADR 0017](decisions/0017-coordinated-proxy-writer.md) and the DE/EN configuration guides now describe a replicated manager writer choice. `manager.proxyWriters` configures an initial writer, fixed candidate and member IDs, per-proxy token files and an administrator token file. The manager exposes leader-only choice, ACK and audited switch endpoints. The proxy's optional `coordination` section polls the leader, uses only its published writer, keeps its last choice when the manager is unreachable, and still rejects locally unhealthy nodes. The optional read endpoint is disabled in this mode because it cannot enforce read-only SQL. A switch commits a no-writer freeze, closes existing write connections, waits for every configured proxy's acknowledgement and only then publishes the new writer. An unreachable member blocks the switch; optional `autoFailover` uses fresh majority/Primary/Synced observations to start the same freeze, while the default remains manual. There is no automatic member removal. Independent legacy proxy policies remain unsafe for redundant write endpoints.

Verification in this sandbox: focused `go test -race -count=1 -p 4 ./internal/api ./internal/manager ./internal/proxy ./cmd/manager -run '^Test(RoutesMatchOpenAPI|ProxyWriter|Coordinated|Coordination)'` passed in all four packages. The tests use in-memory three-node Raft, a fake HTTP writer source, scripted backend probes and `net.Pipe`; they cover divergent local latency, health flapping, fresh-health automatic failover, manager partition, freeze across leader change, stale epochs, member/token rejection and existing connection closure. `gofmt -l .` returned no files; `go vet ./...` passed. `python3 scripts/check_docs.py` reported 74 Markdown files, 69 requirements, 0 errors; `python3 scripts/check_deploy.py` passed (static only). The **final** `go test -race -count=1 -p 4 ./...` returned exit 1: 25 packages passed, 4 had no tests and 19 failed because this sandbox forbids TCP and Unix socket listeners (`bind: operation not permitted`). The first run also found an OpenAPI route-table mismatch from this change; it was fixed and the contract test passed in the focused rerun, with no such mismatch in the final full run. No real manager, proxy, Galera, Swarm or network infrastructure was contacted. The protocol needs a socket-permitted full suite and a controlled E8 lab rerun before the P0 gate can close.

### R1 backend gaps closed (2026-10-23, fakes and unit tests only)

Implemented on branch `worktree-agent-a1a05632b4394fb3f` (from `integration` a5e3fff). Every item was verified **only with fakes** (in-memory Raft, fake agents, an in-memory Docker/Swarm connector, `httptest`, loopback TLS); nothing ran on VMs, against real Docker, MariaDB, mariabackup or several manager replicas.

- **Outage recovery permission and notification (E6 gaps):** approving a `galera/full-outage-recovery` operation needs the new `recover` action on the cluster (administrator preset only; operate and restore no longer cover it; requesting the plan still needs operate). The health collector records a critical event and sends a `health.cluster_outage` notification when a cluster becomes unavailable (no Primary component), and `health.cluster_outage_ended` when it recovers; nothing is planned automatically.
- **Data-key distribution and recovery ([ADR 0013](decisions/0013-replica-keys-peer-tls-and-revocation.md)):** `manager.keyring` stores the data key only wrapped, per replica identity (`<dataDir>/replica.key`) and for the backup recovery key; an existing key file is adopted. Replicas are registered by an administrator (`GET /settings/replicas`, `PUT /settings/replicas/{id}/data-key-recipient`; voters only, audited); a leader without the key hands leadership over. `RecoverDataKeys` recovers keys with the recovery key alone.
- **Metastore peer mTLS:** `manager.peerTLS` (TLS 1.3, peer CA, `dbcm://<installation>/metastore/<id>` identities, expected replica per address, revocation hook); `manager -issue-peer-certs` creates the peer CA and certificates. Tested with three TLS nodes on loopback and with foreign-CA, foreign-installation and revoked certificates.
- **Deny-list distribution and revocation:** agent command `set-deny-list` (persisted, monotonic revision, used by the agent's mTLS listener); the leader pushes the replicated revocation list to every enrolled agent; manager certificates are recorded per replica and `POST /settings/replicas/{id}/revoke-certificates` denies them. End-to-end test on loopback: enrollment, renewal, distribution, a revoked manager certificate refused by the agent's TLS handshake, the reissued one accepted.
- **Backup manifest format 2 ([ADR 0009 amendment](decisions/0009-cluster-backup-method.md)):** chunk and plaintext digests are HMACs keyed from the per-stream data key; format 1 manifests stay restorable and verified.
- **Host maintenance (A08) and drift:** `GET /hosts/{id}/maintenance-plan`, `POST`/`DELETE /hosts/{id}/maintenance`: graceful one-at-a-time stop after sync checks of the rest of the cluster, refusal without a majority outside the host, replacement paused, readiness reported, identity-checked restart and wait for Synced. The product never reboots or drains a host. `GET /clusters/{id}/drift` and a periodic detector compare cluster records with Swarm services (missing, recreated, foreign labels, placement, stopped, not running, unexpected) and propose manual actions; nothing is changed automatically.
- **Backups:** scheduled and manual logical backups (`method: logical`, databases) and a retention policy (`keepLast/keepDaily/keepWeekly`) applied by the scheduler (newest, newest verified and inspected sets kept; skipped while an operation runs; manifest deleted before stream). End-to-end test over four scheduled days with fake jobs writing to an in-memory backend.

Checks run on this branch: `gofmt -l .` (no output), `go vet ./...`, `go test -race -count=1 -p 4 ./...` (38 packages ok), `python3 scripts/check_docs.py` (0 errors). Frontend checks were not run (no UI change). Not done: PITR; key rotation and re-encryption; metastore membership changes; peer certificate renewal; UI for the new endpoints; bilingual user guides; any VM evidence for these features.

## UI aligned with the lifecycle API (2026-10-23)

`web/` now follows `api/openapi.yaml` for: operation `progress` (step bar, current step, host, waiting reason, attempts) and the phases waiting-for-approval and paused; approval with the digest, preceded by the documented step list for `cluster/delete`, `galera/full-outage-recovery` and inspection restores (other kinds say the API lists no steps); cluster delete (`DELETE /clusters/{id}`) with typed cluster-name confirmation, then the plan and approval in the operation card; node replace with a followed operation and the quorum explanation; full-outage diagnosis (safe_to_bootstrap, highest seqno, optional `POST outage-diagnosis` position recovery), `POST outage-recovery` for the proposed candidate and approval of the returned plan; start backup, backup policy edit (cron shape and time zone checked client-side), restore verification note; notification settings page (`/notifications/settings`: minimum severity, webhook with allow-private, SMTP, write-only secrets, test send); recent events per cluster (node outage events appear there when the manager emits them); severity `error`. Pure logic is in `web/src/lib/lifecycle.ts` with vitest tests (94 tests in total pass); `npm run check`, `npm run build` and `scripts/check_docs.py` pass.

Deviations and gaps: at that time the host table could not show an agent version or last seen because the API had neither; since 2026-10-24 it does (see "UI gaps closed (W06)"). The UI never ran in a browser or against a real manager in this batch, and has never been used with a real authenticator or Firefox/Safari; the dev mock server was not extended for the new endpoints (later extended for outage recovery, deletion and progress; see "Icon, screenshots and landing page"). The plan step texts are static descriptions of the documented behavior, not read from the operation. Approval digest-binding, permission differences (delete versus restore) and `approval_mismatch` handling are only unit-tested at the client level.

## UI gaps closed (W06, 2026-10-24)

Most UI for the R1 backend endpoints already existed (commit `47b5a14`, mock only): services and Traefik plan, image builds, registries, host maintenance, drift report, replica keyring, logical backup policy with retention. This batch checked those against the API and closed the two remaining gaps, verified with unit tests, `svelte-check`, the mock server and mock screenshots only (no real manager, no real browser session against a real backend):

- **Operation `progress.lastError`** is shown in red under the current step on the Operations page (`progressView` carries it; the mock operation has one). DE/EN strings, vitest cases.
- **Host agent version and last answer:** the API had neither, so `api.Host` got `agentVersion` and `agentSeenAt` (OpenAPI, contract test `TestHostSchemaListsEveryField`). Source: every answer of the agent command endpoint carries the header `X-Dbcm-Agent-Version`; `AgentDirectory` records version and time on the host record after any HTTP answer, best effort and at most about once a minute (`TestAgentEnrollmentCommandsRenewalRevocation` covers it over mTLS). This is not a heartbeat: nothing contacts idle agents, so an old value is not proof of an outage. Older agents without the header record an empty version.
- Screenshots `{en,de}-light-hosts.png` and `{en,de}-light-operations.png` re-captured from the mock server (mock data banner visible).

Not done: no health-check polling of agents, no UI test in a real browser beyond the mock captures, the Hosts table needs horizontal scroll at 1440 px width (last columns).

## Icon, screenshots and landing page (2026-10-23)

- **Icon:** product-neutral icon (three connected database cylinders) as an Icon Composer document `assets/icon/AppIcon.icon`, written by hand and rendered headlessly with the `ictool` bundled in Icon Composer (PNG 16–1024 px, light and dark). Derived favicons (SVG, ICO, apple-touch PNG) are used by the web UI (`web/static/`, `app.html`, sidebar brand) and the Pages site. Not opened in the Icon Composer GUI.
- **Screenshots:** 23 PNGs (about 1 MB in total, each below 70 KB) in `docs/assets/screenshots/`, captured with Playwright/Chromium against `npm run dev:mock` only; they carry the MOCK DATA banner and are captioned as mock-server screenshots everywhere. The dev mock now also answers `POST /clusters/{id}/outage-recovery` and `DELETE /clusters/{id}` and reports `progress` for a running backup (development-only; the production build contains no mock code, checked by grepping the build output).
- **Docs:** README with icon, hero screenshot and a feature table with verification levels; `docs/index.md` is a landing page (`_layouts/landing.html`, `assets/css/landing.css`, favicons through `_includes/head-custom.html`, `logo` for the minimal theme); screenshots in the EN/DE user guides 01–07; the Pages workflow also triggers on `README.md`.
- **Checks run:** `npm run check` (0 errors), `npm test` (94 passed), `npm run build`, `go test ./internal/webui/`, `python3 scripts/check_docs.py` (0 errors). **Not run:** a local Jekyll build (no Jekyll on the host); the landing layout is verified only when the Pages workflow runs. The UI has still never run against a real manager.

### E4 re-run (2026-10-23): prepared, not executed

`tests/p0/e4-rerun.sh` covers the following on the E4 lab: the E4 gap work, capacity denial, a full volume filesystem, restart during cleanup (manager SIGKILL and agent kill during a deletion), and three manager replicas on m1..m3 with leader SIGKILL during a create and during a backup. It did **not run**. Foreign load on the host (Xcode, iOS simulator tests, iCloud) stayed at a 1-minute load of 30-970 for about 4.5 h, and the start threshold (finally 30/40) was never reached for the length of a run. No VM was created and the unrelated Lima/Colima resources were unchanged ([record](../tests/p0/results/e4-rerun-not-run.md)). A finding from code reading: replicas need their initial membership in `manager.peers`, and there is no membership change, so a single-instance installation cannot become HA without being reinstalled. Every replica also needs a local Swarm-manager Docker socket. The script promotes m2/m3 and starts a second installation for this reason. amd64: emulation on this arm64 host cannot give native evidence. The native strategy (an owner-provided x86 host or VM, or a GitHub-hosted runner for single-host checks) is undecided.

### E4 re-run, Part A attempt 2 (2026-10-23/24, lab VMs, arm64): failed in `failcreate`

Record: [e4-rerun-A2.md](../tests/p0/results/e4-rerun-A2.md), raw log `e4-rerun-A2.log`. Host load 541-990 during the run; timings are unreliable.

- **Passed:** build, vms, swarm, images, prereqs, rprep, backend, manager, setup, swarmapi, rcluster (`e4db` 23/23 steps; `05-exec-sql-n1` failed on attempts 1-8 before passing), verify, rbackup (Galera position `{"seqno":19,"source":"stream"}` in the manifest), rinspect (inspection restore on worker m2 with approval, query and teardown), capacity (two `no_capacity` 409s and one 400, no services created).
- **Completed without an explicit assertion:** rfailure (node-outage events naming n3: 3; nodes healthy again).
- **Failed:** failcreate. The create with an unpullable image stayed in `05-exec-sql-n1` (exec-sql on worker m2, attempts up to 21, only "idempotent step failed; retry N" visible). `abort` returned 202 but the operation stayed `running` for 300 s; the same-name create was refused correctly (409 `cluster_exists`); the delete was refused (409 `busy`), and the script then exited 1.
- **Not reached:** cleanuprestart, diskfull, sentinels, deleteall; Part B not run. Scoped cleanup removed exactly `dbcm-lab-m1..m3`; the three unrelated resources were intact.

### failcreate findings fixed (2026-10-24, fakes and unit tests only, not re-run on VMs)

- **Abort during retries (root cause from code):** the runner checked an abort request only before a step and while polling preconditions; the retry loop of an idempotent step (exec-sql, up to `operationRetries` = 30 retries with backoff up to 30 s, about 13 minutes) never looked at it. Now the abort is checked before every (re)attempt, interrupts the retry backoff, and cancels an in-flight command of an idempotent kind. The cancelled step stays **Unknown** in the journal and the abort reason names it ("outcome unknown for step(s) ... (inspect live state)"); an observed failure stays Failed; nothing is recorded as succeeded. A destructive command in flight is still awaited (bounded by the step timeout) so its outcome stays observable. After the abort the locks are released and the cluster deletion is accepted. Tests: `internal/runner/abort_test.go` (backoff, in-flight, in-doubt retry; they fail against the previous runner) and `internal/manager/abortretry_test.go` (create, abort while exec-sql retries or is in flight, then delete succeeds and the name is free).
- **Error text:** retry reasons now carry the agent's answer (`idempotent step failed: failed (code): message`), the operation view's `progress.lastError` holds the latest failed or unanswered attempt of the current step, and the manager logs `runner: step retry` (WARN, with op, step, retry number, backoff, reason) and status changes (failed/stopped WARN, paused/aborted/succeeded INFO). Texts pass `runner.Redact` (SQL `IDENTIFIED BY '...'`, `password=`/`token:`-style values, DSN passwords; bounded to 1 KiB) on top of the agent's own secret redaction; the patterns are a best effort. The Operations page now shows `lastError` (mock-verified, see "UI gaps closed (W06)").
- **Why exec-sql kept failing (inferred, not observed):** the e4fail service had no container on any host (`owned_objects` before the abort) because its image cannot be pulled, so the node's database socket never appeared. exec-sql then answered `refused (invalid-path): database socket: lstat ...` on every attempt (the same error class was visible for the wait-synced steps in rcluster). The agent now reports a missing socket as `failed (db-not-running)` with an explicit message, and the runner names the retry "dependency not running: the database on <host> is not running". Not done: reading the Swarm task state and its error (for example "No such image") from the manager or agent, and waiting on it as a precondition instead of retrying exec-sql; neither component can list tasks yet. The next lab run must capture `docker service ps --no-trunc` of the e4fail services, `progress.lastError`, and the manager's `runner: step retry` lines; `op_diag` in the script now records all three.
- **Script:** `tests/p0/e4-rerun.sh` failcreate, cleanuprestart and deleteall assert expected refusals and outcomes (`assert_fail`) instead of leaving them to `set -e` (A2 most likely exited at the refused delete because `delete_cluster` returned 1 under `set -e`); the exit status is 1 when any assertion failed, and the later phases still run. `opline` shows `lastError`. Only `bash -n` and `shellcheck` were run.

### E4 re-run A4 and B2 (2026-10-24, lab VMs, arm64): all steps passed

Records: [e4-rerun-A4.md](../tests/p0/results/e4-rerun-A4.md), [e4-rerun-B2.md](../tests/p0/results/e4-rerun-B2.md) with raw logs. Attempt A3/B (same day, records `e4-rerun-A3.md`, `e4-rerun-B.md`) had failed in `failcreate`, `cleanuprestart` and `hamanager`. Host 1-minute load 124-165 during both runs (foreign); timings are single samples.

- **failcreate (A3: state `failed` after the abort).** Cause: the API mapped `runner.StatusAborted` to `failed`; the fakes tests asserted the runner status, not the API state. Now the API has the state `aborted` (`api/openapi.yaml`, UI status, phase and finished checks). Semantics: an operator abort always ends `aborted`; when it coincides with a step failure, the reason keeps it (`aborted at a safe boundary; a step had failed: ...`); a failure without an abort stays `failed`. Tests: `internal/manager/abortretry_test.go` asserts the API view, `internal/runner/abort_test.go` the abort after a failed step. A4: the create ended `aborted` at once; deletion and re-create of the name succeeded.
- **cleanuprestart (A3: `volume is in use` after the restart).** Cause: the volume step was in its retry backoff (Docker removes the removed service's task containers asynchronously; a stopped container still holds the volume) when the manager was SIGKILLed; the journal only said `failed`, and the retry budget lived in memory, so the new leader failed the deletion. The stored definition did keep the retry codes. Now the journal records a due retry (`FailForRetry`, `Step.Retry`) and the agent step ID of each attempt (`Step.AgentStep`, used to query an Unknown step under the ID that was sent); a new leader retries a due retry under the next agent step ID and counts the earlier attempts against `MaxRetries`; a plain Failed step stays failed. New fields are omitted when unset (journal commands and snapshots encode as before). Tests: `internal/runner/retrydue_test.go`, `internal/manager/deleterestart_test.go` (three replicas, leader closed during the backoff, volume busy three times then free), `internal/opjournal/retry_test.go`, `internal/metastore/retrycmd_test.go`. A4: the same situation (`att=2 ... volume-in-use` at the kill, task containers still on m3) ended `succeeded` after the restart (`adopted=1`). No separate "wait for containers" step was added: the remove-volume retry on Docker's 409 is that wait, and it is now durable.
- **hamanager (B: no replica led within 120 s).** Cause: the script wrote `"nodeId": "manager-$11"` in a remote `sh` snippet, which expands to `manager-<ip>1`; no replica was in its own peer list. The keyring hypothesis (fresh replicas without recipients cannot lead) is refuted: the first leader bootstraps the keyring. Now `manager` refuses a node ID outside `manager.peers`, duplicate peers and an own peer address different from `advertise`, and the metastore refuses such a bootstrap. The script uses the documented variant (`manager.keyring`, `manager.peerTLS` from `-issue-peer-certs`, recipients registered in `hasetup`) and captures every replica's journal before the cleanup on failure. B2: a replica led after 2 s; all three held the current key; leader SIGKILL during a create (new leader after 3 s, create resumed and succeeded, no duplicate) and during writes and a backup (30/30 writes, backup succeeded); deletion through the replicated manager succeeded.
- **Limits:** one run each; the replicas ran as plain processes (`systemd-run`), not as Swarm services; peer mTLS was configured and the quorum formed, but no handshake capture or wrong-certificate test ran on VMs; until the other replicas are registered as recipients, only the first leader can lead.
- **Cleanup:** both runs' exit traps removed exactly `dbcm-lab-m1..m3`; a final `bin/lab cleanup -apply` executed no actions; the unrelated `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm` were intact.
- **Checks run:** `gofmt -l`, `go vet ./...`, `go test -race -count=1 -p 4 ./...`, `make check` (incl. `svelte-check` 0 errors, `check_docs.py` 0 errors), `npx vitest run` (146 passed), `bash -n` and `shellcheck` of the script.

### E7 stack deploy (2026-10-24, lab VMs, arm64): all checks passed

`deploy/Dockerfile` built an arm64 image inside the lab VM, `manager init` (run from that image) produced the configuration, and `deploy/stack.yaml` ran as a Swarm stack with the manager pinned to m1 ([results](../tests/p0/results/e7-stack-deploy.md), [`tests/p0/e7-stack-deploy.sh`](../tests/p0/e7-stack-deploy.sh)). Verified: Docker 29.9.2 honours `cap_drop`, the interpolated `user`, the external `host` network and the config `uid`/`gid`/`mode`; health, UI and authentication (401 without a session, WebAuthn setup, 200 after); three mTLS agents enrolled (no client certificate refused on 7443); a 3-node Galera cluster created through the API; manager `docker kill` (leading again in 8 s, session and cluster record intact), `service update --force` (stop-first, 14 s) and drain of the pinned node (manager down while drained because the constraint allows no other node, Galera n3 and the agent on that node left too, everything recovered 54 s after the drain began, once the node was `active`). Not possible from the stack: replicas>1 and thus a peer-TLS refusal (one replica by design; the three-replica Swarm-service layout is still not run). No product code changed.

## Managed-mode Swarm packaging (2026-10-23, static checks and Go tests only)

- **Deploy files:** `deploy/stack.yaml` now runs managed mode: one replica pinned to a named Swarm manager node (`DBCM_MANAGER_NODE`), host network, API and healthcheck on `127.0.0.1:${DBCM_PORT}`, configuration as an external Swarm config (`DBCM_CONFIG_NAME`, `/etc/dbcm/manager.json`), local volume at `/var/lib/dbcm`, Docker socket mounted with uid 65532 and the socket group (`DBCM_DOCKER_GID`), read-only, `cap_drop: [ALL]`, stop-first updates, 1 CPU/512 MiB; optional backend secret commented out. `deploy/compose.yaml` is the single-host equivalent. `deploy/Dockerfile` ships `/var/lib/dbcm` owned by 65532. Decision and alternatives: [ADR 0014](decisions/0014-manager-container-docker-access.md). The three-replica variant (three pinned services, keyring, peer TLS) is documented in `docs/INSTALL.md`, not shipped.
- **`manager init`** (`cmd/manager/init.go`): writes a validated configuration skeleton (secret names only; never overwrites; 0600) and prints first-run instructions to stderr; `-peers` produces a replica config with keyring and peer TLS paths, `-backup -recovery-key-out` generates a recovery key into a new 0600 file and embeds only its public half. `deploy/manager.example.json` is its output; `TestShippedExampleConfig` loads it with `loadConfig`, validates it and checks that `init` reproduces it; `TestExampleConfigStartsManagedRuntime` starts the managed runtime from it (data dir and listen addresses moved to loopback/temp).
- **Checks:** `scripts/check_deploy.py` validates the new layout (pinning, socket guard, host network, loopback API, config/volume paths in step with `init.go`, external configs/secrets, example config without secret material). The old rule "no Docker socket" is replaced by the guard.
- **Not done:** nothing was built or deployed; whether the trial engine's `docker stack deploy` honours `cap_drop`, the interpolated `user` group, the external `host` network and config `uid`/`gid`/`mode`, and whether a new volume inherits the image directory's owner, is unverified. No leader-following proxy for HA; no manager-state backup.

## Scope broadened (2026-10-22)

The owner broadened the product to a general self-hosted platform for clusters and services ([ADR 0012](decisions/0012-broadened-scope-platform.md)): Swarm/Kubernetes including provisioning, web servers with detailed composition, storage protocol servers, generic containers, Traefik, image builds with a managed or external registry and Docker engine updates, all through the UI with visible plans. Documented as requirements X01–X13, proposed phases R6–R9 and work packages W07–W10 (all PENDING). Nothing of the extension is implemented. Galera stays priority 1; W05/W06 and the R1 gates are unchanged. A09 is partly superseded by X09; the Docker update boundaries, product name and other choices are open in [ENTSCHEIDUNGEN.md](ENTSCHEIDUNGEN.md#open-decisions-from-the-scope-extension).

## Image build and registry groundwork (2026-10-23, fakes only)

First code for W08 (R7), proposal level. New packages `internal/imagebuild` and `internal/registry`, plus API routes. Everything was tested with fakes only (`FakeBuilder`, in-memory stores, an `httptest` registry); no image was built, no Docker daemon, BuildKit or real registry was contacted, and no Dockerfile produced here was ever built.

- **Composition model and render.** Typed web server spec (nginx version, PHP-FPM version, PHP extensions from an allow-list with per-version availability, `php.ini` keys from an allow-list with value validators, structured nginx options, a custom nginx snippet limited to 8 KiB/200 lines with a directive deny-list and block allow-list, extra packages from an allow-list, platforms). The base image must be pinned by digest. Rendering is deterministic (sorted files and lists, no timestamps); equal intent gives equal spec and context hashes (tests include order independence). The version catalog, per-version extension availability and Alpine package names (`php83-<ext>`) are a proposal written without checking the real repositories; the first real build will show which are wrong.
- **Plan preview.** `NewPlan` returns Dockerfile, context files, ordered steps, warnings (unpinned Alpine packages, no scan or signature, deny-list-only snippet check) and a plain-language explanation. The plan digest covers spec, context, platforms and target; a build starts only with that digest (409 `approval_mismatch` otherwise).
- **Builder and jobs.** `Builder` interface; `DockerBuilder` (`docker buildx build ... --push`, registry credential in a temporary `0600` docker config, never on the command line) is unit-tested through command construction and a fake runner only. Jobs record spec hash, context hash, plan digest, base digests, image digest, SBOM placeholder (`not_generated`), status and a redacted, size-capped log. Redaction masks known secret values (also split across writes and in base64 basic-auth form) and common credential patterns; patterns are a best effort.
- **Registry.** `Registry` interface with an OCI distribution client (push, resolve, tag, delete, list with pagination; basic and bearer token auth). Credentials are `registry.Secret` values (never formatted or logged) stored through a `SecretStore`; configs hold only a reference. The client sends credentials only to the registry host (and `auth.docker.io` for Docker Hub), refuses upload or pagination redirects to other hosts, and puts no URL, token or body text into errors. Plain http only for loopback. The managed registry is a `ManagedSpec` plus `Descriptor()` (standard `registry:2` image pinned by digest, htpasswd or token auth, mandatory TLS, one replica with a local volume pinned to a host); deployment needs the future service framework and is not implemented. `DistributionPolicy` (managed or external, pull secret per Swarm, air-gapped) validates data only.
- **API.** `GET/POST /image-builds` (POST without `approvalDigest` returns the plan, with it starts the build), `GET/POST /registries` (external only), new authz resources `image_build` and `registry` (viewer: read; operator: also create image builds; registering a registry needs create on `registry`, administrator only), audit through the existing authorizer, OpenAPI entries, 503 until a service is wired.

Not done (after the wiring step below): UI against the real manager, a real build, push to GHCR/Docker Hub/`registry:2`, token-auth registry flows beyond the fake, multi-platform builds, SBOM, scanning, signing, image distribution to Swarms, managed registry deployment, garbage collection, the build-tool and registry decisions in ENTSCHEIDUNGEN.md, and a security review of the snippet deny-list (it is not an nginx parser).
### W08 wiring step (fakes only)

`cmd/manager` and `internal/manager` now construct the image build and registry services; `/image-builds` and `/registries` are backed by real objects. New in `internal/manager/imagestore.go`: replicated stores in the metastore for registry configs (`registries`, create-only so a duplicate ID is `ErrExists`), registry credentials (`registry-secrets`, sealed with the data key through `PutSealed`, same path as session digests), build jobs (`image-jobs`) and previewed plans (`image-plans`, new `imagebuild.PlanStore`, 200 kept). `imagebuild.Service` stores each previewed plan and has `RecoverInterrupted` (jobs left `running` by a restart become `failed`, called when a leader builds its API). The new `manager.imageBuild` section (`dockerBinary`, `workDir`, `maxLogBytes`; documented in the configuration reference) enables a `DockerBuilder` with the real `ExecRunner`; without it builds answer 503 and registries still work. `manager init -image-build` writes the section. Builds run asynchronously on the leader. Authz resources `image_build`/`registry` and audit were already enforced by the API and are unchanged.

Checks run: `go test -race -count=1 -p 4 ./...`, `gofmt -l`, `go vet ./...`, `scripts/check_docs.py`, `scripts/check_deploy.py` (results in the work package report). Tests with fakes: replicated stores on a real single-node metastore (restart persistence, password absent from every stored document, plan pruning, interrupted-job recovery, redacted log in the stored job), the manager runtime with a fake docker command (build through the real stores, wrong digest refused, log redaction), config validation, `init -image-build`.

Limits: no real docker, BuildKit or registry was contacted. The shipped manager image has no docker CLI and `deploy/stack.yaml` gives it no BuildKit, so builds do not work in the shipped deployment. The build runs as the manager process on the leader host. A build interrupted by leadership change is only marked failed, not resumed or cancelled. No API to read stored plans, to delete jobs or to change/delete registries; stored registry credentials of a deleted config are not purged (no delete exists). The API-level authz and audit tests still use in-memory stores; the manager-runtime test checks that the routes are mounted and require a session, not the role matrix against the replicated policy.

## W07 first step: service-type framework (2026-10-23, fakes only)

Branch `w07-service-types`. New package `internal/service` (pure domain code, no agent import): a registry with the `Type` interface (Validate, Plan with ordered steps, artifacts, warnings and blockers, Health that distinguishes unknown, DeletePlan), a generic `container` type (pinned image, env with secret-name refusal, Swarm secrets, networks, managed local volumes with identity marker, resources, placement, restart policy) and a `traefik` type (typed entry points, ACME resolvers, routers, backends and middlewares with a deterministic YAML preview of static and dynamic configuration; no field can hold a secret value). `internal/orchestrate.ServiceToAgentSpec` and `internal/runner.FromServicePlan` convert plans into agent specs and operations. `internal/manager.Services` and the routes `GET/POST /services`, `POST /services/plan`, `GET/DELETE /services/{id}`, `GET /services/{id}/delete-plan` (OpenAPI updated, route contract test passes) reuse the operation/approval machinery: every state-changing step waits for a digest-bound approval; approval needs `create` or `delete` on the new `service` authz resource (viewer and operator presets read only; deny by default; audited). Drift tests (`internal/service/agentcompat_test.go`) pin the mirrored constants, step kinds, field mapping and the port gap. Guide stub: [guides/en/13-services.md](guides/en/13-services.md).

Limits: tests use fakes only (fake agents, in-process store, `httptest`); nothing ran on a Swarm, Docker or Traefik, and the rendered Traefik YAML was never loaded by a Traefik. The agent cannot publish ports or deliver configuration files, so plans with published ports or Traefik dynamic configuration carry blockers and are refused at start. No health wiring, update strategy, UI or CLI. Galera is not yet expressed as a service type. (Superseded in part by the second step below and by W09, which added published ports and config mounts to the agent.)

## W07 second step: Galera type, updates, health, expert view (2026-10-24, fakes only)

Branch `w07-galera-type` (from `integration`).

- **Galera as a service type** (`internal/service/galera.go`): type `galera` in the registry. Its spec has the cluster creation fields; every plan is delegated unchanged to `internal/galera` (`PlanCreate`, `PlanReplacement`, `RecoveryPlan`, `MaintenancePlan`) and attached to the described plan (`Plan.Galera`). `runner.FromServicePlan` refuses galera plans, so they run only through `FromGaleraPlan` and the cluster flows. Size changes (`ErrGaleraScale`), the generic update strategy and spec-only deletion are refused. The manager routes `/services` requests of type galera to the unchanged cluster flows: creation shares the extracted `prepareCreateCluster` with `CreateCluster` (no step added or removed, cluster `create` permission, no approval step as on the direct path), deletion is `DeleteCluster` with its approval; clusters are listed as galera services. The API authorizes galera services as cluster resources, so a service-only role cannot read, create or delete them. Tests: identical `galera.Plan` and identical runner operation for the type and the direct path (`internal/service/galera_test.go`), identical stored definitions after ID normalization for `POST /services` and `CreateCluster` (`internal/manager/servicetypes_test.go`), replacement without quorum or with stale observations refused with the same `ErrRequiresGuidedRecovery`, recovery plan requires approval and is refused without an approval ID and by the generic conversion, maintenance identical.
- **Update strategy** (`internal/service/update.go`): `UpdateStrategy` (stop-first/start-first, parallelism, delay, monitor, max failure ratio, onFailure rollback/pause, canary field that only adds a blocker). Container and Traefik update plans: `update-service` (the existing agent command; the spec now optionally carries a Swarm `UpdateConfig`/`RollbackConfig`, nil keeps the previous encoding, digest and update behaviour, so Galera specs are unchanged), `await-service`, and a `check-health` gate (`runner.CondServiceHealthy`, evaluated live by the `RoutingChecker` from Swarm task state; fails closed without a Swarm view). start-first is refused with local volumes; replica, placement and volume changes are blockers (the agent keeps them). A prepared rollback plan is attached; `POST /services/{id}/update` with `rollback` plans back to the previous spec. The stored spec changes only after success.
- **Service health** (`internal/manager/servicehealth.go`): `ServiceHealthCollector` (leader reconciler, wired in `cmd/manager`) lists the owned Swarm service, the type judges running/desired tasks and the update state (`healthy`, `degraded`, `unhealthy`, `unknown`; stale after 2 min is unknown), stores it in the service record, shows it in `ServiceInfo.health` and hands transitions to the monitoring dispatcher (`health.service_unhealthy`/`health.service_recovered`; none during create/update/delete).
- **Preview, export, expert view**: every service plan response carries `agentSteps` (exact operation steps: command, target, args, preconditions; provisional IDs in previews) and `?format=yaml` exports the plan document as YAML (`/services/plan`, `/services/{id}/delete-plan`, `/services/{id}/update-plan`). New routes `POST /services/{id}/update-plan` and `POST /services/{id}/update` (`update` on the service; approval of `service/update` needs `update`). OpenAPI updated; route contract test and a new schema-field test for the service schemas pass.
- Guides: [EN](guides/en/13-services.md), [DE](guides/de/13-services.md).

Checks run 2026-10-24 on this branch: `gofmt -l cmd internal` empty, `go vet ./...` clean, `go test -race -count=1 -p 4 ./...` passed (40 packages ok), `python3 scripts/check_docs.py` 0 errors (68 files, 69 requirements).

Limits: fakes only; no update, rollback, health observation or galera request ran against a real Swarm or Docker. Swarm's automatic rollback on failing tasks (FailureAction rollback with a monitor period) is taken from the Docker API documentation, not observed. No canary executor, no automatic manager-side rollback (the rollback plan must be requested and approved), the health gate pauses instead of rolling back. Galera health on `/services` is the cluster's wsrep state; there is no galera node update or scale planner. No UI or CLI for updates, health, expert view or the galera type. The R6 exit gates (R1 regression suite behind the interface on a lab, container create/update/rollback/remove on a lab Swarm, plan/audit match in the UI, engine updates) are not met.

## Scope broadened (2026-10-22)

The owner broadened the product to a general self-hosted platform for clusters and services ([ADR 0012](decisions/0012-broadened-scope-platform.md)): Swarm/Kubernetes including provisioning, web servers with detailed composition, storage protocol servers, generic containers, Traefik, image builds with a managed or external registry and Docker engine updates, all through the UI with visible plans. Documented as requirements X01–X12, proposed phases R6–R9 and work packages W07–W10 (all PENDING). Only the W07 first step below exists (fakes only); the rest of the extension is not implemented. Galera stays priority 1; W05/W06 and the R1 gates are unchanged. A09 is partly superseded by X09; the Docker update boundaries, product name and other choices are open in [ENTSCHEIDUNGEN.md](ENTSCHEIDUNGEN.md#open-decisions-from-the-scope-extension).

## UI for the service, image-build, maintenance, drift and keyring APIs (2026-10-24, mock only)

`web/` has pages and panels for the endpoints merged in the previous steps: **Services** (`/services`: list, one form for container and Traefik types with client-side checks of the manager's rules, plan preview with steps, generated files, warnings and blockers, request with the approval digest followed in the operation card, deletion plan with an optional data-volume removal), **Image builds** (`/images`: build list with status, web server composer with nginx/PHP version, catalog extensions, `php.ini`, nginx options, custom snippet hints, packages, platforms and push target, rendered Dockerfile and plan before approval, the plan invalidated by any form edit, approval by digest), **Registries** (`/registries`: list without credentials, add an external registry; the password is write-only and cleared from the page after the request), **host maintenance** (panel on `/hosts`: plan preview incl. refusals, start, end), **drift** (card on the cluster detail), **replica keyring** (`/settings/replicas`: replicas, registering a data-key recipient with a format check, revoking certificates after confirmation) and the **backup schedule form** (physical/logical method, databases, retention). New pure logic in `web/src/lib/services.ts`, `images.ts`, `platform.ts` and `lifecycle.ts` with vitest tests; typed client methods and runtime guards in `api.ts`. The dev mock server (`mock.ts`, development only, not in the production build) answers the new endpoints with invented data. 19 new bilingual screenshot sets in `docs/assets/screenshots/` (captioned as mock-server captures) are in the guides and the README gallery.

Checks run: `npm run check` (0 errors), `npm test` (146 passed), `npm run build` (mock data absent from the build output), `scripts/check_docs.py`.

Limits: everything was exercised only against the mock server in headless Chromium, not against the real manager (the backend handlers themselves are fakes-only, several not wired into `cmd/manager`). The catalog of nginx/PHP versions, extensions and packages in `images.ts` is a copy of the backend's unverified proposal catalog and can drift from it. Snippet and `php.ini` checks in the browser are early hints; the manager decides. Service plan steps are shown as the API returns them; the operation card still says that the API lists no steps for `service/create` operations. The UI uses no live data, so refresh behavior, permissions (read-only presets) and error codes such as `approval_mismatch` are tested at the client level only. No keyboard-only or screen-reader pass was done on the new pages.

## W11 wiring and UI (2026-10-24, fakes, `httptest` and mock server only)

Branch `w11-wiring` (from `integration`). The groundwork above is now wired into `cmd/manager` and has a web UI. **Still nothing real was contacted: no advisory feed, no AI provider or CLI login, no registry, no docker build; the UI was never opened against a real manager.**

- **Manager wiring.** New config section `manager.vuln` (`pollIntervalSeconds` minimum 300, `feeds` with explicit URLs or an offline OSV file, `defaultMode`, `providers`; validated at start, needs `auth.enabled`; `manager init` skeleton unchanged). Without the section nothing runs and the routes answer 503. An empty section gives `notify_only`, no feed, no provider, no network access. `manager.VulnScheduler` (a leader-only reconciler, first poll 30 s after leadership starts) calls `Poll`; `manager.VulnInventory` builds the inventory from service, cluster and image build job records (grouped by image, kind guessed from type and image name, build record and registry target linked for rebuild plans, coverage `reference` or `unknown`; no SBOM exists); `manager.VulnNotifier` sends findings and expiring mitigations as `vuln.*` events through the monitoring dispatcher; redaction names come from Swarm, host and cluster names and registry hosts; provider keys use `SealedSecretStore`; feed and provider HTTP follow no redirects. A new leadership builds a fresh service (`Budget.Reload`, `ResetSeq`).
- **Persistence.** `ai.Budget` got an optional `BudgetBacking` (`vuln.BudgetStore` in the metastore, one document per provider and UTC day, fail-closed when a count cannot be saved). The audit sequence continues after the highest stored sequence (`Service.nextSeq`), no longer a process-wide counter.
- **Web UI.** Menu entry Vulnerabilities (`/vulnerabilities`: findings with filters, feed state, check now; `/vulnerabilities/[id]`: detail, own VEX decision, proposals with plan, rating, validator errors, model remarks as information, digest-acknowledged approval, reject, per-finding audit; `/vulnerabilities/settings`: policy modes with the auto_for_policy warning, allow-list and acknowledgement, kill switch, provider and model, provider list with data-egress column, add provider, consent step with the exact statement). German and English texts, typed client and guards for all 15 routes (null lists from the Go API tolerated), dev mock server data in `mock-vuln.ts` with the documented refusals (notify_only, digest mismatch, stale policy version, consent statement).
- **Screenshots.** 8 PNGs (`{en,de}-light-vulnerabilit*.png`, 636 KB) captured with Playwright against `npm run dev:mock`; they carry the MOCK DATA banner and are labeled as mock captures in the guides and the README.

Checks run on this branch: `gofmt -l` (clean), `go vet ./...`, `go test -race -count=1 -p 4 ./...` (all packages ok), `npm run check` (0 errors, 0 warnings), `npm test` (163 tests), `npm run build`, `python3 scripts/check_docs.py` (0 errors), `python3 scripts/check_deploy.py`. New tests: `cmd/manager/vuln_test.go` (real managed runtime with an offline feed file, inventory, poll, seeded defaults, sealed key, config validation, auth requirement), `internal/manager/vulninventory_test.go`, `vulnruntime_test.go`, `internal/vuln/persist_test.go` (memory and metastore KV), `web/src/lib/vuln.test.ts`.

Limits: the `/vuln/*` approval flow was exercised end to end only through the API fakes and the mock server, never through a real browser session on a real manager, and the Svelte pages were only screenshot-checked on the mock. Inventory coverage is by image name and tag; the kind guess (database, proxy, registry) is by name. Leader loss during a poll is only guarded by the leader check, not by a lease. The scheduler does not poll on demand after a restart sooner than 30 s. No dedicated permission checks in the UI (it shows the manager's 403). Still not done: real feed and AI runs, SBOM at build time, rescan, rollout, canary and rollback, mitigation removal, prompt retention, and all R10 exit gates.

## W11 groundwork: vulnerability response (2026-10-24, fakes only)

Branch `w11-vulnresponse`. First code for W11 (R10, X13, [ADR 0015](decisions/0015-ai-assisted-vulnerability-response.md), proposal). Everything was tested with fakes and `httptest` fixtures only: **no real advisory feed was contacted, no real AI provider or CLI login was called, nothing was built, pushed or rolled out**, and the package was not wired into `cmd/manager` at that time (see "W11 wiring and UI" above).

- **`internal/vuln` (deterministic part).** Advisory model with canonical CVE IDs, dedup and merge across feeds, size caps on untrusted text. Feed adapters behind an `HTTPDoer`: OSV (`/v1/query` per inventory component), GitHub global advisories (cursor, ETag, `Link` pagination restricted to the configured host, optional token), NVD CVE API 2.0 (120-day window, CPE matches, `cisaExploitAdd` as KEV, rejected CVEs withdrawn, optional API key), CISA KEV catalog (ETag) and an offline OSV mirror file. No default URLs. Bodies capped, 429/503 Retry-After backoff, exponential backoff, coarse error classes, stale after 24 h. SBOM-style inventory with coverage labels (`sbom`, `reference`, `unknown`; unknown entries reported as not covered). Deterministic matcher (ecosystem/package/version ranges; Debian version algorithm with semver and Alpine pre-release handling) and findings with severity, CVSS, KEV, EPSS, a deterministic priority score (P1-P4 with basis) and VEX-like status (auto-resolve when no longer matched, rollback re-opens, `not_affected` needs a justification and holds per component version). Persistence through a small `KV` interface with `MetastoreKV` (tested against a real single-node in-memory Raft metastore) and `MemoryKV`.
- **Remediation proposals.** Kinds `patched_rebuild`, `mitigation` (allow-listed `php.ini` setting, time-limited, re-alerts 7 days before expiry), `no_op`. Every proposal has a deterministic benefit/risk rating and a plan with a digest; rebuilds and mitigations of manager-built images produce an `imagebuild` plan with a new tag, other images get a manual plan. The validator checks component, target version (newer, outside every affected range, and for AI answers one of the stated fixed versions) and settings (same validators as manual image builds). Approval recomputes the plan from the current inventory and refuses a changed digest. The only executor (`ImageBuildExecutor`) starts an image build through `imagebuild.Service` (fakes only); rollout is not implemented.
- **Policy modes.** `notify_only` (default; approval refused), `approval_required`, `auto_for_policy` (explicit, recorded opt-in; severities, named services, kinds and a daily limit of 1-20 required). Never automatic: database/Galera, proxy, registry, major upgrades, manual plans, no-op, invalid proposals; kill switch.
- **`internal/ai`.** Providers `claude_api` (Messages API), `openai_api` and `openai_compatible` (Chat Completions; plain http only to loopback/private addresses), `claude_code_cli` and `codex_cli` (experimental acknowledgement required) behind one interface; model allow-list per provider, selected per policy. Consent to the egress statement required before use. API keys as `ai.Secret` via `SecretStore`; `manager.SealedSecretStore` seals them with the data keyring (tested on a one-node manager store). No redirects followed, error bodies discarded, responses capped. CLI calls through a restricted `CommandRunner` (absolute path, no shell, explicit environment, empty temp directory, timeout, output caps); `ExecRunner` was exercised only with `/bin/cat`, `/bin/echo`, `/usr/bin/env` and `/bin/sleep`. Claude Code and Codex flags were read from the local `--help` output, never run with a login. Redaction corpus (keys, tokens, connection strings, credentials, e-mail/IP addresses, environment names), delimited prompts with a random boundary, strict JSON proposal schema (no unknown or duplicate members, enums, single-line values). Per-provider daily request/token budget (in memory).
- **Audit.** Every step is an append-only record in the metastore: polls, findings, analyses (provider, model, prompt and response SHA-256), proposals and validator results, approvals (manual/automatic), declines, executions, policy and provider changes; no prompts or secrets.
- **API.** 15 routes under `/vuln/*` (OpenAPI updated, route contract test passes), new authz resource `vuln` held by no preset except the administrator (deny by default), `VulnBackend` adapter, 503 until wired. Guide: [guides/en/15-vulnerability-response.md](guides/en/15-vulnerability-response.md) ([DE](guides/de/15-vulnerability-response.md)); settings in the configuration reference. The image build `php.ini` allow-list gained `session.upload_progress.enabled`, `file_uploads`, `allow_url_fopen` and `allow_url_include` (Off only).

Checks run: `go test -race -count=1 -p 4 ./...`, `gofmt -l`, `go vet ./...`, `python3 scripts/check_docs.py`.

Limits of the groundwork batch (several closed by the wiring batch above: inventory, scheduler, notifications, UI, persistent budget and audit sequence): no real feed, no real AI call, no measured model quality, no matcher precision/recall corpus of past advisories, no real inventory source (static fake), no scheduler, no notification adapter (in-memory notifier), no UI, no rescan, rollout, canary or rollback, no mitigation removal after a patch, prompt retention not implemented, usage budget and audit sequence are per replica in memory, version comparison does not implement PEP 440/Maven/RPM rules, NVD `versionStartExcluding` is treated as inclusive (possible false positives), redaction is pattern based. None of the R10 exit gates is met.

## W09 groundwork: storage protocol servers (2026-10-24, fakes only)

Branch `w09-storage`. New service types in `internal/service`: `sftp`, `ftp` (FTPS explicit/implicit or plain with `allowPlaintext`, passive range published one to one, at most 100 ports, passive address required), `s3` (single-drive, MinIO conventions as a proposal, root credentials as Swarm secrets, typed Traefik routes as `traefik.*` labels plus a file-provider snippet) and `nfs` (always blocked: needs CAP_SYS_ADMIN and nfsd, the agent has no capability field). Each has typed strict validation, a pinned image rule (`requireDigest` or a tag-only warning), a managed local data volume pinned to a host with one replica, secrets by name only, a published-ports artifact, warnings and blockers, delete plans and health. Agent side: `create-service` and `update-service` accept `ports` and `configs` (new omitempty fields; `TestServiceSpecEncodingStable` unchanged and passing), referenced configs go through the ownership guard and are pinned to the checked ID; `dockerclient` maps them to `EndpointSpec.Ports` and `ContainerSpec.Configs`; the port refusal in `orchestrate` is gone. Traefik plans carry `traefik-management.json`. OpenAPI type enum and DE/EN guide 13 updated.

Limits: fakes and unit tests only; no storage image, Swarm or Traefik was run, so every R8 exit gate is open. Config creation/removal and these delivery gaps are superseded by the cx-configs step below. No UI form, CLI or storage backup. Image names are proposals.

## W07/W09 config delivery (2026-10-24, fakes only)

Branch `cx-configs`. Added fenced `create-config` and `remove-config` agent commands, `DockerAPI` methods and Engine client requests (base64 Data, returned ID, non-forced deletion). Configs carry ADR 0004 installation/cluster/node ownership labels and creating operation/step labels; the executor records the returned ID on its persistent allow-list. Step replay survives restart; an existing name requires allow-list ownership and a matching content digest and identity. Changed content is refused, missing deletion targets succeed, in-use configs return `config-in-use` for runner retries, and unknown mutation outcomes retain the existing reconciliation requirement. Secret-flagged config data/artifacts are refused; config errors retain no daemon text or file contents.

Traefik dynamic YAML, SFTP users JSON (unless `usersConfig` references an external config) and FTP users JSON now produce `create-config` before `create-service`, with a config mount and matching service-spec review artifact. Delete plans remove managed configs after the service, independently of volume deletion. External configs are preserved and resolved into the agent allow-list by the manager; newly planned configs are excluded from pre-create reference resolution. Plan conversion sends config commands to the bound Swarm manager agent. No HTTP routes changed; OpenAPI documents the optional artifact sensitivity flag. DE/EN guide 13 updated. Traefik config changes during updates remain blocked until immutable-config rotation exists; S3 snippets remain review artifacts for a separate Traefik instance, and NFS remains blocked.

Verified with fakes only: planner → runner conversion → real executor create/mount/delete for Traefik, SFTP and FTP; durable replay, ownership refusal, changed-content refusal, missing/in-use deletion, error redaction and unknown outcomes; Engine requests through an in-memory HTTP transport; service/galera compatibility and `TestServiceSpecEncodingStable`. Targeted race tests passed in agent, dockerclient, service, galera, runner and manager. No Docker daemon, Swarm, storage server or Traefik was contacted.

Targeted command (exit 0 in all six packages): `go test -race -count=1 -p 4 ./internal/dockerclient ./internal/service ./internal/galera ./internal/runner ./internal/agent ./internal/manager -run 'TestConfig|TestServiceConfig|TestServiceRefs|TestServiceSpecEncodingStable|Test.*[Cc]ompat|Test.*Storage|TestSFTP|TestFTP|TestTraefik'`. Config JSON decode errors are also tested to never echo malformed sensitive values.

Executed checks in this sandbox (Go uses a dedicated `GOCACHE` under `/tmp`; detected 14 CPUs, `GOMAXPROCS=4` for tests):

- `gofmt -l .`: exit 0, no output.
- `go vet ./...`: exit 0, no output.
- `go test -race -count=1 -p 4 ./...`: exit 1. The sandbox refuses TCP and Unix socket listeners (`bind: operation not permitted`); the final run also timed out in manager `TestAbortCreateWithExecSQLInFlight`. That test passed on an isolated race rerun (`ok .../internal/manager 1.366s`). Host load at inspection was 206.39/162.09/107.86; causation of the timeout is unverified. The full service, galera and runner packages passed in the full run (`1.216s`, `4.291s`, `2.286s`). This is not a passing full-suite result. Targeted socket-free race tests above passed.
- `python3 scripts/check_docs.py`: `Checked 68 Markdown files and 69 requirements; 0 errors.`
- `python3 scripts/check_deploy.py`: `deploy checks passed (static only; no image was built)`; PyYAML is unavailable, so only text-level structural checks ran for compose, stack and release workflow.

Commits: `5521645` (agent/client and fake adapters), `2ace890` (service plans, conversion, reference resolution, tests, OpenAPI and guides). Initial compound-shell commit attempts were denied on `index.lock`; direct Git invocations succeeded. Nothing was pushed. Next: rerun the full race suite in an environment permitting local test listeners and investigate any recurring abort timeout, then validate config create/mount/delete in the dedicated lab before claiming Swarm support.

## Key rotation, PITR and membership groundwork (2026-10-24, fakes only)

Branch `codex-keys-pitr`, based on `integration`. The worktree Git directory was
outside the sandbox write roots, so the requested initial checkout failed with
`index.lock: Operation not permitted`; commits were created in an isolated local
clone. No push or real Docker/Lima/Colima/MariaDB/S3/SFTP action occurred.

- **Rotation:** administrator-only `POST /settings/data-key/rotate` previews and
  accepts a revision-bound approval digest. Versioned random data keys, replica
  and configured recovery envelopes, old-key retention, atomic per-document
  reseal/progress, guarded stale-key writes, status and start/completion events.
  The existing leader reconciler resumes bounded batches after failover. Scans
  cover every top-level sealed document, including registry/AI/backup credentials,
  PKI, sessions and passkeys. Corruption leaves progress incomplete and old keys
  available. Historical backups keep their old envelopes after completion.
- **PITR:** `binlog` manifest kind with encrypted streams and HMAC summaries;
  authenticated metadata binds the full-backup position/time, file range, source
  history and sequence. Physical jobs optionally parse binlog_info. Archive
  retention follows the physical base, including pin protection and dependent
  deletion order. Time/position planning refuses gaps and reuses the isolated
  inspection plan with an explicit agent `check-restore-target` step. Only the
  fake executor can replay, after exact digest approval and stream verification;
  there is no promotion step.
- **Membership:** injected CSR peer-certificate and membership services, API
  routes/OpenAPI, strict administrator checks and audit. Leader-only serialized
  `AddNonvoter` -> caught-up readiness -> `AddVoter`, or quorum-checked
  `RemoveServer`, use a nonzero durable configuration index. The pinned Raft
  library returns zero from GetConfiguration.Index, so the wrapper reads the
  matching log/snapshot configuration. Durable approved intents survive leader
  loss; removal denies recorded certificates, tombstones the identity and removes
  key envelopes. Managed peer TLS consults replicated deny state on new handshakes.
  The default runtime returns 503 for membership administration: peer-CA access
  and authenticated remote readiness still need implementation.

Verification actually executed: `gofmt -l .` returned no files; `go vet ./...`
returned exit 0; full `go test -race -count=1 -p 4 ./...` **failed** because the
sandbox denies socket binding (`bind: operation not permitted`, including
loopback and Unix sockets in existing tests). No tests were skipped or rewritten
to hide that restriction. Full `internal/dbbackup` and `internal/authz` race suites
passed; focused race tests passed for rotation/leader loss, keyring, four-replica
admission/removal/leader loss, quorum and stale-index refusal, snapshot index
fallback, TLS tombstone verification, HTTP authorization and OpenAPI routes.
`python3 scripts/check_docs.py` and `python3 scripts/check_deploy.py` passed;
initial clone deploy checks were text-level because PyYAML was absent; the
synchronized workspace check also passed with PyYAML 6.0.3 available. No image was
built. Go uses a
writable temporary build cache; detected host capacity was 14 logical CPUs, and
the full race command used GOMAXPROCS=1 with four package workers.

Limits: no real rotation or membership change, disk fault/restart during rotation,
continuous archiver, event parser or transaction/event-boundary/time-stop
validation, GTID/source failover, HTTP/UI PITR integration or real replay.
Retention coupling is library-only until the manager discovers binlog sets. No
key retirement/log erasure, peer renewal/cancellation or automatic closure of
existing TLS streams. A removed replica cannot be made to forget cached data/CA
keys. Explicit joint-consensus log entries are not exposed by this Raft library;
we use its single-server configuration transitions and quorum checks. See the
amendments to [ADR 0009](decisions/0009-cluster-backup-method.md) and
[ADR 0013](decisions/0013-replica-keys-peer-tls-and-revocation.md).

Next action for this package: run the complete unchanged verification loop where
local sockets are permitted; then design authenticated peer readiness and offline
CA provisioning, and a real closed-binlog/event parser before lab PITR work.

## Next action

E4 re-run A4 and B2 passed (see above). Continue W05: rerun E8 with the coordinated writer protocol and deliberately partition one proxy during a requested switch; rerun the B2/B3 stuck-node case with slot logs, the three-replica manager as Swarm services (deny-list distribution still to exercise), and native amd64 evidence. Re-run E4/E6 with the gap work (deletion, worker-host jobs, verification, outage notification, recover permission, maintenance, drift, logical backups and retention). The first working version must be validated locally before the user's Swarm trial; `deploy/stack.yaml` with a `manager init` configuration has now run on lab VMs (E7).

## Verification and limits

`python3 scripts/check_docs.py` checks local Markdown file links and the mapping of 56 requirements to releases. It does not validate external URLs, anchors or rendered website links.

No application tests, VM tests or native architecture checks have run. The docs check, Jekyll build and Pages deployment succeeded on GitHub Actions. The Claude CLI itself has not been launched or configured globally.

## Service UI extension (2026-10-24)

Implemented in `web/`: DE/EN forms for Galera and SFTP/FTP/S3/NFS, typed numeric/spec conversion and secret-name references; protocol users/exports; warnings/blockers in previews; freshness-aware health badges (including Galera `unavailable`), refreshed every 15 seconds; image update and previous-spec rollback for container/Traefik with rolling strategy/canary preview fields, prepared rollback plans and the existing operation approval flow; local JSON/YAML 1.2 downloads and agent-step expert views. Editing inputs invalidates previews, including changes made while a preview request is in flight. The development mock now includes health states, storage blockers and updates whose stored spec changes only after approval. Guide 13 (DE/EN), the README capture table and a dedicated mock capture script were updated. No backend implementation or infrastructure changed.

Executed checks: `npm run check` (0 errors, 0 warnings), `npm test` (15 files, 185 tests passed), `npm run build` (static assets emitted; `.gitkeep` restored with `touch` after each build), `gofmt -l .` (no output), `python3 scripts/check_docs.py` (68 Markdown files, 69 requirements, 0 errors), `node --check scripts/screenshots/services.mjs`. The full `go test -race -count=1 -p 4 ./...` was attempted with a writable temporary `GOCACHE`: 22 packages passed, 18 failed, 5 had no tests. Failures include denied socket binds (`operation not permitted`) in local HTTP/TCP test servers; the suite did **not** pass. The first attempt also failed because the default Go cache was outside the writable sandbox.

Limits: no real-manager/Swarm run of these UI additions; optional S3 routing and advanced Galera placement are not exposed; updates currently edit the image while preserving other spec fields. NFS and canary execution remain blocked by the backend. Backend diagnostics remain verbatim English. Playwright and cached Chromium exist, but starting the mock server failed with `listen EPERM`, and launching Chromium failed with `MachPortRendezvous ... Permission denied`; no new screenshots or browser assertions completed. Existing screenshots are labelled as earlier mock captures. The new capture script is syntax-checked only.

Git limitation: the requested `git checkout -B codex-ui-services integration` failed because this worktree's shared Git metadata is outside the writable sandbox. A separate local metadata clone under `/tmp` tracks the same source directory on `codex-ui-services`, based on `integration`, for small commits. The original worktree metadata and branch remain unchanged. No push was made.

Next: rerun the full Go suite and the DE/EN capture script in an environment that permits local sockets and Chromium, inspect the rendered forms/downloads, add the resulting mock-labelled screenshots to guide 13 and the README, then validate against a real manager before any Swarm trial.

## W10 Kubernetes groundwork

2026-10-24, branch `cx-k8s`, **proposal, fakes only**. Added
`service.Orchestrator` with Swarm artifact preservation and an opt-in
`internal/service/k8s` renderer using the existing service definitions. Typed
previews cover Deployment/StatefulSet, Service, local PV/PVC with node affinity
and Retain policy, Secret refs, ConfigMap and Traefik IngressRoute. Export is a
deterministic JSON-style YAML document stream; the common digest binds source,
platform, artifacts and blockers. Galera preview identities are deterministic
and generated manifests remove bootstrap flags. Quorum, `safe_to_bootstrap`,
identity-marker/host-return rules and differing Swarm semantics remain blockers.

`internal/kube`: opt-in KubeAPI, concurrency-safe fake and minimal REST adapter
without client-go. Explicit scope/name/UID inventory plus installation labels
guard apply/list/delete; apply uses atomic create before SSA for absent names,
`force=false` and resourceVersion/UID preconditions; delete uses UID/version
preconditions. Tests use `httptest.ResponseRecorder` with an in-memory HTTP
transport, not a network listener or real API server. TLS handshakes and actual
Kubernetes schema/SSA behavior are unverified.

Registration model and manager adapter store token or opaque kubeconfig in an
AES-GCM sealed metastore document through the existing secret path, with CAS
create and collection/key binding. Only credential-free metadata leaves the
backend. Added GET/POST `/kubernetes-clusters` contract, default-off
`api.Config.ExperimentalKubernetes`, global `kubernetes_cluster` authz resource,
OpenAPI schemas and contract/security tests. The shipping manager does not
construct this backend or any Kubernetes client. No cluster contact,
provisioning, kubeconfig execution, UI/CLI or deployment/recovery route exists.
[ADR 0016](decisions/0016-kubernetes-groundwork.md) records the proposal and
alternatives; [EN](guides/en/16-kubernetes.md) and
[DE](guides/de/16-kubernetes.md) explain the limits. Kubernetes timing and initial
distribution remain owner decisions. No R9 gate is met; R1 priority is unchanged.

Actual verification on this worktree:

- `gofmt -l .`: exit 0, no output.
- `go vet ./...`: exit 0, no output, with a writable task-specific `GOCACHE`.
- `go test -race -count=1 -p 4 ./...`: **exit 1**, 24 packages passed,
  18 packages failed, 5 had no tests. Existing TCP/Unix-socket tests cannot bind
  listeners in this sandbox (`bind: operation not permitted`; some `httptest`
  tests panic for that reason). No all-suite pass is claimed. The new `kube`
  and `service/k8s` packages passed in this full run.
- Focused W10 tests with `-race -count=1 -p 4` passed in `service/k8s`, `kube`,
  `api`, `manager` and `authz`, including registration contract/schema checks,
  authz/default-off behavior, sealed round-trip/AAD, concurrent CAS,
  deterministic Galera previews and ownership guards. These tests need no
  listeners. Existing service tests also passed in the full run.
- `python3 scripts/check_docs.py`: `Checked 71 Markdown files and 69 requirements; 0 errors.`
- `python3 scripts/check_deploy.py`: `deploy checks passed (static only; no image was built)`.

The default Go build cache was initially denied by the sandbox; subsequent Go
commands used `GOCACHE=/tmp/dbcm-cx-k8s-cache`. Detected 14 logical CPUs via
`os.cpu_count()` (`sysctl` was denied); test runs used `GOMAXPROCS=4`, later
focused checks use `GOMAXPROCS=1`. No VM, cluster, production credentials or
infrastructure resources were used.

Commit creation is blocked by this session's filesystem sandbox: `git add` and
`git commit` could not create `.git/worktrees/cx-k8s/index.lock` in the shared
repository directory outside the writable worktree. No commit or push was made.
Changes remain in the worktree. Next: commit the reviewed renderer, API/storage
and documentation as small commits in a session with Git metadata write access;
rerun the complete race suite where local listeners are permitted. Any real
Kubernetes work still needs the timing/distribution decision, durable ownership
inventory, fencing/RBAC/credential review and a scoped, budgeted lab plan.

## Kubernetes and security UI follow-up (2026-10-24)

Scope: frontend only on `cx-ui-k8s`, with existing HTTP contracts, invented mock
responses and unit tests. No infrastructure, Kubernetes API, Docker daemon,
advisory feed or AI provider was contacted.

- Added `/kubernetes` to the existing navigation: metadata list and registration
  form for the GET/POST `/kubernetes-clusters` contract. Validation mirrors DNS
  name, HTTPS-origin and credential byte limits. Exactly one token or opaque
  kubeconfig is sent. Credentials are cleared after each request attempt and on
  type changes; neither credentials nor kubeconfig enter browser storage or
  mock fixture state. Runtime response guards reject credential-bearing views.
  The page explicitly distinguishes saved metadata from verified connectivity.
  The shipping manager still leaves this experimental backend unwired (503).
- Existing vulnerability findings, detail, policy and provider pages were already
  present. Added `/vulnerabilities/audit` for the missing global history: polls,
  policy changes and provider consent without a finding reference, sequence
  ordering, search, finding links and explicit loading/error/retry/empty states.
  Backend event codes and results remain verbatim; no HTML is rendered from them.
- Service plan config sections show owned config names, artifacts and dependency
  steps. The existing expert view continues to list `create-config` and
  `remove-config` agent commands. Mock SFTP/FTP/Traefik plans now demonstrate
  config creation before service creation and cleanup after service removal;
  external SFTP config references do not become owned create/remove steps.
  JSON/YAML exports preserve config metadata. No executor or API changed.
- Added DE/EN UI text and updated guides 13, 15 and 16. Imports added in `web/`
  are relative; existing Svelte 5 and shadcn components are reused.
- Examined `integration` API routes and OpenAPI: replica key registration and
  certificate revocation exist and already have UI; data-key rotation and
  metastore membership mutation have no HTTP contract. No controls or invented
  endpoints for those conditional tasks were added.

Actual checks (14 logical CPUs detected with `os.cpu_count()`; Go commands use
`GOCACHE=/tmp/dbcm-cx-ui-k8s-cache` and `GOMAXPROCS=4`):

- `gofmt -l .`: exit 0, no output.
- `go vet ./...`: exit 0, no output.
- `go test -race -count=1 -p 4 ./...`: exit 1; 24 packages passed, 18 failed,
  5 had no tests. TCP and Unix test listeners are denied by the sandbox
  (`bind: operation not permitted`, including `httptest` panics). This is not a
  passing full-suite result. No backend files changed.
- `python3 scripts/check_docs.py`: `Checked 71 Markdown files and 69 requirements; 0 errors.`
- `python3 scripts/check_deploy.py`: `deploy checks passed (static only; no image was built)`.
- `cd web && npm run check`: 0 errors, 0 warnings.
- `cd web && npm test`: 18 files, 196 tests passed (11 new tests covering
  registration validation/contracts, global audit ordering/search and config
  artifact/dependency/export behavior).
- `cd web && npm run build`: exit 0; static adapter wrote
  `../internal/webui/assets`. Ran `touch internal/webui/assets/.gitkeep` afterward.

Frontend dependencies were initially absent (`svelte-kit: command not found`);
`npm ci --offline --ignore-scripts` installed 100 packages from the local cache
and reported 0 vulnerabilities. An initial frontend check found a test helper
using `string` instead of the service-type union; corrected before the passing
checks above. No dependency files changed.

Limits: no browser rendering, screenshot or real-manager UI integration evidence.
Mock config content/names are illustrative; the real manager supplies authoritative
plans. Kubernetes provisioning, connectivity tests, deployment/recovery and
production wiring remain absent. No release gate or Kubernetes timing/distribution
decision changes. Local implementation commits: `0f66e5f` (Kubernetes),
`93a34ff` (global audit), `78ec0e2` (Swarm configs). Nothing was pushed.

Next: rerun the complete Go race suite with local listeners permitted; render and
inspect both locales with synthetic data, then validate these pages against an
authorized fake-backed manager. Do not infer Kubernetes support from registration.

## Manager listener ACME/TLS (2026-10-24, fakes only)

Implemented the requested W06 manager-listener increment in `cx-acme`:

- `internal/managertls`: RFC 8555 client interface and adapter using the already
  required `golang.org/x/crypto/acme` (no new dependencies). HTTP-01 and
  TLS-ALPN-01 proofs, DNS allow-list, TLS 1.2+ listener, isolated/expiring
  challenges, staging CA default, explicit contact/domains/CA configuration.
  The lifecycle layer uses the lower-level client rather than `autocert.Manager`
  because autocert has no public Stop/Close for leadership-scoped renewal timers;
  rationale and alternatives are in the [ADR 0014 amendment](decisions/0014-manager-container-docker-access.md#amendment-2026-10-24-native-manager-https-fakes-only).
- `internal/manager/acme.go`: account keys, issued keypairs and active challenges
  AES-GCM sealed through the existing key provider/keyring, replicated in
  `manager-acme`. Followers read certificates/challenges; writes and cleanup are
  fenced to one leadership. Three-node in-memory Raft test covers ciphertext,
  collection/key binding, follower refusal and reuse after leader loss.
- `cmd/manager`: `manager.tls.acme`, optional static PEM TLS, verified HTTPS
  healthcheck, separate challenge-only HTTP listener. Only the leader starts a
  cancellable worker: immediate/hourly check, renewal within 30 days, five-minute
  failure retry. Handshakes do not contact the CA. Existing certificates survive
  failed renewal; first HTTPS fails until initial issuance succeeds.
- Unconfigured plain HTTP outside numeric loopback is refused before runtime
  startup. `manager.tls.allowPlainHttp` is the explicit proxy-network opt-in and
  excludes TLS settings. The Dockerfile prototype default now binds loopback;
  shipped compose/stack keep their existing loopback/proxy layout. Added
  [EN](guides/en/17-manager-tls.md)/[DE](guides/de/17-manager-tls.md) guides,
  configuration/security reference updates and installation notes.

Actual verification (Go commands used `GOCACHE=/tmp/dbcm-cx-acme-cache`,
`GOMAXPROCS=4` (final vet rerun: `1`), `GOPROXY=off`; no dependency
downloads). `os.cpu_count()` detected 14 logical CPUs; no VM resources were
allocated:

- `gofmt -l .`: exit 0, no output.
- `go vet ./...`: exit 0, no output.
- `go test -race -count=1 -p 4 ./...`: exit 1; **25 packages passed,
  18 failed, five had no tests**. Existing network-backed tests fail/panic on
  sandbox-denied TCP/Unix listeners (`bind: operation not permitted`), including
  the pre-existing manager health test. No whole-suite pass is claimed.
- Focused ACME/manager storage/listener tests with the race detector passed.
  Actual RFC 8555 requests run against a fake CA through
  `httptest.ResponseRecorder`/an in-memory HTTP transport. Both HTTP key proof
  and RFC 8737's critical digest are checked. Tests cover renewal, CA rejection
  preserving the existing certificate, persisted account reuse on a fresh
  worker, cancellation, SNI/host/path/method scope, ALPN isolation and expiry,
  plus a verified TLS handshake through `net.Pipe`. Two socket-backed fake-CA
  subtests were **skipped** on explicit sandbox listener denial.
- `python3 scripts/check_docs.py`: `Checked 73 Markdown files and 69 requirements; 0 errors.`
- `python3 scripts/check_deploy.py`: `deploy checks passed (static only; no image was built)`.
- No web code changed, so frontend checks/build were not run for this increment.

Limits: no public CA, real DNS, infrastructure, VM, container or browser was
contacted. Native manager TLS and public challenge routing have no deployment
or HA load-balancer evidence. All replicas need identical ACME settings and
available data keys; local follower lag may temporarily fail validation. No
DNS-01 wiring, wildcard/IP issuance, external account binding, revocation,
account-key rollover, cross-term order resumption or dynamic TLS config reload.
Ports 80/443, unprivileged local forwarding, CA trust (especially staging) and
healthcheck startup grace remain operator configuration. The earlier historical
"No ACME" statements above are superseded for the manager listener only;
database certificate issuance remains unwired. No R1 release gate is asserted.

Local implementation commits: `2d28eb3` (ACME core and sealed storage),
`a7791c8` (listener/config/healthcheck/container defaults). An initial commit
inside a combined shell command was denied on `index.lock`; retrying the same
commit as a standalone command worked. Documentation is a separate local
commit. No push was made. Next: rerun the full race suite and socket-backed fake
CA where local listeners are allowed. A real CA/deployment test requires
separately authorized work and is outside this fakes-only task.

## amd64 build and CI support (2026-10-24)

Branch `cx-amd64`; build configuration and fakes/unit tests only. No real
infrastructure, Docker daemon or registry was contacted. The native CI matrix
now uses `ubuntu-24.04` (amd64) and `ubuntu-24.04-arm` (arm64), checks the host
architecture, builds Go/web, runs vet/format/docs/deploy checks, Go race tests
and frontend tests, then builds both manager/agent and backup/restore job
images for the runner's native platform. Buildx writes temporary OCI archives
with SBOM/provenance and no login/push. Release calls that same workflow before
publication; both images are built for `linux/amd64,linux/arm64` on version-tag
pushes only. Only the image job has `packages: write`; other jobs retain their
specific read/release permissions. Existing SHA pins were reused, not refreshed
or independently verified. Tag builds use QEMU for foreign-platform steps.

`scripts/check_deploy.py` guards both workflows (native matrix, both images,
read-only checks, tag-only publication, validation dependency, SHA pins and
SBOM/provenance); 12 mutation regression tests are in
`scripts/test_check_deploy.py`. Both the PyYAML parse path and stdlib fallback
were exercised. These checks are static and do not replace an Actions run.
`deploy/README.md`, `docs/INSTALL.md` and `docs/TESTING.md` describe the artifact
contract and native amd64 P0 plan. An owner-provided amd64 host versus cloud VM
remains an open owner decision; no P0 gate is closed by these changes.

Actual local verification:

- `gofmt -l .`: exit 0, no output.
- `go vet ./...`: exit 0, no output.
- `go test -race -count=1 -p 4 ./...`: exit 1; 24 packages passed,
  18 failed, 5 had no tests. Existing fake servers cannot bind local TCP/Unix
  listeners in this sandbox (`bind: operation not permitted`, including
  `httptest` panics). Additionally `TestAbortCreateRetryingExecSQLThenDelete`
  timed out waiting for operation aborted. Its isolated race rerun with
  `GOMAXPROCS=1` passed (`internal/manager`, 1.351 s); no full-suite pass claimed.
- Go commands used writable `GOCACHE=/tmp/dbcm-cx-amd64-cache` and
  `GOMAXPROCS=4` (14 logical CPUs detected by `os.cpu_count()`).
- `python3 scripts/check_docs.py`: `Checked 71 Markdown files and 69 requirements; 0 errors.`
- `python3 scripts/check_deploy.py`: `deploy checks passed (static only; no image was built)`.
- `python3 scripts/test_check_deploy.py`: 12 tests, `OK`; also passed with
  `python3 -S` to exercise the no-PyYAML path. The deployment checker itself
  also passed with `-S`, explicitly reporting text-only YAML checks.
- `cd web && npm run check && npm test && npm run build`: exit 0;
  840 files, 0 errors/0 warnings; 15 test files/185 tests passed; adapter-static
  wrote `internal/webui/assets`. Restored `.gitkeep` with `touch` afterwards.
  Initial check failed because dependencies were absent; an empty temporary
  npm cache lacked packages (`ENOTCACHED`). `npm ci --offline` using the existing
  cache installed 100 packages successfully. npm reported pending `fsevents`
  script approval; build emitted only the existing NO_COLOR/FORCE_COLOR warning.
- `git diff --check`: exit 0.

No image build, workflow execution, native amd64 runtime or P0 result is
claimed. Commit creation was attempted but the sandbox denied
`.git/worktrees/cx-amd64/index.lock` in shared Git metadata outside the writable
worktree. No commit or push was made; changes remain in this worktree.
Next: commit the reviewed CI/checker and deployment/docs as small commits in a
session with Git metadata write access, run the native CI on GitHub, inspect
both image indexes and attestations on an explicitly triggered version tag,
and obtain the owner environment decision before any native amd64 P0 work.

## Galera operation plans (2026-10-24, cx-galera-ops, fakes only)

Implemented approval-gated single-node scale-out/in, rolling restart/maintenance
and same-series MariaDB patch upgrades in `internal/galera/operations.go`, using
the existing runner, orchestrate conversion and agent commands. Planning refuses
stale/unknown/degraded or inconsistent Primary evidence, maintenance hosts,
missing capacity, duplicate identities/slots and removal below three nodes.
Every stop checks the component live; stops await zero running tasks, preserve
volume identity, update specs at zero replicas and start explicitly. Each node
must rejoin Synced before the next stop. Scale changes converge survivor seed
addresses; removed volumes are retained. No bootstrap, quorum override, automatic
downgrade, host reboot or infrastructure contact was added.

`POST /api/v1/clusters/{id}/operation-plan` persists a reviewable plan, exact
runner definition digest and expert agent steps. Cluster `operate` is required
both for planning and existing `/operations/{id}/approve`; all commands wait for
approval, including referenced-resource allow-list steps. Concurrent plan requests
use the cluster document CAS. Resulting state and completion commit atomically;
takeover re-queries missing create results with their original agent step IDs.
Startup also finalizes fully successful journals left before the state commit.
OpenAPI, route/schema/authz contracts, ADR 0011 and DE/EN runbooks/limits updated.

Version scope: source and target require explicit immutable `x.y.z` images;
only increasing patch releases in the same MariaDB series/repository are
implemented. Major **and series** changes are refused with
`major_upgrade_requires_product_approval`; a curated cross-series upgrade path
remains unimplemented. Ordinary operation approval cannot override this. No UI
was changed and no frontend checks/build were required.

Actual verification (Go commands used writable temporary `GOCACHE` and
`GOMAXPROCS=4`, without changing production defaults):

- `gofmt -l .`: exit 0, no output.
- `go vet ./...`: exit 0, no output.
- `go test -race -count=1 -p 4 ./...`: exit 1; **24 packages passed, 18 failed,
  5 had no tests**. The final run's failures are denied sandbox TCP/Unix
  listeners (`bind: operation not permitted`, including `httptest` panics).
  It is not a passing full suite. An earlier full run also timed out in the
  existing `TestAbortCreateWithExecSQLInFlight`; that test passed in the final
  focused rerun. No unrelated production fix was made for that timeout.
- Focused race run across `internal/galera`, `internal/runner`,
  `internal/manager`, `internal/api`, selecting `TestOperationsSequence`,
  `TestOperationsRefuse*`, `TestUpgradeRefusals`, `TestGaleraOperations*`,
  `TestClusterOperation*`, route/schema contracts and the abort test: exit 0,
  all four packages passed (1.150 s / 21.346 s / 13.001 s / 1.339 s).
  This includes the actual agent executor with an in-memory Docker, all four
  operation kinds resuming after in-process Raft leader changes, digest mismatch,
  no-command-before-approval and completed-journal reconciliation. A new HA test
  initially encountered election churn with the shared 50 ms prototype test
  timeout; its own Raft timing was raised to 300 ms (production unchanged).
- `python3 scripts/check_docs.py`: `Checked 71 Markdown files and 69 requirements; 0 errors.`
- `python3 scripts/check_deploy.py`: `deploy checks passed (static only; no image was built)`.
- `git diff --check`: exit 0. `git add` failed creating the shared worktree
  `index.lock` (`Operation not permitted`); no commits or push were made.

Limits: all new evidence is from fakes and local files. No real MariaDB,
Swarm, image registry, upgrade/SST, proxy-drain timing or infrastructure was
contacted. Failed/aborted operations can leave partial changes and retained
volumes; abort is not rollback. These planners do not settle the product's
supported-version/channel matrix or satisfy the R1 lab gates.

Next action: commit the reviewed changes in small commits when shared Git
metadata is writable, rerun the complete race suite with local listeners
permitted, then obtain owned-lab evidence for these operation paths within the
existing W05/R1 priorities. Do not run real infrastructure from this work package.

## Review fixes (2026-10-25, cx-review-fixes, fakes only)

Eight review findings have regression tests and local fixes. Generic container
specs refuse credential-looking values in ordinary environment variables and
command arguments before a plan or stored service record is built. CLI-login AI
providers require explicit `manager.vuln.allowCLIProviders: true` (default false)
at configuration validation, registration, runtime selection and direct provider
opening; guide 15 and
ADR 0015 warn about manager process rights and the stock stack's Docker socket.
Concurrent registry attempts use separate random sealed-secret references, so a
losing attempt removes only its own credential. Vulnerability approval writes a
unique execution intent before the image build; the build job atomically claims
that ID and retries read the same job without a second build or push. A running
job left by a process crash remains visible as running and needs operator
reconciliation; this work does not claim automatic completion after that crash.

The agent can adopt a config created before its inventory write only when the
installation, operation, step, cluster, node and content digest labels match.
AI usage rejects negative or implausible counters, estimates missing counters
at one token per byte with a per-part cap, and fails closed on budget-store
errors. Every structured prompt string needing redaction is rejected; free-text
fields are redacted. Key rotation refuses to commit if a previously configured
recovery recipient is missing or invalid, and checks the new envelope's key and
recipient metadata before the keyring CAS. Only the externally held private
recovery key can verify decryption; a fake-key regression test does so.

Actual verification on this worktree (Go used a writable `GOCACHE` under
`/private/tmp`): the 13 focused regression/configuration tests passed under
`go test -race -count=1 -p 4` across eight packages. `go vet ./...`
passed. `gofmt -l .` passed with no output after formatting two container files.
`python3 scripts/check_docs.py` reported 73 Markdown files and 69
requirements, 0 errors. `python3 scripts/check_deploy.py` passed its static
checks; no image was built. The first formatting check listed two container
files; they were formatted and the final check is clean. The full
`go test -race -count=1 -p 4 ./...` **failed on three runs, including the final tree**: 18 packages failed on sandbox
denial of TCP/Unix listeners, including existing `httptest` tests; 25 packages
passed and five had no tests. This is not a passing full suite.

No Swarm, Docker daemon, registry, AI provider or other infrastructure was
contacted. W05/R1 and later release gates remain open. Next action: repeat the
full race suite where local listeners work, then review the execution-intent
reconciliation and recovery-path behavior before any infrastructure trial.

## X09 Docker Engine update plans (2026-10-25, fakes and mock only)

W07/X09 now has a read-only per-host planner in `internal/manager/engine_updates.go`.
The planner reads Swarm's recorded engine version, checks an injected
`EngineVersionSource` for the installed and available exact releases, rejects
inventory/source disagreement, and uses the embedded Docker compatibility
matrix. Export is restricted to the exact lab-evidenced recommended release;
unknown/untested and cross-major targets are blocked. It requires a fresh
active/ready host observation, a manager majority outside a manager target,
no other host in maintenance in that Swarm and the existing Galera maintenance
preview. The host must enter guided maintenance and reach `ready` before a
runbook can be exported. Every Swarm manager is treated as potentially pinned
until exact manager placement is available.

`GET /hosts/{id}/engine-update-plan` and
`POST /hosts/{id}/engine-update-runbook` have OpenAPI schemas, route/schema and
permission/audit tests. The separate `engine_update` resource defaults to the
administrator preset only. Runbook export checks a freshly recomputed digest
and requires explicit manager-host confirmation. The downloadable shell script
prints operator instructions; no manager/agent Docker upgrade, drain, restart
or package execution path exists. The DE/EN page `/engine-updates`, development
mock, UI guard/client tests, [ADR 0019](decisions/0019-docker-engine-update-plans.md)
and [EN](guides/en/18-docker-engine-updates.md)/[DE](guides/de/18-docker-engine-updates.md)
guides are included; Pages links were updated.

**Actual verification:** `gofmt -l .` returned no output (exit 0), `go vet
./...` returned no output (exit 0), `python3 scripts/check_docs.py` reported
76 Markdown files / 69 requirements / 0 errors, and
`python3 scripts/check_deploy.py` reported `deploy checks passed (static only;
no image was built)`. Focused `go test -race -count=1 -p 4` for the new planner,
API route/schema and compatibility checks passed in `internal/manager`,
`internal/api` and `internal/manager/swarm`. The required full race suite was
executed on the final Go tree and **failed**: 25 packages passed, 18 failed,
four have no tests. The sandbox denied existing TCP/Unix listener binds,
including `httptest` (`bind: operation not permitted`); this is not a passing
full suite. In `web/`, `npm run check` found 0 errors and 0 warnings;
`npm test` passed 199 tests in 19 files; `npm run build` succeeded and
`internal/webui/assets/.gitkeep` was touched afterwards. The first web check
could not start because `node_modules` was absent; `npm ci --offline` installed
cached dependencies, then the complete web sequence passed. `git diff --check`
passed. No real infrastructure, Docker daemon, package source or browser
session was contacted.

**Limits and next action:** The shipping manager deliberately has no
`EngineVersionSource`, because supported package methods are an open owner
decision. It returns blocked plans and cannot export a real runbook. The
compatibility seed has only one exact lab-evidenced release and no package
rollback evidence. The UI has mock and unit evidence only. Decide supported
installation methods and update/restart boundaries, add a read-only version
source and a reliable manager-placement signal, then rerun the full race suite
where local listeners are allowed before owned-lab validation. W05/R1 gates
remain unchanged; no Swarm-trial readiness is claimed.

## Manager HA operations (2026-10-25, fakes only)

The scoped R1 HA operations increment is implemented in the manager, metastore
TLS configuration and a separate [`deploy/stack-ha.yaml`](../deploy/stack-ha.yaml).
`manager init -peers` now emits peer API forwarding addresses and an mTLS-only
listener on port 7447. Followers forward API calls once to the current Raft
leader, including the authenticated `POST /operations/{id}/approve` route;
uncertain writes return 503 and are not retried. The leader completes a Raft
barrier before a mutating HTTP request, then rechecks its term. A test with
three in-process metastore nodes approved two guided-recovery records through
followers across a leader change. Another test isolated a leader before its
lease settled and confirmed the write barrier refused the request.

Peer certificates can now renew before expiry without restarting: the HA
configuration mounts a sealed peer CA and passphrase as separate online
secrets and keeps each replica's certificate/key in one atomically replaced
local PEM bundle. A fake-clock test crossed the original certificate expiry
and verified the same TLS certificate callback returned and verified the new
identity. This changes the earlier offline-CA assumption; [ADR 0013](decisions/0013-replica-keys-peer-tls-and-revocation.md)
records the risk and alternatives. The leader compacts revocation entries
only after their recorded certificate expiry, advancing a durable deny-list
revision in the same transaction. A three-node in-process test verified the
shorter list and revision after leader change. Legacy entries without a known
expiry remain listed.

The HA stack has three distinct pinned single-replica services, configs and
local volumes; the static deployment checker guards these properties and the
socket restrictions. It has **not** run on Swarm. No Docker, external service,
agent, database or VM was contacted in this increment. No real mTLS HTTP
forwarding handshake or certificate renewal in a running multi-host manager
was executed; the new forwarding test injects an in-process HTTP transport.
The actual approval route, session and authorization stack were exercised in
that test. Already-open Raft streams are not reauthenticated immediately on
certificate renewal or revocation. The default runtime still does not enable
membership-changing endpoints, and a single-instance volume cannot be
converted to this three-member layout by editing its config.

Actual checks on this worktree: `gofmt -l .` produced no output (exit 0),
`go vet ./...` produced no output (exit 0), and the focused race tests for
renewal, forwarding, compaction and an isolated leader passed (three repeats).
`python3 scripts/check_docs.py` reported 73 Markdown files, 69 requirements,
0 errors; `python3 scripts/check_deploy.py` reported `deploy checks passed
(static only; no image was built)`; 13 deployment-checker unit tests passed.
The full `go test -race -count=1 -p 4 ./...` exited 1: 25 packages passed,
4 had no tests and 19 failed because this sandbox denies local TCP/Unix
listeners (`bind: operation not permitted`), including pre-existing test
harnesses. No non-listener failure was seen in the captured output. A web
check was attempted for documentation/Pages consistency, but all three
commands exited 127: `npm run check` (`svelte-kit: command not found`),
`npm test` (`vitest: command not found`) and `npm run build`
(`vite: command not found`). `web/node_modules` is absent;
`internal/webui/assets/.gitkeep` was touched. These
results do not replace the full suite or a Swarm run in an environment that
permits local listeners and has frontend dependencies installed.
