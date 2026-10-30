# ADR 0023: OS patch management and the native patch agent

Status: **Proposal with fakes-first implementation**, 2026-10-27. Requirement
[X15](../KONZEPT.md#scope-extension); owner decision recorded in
[ENTSCHEIDUNGEN.md](../ENTSCHEIDUNGEN.md). Evidence level: unit and API tests
with fakes plus one lab run on two real guest operating systems (see
[STATUS.md](../STATUS.md) for the actual results and limits).

## Context

The owner wants general patch management for servers, modelled on PatchMon:
a lightweight agent reports installed packages, available updates, security
updates, reboot-required and OS/kernel information; the UI shows per-host and
per-package views, host groups and an update history. The owner explicitly
approves an agent installed directly in the OS (native, systemd service)
where necessary, in addition to the Docker-based agent.

Constraints that already exist: Galera quorum rules (a patched or rebooted
member must be taken out by the rolling procedure, [A08](../KONZEPT.md)),
the enrollment and mTLS transport ([ADR 0010](0010-agent-transport-and-enrollment.md)),
term-fenced journaled operations ([ADR 0003](0003-manager-metadata-ha-and-fencing.md)),
SSH-delivered, digest-approved provisioning ([ADR 0018](0018-ssh-cluster-provisioning.md))
and the vulnerability inventory ([ADR 0015](0015-ai-assisted-vulnerability-response.md)).

## Alternatives

**Reuse the existing Docker agent.** The Docker agent runs as a container with
the Docker socket. Patching the host needs the host's package manager, which a
container cannot use without privileged access to the host (`nsenter`, host
mounts), and a host reboot would kill the agent that reports the result. It
would also tie patching to Swarm nodes, while the owner wants servers in
general. Rejected as the only mechanism; the Docker agent stays unchanged.

**A separate native patch agent (chosen).** The same `agent` binary gets a
`patch` mode. It has no Docker code path, needs no socket, can run as a
dedicated unprivileged user for reporting and as root only when applying is
enabled, and survives reboots as a systemd service. It reuses enrollment,
mTLS, term fencing and the journal.

**Agentless over SSH only** (execute package manager commands from the
manager). Simple, but needs standing SSH credentials in the manager and
cannot report continuously. Kept as the installation path only (ADR 0018),
not for operation.

**Distribution tools** (unattended-upgrades, dnf-automatic). They patch
without approval or Galera awareness; the product's rule is the opposite.
They are not managed, and an administrator may still run them outside the
product at their own risk.

## Decision

1. **Pull-based reporting over mTLS.** The manager pulls reports from each
   host's agent (default every 6 hours per policy, on demand through the API).
   The agent serves `POST /patch/v1/command` on the mTLS transport with the
   enrollment flow of ADR 0010; the host ID comes from the authenticated
   identity, never from the payload. Agents never push and hold no manager
   credential beyond their short-lived certificate.
2. **Package-manager abstraction.** One `Adapter` interface (`internal/patch`)
   with apt, dnf, yum, zypper, apk and pacman. Reports use only read-only
   commands (`dpkg-query`, `apt list --upgradable`, `rpm -qa`,
   `dnf check-update`, `dnf updateinfo list --security`, `zypper list-updates`,
   `zypper patch --dry-run --category security`, `apk list`, `checkupdates`).
   Security classification comes from the manager's own metadata (Debian and
   Ubuntu security suites; RPM advisory data); apk and pacman cannot classify
   and say so (`securityKnown=false`); the CVE inventory match supplies the
   rest. An unknown classification is never reported as "no security update".
   pacman does not support partial upgrades; partial plans are refused.
3. **Execution model.** Report-only is the default and the zero value of
   every policy. Changing packages needs a **plan** (explicit package list,
   package-manager simulation, digest), **approval bound to the digest** by a
   user with update permission, and then an **apply** that checks, in order:
   policy mode, maintenance window, the Galera guard and leader term. The
   plan moves to `running` with a conditional write (two applies cannot
   start), the run is journaled with `opjournal` (apply, verify) and each
   mutating agent command carries the leader term; the agent refuses a stale
   term and replays a repeated step ID without re-running it. Policies resolve
   host override, then group, then installation default as a whole (no field
   merge). Options: security-only, maintenance windows (days, start,
   duration, IANA zone, overnight), report interval, reboot policy.
