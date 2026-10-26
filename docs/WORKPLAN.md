# Execution workplan

Status labels: READY, PENDING, IN PROGRESS, DONE, BLOCKED. Only mark DONE with recorded evidence. The product release roadmap remains in [RELEASES.md](RELEASES.md).

| ID | Status | Work package | Completion evidence |
| --- | --- | --- | --- |
| W00 | DONE | Prepare Claude instructions, bootstrap task, Go/monorepo ADR and initial docs workflows | Local documentation check passed; no Git/remote or infrastructure changes |
| W01 | DONE | Resolve publication coordinates/license detail, initialize Git, public push and enable Pages | Public repo https://github.com/MagicalWig34653/db-cluster-manager, commit 684c2ea pushed to `main`, docs-check and Pages runs succeeded, https://magicalwig34653.github.io/db-cluster-manager/ returns HTTP 200 |
| W02 | DONE | Scaffold Go manager/agent/CLI and SvelteKit UI, versioned API contract, reproducible build | `make build/test/check` pass locally and in the `Build and test` workflow; manager served `/api/v1/health`, `/api/v1/version` and the built UI over HTTP. Limitation: UI not rendered in a browser (none installed) |
| W03 | DONE | Design and prototype manager metadata HA/fencing and Swarm/local-storage ownership | ADR 0003 with in-process evidence (`go test -race -count=3 ./...` passes; 4 failure scenarios in `internal/metastore/scenarios_test.go`), library evaluation, ADR 0004 (proposal, unverified against Docker). Limits: in-memory, no VMs, no real network |
| W04 | DONE | Implement dedicated local lab creation/inventory/cleanup within resource budget | `bin/lab` (status/plan/apply/cleanup with ownership manifest, 47+16 unit tests with fake runner). Real cycle on 2026-10-19: one VM `dbcm-lab-m1` (2 CPU, 2 GiB, 10 GiB) created, measured (guest 2 vCPU, ~1.9 GiB RAM, Docker 29.9.1, 3.1 GiB on host disk), removed; the 3 unrelated resources (`colima:default`, 2 `thermoctl-*` VMs) were unchanged and reported intact. Limit: host free space dropped about 3.4 GB across the cycle and was not shown to be reclaimed (other host activity not ruled out) |
| W05 | IN PROGRESS | Execute P0 Galera/agent/routing experiments | P0 acceptance gates in release plan, compatibility limits and test artifacts. Done so far: E1-E8 as recorded in `tests/p0/results/`. E8 (2026-10-24/25) found two independent proxies selecting different writers. The coordinated writer protocol ([ADR 0017](decisions/0017-coordinated-proxy-writer.md)) is implemented and race-tested with fakes and in-memory Raft only; it still needs a controlled E8 rerun. E8's third-node restart failure remains unexplained. Not met: coordinated protocol on VMs, three-manager Swarm-service failover, resource exhaustion/capacity denial, restart during cleanup, native amd64 and remaining P0 gate evidence |
| W06 | IN PROGRESS | Refine R1 work packages from evidence and implement incrementally | Traceable requirements, tests and bilingual user guides per package. 2026-10-23, fakes only: recover permission and full-outage notification, replicated data keyring with recovery envelope, metastore peer mTLS, deny-list distribution and replica certificate revocation (ADR 0013), keyed backup manifest digests (ADR 0009 amendment), host maintenance, drift detection, logical and scheduled backups with retention (see `docs/STATUS.md` "R1 backend gaps closed"). UI for these endpoints exists (mock only; 2026-10-24: progress.lastError, agent version/last answer on Hosts added). 2026-10-24: versioned key rotation with resumable re-encryption, binlog archive/PITR planning and fake replay, CSR provisioning and staged/quorum-checked Raft membership transitions implemented with fakes (ADR 0009/0013 amendments; see STATUS "Key rotation, PITR and membership groundwork"). Required full race check failed on sandbox socket restrictions; focused race suites, vet, format and Python checks passed. Not done: VM evidence, real binlog/event replay, HTTP/UI PITR, authenticated peer readiness and membership backend wiring (default 503). Also 2026-10-23: product-neutral app icon (Icon Composer document, exported with `ictool`), favicons in UI and Pages, mock-server screenshots (EN/DE, light, some dark) in README, Pages landing page and user guides (see `docs/DESIGN.md` "Icon and screenshots"); Jekyll build not run locally |
| W07 | IN PROGRESS (fakes only) | R6 (proposal): service-type framework, Galera expressed as a service type without changing its safety logic, generic containers, plan preview/export/expert view, Docker engine updates after the X09 boundary decision | R6 exit gates in the release plan. Done so far, **fakes only** (2026-10-23, branch `w07-service-types`): `internal/service` registry with `container` and `traefik` types, plans with artifacts/warnings/blockers, conversion to agent specs and runner operations, manager `Services` and `/services` API routes with approval and audit, `service` authz resource. 2026-10-24, **fakes only** (branch `w07-galera-type`): `galera` service type delegating every plan to `internal/galera` unchanged and routed to the unchanged cluster flows (tests prove identical plans/operations to the direct path and no bypass of quorum, recovery approval or cluster authz); update strategy (stop-first/start-first, Swarm rollback on failure, canary field blocked) through the existing `update-service` command with optional Swarm update config, `await-service` and a live service-healthy gate, rollback plan; service health from Swarm task state in the API and to notifications; expert view (`agentSteps`) and YAML export for every plan; `/services/{id}/update-plan` and `/update`, OpenAPI and contract tests (see `docs/STATUS.md` "W07 second step"). Checks: gofmt, `go vet`, `go test -race -count=1 -p 4 ./...`, `check_docs.py` passed. Not done: any real run (no Swarm, Docker or Traefik contact), canary executor, automatic manager-side rollback, galera node update/scale planner, UI/CLI for updates, health, expert view and galera type, real Docker version discovery and engine execution. 2026-10-25 added fakes-only X09 plans, digest-gated operator runbook, API, UI and guides; see the X09/W07 handover below. Starts after R1 unless the owner decides otherwise |
| W08 | IN PROGRESS | R7 (proposal): image build service, managed/external registries, web server composer (Nginx/PHP/extensions/config) | R7 exit gates; depends on build-tool and registry decisions. 2026-10-23, fakes only: `internal/imagebuild` (composition model, deterministic Dockerfile render, plan with approval digest, Builder interface, docker/BuildKit command construction, job records with log redaction), `internal/registry` (OCI client against an `httptest` registry, managed `registry:2` spec, distribution policy) and `/image-builds`, `/registries` API routes (see `docs/STATUS.md` "Image build and registry groundwork"). 2026-10-24 wiring step, fakes only: replicated stores for registries, sealed registry credentials, jobs and plans, `manager.imageBuild` config with `DockerBuilder`, `manager init -image-build`, interrupted-job recovery (see `docs/STATUS.md` "W08 wiring step"). Not done: real build/push (manager image has no docker CLI), UI against the real manager, any real build or push, SBOM/scan/signing, managed registry deployment (needs W07) |
| W09 | IN PROGRESS | R8 (proposal): storage protocol servers (SFTP, FTP(S), S3-compatible, NFS) and Traefik UI management | R8 exit gates. 2026-10-24, fakes only: `sftp`, `ftp`, `s3`, `nfs` service types (NFS always blocked), agent support for published ports and owned Swarm configs, Traefik management metadata (see `docs/STATUS.md` "W09 groundwork"). 2026-10-24 (`cx-configs`), fakes only: agent config create/remove, ownership and persistent allow-list, Traefik/SFTP/FTP config delivery and cleanup, external-config allow-list resolution (see STATUS config delivery). Targeted race tests pass; full race suite fails on sandbox listener denial plus one manager abort timeout (isolated rerun passed). Not done: real config lifecycle evidence, immutable-config rotation, any real run, UI, backup, credential rotation, NFS feasibility evidence |
| W10 | IN PROGRESS (fakes only) | R9 (proposal): Kubernetes back end and Swarm/Kubernetes cluster provisioning | 2026-10-24, branch `cx-k8s`: opt-in Kubernetes renderer, local PV blockers, fake KubeAPI and sealed metadata registration (ADR 0016; [prior evidence](STATUS.md#w10-kubernetes-groundwork)). 2026-10-25, `cx-provision`: Swarm SSH host preflight, exportable script plans, sealed credentials, strict pinned host-key transport, digest-bound approval, term-fenced runner/ownership manifest, API/OpenAPI/authz and DE/EN UI/mock/guide; k3s scripts preview behind a second flag, no execution (ADR 0018; [actual verification](STATUS.md#w10-ssh-cluster-provisioning-increment-2026-10-25-fakes-only)). Focused race tests and web checks pass; final full Go race suite fails on sandbox-denied listeners (26 pass, 19 fail, four without tests). One manager abort timeout in an earlier run passed alone and did not recur in the final run. No real SSH/cluster/browser run, no cleanup command or R9 gate. Next: threat review, owned lab disconnect/restart tests and manifest-scoped cleanup, with Kubernetes timing/distribution still an owner decision. No push. |
| W11 | IN PROGRESS | R10 (proposal): AI-assisted vulnerability response (X13): advisory feed polling and inventory matching, optional AI analysis with selectable providers/models, approval-gated or policy-gated patched rebuilds and mitigations | R10 exit gates; depends on W07 and W08 and on the open decisions in ADR 0015. 2026-10-24, fakes only: `internal/vuln` (OSV/GitHub/NVD/KEV/offline feed adapters against `httptest`, SBOM-style inventory, deterministic matcher, findings with KEV/EPSS priority and VEX status, dedup and persistence in the metastore, proposals with approvable plans and rating, policy modes notify-only/approval-required/auto-for-policy, audit), `internal/ai` (Claude API, OpenAI API, OpenAI-compatible, Claude Code and Codex CLI providers through a restricted runner, consent, sealed keys, redaction, delimited prompts, strict proposal schema), `/vuln/*` routes with authz resource `vuln` (see `docs/STATUS.md` "W11 groundwork"). 2026-10-24 wiring (branch `w11-wiring`, fakes and mock only): `manager.vuln` configuration, leader-only scheduler, inventory from service/cluster/build records, notification adapter, persistent budgets and audit sequence, UI (findings, detail with approve/reject, policy with auto warning, providers with consent), DE/EN, mock screenshots (see `docs/STATUS.md` "W11 wiring and UI"). Not done: any real feed or AI call, SBOM at build time, rescan, rollout/canary/rollback, matcher and injection corpora at exit-gate scale |

W11 comes from the owner's 2026-10-23 proposal ([ADR 0015](decisions/0015-ai-assisted-vulnerability-response.md), proposed). W07–W10 come from the 2026-10-22 scope extension ([ADR 0012](decisions/0012-broadened-scope-platform.md)). They do not change the current next action; Galera/R1 (W05, W06) stays first.

W10 `cx-provision` commit limitation (2026-10-25): staging could not create the shared worktree index lock outside the writable sandbox. Changes are uncommitted in this worktree; no push. When metadata is writable, review and make small core, API/manager and UI/docs commits. The [actual checks and limits](STATUS.md#w10-ssh-cluster-provisioning-increment-2026-10-25-fakes-only) remain the handover source.

W03 experiments requiring VMs depend on the safe lab controls from W04. Design work can start first; do not run ad hoc experiments before ownership and cleanup are in place.

## Session handover fields

For the active task, maintain: objective, changed files/contracts, executed checks/results, unresolved issues, and next concrete action. Keep this concise and update `docs/STATUS.md`; avoid duplicating the full requirement catalog.

## Current next action

Abort view-ordering fix (2026-10-25, `cx-abortfix`): a deterministic regression
test now covers a stale `running` snapshot arriving after `aborted`; the manager
keeps the terminal view and the in-flight step's unknown outcome. Three
separate 30-count `TestAbortCreate` race runs passed. The baseline `536154d`
also failed once under concurrent host load, so the suspected range is not a
valid monotonic bisect range. The full race suite was attempted and failed on
sandbox-denied local listeners; formatting, vet and documentation checks
passed. See [actual results](STATUS.md#abort-view-ordering-fix-2026-10-25-cx-abortfix).
Next for this fix: rerun `go test -race -count=1 -p 4 ./...` where local
listeners are permitted. W05 and R1 release gates remain open.

E4 re-run 2026-10-24: Part A attempt 4 passed all 19 steps and Part B attempt 2 (three manager replicas with `manager.keyring` and `manager.peerTLS`, two leader SIGKILLs) passed all steps, both on lab VMs under foreign host load ([A4](../tests/p0/results/e4-rerun-A4.md), [B2](../tests/p0/results/e4-rerun-B2.md)). Fixed before the runs (fakes first): the API reports an operator abort as `aborted`; the runner journals a due retry and the attempt's agent step ID, so a deletion survives a manager restart during the "volume in use" backoff; replica configs whose node ID is not in the peers are refused; the script's node-ID quoting and replica-log capture. Next: the remaining W05 items below (redundant proxy/VIP, VM power-loss outage, native amd64 evidence, `deploy/stack.yaml` with a `manager init` configuration on lab VMs, done in E7).

E8 writer coordination (2026-10-25): manager and proxy configuration, leader-only authenticated choice/ACK/switch API, replicated freeze state and DE/EN guides are implemented. Focused race tests passed in `internal/api`, `internal/manager`, `internal/proxy` and `cmd/manager`; the full suite is blocked by this sandbox's listener prohibition. Next: run the full race suite where local sockets are permitted, then rerun E8 in a dedicated lab with two proxies and a partition during the freeze. The user requested fake/unit tests only for this worktree task, so no infrastructure run belongs to this package. Other W05 gaps remain in [STATUS.md](STATUS.md#next-action).


## Service UI follow-up (2026-10-24)

Implemented the frontend part of W07/W09 for the existing backend contracts: Galera plus typed storage creation, health, container/Traefik image updates and rollback strategies, plan downloads and expert agent steps. Added DE/EN labels and guide 13 instructions, extended mock fixtures, and 22 frontend tests (185 total passing across 15 files). `npm run check` and `npm run build` passed; `gofmt -l .` was empty; the documentation checker passed (68 files / 69 requirements). The build placeholder was restored after builds.

This package remains **IN PROGRESS** for browser evidence: the mock server and cached Chromium both failed to start under sandbox restrictions, so no new screenshot is claimed. `scripts/screenshots/services.mjs` passed syntax checking but its DE/EN capture/download assertions have not run. The full Go race suite was executed with a temporary writable cache and failed (22 packages passed / 18 failed / 5 without tests); denied local socket binds prevent the network-backed tests. Git commits are in separate temporary metadata because the shared worktree Git directory is read-only here; no push.

Next concrete action: import the local commits into a writable checkout, rerun the Go race suite where loopback listeners work, run and inspect the service capture script, replace the earlier mock screenshots in guide 13/README, then test the new flows against a real manager. Optional S3 Traefik-route editing, advanced Galera placement, service configuration editing beyond images, NFS execution and canary execution remain outside this UI increment. W05/R1 release gates are unchanged.

## cx-configs handover (2026-10-24)

Implemented the scoped W07/W09 config delivery step; contracts, safety rules, actual checks and limitations are recorded in [STATUS](STATUS.md#w07w09-config-delivery-2026-10-24-fakes-only). No infrastructure was created. Targeted race tests pass (including encoding and Galera/service drift contracts); full race suite fails on sandbox-denied listeners plus one manager abort timeout (isolated race rerun passed). Formatting, vet, documentation and static deployment checks pass; deployment checks lack PyYAML. Implementation and guides committed as `5521645` and `2ace890`; no push. Next concrete action: rerun the full race suite with local listeners available, investigate any recurring abort timeout, then test config lifecycle in the owned lab. This does not replace the W05 priorities above.

## Kubernetes and security UI handover (2026-10-24)

Completed the requested frontend increment on `cx-ui-k8s`: Kubernetes metadata
registration/listing with write-only credentials, global vulnerability audit
history, Swarm config artifact/dependency display and mock `create-config` /
`remove-config` agent steps. Existing findings/detail/policy/provider pages and
replica-key/certificate screens were retained. `integration` has no HTTP API for
data-key rotation or metastore membership mutation, so those conditional screens
were not added. DE/EN guides 13/15/16 and UI strings are current.

Actual verification: `gofmt -l .` and `go vet ./...` exit 0 without output;
`check_docs.py` reports 71 Markdown files / 69 requirements / 0 errors;
`check_deploy.py` passes static checks (no image built). Frontend check reports
0 errors / 0 warnings; vitest passes 18 files / 196 tests; static build passes
and the assets `.gitkeep` was restored with `touch`. Full
`go test -race -count=1 -p 4 ./...` exits 1: 24 packages pass, 18 fail, 5 have no
tests; sandbox-denied local TCP/Unix listeners prevent completion. No real
infrastructure was contacted. See [actual scope and limits](STATUS.md#kubernetes-and-security-ui-follow-up-2026-10-24).

Remaining: browser/DE/EN visual evidence and fake-backed manager UI integration;
full race rerun where local listeners work. Kubernetes remains metadata-only,
experimental and unwired in the shipping manager. These local UI changes do not
alter W05/R1 priorities or satisfy W10/W11 release gates. Next action: review
both locales with synthetic fixtures and rerun the Go suite in a suitable sandbox.
No push.

## W06 manager listener ACME/TLS handover (2026-10-24)

Implemented the scoped ACME/TLS manager listener increment; contracts,
verification and limits are in [STATUS](STATUS.md#manager-listener-acmetls-2026-10-24-fakes-only).
HTTP-01/TLS-ALPN-01, sealed replicated account/keypair/challenge storage,
leadership-scoped renewal, explicit `manager.tls` exposure policy and verified
HTTPS healthchecks are wired. The lower-level existing x/crypto ACME client
permits cancellation instead of autocert's unmanaged renewal timers (ADR 0014
amendment). EN/DE guide 17, configuration/security references and installation
notes are current. The prototype container now defaults to loopback.

Actual checks: formatting and vet passed; focused new race tests passed, with
two socket-backed fake-CA subtests skipped on sandbox bind denial. Full
`go test -race -count=1 -p 4 ./...` failed: 25 packages passed, 18 failed on
sandbox-denied TCP/Unix listeners, five had no tests. Documentation checker:
73 Markdown files / 69 requirements / 0 errors. Deployment checker passed
statically; no image built. No frontend code changed or frontend checks run.
No real infrastructure or public CA was contacted. No release gate is met by
this increment; DNS-01/database issuance and real routing/trust evidence remain
open. The existing W05/R1 priorities are unchanged.

Local implementation commits: `2d28eb3` (core/storage) and `a7791c8`
(listener/config/healthcheck/container defaults); documentation is committed
separately. The first combined-shell commit failed on `index.lock`, but the
standalone retry succeeded. No push made. Next concrete action: rerun the full
suite and socket-backed fake-CA tests where loopback listeners are allowed.
Any real CA, DNS or deployment work needs a separately authorized scope.

## amd64 CI handover (2026-10-24)

Build/CI configuration for W05/P10 is implemented on `cx-amd64`: native Ubuntu
amd64/arm64 Go/web checks and both native image builds without push; version-tag
release waits for those checks and builds/publishes both multi-platform images
with SBOM/provenance. Existing action SHA pins are retained. Deployment workflow
guards and 12 mutation tests cover publication permissions, trigger/dependency,
platforms and attestations. Deployment notes and the native amd64 P0 evidence
plan are current; choosing an owner amd64 host versus cloud VM remains open.

[Actual verification and limits](STATUS.md#amd64-build-and-ci-support-2026-10-24):
format/vet/docs/deploy checks and 12 checker tests passed (also without PyYAML);
web check, 185 tests and production build passed after offline dependency
installation, and `.gitkeep` was restored. Full Go race suite failed (24 packages
passed, 18 failed, 5 without tests) on sandbox listener restrictions plus one
manager abort timeout; that test passed on an isolated race rerun. No image,
Actions run or native amd64 P0 evidence exists for this change. No infrastructure
contact or push occurred. Git staging/commits are blocked by the shared metadata
`index.lock` permission; all changes remain in this worktree.

Next: create small reviewed commits when metadata is writable, execute native
CI, inspect version-tag image manifests/attestations after an explicit release
trigger, then obtain the owner host/VM decision and a scoped lab plan before
P0. W05 stays IN PROGRESS and its release gates are unchanged.

## Galera operations handover (2026-10-24, cx-galera-ops)

W05/W06 follow-up implemented with fakes only: approvable scale-out/in,
rolling restart/maintenance and same-series patch upgrades, live quorum/Synced
and identity gates, retained removed volumes, replicated digest approval and
atomic desired-state completion across leader changes. Cluster operation-plan
API, operate authorization/approval mapping, OpenAPI and route/schema contracts,
refusal tests, actual agent executor with fake Docker, in-process Raft takeover
and completed-journal reconciliation, ADR 0011 and DE/EN guides are included.
Major/series upgrades remain refused pending product-level approval and a curated
path; no frontend was changed. This supersedes earlier statements that no Galera
scale/update planner exists; real rollout evidence remains missing.

Actual checks: gofmt empty, vet passed, focused race tests in four packages
passed, docs passed (71 Markdown files / 69 requirements), static deploy check
passed. Full `go test -race -count=1 -p 4 ./...` **failed**: 24 packages passed,
18 failed on sandbox-denied local listeners, 5 without tests. An earlier
full-run abort timeout passed on focused rerun; new HA test timing was raised
from the shared 50 ms prototype default to 300 ms after election churn, without
changing production settings. See [full results and limitations](STATUS.md#galera-operation-plans-2026-10-24-cx-galera-ops-fakes-only).

Git staging failed because the shared worktree Git metadata is not writable;
no commits and no push. Next: small commits with metadata write access and a
full race rerun with local listeners permitted. New feature lab evidence remains
pending, requires the owned lab, and does not replace current W05/R1 release
gates. This task used no real infrastructure.


## cx-review-fixes handover (2026-10-25)

Eight review findings were fixed in small local commits with test-first
regressions: generic container credential-value rejection, CLI provider
configuration opt-in and warning, registry secret isolation under concurrent
registration, durable vulnerability execution intent and idempotent image-build
claim, config creation adoption after a crash, conservative validated AI usage
with store-error handling, complete prompt string screening, and recovery-envelope
verification before data-key rotation. [Actual behavior and check results](STATUS.md#review-fixes-2026-10-25-cx-review-fixes-fakes-only)
are recorded in STATUS. No infrastructure was used. Focused new tests pass under
`-race`; the required full race suite was run three times, including the final tree,
and failed because the sandbox
denies existing local listener tests (18 packages failed, 25 passed, five without
tests). Vet, formatting, documentation and static deployment checks passed; the
first formatting check listed two files, which were corrected before the final
clean check. Next: rerun the full
race suite with local listeners permitted, inspect any remaining failures, then
resume W05/R1 release work. No Swarm-trial readiness is claimed.


## Keys/PITR/membership handover (2026-10-24)

Implemented in small local commits on `codex-keys-pitr`; no push. Scope stayed on
fakes and in-memory Raft. STATUS and ADR amendments distinguish runnable rotation
from library-only PITR/fake replay and opt-in membership (default runtime 503).
The required complete race run was executed and failed on sandbox-denied socket
binding; passing focused race tests do not replace that release check. Next:
repeat the unchanged verification loop with local sockets permitted, then provide
authenticated remote readiness and a real binlog parser before infrastructure
validation. Existing W05 gates remain open; no Swarm-trial readiness is claimed.
W05 remaining (needs VMs, one at a time): validate the new coordinated writer choice and its partition behavior in a controlled E8 rerun; re-run the E8 B2/B3 stuck-node case with slot logs and a case where Galera cannot self-recover; native amd64 evidence (strategy still open); the three-replica manager as Swarm services from `deploy/stack.yaml` (only plain processes ran). Done 2026-10-24 (E7): images built from `deploy/Dockerfile` and `deploy/stack.yaml` deployed with a `manager init` configuration on lab VMs, see [results](../tests/p0/results/e7-stack-deploy.md); still open there: the per-replica three-service layout, a TLS reverse proxy and backups from the stack. W06 (R1) then continues with UI and user guides for maintenance, drift, keyring/replicas and backup retention, key rotation, and the rest of `docs/STATUS.md` "What R1 still lacks".

W05 remaining (needs VMs, one at a time): coordinated writer choice for redundant proxies (E8 finding); re-run of the E8 B2/B3 stuck-node case with slot logs and a case where Galera cannot self-recover; native amd64 evidence (strategy still open); the three-replica manager as Swarm services from `deploy/stack.yaml` (only plain processes ran). Done 2026-10-24 (E7): images built from `deploy/Dockerfile` and `deploy/stack.yaml` deployed with a `manager init` configuration on lab VMs, see [results](../tests/p0/results/e7-stack-deploy.md); still open there: the per-replica three-service layout, a TLS reverse proxy and backups from the stack. W06 (R1) then continues with UI and user guides for maintenance, drift, keyring/replicas and backup retention, key rotation, and the rest of `docs/STATUS.md` "What R1 still lacks".

## X09/W07 Docker Engine update plan handover (2026-10-25)

Added single-host read-only Docker Engine update planning, a fake installed/
available version source, strict compatibility and version checks, Swarm
manager quorum, one-host maintenance and Galera checks, a separate
`engine_update` authorization resource, digest-gated audited runbook export,
OpenAPI and contract tests, a DE/EN UI page with mock/tests, guides, Pages links
and [ADR 0019](decisions/0019-docker-engine-update-plans.md). The runbook only
prints operator steps. The shipping manager has no version source configured,
so export remains blocked. This is fakes and mock evidence only, not an engine
update or an R6 exit gate.

[Actual checks and limits](STATUS.md#x09-docker-engine-update-plans-2026-10-25-fakes-and-mock-only):
gofmt, vet, docs and static deploy checks passed; the focused planner/API/
matrix race suite passed; web check passed with 0 errors/warnings, 199 tests
passed and the build succeeded. The required full Go race suite **failed** in
the sandbox because existing TCP/Unix listener tests cannot bind (25 packages
passed, 18 failed, four without tests). No real infrastructure was contacted.
Next: decide the installation methods and automatic/restart boundaries, add a
read-only package source and exact pinned manager placement, then rerun the
full race suite where listeners work before scoped lab testing. W05/R1
priorities and release gates remain unchanged.

W05 remaining (needs VMs, one at a time): coordinated writer choice for redundant proxies (E8 finding); re-run of the E8 B2/B3 stuck-node case with slot logs and a case where Galera cannot self-recover; native amd64 evidence (strategy still open); the three-replica manager as Swarm services from `deploy/stack-ha.yaml` (only plain processes ran; the new file has static checks only). Done 2026-10-24 (E7): images built from `deploy/Dockerfile` and `deploy/stack.yaml` deployed with a `manager init` configuration on lab VMs, see [results](../tests/p0/results/e7-stack-deploy.md); still open there: the per-replica three-service layout, a TLS reverse proxy and backups from the stack. W06 (R1) then continues with UI and user guides for maintenance, drift, keyring/replicas and backup retention, key rotation, and the rest of `docs/STATUS.md` "What R1 still lacks".

## W05 / R1 manager HA operations (2026-10-25)

Implemented the scoped fake-only HA increment: optional peer certificate
renewal from separately mounted sealed-CA/passphrase secrets and an atomic
local bundle; expiry-aware deny-list compaction with a replicated monotonic
revision; follower-to-leader mTLS API forwarding with one-send semantics,
including the real authenticated recovery-approval route; a per-write Raft
barrier for the isolated-leader lease edge; and `deploy/stack-ha.yaml` with
three pinned Swarm services. Code, static checker, ADR 0013, INSTALL,
English/German guides and Pages copy were updated. Evidence and exact check
results are in [STATUS](STATUS.md#manager-ha-operations-2026-10-25-fakes-only).

Next: in a test environment that permits local listeners and has frontend
dependencies, rerun the complete race and web suites, then exercise the
mTLS forwarding listener and certificate renewal on a three-node owned lab.
Only after that, deploy `stack-ha.yaml` on the owned lab Swarm and record
placement, leader failover, recovery approval through a follower, renewal
across certificate expiry, and deny-list shrink propagation. Do not treat
static checks or the in-process tests as Swarm evidence. The other W05/P0
release gates remain open.
