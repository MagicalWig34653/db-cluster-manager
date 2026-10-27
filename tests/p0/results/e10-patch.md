# E10: OS patch management on real guests (X15, ADR 0023)

Run: 2026-10-27, `tests/p0/e10-patch.sh`, raw log `e10-patch.log` (sanitized). Two lab VMs created and removed through `bin/lab` (budget: 2 VMs x 2 CPUs, 3 GiB RAM, 15 GiB disk; limit 7 CPUs, 16 GiB, 100 GiB). The scoped cleanup ran, reported `unrelated resources intact (3)`, and `limactl list` afterwards shows only the unrelated `thermoctl-*` instances. No host Docker, host package manager or non-lab VM was touched.

| VM | Template | OS (real) | Package manager |
| --- | --- | --- | --- |
| dbcm-lab-m1 | debian-12 | Debian GNU/Linux 12, kernel 6.1.0-50-cloud-arm64, aarch64 | apt |
| dbcm-lab-m2 | almalinux-9 | AlmaLinux 9.8, kernel 5.14.0-687.5.3.el9_8, aarch64 | dnf |

Both VMs exited 0 with no FAILED check. The per-check list is in the log.

## Real values

- Debian: 328 installed packages, 20 updates, all 20 from `stable-security`, so classified as security (`securityKnown=true`). A plan for `bind9-host` and `bind9-libs` was simulated with `apt-get -s` (native simulation), approved, applied as root, verified; the history then shows 18 pending and the two packages cleared.
- AlmaLinux: 390 installed, 99 updates of which 84 security with advisory IDs from `dnf updateinfo` (for example ALSA-2026:58572). A plan for `NetworkManager` and `NetworkManager-libnm` was simulated, approved, applied, verified: 95 pending, 80 security.
- Both: the default policy (report-only) could not be approved; the report-mode agent (user `dbcm-patch`) refused apply and reboot; a Galera member without coordination was refused and the host was unchanged; a wrong digest and an unapproved plan were refused; the journal shows `apply` and `verify` succeeded.
- Install: the install plan was built from a real preflight (Debian: `sudo`, apt, systemd). A wrong SHA-256 left no binary on the host; the correct run installed the binary with the planned checksum, enabled the unit under `User=dbcm-patch` with `NoNewPrivileges`, `ProtectSystem=strict`, token file `0600` owned by the agent user; a second install was refused; removal left no unit, binary, directories or user. `systemd-analyze security` rates the apply-mode unit 7.7 EXPOSED (root with package installation needs wide write access).
- Reboot: on Debian a reboot plan under a policy of `with_plan` and a 24 h window triggered `systemctl reboot`; the boot ID changed within 13 s, the unit started at boot and reported reboot-required `no`. The `/var/run/reboot-required` marker was created by the test itself (this verifies detection, not that a real package postinst creates it).
- Vuln bridge: both reports converted to valid inventory entries (`Debian:12`, `AlmaLinux:9`).

## Findings from the real run

1. **Index refresh does not work in report mode.** The sandboxed user cannot lock `/var/lib/apt/lists` (read-only file system); the report says `warning: index refresh failed`. Reports are therefore only as fresh as the host's own apt/dnf timers. Apply mode (root) was not checked for refresh. Not fixed: refresh needs privileges that report mode deliberately lacks.
2. **dnf report took about 56 s** (metadata load as a non-root user). Poll timeouts and the manager's HTTP client must allow minutes.
3. **dnf simulation is derived from the update list**: `dnf upgrade --assumeno` ran successfully but its output is not parsed (`simulation source=update-list`, `From` empty). apt's simulation is native.

## Not verified

- SSH transport (`limactl shell` stood in), enrollment and mTLS of the patch agent (the unit was switched to the loopback lab transport after the install was verified; the enrollment token was written but never used).
- Reboot execution on AlmaLinux (no marker mechanism exercised; `needs-restarting` or a kernel update was not part of the run) and reboot-required after a real kernel update on either OS.
- yum, zypper, apk, pacman on real hosts; the cluster planner coordination (the Galera directory and coordinator were fakes).
- Manager failover during an apply; the apply step replay after an agent restart.
- Runs against kernel, glibc or systemd updates were deliberately excluded from the selection.
