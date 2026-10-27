// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

func TestHostSourcesSealedModelAndPermissions(t *testing.T) {
	l, svcs, _ := serviceFixture(t)
	setReports(t, l, "sw1", "h1", agentrt.ContainerInfo(true))
	s := &HostSources{Ops: svcs.Ops, Keys: testKey}
	ctx := context.Background()
	in := api.HostSourceInput{HostSource: api.HostSource{ID: "home", Name: "Home", Kind: "managed-host", Transport: "agent",
		Swarm: "sw1", HostID: "h1", Paths: []string{"/srv/home"}, Backend: restic.BackendS3,
		Repository: "s3:backup.test/bucket", PasswordSecret: "restic-password", CredentialSecret: "restic-backend",
		Schedule: "0 2 * * *", Retention: restic.Retention{KeepDaily: 7, Prune: true}, CheckSubset: "5%"},
		Password: "test-only-password", Credentials: map[string]string{"AWS_SECRET_ACCESS_KEY": "test-only-secret"}}
	if _, err := s.Put(ctx, operatorSubject, in); err == nil {
		t.Fatal("operator configured source")
	}
	out, err := s.Put(ctx, adminSubject, in)
	if err != nil || out.ID != "home" {
		t.Fatal(out, err)
	}
	raw, ok, err := l.Store().Raw(CollHostSourceSecrets, "home")
	if err != nil || !ok || bytes.Contains(raw.Value, []byte("test-only-password")) || bytes.Contains(raw.Value, []byte("test-only-secret")) {
		t.Fatal("secret not sealed", err)
	}
	listed, err := s.List(ctx)
	if err != nil || len(listed) != 1 || strings.Contains(listed[0].Repository, "test-only") {
		t.Fatal(listed, err)
	}
	ref, err := s.Run(ctx, operatorSubject, "home")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "host backup operation", func() bool {
		op, ok, _ := Get[OperationRecord](l.Store(), CollOperations, ref.ID)
		return ok && op.Value.View != nil && op.Value.View.Status == runner.StatusSucceeded
	})
	if _, err := s.Restore(ctx, operatorSubject, "home", api.HostRestoreInput{Snapshot: "abc123", Target: "/srv/restored"}); err == nil {
		t.Fatal("operator started restore")
	}
	restore, err := s.Restore(ctx, adminSubject, "home", api.HostRestoreInput{Snapshot: "abc123", Target: "/srv/restored"})
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := Get[OperationRecord](l.Store(), CollOperations, restore.ID)
	if err != nil || !ok || rec.Value.ApprovalID == "" || len(rec.Value.Definition.Steps) != 1 ||
		len(rec.Value.Definition.Steps[0].Preconditions) != 1 || rec.Value.Definition.Steps[0].Preconditions[0].Kind != runner.CondApproved {
		t.Fatalf("restore is not approval gated: %+v %v", rec, err)
	}
	if _, err := s.Run(ctx, nobody, "home"); err == nil {
		t.Fatal("viewer started backup")
	}
}

func TestExternalHostSourceSSHPlanIsStored(t *testing.T) {
	l, svcs, _ := serviceFixture(t)
	s := &HostSources{Ops: svcs.Ops, Keys: testKey}
	in := api.HostSourceInput{HostSource: api.HostSource{ID: "outside", Name: "Outside", Kind: "external-host", Transport: "ssh-plan", HostID: "external-1",
		SSHAddress: "outside.example.test", SSHUser: "backup", Paths: []string{"/srv/data"}, Backend: restic.BackendLocal, Repository: "/backup/repo", PasswordSecret: "restic-password"},
		Password: "test-only", SSHPrivateKey: "synthetic-private-key", SSHKnownHosts: "outside.example.test ssh-ed25519 synthetic"}
	if _, err := s.Put(context.Background(), adminSubject, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), adminSubject, "outside"); err != nil {
		t.Fatalf("external SSH operation plan: %v", err)
	}
	if v, ok, _ := Get[api.HostSource](l.Store(), CollHostSources, "outside"); !ok || v.Value.Kind != "external-host" {
		t.Fatal("external host source missing")
	}
	changed := in
	changed.HostSource.SSHAddress = "different.example.test"
	if _, err := s.Put(context.Background(), adminSubject, changed); err == nil {
		t.Fatal("queued SSH operation could be redirected by changing the host address")
	}
}

type fakeHostSSH struct {
	command string
	stdin   string
	host    provision.Host
	cred    provision.Credential
	output  string
}

func (f *fakeHostSSH) Run(_ context.Context, h provision.Host, c provision.Credential, command, stdin string) (string, error) {
	f.command, f.stdin, f.host, f.cred = command, stdin, h, c
	if f.output != "" {
		return f.output, nil
	}
	return `{"message_type":"summary","snapshot_id":"abc123"}` + "\n", nil
}

