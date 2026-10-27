// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

func TestInstallPlanWithResticCapability(t *testing.T) {
	req := installReq(InstallReport)
	req.ResticPaths = []string{"/srv/data", "/home/app"}
	unit := UnitFile(req)
	for _, want := range []string{"-restic-allow-path /srv/data", "-restic-allow-path /home/app", "-restic-secrets-dir " + AgentSecretsDir,
		"ProtectHome=read-only", "ReadWritePaths=" + AgentStateDir + " /srv/data /home/app", "User=" + agentUser} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	script := InstallScript(req, unit)
	for _, want := range []string{"command -v restic", "install -d -m 0700 " + AgentSecretsDir, "chown " + agentUser + " " + AgentSecretsDir} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	// No restic paths: the unit and script are unchanged (patch only).
	plain := UnitFile(installReq(InstallReport))
	if strings.Contains(plain, "restic") || strings.Contains(InstallScript(installReq(InstallReport), plain), "restic") {
		t.Error("patch-only install mentions restic")
	}
	if !strings.Contains(plain, "ProtectHome=true") {
		t.Error("patch-only install must keep ProtectHome=true")
	}

	cred := provision.Credential{PrivateKey: "synthetic-key", KnownHosts: "synthetic"}
	// The restic binary is a precondition; without it no plan is built.
	chk, err := InstallPreflight(context.Background(), &fakeSSH{out: goodPre}, req.Host, cred)
	if err != nil {
		t.Fatal(err)
	}
	if chk.Restic || chk.SSHClient {
		t.Fatalf("fake preflight reported restic/ssh: %+v", chk)
	}
	if _, err := BuildInstallPlan(req, chk); !errors.Is(err, ErrInstallPreflight) {
		t.Fatalf("plan without restic binary: %v", err)
	}
	withRestic := strings.Replace(goodPre, "disk=", "restic=yes\nssh=yes\ndisk=", 1)
	chk, err = InstallPreflight(context.Background(), &fakeSSH{out: withRestic}, req.Host, cred)
	if err != nil || !chk.Restic || !chk.SSHClient {
		t.Fatalf("preflight %+v %v", chk, err)
	}
	if _, err := BuildInstallPlan(req, chk); err != nil {
		t.Fatal(err)
	}
}

func TestInstallResticPathValidation(t *testing.T) {
	for _, p := range []string{"relative", "/", "/srv/../etc", "/srv//x", "/srv/x y", "/srv/x;rm", "/srv/$(id)", "/etc/dbcm-patch", "/etc/dbcm-patch/secrets",
		"/var/lib/dbcm-patch/x", "/run/secrets", "/var/run/docker.sock", "/proc", "/srv/x/"} {
		r := installReq(InstallReport)
		r.ResticPaths = []string{p}
		if err := r.Validate(); !errors.Is(err, ErrInstallInvalid) {
			t.Errorf("path %q accepted: %v", p, err)
		}
	}
	r := installReq(InstallReport)
	r.ResticPaths = []string{"/srv/data", "/mnt/backup-src"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}
