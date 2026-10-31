package api

import (
	"context"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

type outSSH struct {
	out string
	err error
}

func (o outSSH) Run(context.Context, provision.Host, provision.Credential, string, string) (string, error) {
	return o.out, o.err
}

// Install preflight refusals are all 409 conflict, but carry distinct,
// redacted reasons (and never the host output).
func TestPatchInstallPreflightReasons(t *testing.T) {
	good := "os=debian\narch=aarch64\nsystemd=yes\npriv=root\nmanager=apt-get\nagent=absent\ndisk=5000000000\nrestic=no\nssh=no\n"
	cases := []struct{ name, out, reason string }{
		{"no systemd", strings.Replace(good, "systemd=yes", "systemd=no", 1), patch.ReasonNoSystemd},
		{"no privilege", strings.Replace(good, "priv=root", "priv=none", 1), patch.ReasonNoPrivilege},
		{"low disk", strings.Replace(good, "disk=5000000000", "disk=1000", 1), patch.ReasonLowDisk},
		{"arch", strings.Replace(good, "arch=aarch64", "arch=riscv64", 1), patch.ReasonUnsupportedArch},
		{"agent present", strings.Replace(good, "agent=absent", "agent=present", 1), patch.ReasonAgentPresent},
		{"garbage", "SECRET-HOST-OUTPUT", patch.ReasonOutputInvalid},
	}
	f, _, _ := newPatchFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	req := map[string]any{
		"id": "inst1", "host": map[string]any{"id": "h1", "address": "h1.example.test", "user": "admin", "role": "worker"}, "mode": "report",
		"binaryUrl": "https://releases.example.test/agent", "binarySha256": strings.Repeat("ab", 32), "version": "0.9.0",
		"managerUrl": "https://manager.example.test:7444", "caFingerprint": strings.Repeat("AB", 32), "installation": "inst-1"}
	cred := map[string]any{"privateKey": "synthetic-private-key", "knownHosts": "h1.example.test ssh-ed25519 synthetic"}
	seen := map[string]bool{}
	for _, c := range cases {
		inst := &patch.Installer{Runner: outSSH{out: c.out}, Store: &fakeSealer{}}
		f.srv.cfg.Patch = &PatchBackend{Svc: f.srv.cfg.Patch.(*PatchBackend).Svc, Installer: inst}
		r := admin.do("POST", "/api/v1/patch/install/plans", map[string]any{"request": req, "credential": cred})
		wantStatus(t, r, 409)
		var body ErrorBody
		r.decode(t, &body)
		if body.Error != errConflict || body.Reason != c.reason {
			t.Fatalf("%s: %+v, want reason %s", c.name, body, c.reason)
		}
		if strings.Contains(r.Body.String(), "SECRET") {
			t.Fatalf("%s: host output leaked: %s", c.name, r.Body)
		}
		seen[body.Reason] = true
	}
	if len(seen) != len(cases) {
		t.Fatalf("reasons not distinct: %v", seen)
	}
}
