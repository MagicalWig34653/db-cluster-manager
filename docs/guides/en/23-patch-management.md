# OS patch management (experimental)

**[Fakes + lab]** Patch management ([X15](../../KONZEPT.md#scope-extension), [ADR 0023](../../decisions/0023-os-patch-management.md)) is implemented and tested with fakes. The native agent and the package-manager handling also ran once on two real guest operating systems in the local lab (Debian 12 with apt, AlmaLinux 9 with dnf); see [E10 results](../../../tests/p0/results/e10-patch.md) for what was and was not verified. It is not wired into the managed manager runtime yet, so the endpoints answer 503 in a normal installation, and there is no UI for it. Do not use it on production hosts.

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

Applying packages to, or rebooting, a host that runs a Galera member is refused unless the cluster planner's rolling procedure coordinates exactly that action (`coordination` in the plan). If the membership cannot be determined, the action is refused. In this increment the guard works against an interface; the real planner is not connected yet. Patch Galera hosts only through a rolling procedure.

## Installing the native agent

The administrator posts an install request (`POST /api/v1/patch/install/plans`): host, mode, HTTPS URL and SHA-256 of the agent binary, the manager URL and CA fingerprint. The manager runs a read-only SSH preflight (Linux, supported architecture, systemd, root or sudo, package manager, no existing agent) and returns the exact script, the systemd unit and a digest. The SSH key is write-only and sealed. Executing the plan with the digest repeats the preflight, downloads the binary, **verifies the SHA-256 on the host before installing**, creates the user, and reads the one-time enrollment token from stdin. Two modes: `report` (user `dbcm-patch`, sandboxed unit, cannot apply) and `apply` (root, `-allow-apply`). An install over an existing agent is refused. Removal: stop and disable `dbcm-patch-agent.service`, delete the unit, `/usr/local/bin/dbcm-agent`, `/etc/dbcm-patch`, `/var/lib/dbcm-patch` and the user (the manager ships the same steps as `patch.UninstallScript`).

Run the agent by hand for a read-only look: `dbcm-agent patch -once` prints one report as JSON and changes nothing.

## Limits

Not verified on real hosts: SSH transport, enrollment and mTLS of the patch agent, yum, zypper, apk and pacman (parsers and fakes only), the real cluster planner integration, behaviour across a manager failover. Replays of a mutating step are suppressed in agent memory only. RPM version ordering in the CVE match is approximate. No UI exists; the API and this guide are the interface.
