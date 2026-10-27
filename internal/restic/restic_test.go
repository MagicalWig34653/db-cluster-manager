// SPDX-License-Identifier: AGPL-3.0-only

package restic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	requests []Request
	result   Result
	err      error
}

func (f *fakeRunner) Run(_ context.Context, r Request) (Result, error) {
	f.requests = append(f.requests, r)
	return f.result, f.err
}

func testClient(t *testing.T, f *fakeRunner) *Client {
	t.Helper()
	p := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(p, []byte("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Repository: "s3:s3.example.test/bucket", Backend: BackendS3, PasswordFile: p,
		Credentials: map[string]string{"AWS_ACCESS_KEY_ID": "test-key", "AWS_SECRET_ACCESS_KEY": "test-secret"}}, f)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBackupJSONAndPrivateChildEnvironment(t *testing.T) {
	f := &fakeRunner{result: Result{Stdout: []byte(`{"message_type":"status","seconds_elapsed":2,"percent_done":0.5}` + "\n" + `{"message_type":"summary","snapshot_id":"abc123","files_new":2,"data_added":42}` + "\n")}}
	c := testClient(t, f)
	events, err := c.Backup(context.Background(), []string{"/srv/data"}, []string{"/srv/data/cache"})
	if err != nil || len(events) != 2 || events[1].SnapshotID != "abc123" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	r := f.requests[0]
	if strings.Join(r.Args, " ") != "-r s3:s3.example.test/bucket backup --json --exclude /srv/data/cache /srv/data" {
		t.Fatalf("argv=%v", r.Args)
	}
	if r.Env["RESTIC_PASSWORD_FILE"] == "" || r.Env["AWS_SECRET_ACCESS_KEY"] != "test-secret" {
		t.Fatalf("child environment incomplete: %v", r.Env)
	}
	if strings.Contains(strings.Join(r.Args, " "), "test-secret") || strings.Contains(strings.Join(r.Args, " "), "test-only") {
		t.Fatal("secret in argv")
	}
}

func TestOperationsParseJSONAndAvoidUnlock(t *testing.T) {
	f := &fakeRunner{}
	c := testClient(t, f)
	f.result.Stdout = []byte(`[{"id":"abc","time":"2026-01-01T00:00:00Z","paths":["/srv/data"]}]`)
	s, err := c.Snapshots(context.Background())
	if err != nil || len(s) != 1 || s[0].ID != "abc" {
		t.Fatalf("snapshots=%+v err=%v", s, err)
	}
	f.result.Stdout = []byte(`[{"host":"test","keep":[],"remove":[{"id":"abc123"}]}]` + "\n" + "prune completed\n")
	if _, err := c.Forget(context.Background(), Retention{KeepDaily: 7, Prune: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.requests[1].Args, " "); !strings.Contains(got, "forget --json --keep-daily 7 --prune") {
		t.Fatal(got)
	}
	f.result.Stdout = []byte(`{"message_type":"summary","num_errors":0}` + "\n")
	if _, err := c.Check(context.Background(), "5%"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.requests[2].Args, " "); !strings.Contains(got, "check --json --read-data-subset 5%") {
		t.Fatal(got)
	}
	f.result.Stdout = []byte(`{"total_size":123,"total_file_count":4}`)
	if got, err := c.Stats(context.Background()); err != nil || got.TotalSize != 123 {
		t.Fatalf("stats=%+v err=%v", got, err)
	}
	f.result, f.err = Result{Stderr: []byte("repository is already locked by PID 8; password=test-secret")}, errors.New("exit status 1")
	_, err = c.Snapshots(context.Background())
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "stale lock") || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("lock error: %v", err)
	}
	for _, r := range f.requests {
		if strings.Contains(strings.Join(r.Args, " "), "unlock") {
			t.Fatal("automatic unlock")
		}
	}
}

func TestRestoreRequiresApprovalAndFreshPath(t *testing.T) {
	f := &fakeRunner{}
	c := testClient(t, f)
	parent := t.TempDir()
	target := filepath.Join(parent, "restored")
	if err := c.Restore(context.Background(), "abc123", target, false); !errors.Is(err, ErrApprovalRequired) {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(context.Background(), "abc123", target, true); !errors.Is(err, ErrTargetExists) {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(context.Background(), "abc123", target, true); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.requests[0].Args, " "); !strings.Contains(got, "restore --json abc123 --target "+target+" --overwrite never") {
		t.Fatal(got)
	}
}

func TestRejectCredentialBearingRepositoriesAndUnsafeArgs(t *testing.T) {
	for _, repo := range []string{"rest:https://user:pass@example.test/repo", "sftp:user:pass@example.test:/repo", "s3:https://access:secret@example.test/bucket"} {
		if _, err := New(Config{Repository: repo, Backend: BackendREST, PasswordFile: "/tmp/password"}, &fakeRunner{}); err == nil {
			t.Fatalf("accepted %q", repo)
		}
	}
	for _, tc := range []struct {
		backend Backend
		repo    string
	}{
		{BackendLocal, "/var/backups/restic"},
		{BackendSFTP, "sftp:backup@example.test:/repo"},
		{BackendS3, "s3:https://s3.example.test/bucket"},
		{BackendREST, "rest:https://backup.example.test/repo"},
	} {
		if _, err := New(Config{Backend: tc.backend, Repository: tc.repo, PasswordFile: "/tmp/password"}, &fakeRunner{}); err != nil {
			t.Errorf("%s %q: %v", tc.backend, tc.repo, err)
		}
	}
	c := testClient(t, &fakeRunner{})
	if _, err := c.Backup(context.Background(), []string{"--files-from=/tmp/x"}, nil); err == nil {
		t.Fatal("option injection accepted")
	}
}
