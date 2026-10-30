# OS patch management (experimental)

**[Fakes + lab]** Patch management ([X15](../../KONZEPT.md#scope-extension), [ADR 0023](../../decisions/0023-os-patch-management.md)) is implemented and tested with fakes. The native agent and the package-manager handling also ran once on two real guest operating systems in the local lab (Debian 12 with apt, AlmaLinux 9 with dnf); see [E10 results](../../../tests/p0/results/e10-patch.md) for what was and was not verified. The managed manager runtime can run it behind an explicit switch (`manager.patch.enabled`, default off; see "Enabling it"), verified with fakes and a real mTLS listener on loopback only, not on lab hosts. With the switch off the endpoints answer 503. There is no UI for it. Do not use it on production hosts.

## Enabling it

Add the section to the manager configuration (the shipped `deploy/manager.example.json` carries it with `enabled: false`):

```json
"patch": { "enabled": true, "agentPort": 7445, "pollIntervalSeconds": 300 }
```

It needs `auth.enabled` and `manager.agentTransport` (the patch agents use the same CA and the manager's client certificate as the main agents); the manager refuses to start otherwise. `agentPort` is the patch agents' listen port (default 7445; the shipped metastore default also uses 7445 on loopback, so on a manager host start that host's patch agent on another port and set `agentPort` accordingly). `pollIntervalSeconds` (default 300, minimum 60) is how often the leader looks for hosts whose report interval elapsed and for a changed certificate deny list. Only the leader runs it; records live in the replicated metastore. The patch host ID is the Swarm node ID of the enrolled host, and the patch agent must be started with that `-host-id`: the manager pins the agent identity to it. When a new leader starts, plans that were `running` are marked failed with an unknown outcome (the apply journal is in memory), and nothing is sent to the host.

Certificate revocation: the leader sends the deny list to every patch host that has not confirmed the current one (command `deny-list`, also to report-only agents). The patch agent stores it in `patch-state.json` in its state directory and rejects a presented certificate with a listed serial, even before it expires. A host that has not yet confirmed a list is only protected by certificate renewal.

## What it does

A small agent on each server reports the OS and kernel, installed packages, available updates, security updates and whether a reboot is required. The manager keeps the latest report and a bounded history per host (200 entries), groups hosts, and shows per-host and per-package views through the API. New security updates raise a notification once per new update version. Installed packages also feed the vulnerability inventory so the CVE feeds ([guide 15](15-vulnerability-response.md)) can match operating system packages. A distribution without a known feed ecosystem is shown as "unknown", never as safe.

## Report-only by default

Nothing is changed unless all of these hold: the host's effective policy is `approval_required`; a plan with an explicit package list and a package-manager simulation exists; a user with update permission approved exactly that plan (digest); the current time is inside the policy's maintenance window (if windows are set); the host is not an uncoordinated Galera member; the agent was installed in apply mode. The effective policy is the host's own policy, else its group's, else the installation default (no field-by-field merge).

Policy fields: `mode` (`report_only` | `approval_required`), `securityOnly`, `windows` (days, `start` HH:MM, `durationMinutes`, IANA `zone`, overnight windows work), `reboot` (`never` | `with_plan` | `auto_in_window`; anything but `never` needs a window), `reportEveryMinutes` (default 360).

## Roles

Viewers read. Operators read, pull a fresh report and propose plans. Approving, applying, changing policies, groups and hosts and installing agents needs the administrator preset (or a custom role with `patch` update/create).

## Typical flow (API)

1. `PUT /api/v1/patch/groups/web` and `PUT /api/v1/patch/hosts/h1` (name, group, optional policy).
2. `POST /api/v1/patch/hosts/h1/poll`, then `GET /api/v1/patch/hosts/h1` and `/api/v1/patch/packages`.
3. `POST /api/v1/patch/hosts/h1/plans` with `{"securityOnly": true}`. The response lists the packages, the simulation and a digest.
4. `POST /api/v1/patch/plans/{id}/approve` with the digest, then `POST /api/v1/patch/plans/{id}/apply`.
5. Check `GET /api/v1/patch/hosts/h1/history`. A run that left updates pending says so in `remaining`.

## Reboots

A reboot never happens unless the policy allows it. With `with_plan` the plan must contain the reboot step (`"reboot": true`) and its approval is the approval of the reboot; with `auto_in_window` an approved apply is followed by a reboot if the host reports reboot-required. The reboot also needs the window and a second guard check right before it. If the host cannot tell whether a reboot is needed (Alpine, Arch) it reports `unknown`, and `auto_in_window` does not reboot on `unknown`.

## Galera members

Applying packages to, or rebooting, a host that runs a Galera member is refused unless the cluster planner's rolling procedure coordinates exactly that action (`coordination` in the plan). If the membership cannot be determined, the action is refused. With `manager.patch.enabled` the guard is connected to the planner's guided host maintenance (section "Host maintenance" of the [operator runbook](11-operator-runbook.md)): put the host into maintenance (`POST /api/v1/hosts/{id}/maintenance`, which gracefully stops its Galera nodes one at a time after checking that the rest of the cluster is Synced), wait until the host is ready, then propose the plan with `"coordination": "<maintenance operation ID>"` from the host's `maintenance.operations`. Apply and reboot are permitted only while the host is in maintenance, every operation listed on the host succeeded and every cluster with a node on the host is covered; otherwise they are refused. After the patch (and the reboot) leave maintenance yourself (`DELETE /api/v1/hosts/{id}/maintenance`) so the nodes start again. A host without cluster nodes is not treated as a member. The guard is verified with fake agents only, not against a real cluster.

## Installing the native agent

The administrator posts an install request (`POST /api/v1/patch/install/plans`): host, mode, HTTPS URL and SHA-256 of the agent binary, the manager URL and CA fingerprint. The manager runs a read-only SSH preflight (Linux, supported architecture, systemd, root or sudo, package manager, no existing agent) and returns the exact script, the systemd unit and a digest. The SSH key is write-only and sealed. Executing the plan with the digest repeats the preflight, downloads the binary, **verifies the SHA-256 on the host before installing**, creates the user, and reads the one-time enrollment token from stdin. Two modes: `report` (user `dbcm-patch`, sandboxed unit, cannot apply) and `apply` (root, `-allow-apply`). An install over an existing agent is refused. Removal: stop and disable `dbcm-patch-agent.service`, delete the unit, `/usr/local/bin/dbcm-agent`, `/etc/dbcm-patch`, `/var/lib/dbcm-patch` and the user (the manager ships the same steps as `patch.UninstallScript`).

Run the agent by hand for a read-only look: `dbcm-agent patch -once` prints one report as JSON and changes nothing.

## Limits

Not verified on real hosts: SSH transport, enrollment and mTLS of the patch agent against a real manager, yum, zypper, apk and pacman (parsers and fakes only), the planner integration against a real cluster, behaviour across a real manager failover. The manager-to-agent mTLS path, the deny list, durable replay suppression and the maintenance-based guard were tested with fakes and loopback listeners.

Replays of a mutating step are suppressed durably: the agent records a step before running it. If the agent or host died while a step ran, a replay is refused with "step interrupted", the plan is marked failed, and you check the host's report before proposing a new plan. The agent keeps the latest 1000 steps. The apply journal on the manager is in memory only (see "Enabling it"). CVE matching for RPM distributions (AlmaLinux, Rocky, Red Hat, CentOS, Fedora, Oracle Linux, SUSE) uses `rpmvercmp` ordering with epoch and release; advisories exist only if a configured feed has them (OSV is mapped for AlmaLinux and Rocky Linux). The SSH install routes work in the managed runtime when `manager.patch` is enabled with the agent transport: install plans and the SSH credential are sealed with the data keyring in the replicated metastore (they survive a restart or failover), expire after one hour and are deleted after any run that reached the host; the enrollment token is issued per execution for the host whose enrolled host record has the same node ID (a server that is not yet an enrolled host gets none and the install is refused before it touches the host; the draft stays until it expires). Verified with fakes only; you can still install the agent by hand as described below. No UI exists; the API and this guide are the interface.

## Runtime and restic (ADR 0024)

The patch agent is the **native** runtime: it reports `runtime=native` and the capability `patch` (plus `restic` when installed with `resticPaths`). Patch commands are routed only to a native agent; a host that reports only a container agent is refused with a reason. Plans record `executedBy`, patch hosts `agentRuntime`. See [guide 24](24-agent-runtimes.md).
