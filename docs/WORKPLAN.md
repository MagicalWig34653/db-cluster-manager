# Execution workplan

Status labels: READY, PENDING, IN PROGRESS, DONE, BLOCKED. Only mark DONE with recorded evidence. The product release roadmap remains in [RELEASES.md](RELEASES.md).

| ID | Status | Work package | Completion evidence |
| --- | --- | --- | --- |
| W00 | DONE | Prepare Claude instructions, bootstrap task, Go/monorepo ADR and initial docs workflows | Local documentation check passed; no Git/remote or infrastructure changes |
| W01 | DONE | Resolve publication coordinates/license detail, initialize Git, public push and enable Pages | Public repo https://github.com/MagicalWig34653/db-cluster-manager, commit 684c2ea pushed to `main`, docs-check and Pages runs succeeded, https://magicalwig34653.github.io/db-cluster-manager/ returns HTTP 200 |
| W02 | DONE | Scaffold Go manager/agent/CLI and SvelteKit UI, versioned API contract, reproducible build | `make build/test/check` pass locally and in the `Build and test` workflow; manager served `/api/v1/health`, `/api/v1/version` and the built UI over HTTP. Limitation: UI not rendered in a browser (none installed) |
| W03 | DONE | Design and prototype manager metadata HA/fencing and Swarm/local-storage ownership | ADR 0003 with in-process evidence (`go test -race -count=3 ./...` passes; 4 failure scenarios in `internal/metastore/scenarios_test.go`), library evaluation, ADR 0004 (proposal, unverified against Docker). Limits: in-memory, no VMs, no real network |
| W04 | DONE | Implement dedicated local lab creation/inventory/cleanup within resource budget | `bin/lab` (status/plan/apply/cleanup with ownership manifest, 47+16 unit tests with fake runner). Real cycle on 2026-10-19: one VM `dbcm-lab-m1` (2 CPU, 2 GiB, 10 GiB) created, measured (guest 2 vCPU, ~1.9 GiB RAM, Docker 29.9.1, 3.1 GiB on host disk), removed; the 3 unrelated resources (`colima:default`, 2 `thermoctl-*` VMs) were unchanged and reported intact. Limit: host free space dropped about 3.4 GB across the cycle and was not shown to be reclaimed (other host activity not ruled out) |
| W05 | IN PROGRESS | Execute P0 Galera/agent/routing experiments | P0 acceptance gates in release plan, compatibility limits and test artifacts. Done so far: E1-E8 as recorded in `tests/p0/results/`. E8 (2026-10-24/25) found two independent proxies selecting different writers. The coordinated writer protocol ([ADR 0017](decisions/0017-coordinated-proxy-writer.md)) is implemented and race-tested with fakes and in-memory Raft only; E9 (2026-10-26, [results](../tests/p0/results/e9-ha-proxy-writer.md)) ran the three-service HA manager stack and ADR 0017 on lab VMs (worked; costs: a blocked switch stops all writes, loss of the writer VM leaves no writer for 62 s) and diagnosed E8's third-node failure (zero cluster UUID after a total power loss, joiner SST/IST fails); E12 (2026-10-28/29, [results](../tests/p0/results/e12-recovery-autofailover-rolling.md)) verified `autoFailover` on VMs (after a fix), the zero-UUID incident and 409 reason on the real cluster, a manual recovery and a valid B3, HA stack rolling update and certificate renewal; the product itself cannot recover the zero-UUID state (no candidate). Not met: a product recovery path for the zero-UUID state, resource exhaustion/capacity denial, restart during cleanup, native amd64 and remaining P0 gate evidence |
| W06 | IN PROGRESS | Refine R1 work packages from evidence and implement incrementally | Traceable requirements, tests and bilingual user guides per package. 2026-10-23, fakes only: recover permission and full-outage notification, replicated data keyring with recovery envelope, metastore peer mTLS, deny-list distribution and replica certificate revocation (ADR 0013), keyed backup manifest digests (ADR 0009 amendment), host maintenance, drift detection, logical and scheduled backups with retention (see `docs/STATUS.md` "R1 backend gaps closed"). UI for these endpoints exists (mock only; 2026-10-24: progress.lastError, agent version/last answer on Hosts added). 2026-10-24: versioned key rotation with resumable re-encryption, binlog archive/PITR planning and fake replay, CSR provisioning and staged/quorum-checked Raft membership transitions implemented with fakes (ADR 0009/0013 amendments; see STATUS "Key rotation, PITR and membership groundwork"). Required full race check failed on sandbox socket restrictions; focused race suites, vet, format and Python checks passed. Not done: VM evidence, real binlog/event replay, HTTP/UI PITR, authenticated peer readiness and membership backend wiring (default 503). Also 2026-10-23: product-neutral app icon (Icon Composer document, exported with `ictool`), favicons in UI and Pages, mock-server screenshots (EN/DE, light, some dark) in README, Pages landing page and user guides (see `docs/DESIGN.md` "Icon and screenshots"); Jekyll build not run locally |
| W07 | IN PROGRESS (fakes only) | R6 (proposal): service-type framework, Galera expressed as a service type without changing its safety logic, generic containers, plan preview/export/expert view, Docker engine updates after the X09 boundary decision | R6 exit gates in the release plan. Done so far, **fakes only** (2026-10-23, branch `w07-service-types`): `internal/service` registry with `container` and `traefik` types, plans with artifacts/warnings/blockers, conversion to agent specs and runner operations, manager `Services` and `/services` API routes with approval and audit, `service` authz resource. 2026-10-24, **fakes only** (branch `w07-galera-type`): `galera` service type delegating every plan to `internal/galera` unchanged and routed to the unchanged cluster flows (tests prove identical plans/operations to the direct path and no bypass of quorum, recovery approval or cluster authz); update strategy (stop-first/start-first, Swarm rollback on failure, canary field blocked) through the existing `update-service` command with optional Swarm update config, `await-service` and a live service-healthy gate, rollback plan; service health from Swarm task state in the API and to notifications; expert view (`agentSteps`) and YAML export for every plan; `/services/{id}/update-plan` and `/update`, OpenAPI and contract tests (see `docs/STATUS.md` "W07 second step"). Checks: gofmt, `go vet`, `go test -race -count=1 -p 4 ./...`, `check_docs.py` passed. Not done: any real run (no Swarm, Docker or Traefik contact), canary executor, automatic manager-side rollback, galera node update/scale planner, UI/CLI for updates, health, expert view and galera type, real Docker version discovery and engine execution. 2026-10-25 added fakes-only X09 plans, digest-gated operator runbook, API, UI and guides; see the X09/W07 handover below. Starts after R1 unless the owner decides otherwise |
| W08 | IN PROGRESS | R7 (proposal): image build service, managed/external registries, web server composer (Nginx/PHP/extensions/config) | R7 exit gates; depends on build-tool and registry decisions. 2026-10-23, fakes only: `internal/imagebuild` (composition model, deterministic Dockerfile render, plan with approval digest, Builder interface, docker/BuildKit command construction, job records with log redaction), `internal/registry` (OCI client against an `httptest` registry, managed `registry:2` spec, distribution policy) and `/image-builds`, `/registries` API routes (see `docs/STATUS.md` "Image build and registry groundwork"). 2026-10-24 wiring step, fakes only: replicated stores for registries, sealed registry credentials, jobs and plans, `manager.imageBuild` config with `DockerBuilder`, `manager init -image-build`, interrupted-job recovery (see `docs/STATUS.md` "W08 wiring step"). Not done: real build/push (manager image has no docker CLI), UI against the real manager, any real build or push, SBOM/scan/signing, managed registry deployment (needs W07) |
| W09 | IN PROGRESS | R8 (proposal): storage protocol servers (SFTP, FTP(S), S3-compatible, NFS) and Traefik UI management | R8 exit gates. 2026-10-24, fakes only: `sftp`, `ftp`, `s3`, `nfs` service types (NFS always blocked), agent support for published ports and owned Swarm configs, Traefik management metadata (see `docs/STATUS.md` "W09 groundwork"). 2026-10-24 (`cx-configs`), fakes only: agent config create/remove, ownership and persistent allow-list, Traefik/SFTP/FTP config delivery and cleanup, external-config allow-list resolution (see STATUS config delivery). Targeted race tests pass; full race suite fails on sandbox listener denial plus one manager abort timeout (isolated rerun passed). Not done: real config lifecycle evidence, immutable-config rotation, any real run, UI, backup, credential rotation, NFS feasibility evidence |
| W10 | IN PROGRESS (fakes only) | R9 (proposal): Kubernetes back end and Swarm/Kubernetes cluster provisioning | 2026-10-24, branch `cx-k8s`: opt-in Kubernetes renderer, local PV blockers, fake KubeAPI and sealed metadata registration (ADR 0016; [prior evidence](STATUS.md#w10-kubernetes-groundwork)). 2026-10-25, `cx-provision`: Swarm SSH host preflight, exportable script plans, sealed credentials, strict pinned host-key transport, digest-bound approval, term-fenced runner/ownership manifest, API/OpenAPI/authz and DE/EN UI/mock/guide; k3s scripts preview behind a second flag, no execution (ADR 0018; [actual verification](STATUS.md#w10-ssh-cluster-provisioning-increment-2026-10-25-fakes-only)). Focused race tests and web checks pass; final full Go race suite fails on sandbox-denied listeners (26 pass, 19 fail, four without tests). One manager abort timeout in an earlier run passed alone and did not recur in the final run. No real SSH/cluster/browser run, no cleanup command or R9 gate. Next: threat review, owned lab disconnect/restart tests and manifest-scoped cleanup, with Kubernetes timing/distribution still an owner decision. No push. |
| W11 | IN PROGRESS | R10 (proposal): AI-assisted vulnerability response (X13): advisory feed polling and inventory matching, optional AI analysis with selectable providers/models, approval-gated or policy-gated patched rebuilds and mitigations | R10 exit gates; depends on W07 and W08 and on the open decisions in ADR 0015. 2026-10-24, fakes only: `internal/vuln` (OSV/GitHub/NVD/KEV/offline feed adapters against `httptest`, SBOM-style inventory, deterministic matcher, findings with KEV/EPSS priority and VEX status, dedup and persistence in the metastore, proposals with approvable plans and rating, policy modes notify-only/approval-required/auto-for-policy, audit), `internal/ai` (Claude API, OpenAI API, OpenAI-compatible, Claude Code and Codex CLI providers through a restricted runner, consent, sealed keys, redaction, delimited prompts, strict proposal schema), `/vuln/*` routes with authz resource `vuln` (see `docs/STATUS.md` "W11 groundwork"). 2026-10-24 wiring (branch `w11-wiring`, fakes and mock only): `manager.vuln` configuration, leader-only scheduler, inventory from service/cluster/build records, notification adapter, persistent budgets and audit sequence, UI (findings, detail with approve/reject, policy with auto warning, providers with consent), DE/EN, mock screenshots (see `docs/STATUS.md` "W11 wiring and UI"). Not done: any real feed or AI call, SBOM at build time, rescan, rollout/canary/rollback, matcher and injection corpora at exit-gate scale |
| W12 | IN PROGRESS (fakes + lab) | R11 (proposal): OS patch management with native agent (X15, [ADR 0023](decisions/0023-os-patch-management.md)) | 2026-10-30 (fakes only): Docker engine packages are the report-only `container-runtime` patch class with engine guard, post-update health check and version record; ADR 0019 plans are now only the preview ([STATUS](STATUS.md)); next: real `EngineRuntimeSource` and a lab run. 2026-10-27: `internal/patch`, `agent patch`, REST API, authz, DE/EN guide 23 with fakes; lab run E10 on Debian 12 (apt) and AlmaLinux 9.8 (dnf), [results](../tests/p0/results/e10-patch.md). 2026-10-27 (fakes and loopback): wired into the managed runtime behind `manager.patch.enabled` (default off, 503 when off; metastore, leader-term fenced, scheduler, interrupted-plan recovery), real `PatchGaleraGuard` on the guided host maintenance, patch-agent deny list and durable replay suppression, `rpmvercmp` ordering for RPM ecosystems ([STATUS](STATUS.md)). Next: lab run with the managed runtime and an enrolled patch agent (VM budget), SSH install wiring (sealed draft store, token source), planner coordination against a real cluster, UI (gap). |
| W12 | IN PROGRESS (fakes only) | X14 native restic backup of managed and external hosts | 2026-10-27: fake-runner CLI/JSON adapter, managed-agent commands/path policy, metastore source records and sealed credentials, scheduler/failure notification, approval-gated new-path restore, REST/OpenAPI/authz tests and DE/EN guides ([actual results](STATUS.md#x14w12-native-restic-host-backup-increment-2026-10-27-fakes-only)). Open: deliver mounted agent secrets and source paths to deployed global agents; test the fake-tested SSH adapter on an owned external host; implement independent external-agent enrollment; test real restic/backends/restore in an owned disposable environment; UI. No R1/W05 gate changed. |

| W12 | IN PROGRESS (fakes + lab) | R11 (proposal): OS patch management with native agent (X15, [ADR 0023](decisions/0023-os-patch-management.md)) | 2026-10-27: `internal/patch`, `agent patch`, REST API, authz, DE/EN guide 23 with fakes; lab run E10 on Debian 12 (apt) and AlmaLinux 9.8 (dnf), [results](../tests/p0/results/e10-patch.md). Next: wire into the managed manager runtime (KV, sealer, token source, schedule reconciler), real cluster planner coordination, patch-agent revocation, enrollment/mTLS and SSH on a lab VM, UI (gap). |
| W12 | IN PROGRESS (fakes + lab) | X14 native restic backup of managed and external hosts | 2026-10-27: lab run E11 with real restic, local/SFTP/rest-server/S3 backends and an SSH external host passed after 6 bug fixes ([results](../tests/p0/results/e11-restic.md)); image carries pinned restic, `AgentMTLS.Restic` opt-in deployment (unit-tested only). Earlier: fake-runner CLI/JSON adapter, managed-agent commands/path policy, metastore source records and sealed credentials, scheduler/failure notification, approval-gated new-path restore, REST/OpenAPI/authz tests and DE/EN guides ([actual results](STATUS.md#x14w12-native-restic-host-backup-increment-2026-10-27-fakes-only)). Open: build the image and run the opt-in deployment as a Swarm service with mounted secrets; wire `AgentRestic` into manager configuration, create secrets from sealed credentials and update an existing agent service; repository init and SFTP key provisioning (and an image with `ssh`); SSH-plan lock/credential cases and a remote allow-list; independent external-agent enrollment; amd64; UI. No R1/W05 gate changed. |
| W12 | IN PROGRESS (fakes only) | Agent runtimes and capability routing (ADR 0024) | 2026-10-27: `internal/agentrt` table, reports at enrollment and in every answer, mismatch rejection, no-fallback routing, executing runtime in journal/API/audit, restic as native-agent capability with install-plan support, host runtime badge in the UI, DE/EN guide 24 ([actual results](STATUS.md#agent-runtimes-and-capability-routing-2026-10-27-adr-0024-fakes-and-loopback-only-no-vm-used)). Open: per-runtime identity, VM lab run, per-function UI. No R1/W05 gate changed. |
| W13 | IN PROGRESS (fakes only) | X16 image update service (Watchtower-style, [ADR 0025](decisions/0025-image-update-service.md)) | 2026-10-28/29: `internal/imageupdate`, manager adapter and scheduler behind `manager.imageUpdate.enabled` (default off), REST/OpenAPI/authz, DE/EN guide 25 ([STATUS](STATUS.md)). Open: lab run against a real registry and Swarm (VM budget), automatic offer from vulnerability builds, registry webhooks, running-digest discovery, UI. No R1/W05 gate changed. |
| W14 | IN PROGRESS (fakes only) | X17 image composer ([ADR 0026](decisions/0026-image-composer.md)): component catalog, recipe validator, deterministic generator, REST API, UI page, guide 26 | 2026-10-29: `internal/imagecompose` with golden-file tests, `imagebuild.PlanRendered/StartRendered`, `/image-composer/*` routes with authz tests and OpenAPI, `/image-composer` page against the mock server, DE/EN guide 26 ([STATUS](STATUS.md)). 2026-10-29 catalog extension (catalog 2026.10.29-2: seven bases, Apache/Caddy/Node.js/Python/Java/SFTP/job runner/tools/system settings, PHP 8.1-8.4 with 39 extensions; golden files, see STATUS). Deferred: Go/distroless payload images, vsftpd/chroot SFTP, multiple FPM pools. 2026-10-30: backend wired in the managed manager behind `manager.imageComposer.enabled` (default off, needs imageBuild, [ADR 0028](decisions/0028-managed-runtime-wiring.md), fakes only). Open: catalog unverified (no package name checked), no real build, SBOM/scan not connected, no screenshots. Next: verify one build per distribution family in a lab VM. 2026-10-30: Minecraft server component (hash-pinned official download, EULA required) built and started once in a lab VM (vanilla 26.3 on Ubuntu 24.04, Paper 1.21.11 on Alpine 3.22; [STATUS](STATUS.md)); other bases/versions and Swarm not covered. 2026-10-30: Bedrock crossplay option (Geyser + Floodgate, Paper 26.3 only, catalog 2026.10.30-2) built and started once in a lab VM, UDP 19132 bound; no real Bedrock client yet ([STATUS](STATUS.md)). |

| W16 | IN PROGRESS | RC stage 3 lab (E14): backup/restore/copy/failure cases on real VMs | 2026-10-30: [e14-backup-restore.md](../tests/p0/results/e14-backup-restore.md); passed with S3/MinIO TLS and local copy target, five fixes. 2026-10-31: PITR end to end ([ADR 0031](decisions/0031-point-in-time-recovery.md), lab E15 passed with the real job helper, [results](../tests/p0/results/e15-pitr.md); manager path fakes only); abort removes job services, host by name. Next: PITR through the real manager on VMs, archive schedule, restore of a copy into a new cluster, dead-job detection in the operation, a fresh-reader INSTALL walkthrough incl. German guides |
| W15 | IN PROGRESS (simulated host only) | X18 fresh-server bootstrap ([ADR 0027](decisions/0027-fresh-server-bootstrap.md)): support matrix, preflight, bootstrap plan generator, step executor with journal and resume, host-key confirmation, separate lockdown approval, API, wizard, guide 27 | 2026-10-29: `internal/provision` bootstrap plan and executor with an in-memory simulated host (fresh Debian, fresh AlmaLinux, partial host, failure then resume, wrong fingerprint), `/bootstrap/*` routes (administrator only, 503 until configured), `/bootstrap` wizard against the mock server, DE/EN guide 27 ([STATUS](STATUS.md)). 2026-10-30: wired in the managed manager behind `manager.serverBootstrap.enabled` (default off): sealed durable drafts in the replicated metastore with credential wipe and expiry, enrollment token source, container-agent deployment through the Swarm code, leader-only runner with term fencing, interrupted runs become `unknown` after failover ([ADR 0028](decisions/0028-managed-runtime-wiring.md), fakes only). Open: nothing ran against a real host; scripts only `sh -n` checked; Docker key fingerprint unverified; the step journal is in memory; a not-yet-enrolled server gets no first enrollment token. Next: one budgeted lab VM per distribution family (Debian, AlmaLinux, Alpine) after the owner's go. |
| W14 | IN PROGRESS (fakes only) | X17 image composer ([ADR 0026](decisions/0026-image-composer.md)): component catalog, recipe validator, deterministic generator, REST API, UI page, guide 26 | 2026-10-29: `internal/imagecompose` with golden-file tests, `imagebuild.PlanRendered/StartRendered`, `/image-composer/*` routes with authz tests and OpenAPI, `/image-composer` page against the mock server, DE/EN guide 26 ([STATUS](STATUS.md)). Open: catalog unverified, no real build, backend not wired in the manager, SBOM/scan not connected, no screenshots. Next: wire the backend, verify one build per distribution family in a lab VM. |

| W12 | IN PROGRESS (fakes only) | Agent runtimes and capability routing (ADR 0024) | 2026-10-27: capability routing and execution evidence ([results](STATUS.md#agent-runtimes-and-capability-routing-2026-10-27-adr-0024-fakes-and-loopback-only-no-vm-used)). 2026-10-28: independent host/runtime certificate slots, URI and current-serial pinning, token use, renewal, revocation, deny-list delivery and legacy container routing; outage-test fixture fixed ([actual results](STATUS.md#per-runtime-agent-identities-and-outage-test-stabilization-2026-10-28-adr-0024-addendum-fakes-only-no-vm)). Open: listener-backed race gate in an environment that permits local binds, VM coexistence trial, native restic/install-flag lab evidence and per-function UI. No R1/W05 gate changed. |

W11 comes from the owner's 2026-10-23 proposal ([ADR 0015](decisions/0015-ai-assisted-vulnerability-response.md), proposed). W07–W10 come from the 2026-10-22 scope extension ([ADR 0012](decisions/0012-broadened-scope-platform.md)). They do not change the current next action; Galera/R1 (W05, W06) stays first.

W05 E9 follow-up (2026-10-26, fake tests only): zero-UUID Primary is now an unhealthy, critical incident; outage 409 responses carry stable reasons, with OpenAPI and DE/EN guidance ([actual results](STATUS.md#e9-zero-uuid-primary-incident-and-outage-conflict-reasons-2026-10-26-fake-tests-only)). Focused race tests, formatting, vet and docs checks passed. The complete race run for `internal/galera`, `internal/api` and `internal/manager` was attempted; Galera passed, but API/manager tests stopped at sandbox-denied local listeners. No repair or VM verification was done. Next: rerun the complete package race suites where listeners work, verify the incident/409 against E9 B2 in an owned lab, decide and approve a safe operator recovery, then rerun B3 on a restored cluster. W05/R1 remain open; no push.

W10 `cx-provision` commit limitation (2026-10-25): staging could not create the shared worktree index lock outside the writable sandbox. Changes are uncommitted in this worktree; no push. When metadata is writable, review and make small core, API/manager and UI/docs commits. The [actual checks and limits](STATUS.md#w10-ssh-cluster-provisioning-increment-2026-10-25-fakes-only) remain the handover source.

W03 experiments requiring VMs depend on the safe lab controls from W04. Design work can start first; do not run ad hoc experiments before ownership and cleanup are in place.

## Session handover fields

For the active task, maintain: objective, changed files/contracts, executed checks/results, unresolved issues, and next concrete action. Keep this concise and update `docs/STATUS.md`; avoid duplicating the full requirement catalog.

## Current next action

Release candidate E13 (2026-10-30): lab ran partly later that day ([e13-rc-lab.md](../tests/p0/results/e13-rc-lab.md)); remaining: stage 3 backup/restore/PITR with local and MinIO targets and the manual INSTALL.md walkthrough. Earlier: **lab run not executed** because the host was out of resources (disk 8-10 GiB free, swap about 91 % used, load 18-584; [record](../tests/p0/results/e13-not-run.md)); no VM was created. Done host-only: `scripts/rc-package.sh` (archives, SBOMs, checksums, release notes run locally), `tests/p0/rc-demo.sh` (never executed), [RELEASE-CANDIDATE.md](RELEASE-CANDIDATE.md) draft. Next action: when the host has at least 40 GiB free disk, swap headroom and load below about 30, run `tests/p0/rc-demo.sh up`, the INSTALL.md walkthrough, the Playwright end-to-end and the backup/PITR runs, then fix what they find test-first.
Optional modules (2026-10-30, ADR 0030, fakes only): registry, runtime switch API, UI page and guide 28 are done; see [STATUS](STATUS.md#optional-modules-2026-10-30-adr-0030-fakes-and-in-process-manager-only-no-vm-used). Next: check the page in a browser at 360 px and test a switched state across a leader change.

Zero-UUID recovery (2026-10-30, ADR 0029, fakes only): guided two-phase Galera recovery, mandatory snapshot, per-cluster `autoRecover` policy, REST API and guides are implemented and tested with fakes; see [STATUS](STATUS.md#guided-galera-recovery-from-the-zero-cluster-uuid-2026-10-30-adr-0029-fakes-only-no-vm-used). Next: run it on lab VMs against the E12 power-loss setup, then add the UI.

Agent runtimes (2026-10-28, W12, ADR 0024, fakes only): per-runtime certificate slots, pinning, renewal, revocation, deny-list delivery and a single-runtime revocation API are implemented with backward compatibility for legacy container records ([STATUS](STATUS.md#per-runtime-agent-identities-and-outage-test-stabilization-2026-10-28-adr-0024-addendum-fakes-only-no-vm)). Staging/commit was denied by the sandbox's shared Git index lock; no push. Next: commit in a writable checkout and rerun listener-backed race tests where local binds are permitted; then trial simultaneous native/container agents and native restic/install flags in an owned lab when the VM budget allows, and add per-function UI.

Patch management (2026-10-27, W12): `patch.Service` is wired into the managed manager runtime (off by default) and the Galera guard uses the guided host maintenance (fakes); the next increment is a lab run of this wiring and the SSH install routes; the UI is a gap ([STATUS.md](STATUS.md#os-patch-management-and-native-agent-2026-10-27-x15-adr-0023-fakes-first-then-a-two-vm-lab-run)).

Operator CLI increment (2026-10-26, `cx-cli`, fakes only): REST commands, private session storage, contexts, digest approvals, inspection-result file, output formats, completion, tests and DE/EN guide 20 are implemented ([actual results](STATUS.md#operator-cli-increment-2026-10-26-cx-cli-fakes-only), [ADR 0020](decisions/0020-operator-cli.md)). The manager lacks general operator API tokens; CLI login validates an existing session cookie. The final full Go race suite was attempted and failed on sandbox-denied TCP/Unix listeners (26 pass, 20 fail, three without tests). Next: rerun listener-backed tests where binding is allowed and test against a real manager session in an owned environment. This increment does not close any release gate.

Static accessibility/UX follow-up (2026-10-26, `cx-a11y`): `web/src` now has shared loading/empty/error states, validation announcements in the main forms, stronger light/dark contrast tokens, keyboard/focus corrections and a DE/EN Svelte-template checker. Web checks pass (22 files / 212 Vitest tests); the full Go race gate was attempted and failed on sandbox-denied TCP/Unix listeners after moving its cache to a writable directory. The 360 px review is source/CSS only. See [actual results and limits](STATUS.md#static-web-accessibility-and-ux-review-2026-10-26-cx-a11y-fakesunit-only) and [design note](DESIGN.md#static-accessibility-and-narrow-screen-review-2026-10-26). Next: synthetic-data browser and screen-reader verification at 360 px/zoom, then the full Go race gate where local listeners are allowed. This does not change W05/R1 priorities.


Release pipeline increment (2026-10-26, `cx-release`): exact stable SemVer tag guard, Linux amd64/arm64 archives, per-binary CycloneDX SBOMs, checksums, conventional-subject notes, multi-platform manager/jobs images with BuildKit SBOM/provenance and keyless digest signing are configured. [Policy](RELEASING.md), notes template, installer, bilingual guide indexes and Pages link are current. Local `actionlint`, Python release-note tests (3), deployment mutation tests (15), format, vet with writable cache, docs and deploy checks passed. The full Go race command exited 1 on sandbox-denied TCP/Unix binds; the default Go cache was denied before the cache override. No image, tool download, Actions run, release, tag or push is claimed. The commit attempt failed on the shared `.git` index lock; changes remain staged. [Actual output and limits](STATUS.md#release-pipeline-increment-2026-10-26-static-and-unit-checks-only). Next: commit in a checkout with writable Git metadata, rerun the race gate with local listeners enabled, then execute a reviewed version-tag release only when authorized and verify every published artifact and image signature. W05/R1 gates remain open.

UI coverage round 2 (2026-10-26, `w06-ui2`): the Galera operation planner, data-key rotation, metastore membership and proxy writer switch now have DE/EN UI with digest confirmation, mock data, vitest and mock screenshots; all checks passed locally (see [actual results](STATUS.md#ui-coverage-round-2-2026-10-26-w06-ui2-mock-only)). Next: verify these panels against a real manager with the opt-in backends enabled.

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

## Security hardening round 2 handover (2026-10-25, cx-security2)

The scoped changes are implemented with fakes and unit tests only. Snippet,
route and home-path input checks were tightened; a malicious-input table and
fuzz seed corpus were added. Build-context path handling, builder-error and
Docker daemon error-body secrecy have regression tests. Auth login
begin/finish now use distinct
per-peer lockout keys; OpenAPI and both security guides describe `429`;
setup-token handling and synthetic secret
non-disclosure are tested. Optional manual `govulncheck`/`gosec` CI jobs use
existing SHA-pinned actions and read-only permissions. The threat model,
catalog provenance record, EN/DE guides and testing documentation are current.

[Actual results and limitations](STATUS.md#security-hardening-round-2-2026-10-25-cx-security2-fakes-only):
format and vet passed; focused three-package race run passed; documentation
and static deployment checks passed, with 12 deployment checker tests.
The required full race run failed on sandbox-denied local listeners (25
packages passed, 19 failed, four without tests). No infrastructure was used,
no optional Actions scan ran and every catalog entry remains unverified
because no upstream documentation was available offline. No web code changed,
so no frontend commands ran. `git add` was denied at the shared worktree
`index.lock`; no commit or push was made.

Next action: commit in a writable Git-metadata session, then verify catalog
entries against release-specific upstream docs,
run the optional scans and complete race suite with local listeners permitted,
then triage findings. This does not replace W05/R1 gates or authorize a real
infrastructure trial.

## W06 backup gaps handover (2026-10-26, w06-backups)

Done with fakes: signed copies between targets with approval-gated restore proposal, client-side encrypted local/S3/SFTP targets with retention coupling, scheduled restore verification with notification, metastore backup and offline import, API/OpenAPI/authz, guide 21 DE/EN. See [actual results](STATUS.md#w06-backup-gaps-copies-targets-scheduled-verification-metastore-backup-2026-10-26-fakes-only).
Next: wire execution of a restore from an approved proposal; UI for copies/verification/metastore backups; run the metastore-import runbook and a copy plus inspection restore on lab VMs.
