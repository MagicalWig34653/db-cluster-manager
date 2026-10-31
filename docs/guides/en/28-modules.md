---
title: Optional modules
---

# Optional modules [In-process tests and one lab run on a real manager]

Optional features are modules ([ADR 0030](../../decisions/0030-optional-modules.md)): OS patch management, host backups (restic), backup copies, image build, image composer, image updates, vulnerabilities and AI response, server bootstrap, SSH provisioning and Kubernetes. An administrator switches each one on or off while the manager runs; no restart is needed.

**Status.** Besides the in-process tests, lab [E17](../../../tests/p0/results/e17-modules.md) ran the switch against a real manager with real agents behind patch management and host backups: a disabled module answers `404 module_disabled`, enabling with a missing prerequisite answers `409 module_blocked`, the records of a switched-off module are listed again after switching it back on, and the settings page toggles work. A `PUT` without `enabled` is a `400` (it used to switch the module off). Not run: several replicas, other roles, a module whose prerequisite chain is satisfied (image build needs a build configuration).

## Switch a module

Open **Settings > Optional modules** (administrator to change, any signed-in user to read).

- **Enable:** tick the switch. If it cannot be enabled the page says why: a configuration key is missing (for example `manager.agentTransport` for patch management) or a module it needs is off (the composer needs the image build).
- **Disable:** untick it. This is refused while another enabled module needs it; disable that one first.
- Each card shows who switched the module and when. Every change is audited.

A disabled module stops new work, its navigation entries disappear and its API routes answer `404 module_disabled`. **Nothing is deleted:** hosts, plans, history, sealed credentials and settings stay and are back when you enable the module again. An operation that is already running finishes; it is not cancelled.

## Configuration file

The sections `manager.patch`, `imageUpdate`, `imageComposer`, `serverBootstrap`, `imageBuild`, `vuln`, `experimentalProvisioning` and `experimentalKubernetesProvisioning` keep working. They now supply the initial state of a module that was never switched; once you switch it on the page, the stored state wins. Host backups and backup copies start enabled when `manager.backup` is configured. Settings that a module needs (agent transport, `imageBuild`, the reviewed agent release, intervals) stay in the file.

## API

`GET /api/v1/modules` lists the modules with `enabled`, `missingConfig`, `missingModules`, `dependents`, `by` and `at`. `PUT /api/v1/modules/{id}` with `{"enabled": true}` switches one; `409 module_blocked` means the reasons from the list apply.