4. **Reboots.** Never unless the policy says so: `never` (default),
   `with_plan` (the approved plan contains a reboot step) or
   `auto_in_window` (policy allows a reboot after an approved apply when the
   host reports reboot-required). Both need a maintenance window, the guard
   is evaluated again immediately before the reboot, and a host reporting
   reboot-required "unknown" is never rebooted by `auto_in_window`.
5. **Galera guard.** `Guard.Check(host, action, coordination)` runs before
   apply and before reboot. A host that is a Galera member is blocked unless
   the cluster planner's rolling procedure confirms the given coordination ID
   for exactly that cluster, host and action. An unknown membership blocks
   (fail closed). The managed runtime wires `manager.PatchGaleraGuard`
   (2026-10-27, fakes): a host is a member when any cluster record has a node
   on it; the coordination ID is the ID of a guided host maintenance
   operation (A08, `galera/host-maintenance`) recorded on that host. It
   permits apply and reboot only while the host is in maintenance, every
   operation listed on the host succeeded (nodes stopped gracefully after the
   Synced check of the rest of the cluster), and every cluster with a node on
   the host is covered by one of them. Entering and leaving maintenance stay
   explicit planner actions; the patch service never starts them. A running,
   failed or unrelated operation, a cluster added after the maintenance
   began, or an ambiguous host record blocks.
6. **Native agent installation.** A reviewed `InstallPlan` (preflight, unit
   file, script, digest) delivered like ADR 0018: read-only preflight (OS,
   architecture, systemd, privilege, package manager, existing agent, disk),
   sealed SSH credential, digest-bound execution that repeats the preflight.
   The script downloads the binary over verified HTTPS, checks its SHA-256
   **on the host** before installing, creates the dedicated user, and reads
   the one-time enrollment token from stdin (never in the plan or on a
   command line). Two modes: `report` (user `dbcm-patch`, `NoNewPrivileges`,
   `ProtectSystem=strict`, empty capability set, no apply possible) and
   `apply` (root, `-allow-apply`, sandbox as tight as package installation
   allows). A removal script exists. Reinstalling over an existing agent is
   refused.
7. **Vulnerability inventory.** Each host's installed packages become an
   inventory entry (ecosystem from os-release: Debian, Ubuntu, Alpine, Rocky,
   AlmaLinux, SUSE); a distribution without a feed ecosystem is `unknown`
   coverage, not safe. The same feeds as for images then match OS packages.
8. **Authorization and audit.** New global resource `patch`: viewers read;
   operators read, poll and propose; plan approval, apply, policy, group and
   host changes and the agent installation need update/create and are held by
   the administrator preset only. Every route is audited by the existing
   authorization layer. Notifications: `patch.security_updates` (new security
   update versions only, no repeat), `patch.run_failed`, `patch.run_incomplete`,
   `patch.poll_failed` (once per transition).

## Consequences and limits

- A native agent adds a privileged component on the host. Apply mode runs as
  root; a compromise of the agent or of the manager's mTLS identity is a root
  compromise of that host. Mitigations are mTLS with identity pinning, term
  fencing, explicit package lists, digest-bound approval and the hardened
  unit; there is no mitigation for a malicious approver.
- Revocation (2026-10-27, fakes): the patch agent's mTLS listener consults a
  certificate deny list (`Revoked`). The list is stored in the agent's own
  state file and delivered by the leader as the term-fenced command
  `deny-list` (also to report-only agents; a lower revision never replaces a
  newer list). The leader sends the list to each patch host whose confirmed
  digest differs, from the patch scheduler; failures are retried. Until a
  host confirmed a list, only certificate renewal protects it.
