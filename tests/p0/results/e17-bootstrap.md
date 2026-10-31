# E17 part a: fresh-server bootstrap on real Debian 12 and AlmaLinux 9 (ADR 0027, X18)

Run: 2026-10-31. Two fresh Lima VMs created with `bin/lab` (`-templates debian-12,almalinux-9`, 2 vCPU / 4 GiB / 20 GiB each, arm64 `vz`), removed with `bin/lab cleanup --apply` at the end of E17. Host before the run: load 8-10, 63 GiB free. Lab-own credentials only: a throw-away ed25519 key authorized for `root` on Debian, a throw-away root password plus `PermitRootLogin yes` / `PasswordAuthentication yes` drop-in on AlmaLinux (to exercise the password path); nothing of it is tracked. The harness is `cmd/bootlab` (host side): the real `provision.BootstrapService` with the real `SSHRunner` and `SSHHostKeyProbe` over the VM's forwarded SSH port on `127.0.0.1`; only the manager-side actions (`DeployContainerAgent`, sealed key store) are stubs, and the profile selected no native agent (E17 part c covers the agent). Raw logs: `e17/bootstrap-*.log`.

## What ran

| Host | Profile | Result |
| --- | --- | --- |
| Debian 12 (key login as root) | firewall (ufw), Docker, Swarm init | **completed** after two product fixes (below). Docker 29.9.2 from `download.docker.com`, `swarm=active control=true`, ufw active with 22, 2377, 7946/tcp+udp, 4789/udp, user `dbcm` (uid 1000) with a parsed sudoers drop-in, all five `*.done` markers. |
| AlmaLinux 9.8 (password login as root) | firewall (firewalld), unattended upgrades (dnf-automatic), Docker, Swarm init, dedicated key and root lockdown | **completed** after one more product fix. Docker 29.9.2, Swarm active, `22/tcp 2377/tcp 7946/tcp 4789/udp 7946/udp` open, `dnf-automatic.timer`, `docker`, `firewalld` active. Main phase and the separately approved lockdown phase ran; afterwards root (key **and** password) is refused over SSH, the generated dedicated key logs in as `dbcm`, `sudo -n` works, `sshd -T` shows `permitrootlogin no`. |
| Debian again (second bootstrap of the same host) | same | refused: `host already belongs to a cluster` (existing Swarm), no plan at all. |

Checked in the plan before approval: fingerprint confirmation (the probe reads the host key without authenticating; the typed fingerprint pins it), read-only preflight, plan digest, per-step status polling. The preflight of the Debian image reported `docker=none`, `privilege=root`, `os=debian 12`, `arch=arm64`.

## Bugs found by the real hosts (all fixed with tests)

1. **Preflight read 4 GiB as 0 bytes.** `awk '{print $2*1024}'` prints `4.12039e+09` under mawk (Debian's default awk); the parser read 0 and the plan was blocked as `memory-low`. The command now uses `printf "%.0f"` (`TestPreflightMemoryIsAnInteger`). The simulated host of the earlier tests could not show this because it does not run shell.
2. **Base-packages step skipped on a host without gpg.** Its "already done" check looked for curl, sudo and visudo; a Debian cloud image has all three but no `gpg`, so `gnupg` was never installed and the Docker step died at the key verification. The check now includes `gpg` (`TestBasePackagesStepChecksForGpg`).
3. **Wrong Docker key fingerprint for dnf.** The rpm repository (`download.docker.com/linux/centos/gpg`) is signed with a different key from the apt one: `060A61C51B558A7F742B77AAC52FEB6B621E9F35` against the apt `9DC858229FC7DD38854AE2D88D81803C0EBFCD88`. The single pinned constant made every AlmaLinux and Rocky bootstrap fail with "key fingerprint mismatch". There are now two constants (`DockerKeyFingerprint` for apt, `DockerRPMKeyFingerprint` for dnf), each checked against a live download (the apt one on Debian 12, the rpm one on AlmaLinux 9.8); the existing dnf test asserts the rpm one.

## Findings that are not fixed

- A failing mutating step is reported as `remote outcome unknown: step <id>` with no stderr, by design (the command output could carry secrets). Finding the cause needs the saved plan script run by hand; both causes above were found that way. A short, filtered failure reason (exit status and the last stderr line of non-credential steps) would help; it is a design decision, not changed here.
- Ubuntu, Rocky, Debian 13 and Alpine were not run (matrix entries unverified on real hosts); the native-agent step, container-agent step, `join-manager`/`join-worker` and `Restic` capability of the bootstrap profile were not run through the bootstrap (E17 part c installs the agent through the patch installer instead).
- The bootstrap was driven by the harness, not through the HTTP API and the UI wizard; the manager runtime wiring (`manager.serverBootstrap`, sealed drafts, enrollment tokens) stays verified with fakes only. The module `bootstrap` also stays blocked in a real manager without `manager.serverBootstrap.agentRelease` (E17 part e).
- ufw/firewalld were enabled with only the profile's ports; Docker's own iptables rules still publish container ports regardless (the plan warns about this, not re-measured).
- The VMs were reachable from the host; no cloud image with a pre-set root password and an expired password was tried.