func TestExternalResticSSHAdapterKeepsSecretsOffCommand(t *testing.T) {
	l, svcs, _ := serviceFixture(t)
	s := &HostSources{Ops: svcs.Ops, Keys: testKey}
	in := api.HostSourceInput{HostSource: api.HostSource{ID: "outside", Name: "Outside", Kind: "external-host", Transport: "ssh-plan", HostID: "outside",
		SSHAddress: "outside.example.test", SSHUser: "backup", Paths: []string{"/srv/data"}, Backend: restic.BackendS3,
		Repository: "s3:backup.example.test/repo", PasswordSecret: "pw", CredentialSecret: "s3"},
		Password: "test-only-password", Credentials: map[string]string{"AWS_SECRET_ACCESS_KEY": "test-only-secret"},
		SSHPrivateKey: "synthetic-private-key", SSHKnownHosts: "outside.example.test ssh-ed25519 synthetic"}
	if _, err := s.Put(context.Background(), adminSubject, in); err != nil {
		t.Fatal(err)
	}
	sealed, ok, err := l.Store().Raw(CollHostSourceSecrets, "outside")
	if err != nil || !ok || bytes.Contains(sealed.Value, []byte("synthetic-private-key")) || bytes.Contains(sealed.Value, []byte("test-only-password")) {
		t.Fatal("external credentials are not sealed", err)
	}
	f := &fakeHostSSH{}
	client := HostSourceSSHClient{Store: l.Store(), Keys: testKey, Commands: f}
	args, _ := json.Marshal(agent.ResticArgs{Backend: in.Backend, Repository: in.Repository, PasswordSecret: in.PasswordSecret,
		CredentialSecret: in.CredentialSecret, Paths: in.Paths})
	res, err := client.Send(context.Background(), "restic-source/outside", agent.Command{OpID: "op", StepID: "backup", Kind: KindResticSSHBackup, Args: args})
	if err != nil || res.Status != agent.StatusSucceeded || f.host.Address != in.SSHAddress || f.cred.KnownHosts != in.SSHKnownHosts {
		t.Fatalf("result=%+v err=%v host=%+v", res, err, f.host)
	}
	if strings.Contains(f.command, "test-only") || !strings.Contains(f.stdin, "test-only-password") || !strings.Contains(f.stdin, "test-only-secret") ||
		!strings.Contains(f.command, "RESTIC_PASSWORD_FILE") || !strings.Contains(f.command, "backup") || !strings.Contains(f.command, "--quiet") {
		t.Fatalf("credential transport command=%q stdin length=%d", f.command, len(f.stdin))
	}
	wrongArgs, _ := json.Marshal(agent.ResticArgs{Backend: in.Backend, Repository: in.Repository, PasswordSecret: in.PasswordSecret,
		CredentialSecret: in.CredentialSecret, Paths: []string{"/etc"}})
	if _, err := client.Send(context.Background(), "restic-source/outside", agent.Command{OpID: "op", StepID: "wrong", Kind: KindResticSSHBackup, Args: wrongArgs}); err == nil {
		t.Fatal("altered source path reached SSH")
	}
	f.output = "\n__DBCM_RESTIC_EXIT__:11\n"
	locked, err := client.Send(context.Background(), "restic-source/outside", agent.Command{OpID: "op", StepID: "locked", Kind: KindResticSSHBackup, Args: args})
	if err != nil || locked.Status != agent.StatusFailed || !strings.Contains(locked.Message, "stale lock") || strings.Contains(f.command, "unlock") {
		t.Fatalf("lock handling %+v err=%v command=%q", locked, err, f.command)
	}
	f.output = `{"message_type":"summary"}` + "\n"
	restoreArgs, _ := json.Marshal(agent.ResticArgs{Backend: in.Backend, Repository: in.Repository, PasswordSecret: in.PasswordSecret,
		CredentialSecret: in.CredentialSecret, Snapshot: "abc123", Target: "/srv/new-restore", Approved: true})
	restored, err := client.Send(context.Background(), "restic-source/outside", agent.Command{OpID: "op", StepID: "restore", Kind: KindResticSSHRestore, Args: restoreArgs})
	if err != nil || restored.Status != agent.StatusSucceeded || !strings.Contains(f.command, "mkdir -m 700") || !strings.Contains(f.command, "overwrite") {
		t.Fatalf("remote restore %+v err=%v command=%q", restored, err, f.command)
	}
	var stored OperationRecord
	ref, err := s.Run(context.Background(), adminSubject, "outside")
	if err != nil {
		t.Fatal(err)
	}
	r, ok, err := Get[OperationRecord](l.Store(), CollOperations, ref.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	stored = r.Value
	if bytes.Contains(stored.Definition.Steps[0].Args, []byte("test-only")) || stored.Definition.Steps[0].Command != KindResticSSHBackup {
		t.Fatal("secret in operation or wrong transport")
	}
}

func TestHostBackupFailureNotificationUsesRunnerOutcome(t *testing.T) {
	l, svcs, agents := serviceFixture(t)
	setReports(t, l, "sw1", "h1", agentrt.ContainerInfo(true))
	agents.refuse = func(cmd agent.Command) bool { return cmd.Kind == agent.KindResticBackup }
	var got []notify.Event
	s := &HostSources{Ops: svcs.Ops, Keys: testKey, Notify: func(_ context.Context, e notify.Event) bool {
		got = append(got, e)
		return true
	}}
	in := api.HostSourceInput{HostSource: api.HostSource{ID: "failing", Name: "Failing", Kind: "managed-host", Transport: "agent",
		Swarm: "sw1", HostID: "h1", Paths: []string{"/srv/data"}, Backend: restic.BackendLocal,
		Repository: "/backup/repo", PasswordSecret: "restic-password"}, Password: "test-only"}
	if _, err := s.Put(context.Background(), adminSubject, in); err != nil {
		t.Fatal(err)
	}
	ref, err := s.Run(context.Background(), adminSubject, "failing")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed host backup", func() bool {
		op, ok, _ := Get[OperationRecord](l.Store(), CollOperations, ref.ID)
		return ok && op.Value.View != nil && op.Value.View.Status == runner.StatusFailed
	})
	s.RefreshOutcomes(context.Background())
	if len(got) != 1 || got[0].Kind != KindHostBackupFailed || got[0].Details["operation"] != ref.ID {
		t.Fatalf("notifications %+v", got)
	}
	s.RefreshOutcomes(context.Background())
	if len(got) != 1 {
		t.Fatal("duplicate notification")
	}
}
