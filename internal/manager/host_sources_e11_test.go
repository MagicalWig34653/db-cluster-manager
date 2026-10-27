// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

// TestE11ExternalHostSSHPlanReal is the in-VM half of lab experiment E11
// (tests/p0/e11-restic.sh). It is skipped unless E11_EXT_ADDR is set, so it
// never runs in the normal test suite. Run inside a dbcm-lab VM it drives the
// real HostSourceSSHClient (sealed source, pinned host key, real SSH session,
// real restic on the remote host) against a lab VM that belongs to no
// cluster. The repository must already be initialized by the operator.
//
// Variables: E11_EXT_ADDR, E11_EXT_USER, E11_EXT_KEY (private key file),
// E11_EXT_KNOWN_HOSTS (file), E11_EXT_REPO, E11_EXT_DATA, E11_EXT_RESTORE
// (new restore path), E11_EXT_PASSWORD.
func TestE11ExternalHostSSHPlanReal(t *testing.T) {
	addr := os.Getenv("E11_EXT_ADDR")
	if addr == "" {
		t.Skip("E11_EXT_ADDR not set (lab only)")
	}
	read := func(name string) string {
		b, err := os.ReadFile(os.Getenv(name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return string(b)
	}
	repo, data, restoreTo, pw := os.Getenv("E11_EXT_REPO"), os.Getenv("E11_EXT_DATA"), os.Getenv("E11_EXT_RESTORE"), os.Getenv("E11_EXT_PASSWORD")
	l, svcs, _ := serviceFixture(t)
	s := &HostSources{Ops: svcs.Ops, Keys: testKey}
	in := api.HostSourceInput{HostSource: api.HostSource{ID: "e11-outside", Name: "E11 outside host", Kind: "external-host", Transport: "ssh-plan",
		HostID: "outside", SSHAddress: addr, SSHUser: os.Getenv("E11_EXT_USER"), Paths: []string{data}, Excludes: []string{data + "/skip"},
		Backend: restic.BackendLocal, Repository: repo, PasswordSecret: "pw", Retention: restic.Retention{KeepLast: 1, Prune: true}, CheckSubset: "50%"},
		Password: pw, SSHPrivateKey: read("E11_EXT_KEY"), SSHKnownHosts: read("E11_EXT_KNOWN_HOSTS")}
	if _, err := s.Put(context.Background(), adminSubject, in); err != nil {
		t.Fatal(err)
	}
	client := HostSourceSSHClient{Store: l.Store(), Keys: testKey, Commands: stderrCapture{provision.SSHRunner{}}}
	base := agent.ResticArgs{Backend: in.Backend, Repository: repo, PasswordSecret: "pw"}
	send := func(step string, kind agent.Kind, a agent.ResticArgs) agent.Result {
		t.Helper()
		raw, _ := json.Marshal(a)
		res, err := client.Send(context.Background(), "restic-source/e11-outside", agent.Command{OpID: "e11", StepID: step, Kind: kind, Args: raw})
		if err != nil {
			t.Logf("E11 %s: handler error %v", step, err)
			return agent.Result{Status: agent.StatusFailed, Message: err.Error()}
		}
		msg := res.Message
		if len(res.Output) > 0 && len(msg) == 0 {
			msg = string(res.Output)
			if len(msg) > 120 {
				msg = msg[:120] + "..."
			}
		}
		t.Logf("E11 %s: status=%s code=%q %s", step, res.Status, res.Code, msg)
		if strings.Contains(res.Message, pw) || strings.Contains(string(res.Output), pw) {
			t.Fatalf("E11 %s: password in result", step)
		}
		return res
	}
	b := base
	b.Paths, b.Excludes = in.Paths, in.Excludes
	if r := send("backup1", KindResticSSHBackup, b); r.Status != agent.StatusSucceeded {
		t.Fatalf("backup1 %+v", r)
	}
	// Second snapshot after the script modified the data (the script does that
	// between the phases by way of E11_EXT_PHASE_FILE when present).
	if p := os.Getenv("E11_EXT_PHASE_HOOK"); p != "" {
		runner := provision.SSHRunner{}
		if out, err := runner.Run(context.Background(), provision.Host{Address: addr, User: in.SSHUser},
			provision.Credential{PrivateKey: in.SSHPrivateKey, KnownHosts: in.SSHKnownHosts}, p, ""); err != nil {
			t.Fatalf("phase hook: %v %s", err, out)
		}
	}
	r2 := send("backup2", KindResticSSHBackup, b)
	if r2.Status != agent.StatusSucceeded {
		t.Fatalf("backup2 %+v", r2)
	}
	id := regexp.MustCompile(`"snapshot_id":"([0-9a-f]+)"`).FindStringSubmatch(string(r2.Output))
	if id == nil {
		t.Fatalf("no snapshot id in %s", r2.Output)
	}
	t.Logf("E11 snapshot_id=%s", id[1])
	f := base
	f.Retention = in.Retention
	if r := send("forget", KindResticSSHForget, f); r.Status != agent.StatusSucceeded {
		t.Fatalf("forget %+v", r)
	}
	c := base
	c.ReadDataSubset = in.CheckSubset
	if r := send("check", KindResticSSHCheck, c); r.Status != agent.StatusSucceeded {
		t.Fatalf("check %+v", r)
	}
	// Restore: unapproved is refused by the client.
	{
		id := id[1]
		rr := base
		rr.Snapshot, rr.Target = id, restoreTo
		if _, err := client.Send(context.Background(), "restic-source/e11-outside", agent.Command{OpID: "e11", StepID: "restore-unapproved", Kind: KindResticSSHRestore, Args: mustJSON(rr)}); err == nil {
			t.Fatal("unapproved remote restore was not refused")
		}
		rr.Approved = true
		if r := send("restore", KindResticSSHRestore, rr); r.Status != agent.StatusSucceeded {
			t.Fatalf("restore %+v", r)
		}
		if r := send("restore-existing", KindResticSSHRestore, rr); r.Status == agent.StatusSucceeded {
			t.Fatalf("restore onto an existing target succeeded")
		}
		rr.Snapshot, rr.Target = id, "/etc/e11-evil"
		if r := send("restore-etc", KindResticSSHRestore, rr); r.Status == agent.StatusSucceeded {
			t.Log("NOTE: restore below /etc is only limited by the remote account's permissions (no allow-list on the SSH path)")
		}
	}
}

// Lab E11: the remote command ran restic under `env -i`, which removed HOME;
// restic then died with "neither $XDG_CACHE_HOME nor $HOME are defined" and
// every external-host operation failed.
func TestSSHResticCommandKeepsHomeForCache(t *testing.T) {
	f := &fakeHostSSH{}
	r := &sshResticRunner{host: provision.Host{Address: "h", User: "u"}, password: "pw", commands: f}
	req := restic.Request{Args: []string{"-r", "/srv/repo", "snapshots", "--json"}, Env: map[string]string{"RESTIC_PASSWORD_FILE": "/x"}}
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.command, `HOME="${HOME:-/tmp}"`) {
		t.Fatalf("restic needs HOME for its cache: %q", f.command)
	}
}

// stderrCapture is lab-only: the product command discards restic's stderr
// (it may echo secrets), which hides the cause of a failure. This wrapper
// redirects it to a file on the remote lab host so the log can show it.
type stderrCapture struct{ inner provision.SSHRunner }

func (c stderrCapture) Run(ctx context.Context, h provision.Host, cr provision.Credential, command, stdin string) (string, error) {
	out, err := c.inner.Run(ctx, h, cr, strings.ReplaceAll(command, "2>/dev/null", "2>>/tmp/e11-restic-stderr.txt"), stdin)
	shown := out
	if len(shown) > 300 {
		shown = shown[:300] + "..."
	}
	fmt.Fprintf(os.Stderr, "E11 remote output (restic JSON, no secrets): err=%v out=%q\n", err, shown)
	return out, err
}

// Lab E11: `set -e` does not stop a script at a failing command inside an
// `a && b && c` list, so the remote restore ran into an existing target. The
// guard must be an explicit if/exit that is executed by a real shell.
func TestSSHRestoreRefusesExistingTargetInRealShell(t *testing.T) {
	dir := t.TempDir()
	f := &fakeHostSSH{}
	r := &sshResticRunner{host: provision.Host{Address: "h", User: "u"}, password: "pw", commands: f, newTarget: dir}
	req := restic.Request{Args: []string{"-r", "/srv/repo", "restore", "--json", "abc123", "--target", dir, "--overwrite", "never"}, Env: map[string]string{"RESTIC_PASSWORD_FILE": "/x"}}
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// Run only the guard part of the generated script in /bin/sh.
	script := f.command[:strings.Index(f.command, "if env -i")]
	script = strings.Replace(script, "IFS= read -r DBCM_RESTIC_PW\n", "DBCM_RESTIC_PW=x\n", 1)
	cmd := exec.Command("/bin/sh", "-c", script+"echo REACHED_RESTIC\n")
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "REACHED_RESTIC") {
		t.Fatalf("an existing restore target did not stop the script: %s", out)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
