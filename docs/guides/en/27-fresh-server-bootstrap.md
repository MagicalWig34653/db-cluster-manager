---
title: Fresh-server bootstrap
---

# Fresh-server bootstrap [Lab-run on Debian 12 and AlmaLinux 9 with a harness; API and wizard not run on VMs]

The bootstrap wizard ([ADR 0027](../../decisions/0027-fresh-server-bootstrap.md), requirement X18) turns a freshly installed Linux server that you administer into a managed host. You enter root (or sudo) SSH access once; the manager checks the server, shows you a plan made of reviewed scripts, and changes nothing until you confirm the SSH host key and approve the plan. It builds on the SSH plan flow of [guide 19](19-provisioning.md) and the agents of [guide 24](24-agent-runtimes.md).

**Status.** The plan generator, executor, API and wizard are tested against an in-memory simulated host. In lab [E17](../../../tests/p0/results/e17-bootstrap.md) the plan and executor ran with the real SSH runner against a fresh Debian 12 VM (key login) and a fresh AlmaLinux 9 VM (password login, including the separately approved lockdown): firewall, Docker from the vendor repository, Swarm init and the lockdown worked after three fixes (memory parsing, gpg, a separate Docker rpm key fingerprint). Ubuntu, Rocky, Alpine, the native-agent step, the cluster-join steps and the HTTP API/wizard path were not run on a real host. The managed manager wires the backend behind `manager.serverBootstrap.enabled` (default off, see "Enabling it"); with the switch off the routes answer 503. The wiring is verified with fakes only. Do not point it at a server you cannot rebuild.

## Supported systems

Debian 12/13, Ubuntu 22.04/24.04, AlmaLinux 9/10, Rocky Linux 9/10 and Alpine 3.19-3.22 on amd64 or arm64. Anything else is refused. On Alpine the native agent, the firewall option and unattended updates are not available (they need systemd, ufw/firewalld and apt/dnf).

## Use the wizard

Open **Fresh server** in the navigation (administrator only).

1. **Host and login.** Name the bootstrap, enter address, login user (usually `root`) and either a password or a private key. Both are write only: they are used for this run, never stored, logged or shown again, and wiped after the run, on cancel, after a failed fingerprint check and after one hour without activity.
2. **What the server becomes.** Choose the service user to create (default `dbcm`), the container runtime (Docker from the vendor repository, or none), optional firewall and unattended security updates, the native and/or container agent, the patch and restic capabilities (they need the native agent; restic needs allowed paths) and the cluster (none, Swarm init/join; k3s is a blocked preview). Contradictions are listed before anything is contacted.
3. **Confirm the SSH host key.** The manager reads the host key of that one server without logging in and shows its type and fingerprint. Compare it with your provider console or `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the server and type it. A wrong fingerprint cancels the draft and wipes the credentials; your login is not used before this step succeeds.
4. **Review the plan.** The preflight (read-only) runs and the plan appears: findings (blocking, warning, note), every step with its script and digest, its read-only check and its cleanup plan. Steps the server already satisfies are marked. A blocked plan cannot be approved. Approve with the plan digest.
5. **Run and progress.** *Run* executes the approved steps in order. Each step is journaled; a satisfied step is skipped; after a failure the run pauses and *Resume* continues after the last finished step (a step with an unknown outcome is re-checked first). The progress list updates while the run is active.
6. **Lockdown (separate approval).** If you chose the dedicated key and/or disabling root login, these steps wait after the main phase. They install a dedicated key for the service user, verify a login with it over a new connection, seal the private key in the manager, then write an sshd drop-in that disables root login with an automatic revert after three minutes, and cancel the revert only after a new dedicated-key connection works. Review them and approve the lockdown digest separately.

## What a step looks like

Every step is a shell script with a marker line, a SHA-256 digest, an idempotency guard (a marker file in `/var/lib/dbcm-bootstrap` plus the real condition), a read-only check and a cleanup script. The cleanup plan only reverts what the step added (repository files, sudoers drop-in, firewall rules it opened, the sshd drop-in) and never prunes, force-removes or deletes data; it states what is intentionally left (the Docker engine, users, packages). The product never runs the cleanup; you do, after review.

Secrets: the enrollment token and the Swarm join token travel on standard input, never in a command line. The plan and every API response are free of credentials.

## Enabling it

Add the section (the shipped `deploy/manager.example.json` carries it with `enabled: false`):

```json
"serverBootstrap": { "enabled": true, "sweepSeconds": 60,
  "agentRelease": { "version": "0.1.0", "binaryUrl": "https://releases.example.org/dbcm-agent-linux-amd64", "sha256": "<64 hex>" } }
```

The key is `serverBootstrap` because `manager.bootstrap` already starts the metastore cluster. It needs `auth.enabled`, `manager.agentTransport` (the installed native agent enrolls there) and the reviewed native agent release; the manager refuses to start otherwise. Drafts and credentials are sealed with the data keyring in the replicated metastore: the draft document holds the plan and the pinned host key but no credential, the credentials are a second document that is deleted after the run, on cancel, on a fingerprint mismatch and when the one-hour lifetime ends (the leader sweeps every `sweepSeconds`; a failed deletion is retried). Only the leader runs a bootstrap, and a run that finishes after the leadership changed does not write its result. Administrator only.

## Limits

- Simulation only; scripts are syntax-checked with `sh -n` but never executed on a real system. Package names, repository URLs, service names, firewall commands and the pinned Docker key fingerprint are unverified assumptions.
- Drafts are durable (ADR 0028) but the step journal is in memory. After a restart or failover the new leader marks a run whose end was not recorded as `unknown`; running it again resumes it, and every step is checked on the host before it is repeated (the steps recorded as succeeded are restored, the step that was in flight is checked again). The wiring has only run against fakes, never across two real manager processes.
- The enrollment token is issued only for a host with an enrolled host record whose node ID equals the host ID of the bootstrap. A server that is not yet a Swarm node has none, so the native agent step fails with "enrollment token unavailable" until the node is known (join the Swarm first, sync, enroll it, then run again). This is a known gap, not yet verified on a lab host.
- The container agent step enrolls the node and runs the existing agent deployment for its Swarm (labels, agent service); a revoked host is never re-enrolled.
- One host per bootstrap; k3s is blocked; an existing Docker engine is kept as it is; a firewall does not filter ports published by Docker.
- A reboot of the server within the three minutes after disabling root login drops the automatic revert; the dedicated key must work then.

**Module switch.** Since [ADR 0030](../../decisions/0030-optional-modules.md) this feature is a module that an administrator switches at runtime ([guide 28](28-modules.md)). The configuration section above only sets its initial state, and the routes of a disabled feature answer `404 module_disabled` (not 503).
