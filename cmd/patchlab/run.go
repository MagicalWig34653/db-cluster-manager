// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

func hostOf() provision.Host {
	return provision.Host{ID: vm, Address: vm, User: "lab", Role: "worker"}
}

func credOf() provision.Credential { return provision.Credential{} }

func bootID(ctx context.Context) string {
	out, _ := sh(ctx, "cat /proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(out)
}

func run(ctx context.Context, binary string, n int) error {
	section("Lab VM facts: " + vm)
	facts, err := sh(ctx, `. /etc/os-release; echo "$PRETTY_NAME"; uname -rm; id -un; sudo -n true && echo sudo=ok; ps -p 1 -o comm=; free -m | awk 'NR==2{print "mem_mb="$2}'; df -PB1 / | awk 'NR==2{print "disk_free_bytes="$4}'`)
	if err != nil {
		return fmt.Errorf("VM not reachable: %w", err)
	}
	fmt.Println(facts)

	section("Serve the agent binary over HTTPS (lab CA) and trust the CA in the VM")
	url, sha, caPEM, stop, err := serveBinary(binary)
	if err != nil {
		return err
	}
	defer stop()
	say("binary %s sha256 %s", url, sha)
	trust := `sudo -n sh -c 'if command -v update-ca-certificates >/dev/null 2>&1; then cat > /usr/local/share/ca-certificates/dbcm-patchlab.crt && update-ca-certificates >/dev/null; else cat > /etc/pki/ca-trust/source/anchors/dbcm-patchlab.crt && update-ca-trust; fi'`
	if _, err := (limaRunner{}).Run(ctx, hostOf(), credOf(), trust, string(caPEM)); err != nil {
		return fmt.Errorf("trust CA: %w", err)
	}
	if out, err := sh(ctx, "curl -sS -o /dev/null -w '%{http_code}' "+url); err != nil || strings.TrimSpace(out) != "200" {
		return fmt.Errorf("the VM cannot fetch the binary from the host: %q %v", out, err)
	}
	ok("VM reaches host.lima.internal over verified HTTPS")

	section("Install plan: refusals")
	if _, err := install(ctx, patch.InstallReport, url, sha, true); err == nil {
		check(false, "wrong binary checksum is refused")
	} else {
		say("expected failure: %v", err)
		out, _ := sh(ctx, "test -e "+patch.AgentBinaryPath+" && echo present || echo absent")
		check(strings.TrimSpace(out) == "absent", "wrong binary checksum leaves no binary on the host", out)
	}
	chk, err := patch.InstallPreflight(ctx, limaRunner{}, hostOf(), credOf())
	if err != nil {
		return err
	}
	plan, err := patch.BuildInstallPlan(installReq(patch.InstallReport, url, sha), chk)
	if err != nil {
		return err
	}
	check(errors.Is(patch.ApplyInstall(ctx, limaRunner{}, plan, strings.Repeat("0", 64), "t", credOf()), patch.ErrInstallDigest), "wrong plan digest is refused before anything runs")

	section("Install plan: report mode (dedicated user, hardened unit)")
	if _, err := install(ctx, patch.InstallReport, url, sha, false); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	out, _ := sh(ctx, fmt.Sprintf(`echo "binary_sha=$(sha256sum %s | cut -d' ' -f1)"; echo "unit_active=$(systemctl is-active %s)"; echo "unit_enabled=$(systemctl is-enabled %s)"; id %s; stat -c 'token=%%U:%%a' %s/enroll-token; stat -c 'state=%%U:%%a' %s; systemctl show %s -p User -p NoNewPrivileges -p ProtectSystem -p MemoryDenyWriteExecute | tr '\n' ' '; echo; systemd-analyze security %s --no-pager 2>&1 | tail -1`,
		patch.AgentBinaryPath, patch.AgentUnitName, patch.AgentUnitName, "dbcm-patch", patch.AgentConfigDir, patch.AgentStateDir, patch.AgentUnitName, patch.AgentUnitName))
	fmt.Println(out)
	check(strings.Contains(out, "binary_sha="+sha), "installed binary matches the planned checksum")
	check(strings.Contains(out, "unit_enabled=enabled") && strings.Contains(out, "User=dbcm-patch") && strings.Contains(out, "NoNewPrivileges=yes") && strings.Contains(out, "ProtectSystem=strict"), "unit is enabled and runs as dbcm-patch with strict sandboxing")
	check(strings.Contains(out, "token=dbcm-patch:600"), "enrollment token file is 0600 and owned by the agent user (token never in the plan)")
	// Rerun must be refused (idempotence / no overwrite).
	_, err = install(ctx, patch.InstallReport, url, sha, false)
	check(errors.Is(err, patch.ErrInstallExists), "a second install is refused by the preflight", err)

	section("Report mode on the real OS")
	if err := labSwitch(ctx, false); err != nil {
		return err
	}
	ok("installed unit (user dbcm-patch, hardening unchanged) serves the loopback lab transport")
	ag := limaAgent{}
	svc, events := newService(ag)
	if _, err := svc.UpsertHost(patch.Host{ID: vm}); err != nil {
		return err
	}
	rep, err := svc.Poll(ctx, vm, true)
	if err != nil {
		return fmt.Errorf("poll: %w", err)
	}
	summarize(rep)
	check(rep.Manager != "" && len(rep.Installed) > 50 && rep.OS.Kernel != "" && rep.OS.ID != "", "report contains OS, kernel, package manager and installed packages")
	if rep.SecurityKnown {
		ok("package manager classifies security updates (" + string(rep.Manager) + ")")
	} else {
		nok("security classification (" + string(rep.Manager) + ")")
	}
	say("pending updates=%d security=%d; events=%d", len(rep.Updates), len(rep.SecurityUpdates()), len(*events))
	if len(rep.SecurityUpdates()) > 0 {
		check(len(*events) == 1 && (*events)[0].Kind == "patch.security_updates", "new security updates raise one notification")
	}
	// The same checks through the vuln inventory bridge.
	e := patch.ReportInventoryEntry(rep)
	say("vuln inventory entry: id=%s coverage=%s components=%d ecosystem=%s", e.ID, e.Coverage, len(e.Components), patch.OSEcosystem(rep.OS))
	check(e.Validate() == nil && e.Coverage == "sbom" && len(e.Components) == len(rep.Installed), "installed packages convert to a valid vuln inventory entry")

	section("Report-only guarantees")
	sim, err := ag.Do(ctx, vm, patch.AgentCommand{Kind: patch.CmdSimulate, Packages: eligible(rep, 1)})
	if len(eligible(rep, 1)) == 0 {
		nok("simulation on the real package manager (no eligible update pending)")
	} else {
		check(err == nil && sim.Simulation != nil, "simulation (package manager dry run) works as the unprivileged user", err)
		if sim.Simulation != nil {
			say("simulation source=%s changes=%+v", sim.Simulation.Source, sim.Simulation.Changes)
		}
	}
	_, err = ag.Do(ctx, vm, patch.AgentCommand{Kind: patch.CmdApply, Term: 1, StepID: "x", Packages: []string{"nothing"}})
	check(errors.Is(err, patch.ErrReadOnly), "report-mode agent refuses apply", err)
	_, err = ag.Do(ctx, vm, patch.AgentCommand{Kind: patch.CmdReboot, Term: 1, StepID: "y"})
	check(errors.Is(err, patch.ErrReadOnly), "report-mode agent refuses reboot", err)
	p, err := svc.Propose(ctx, "lab", vm, patch.ProposeInput{Packages: eligible(rep, 1)})
	if err == nil {
		_, aerr := svc.Approve(ctx, "lab", p.ID, p.Digest)
		check(errors.Is(aerr, patch.ErrReportOnly), "default policy (report-only) cannot be approved", aerr)
	} else {
		nok("proposal on the report-only host: " + err.Error())
	}

	section("Replace by the apply-mode agent")
	uninstall(ctx)
	if _, err := install(ctx, patch.InstallApply, url, sha, false); err != nil {
		return fmt.Errorf("install apply mode: %w", err)
	}
	out, _ = sh(ctx, fmt.Sprintf(`systemctl show %s -p User -p ProtectKernelTunables -p MemoryDenyWriteExecute | tr '\n' ' '; echo; systemd-analyze security %s --no-pager 2>&1 | tail -1`, patch.AgentUnitName, patch.AgentUnitName))
	fmt.Println(out)
	if err := labSwitch(ctx, true); err != nil {
		return err
	}
	ok("apply-mode unit (root, -allow-apply) serves the loopback lab transport")

	section("Approval, apply, verification")
	svc, events = newService(ag)
	pol := patch.Policy{Mode: patch.ModeApprovalRequired}
	svc.UpsertHost(patch.Host{ID: vm, Policy: &pol})
	rep, err = svc.Poll(ctx, vm, true)
	if err != nil {
		return err
	}
	summarize(rep)
	pkgs := eligible(rep, n)
	if len(pkgs) == 0 {
		nok("apply (no eligible update is pending in this image)")
	} else {
		before := map[string]string{}
		for _, u := range rep.Updates {
			before[u.Name] = u.Installed
		}
		plan, err := svc.Propose(ctx, "lab-operator", vm, patch.ProposeInput{Packages: pkgs})
		if err != nil {
			return fmt.Errorf("propose: %w", err)
		}
		say("plan %s packages=%v simulation=%s changes=%+v digest=%s", plan.ID, plan.Packages, plan.Simulation.Source, plan.Simulation.Changes, plan.Digest[:16])
		_, err = svc.Apply(ctx, plan.ID)
		check(errors.Is(err, patch.ErrConflict), "an unapproved plan is not applied", err)
		_, err = svc.Approve(ctx, "lab-admin", plan.ID, strings.Repeat("0", 64))
		check(errors.Is(err, patch.ErrDigest), "approval with a wrong digest is refused", err)
		// Galera guard: block first.
		real := svc.Guard
		svc.Guard = patch.GaleraGuard{Directory: galeraDir{}}
		if _, err := svc.Approve(ctx, "lab-admin", plan.ID, plan.Digest); err != nil {
			return err
		}
		_, err = svc.Apply(ctx, plan.ID)
		check(errors.Is(err, patch.ErrGaleraBlocked), "a Galera member without cluster planner coordination is not patched", err)
		after, _ := ag.Do(ctx, vm, patch.AgentCommand{Kind: patch.CmdReport})
		still := 0
		if after.Report != nil {
			for _, u := range after.Report.Updates {
				if before[u.Name] != "" || contains(pkgs, u.Name) {
					still++
				}
			}
		}
		check(after.Report != nil && still >= len(pkgs), "the blocked apply changed nothing on the host")
		svc.Guard = real
		done, err := svc.Apply(ctx, plan.ID)
		if err != nil {
			say("apply error: %v (plan state %s: %s)", err, done.State, done.Error)
		}
		check(err == nil && done.State == patch.PlanSucceeded, "approved plan applies on the real package manager", done.State, done.Error)
		say("plan result: state=%s remaining=%v op=%s", done.State, done.Remaining, done.OpID)
		op, _ := svc.Journal.Get(done.OpID)
		say("journal steps: %+v", op.Steps)
		check(len(op.Steps) == 2 && op.Steps[0].State == "succeeded" && op.Steps[1].State == "succeeded", "apply and verify steps are journaled as succeeded")
		check(len(done.Remaining) == 0, "no planned package still has an update afterwards", done.Remaining)
		hist, _ := svc.History(vm)
		for _, h := range hist {
			say("history %s kind=%s result=%s pending=%d security=%d newUpdates=%d clearedUpdates=%d changed=%d", h.At.Format("15:04:05"), h.Kind, h.Result, h.UpdatesPending, h.SecurityCount, len(h.Diff.NewUpdates), len(h.Diff.ClearedUpdates), len(h.Diff.InstalledChanged))
		}
		rep, _ = svc.Report(vm)
		summarize(rep)
	}

	section("Reboot-required detection and reboot policy")
	hasDeb := strings.Contains(string(rep.Manager), "apt")
	if hasDeb {
		// This reproduces what update-notifier/needrestart hooks write; it tests
		// the detection, not the producer.
		sh(ctx, `sudo -n sh -c 'touch /var/run/reboot-required; echo lab-package > /var/run/reboot-required.pkgs'`)
	}
	rep, err = svc.Poll(ctx, vm, false)
	if err != nil {
		return err
	}
	say("reboot required=%s reasons=%v", rep.RebootRequired, rep.RebootReasons)
	if hasDeb {
		check(rep.RebootRequired == patch.Yes, "reboot-required marker is detected")
	} else if rep.RebootRequired == patch.Unknown {
		nok("reboot-required detection on " + string(rep.Manager) + " (no indicator available: reported as unknown, not as no)")
	} else {
		ok("reboot-required state reported as " + string(rep.RebootRequired) + " on " + string(rep.Manager))
	}
	_, err = svc.Propose(ctx, "lab", vm, patch.ProposeInput{Packages: eligible(rep, 1), Reboot: true})
	check(errors.Is(err, patch.ErrRebootDenied) || len(eligible(rep, 1)) == 0, "a reboot plan is refused while the policy says never", err)
	boot := bootID(ctx)

	if hasDeb && len(eligible(rep, 1)) > 0 {
		pol := patch.Policy{Mode: patch.ModeApprovalRequired, Reboot: patch.RebootWithPlan, Windows: []patch.Window{{Start: "00:00", DurationMinutes: 1440}}}
		svc.UpsertHost(patch.Host{ID: vm, Policy: &pol})
		plan, err := svc.Propose(ctx, "lab", vm, patch.ProposeInput{Packages: eligible(rep, 1), Reboot: true})
		if err != nil {
			return err
		}
		svc.Approve(ctx, "lab-admin", plan.ID, plan.Digest)
		done, err := svc.Apply(ctx, plan.ID)
		say("reboot plan: state=%s reboot=%q err=%v", done.State, done.RebootStatus, err)
		check(err == nil && done.RebootStatus == "requested", "an approved reboot plan inside its window reboots the host")
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)
			if b := bootID(ctx); b != "" && b != boot {
				ok("host came back with a new boot ID after the reboot")
				break
			}
		}
		if bootID(ctx) == boot {
			nok("host did not come back with a new boot ID within 4 minutes")
		}
		sh(ctx, "sudo -n systemctl is-active "+patch.AgentUnitName)
		if err := waitAgent(ctx); err != nil {
			nok("agent unit did not come back after the reboot: " + err.Error())
		} else {
			rep, err := svc.Poll(ctx, vm, false)
			check(err == nil && rep.RebootRequired == patch.No, "the agent restarted at boot and reports reboot-required=no again", rep.RebootRequired, err)
		}
	} else {
		nok("reboot execution (no apt host or no eligible update)")
	}

	section("Removal")
	uninstall(ctx)
	return nil
}

func waitAgent(ctx context.Context) error {
	for i := 0; i < 60; i++ {
		if out, _ := sh(ctx, `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7445`+patch.CommandPath); strings.TrimSpace(out) == "405" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return errors.New("timeout")
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