- Replay suppression is durable (2026-10-27, fakes): the agent writes a step as
  started (fsync, atomic rename, mode 0600) before it runs it and stores the
  result afterwards; it keeps the latest 1000 steps. A replay of a finished
  step returns the stored result. A replay of a step that has no result (agent
  or host died during it) is refused with `step interrupted`: the outcome is
  unknown and the manager marks the plan failed, so an operator checks the host
  and proposes a new plan. A step that failed is forgotten and may be retried.
  If the start record cannot be written, nothing runs. A corrupt state file
  stops the agent.
- The manager's apply journal (`opjournal`) is in memory. After a manager
  restart or failover, plans left `running` are marked failed with an
  unknown outcome when the new leader starts (no command is sent).
- Version comparison for RPM-based ecosystems (AlmaLinux, Rocky, Red Hat,
  CentOS, Fedora, Oracle Linux, SUSE, openSUSE) uses `rpmvercmp` with epoch,
  version and release (`~` and `^` supported). Advisory data for these
  distributions depends on the configured feeds; OSV is mapped for AlmaLinux
  and Rocky Linux only.
- Wiring (2026-10-27, fakes): `manager.patch.enabled` (default off) builds the
  service per leadership on the replicated metastore and mTLS agent
  transport; with it off, every `/patch/*` route answers 503. The SSH install
  routes still answer 503 (sealed draft store and enrollment token source are
  not wired). There is no UI. yum, zypper, apk and pacman are covered by
  parsers and fakes only.

## Addendum 2026-10-30: container-runtime class (Docker engine)

Owner decision 2026-10-30: the Docker engine is patched here like any OS
package; the image update service ([ADR 0025](0025-image-update-service.md))
handles container images only.

- **Class.** `patch.Classify` marks docker-ce, docker-ce-cli, containerd.io,
  docker-buildx-plugin, docker-compose-plugin, docker.io and `moby-*` as
  `container-runtime`; everything else is `os`. The class is set at ingest and
  exposed as `class` on each update.
- **Policy.** `Policy.containerRuntime` (`report_only` default,
  `approval_required` opt-in) is separate from `mode`. Under report-only a
  proposal can be previewed but approval and apply fail with
  `ErrRuntimeReportOnly`. Nothing is ever applied automatically.
- **Selection.** Engine packages are never part of an "all packages" plan; they
  must be named, and a plan contains either engine or OS packages, not both.
- **Engine guard** (`patch.EvaluateEngine`, state from `EngineProbe`): `docker info` answers; Swarm node
  is ready; no running Swarm tasks other than those the approver acknowledged
  in the plan (`acknowledgeTasks`, part of the digest; an unknown task list
  refuses); on a Swarm manager the ADR 0019 quorum math must hold; live-restore
  is detected and stated. Every plan states that a daemon restart affects all
  containers. Galera members still go through the unchanged `GaleraGuard` and
  host-maintenance coordination. The guard runs at proposal (preview) and again
  at apply, and a refusal leaves the plan approved and sends nothing to the host.
- **Verification and record.** After the apply and the host report, the probe is
  polled (5 attempts, 10 s) until `docker info`, node readiness and task
  reconvergence are healthy; otherwise the plan is failed and the admins are
  notified. Engine version before and after are stored on the plan and in the
  patch history entry. The operation journal has the extra step
  `engine-verify`; its step records carry no version text, the plan and
  history do.
- **Limits.** The shipping manager has no `EngineRuntimeSource` (docker info,
  tasks, live-restore, convergence), so the guard refuses every engine plan
  until one exists; all behaviour is verified with fakes only. The apply
  journal remains in memory. Containerd and plugin packages are classified but
  not individually version-checked against the compatibility matrix.

## Evidence

Tests (fakes): `internal/patch` (parsers for all six managers, collector,
diff and history, policy and windows, guard, state machine, agent service with
fencing, install plan and refusals, vuln bridge), `internal/api/patch_test.go`
(role matrix, end to end, secrets), `cmd/agent/patch_test.go`. Lab run E10:
[tests/p0/e10-patch.sh](../../tests/p0/e10-patch.sh), results in
[tests/p0/results/e10-patch.md](../../tests/p0/results/e10-patch.md).
