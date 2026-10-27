// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

type fakeResticRunner struct {
	requests []restic.Request
}

func (f *fakeResticRunner) Run(_ context.Context, r restic.Request) (restic.Result, error) {
	f.requests = append(f.requests, r)
	return restic.Result{Stdout: []byte(`{"message_type":"summary","snapshot_id":"abc123"}` + "\n")}, nil
}

func TestResticAgentAllowlistSecretsAndRestore(t *testing.T) {
	f := newFixture(t)
	runner := &fakeResticRunner{}
	root := filepath.Join(f.dir, "source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(f.dir, "secrets")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "restic-password"), []byte("test-only-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.exec.cfg.SecretsDir = secrets
	f.exec.cfg.ResticAllowedPaths = []string{root}
	f.exec.cfg.ResticRunner = runner
	a := ResticArgs{Backend: restic.BackendLocal, Repository: filepath.Join(f.dir, "repo"), PasswordSecret: "restic-password", Paths: []string{root}, Excludes: []string{filepath.Join(root, "not-created-yet")}}
	wantStatus(t, f.mustDo(1, "op", "backup", KindResticBackup, a), StatusSucceeded, "")
	if len(runner.requests) != 1 || !strings.Contains(strings.Join(runner.requests[0].Args, " "), "backup") {
		t.Fatalf("runner calls %+v", runner.requests)
	}
	if got := runner.requests[0].Env["RESTIC_PASSWORD_FILE"]; got != filepath.Join(secrets, "restic-password") {
		t.Fatalf("password file %q", got)
	}
	for _, p := range []string{"/var/run/docker.sock", secrets, f.dir} {
		b := a
		b.Paths = []string{p}
		wantStatus(t, f.mustDo(1, "op", "reject-"+filepath.Base(p), KindResticBackup, b), StatusRefused, CodeInvalidPath)
	}
	if len(runner.requests) != 1 {
		t.Fatal("refused source reached restic")
	}
	target := filepath.Join(root, "restored")
	a.Paths, a.Snapshot, a.Target = nil, "abc123", target
	wantStatus(t, f.mustDo(1, "op", "unapproved", KindResticRestore, a), StatusRefused, CodeInvalidPath)
	a.Approved = true
	wantStatus(t, f.mustDo(1, "op", "restore", KindResticRestore, a), StatusSucceeded, "")
	wantStatus(t, f.mustDo(1, "op", "existing", KindResticRestore, a), StatusRefused, CodeInvalidPath)
}
