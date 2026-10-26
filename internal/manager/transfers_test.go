// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// completedBackupWithData stores a finished physical backup whose stream
// bytes match its manifest, so that copies verify.
func (env *backupEnv) completedBackupWithData(t *testing.T, id string, ended time.Time) string {
	t.Helper()
	obj := dbbackup.ObjectName("clu-1", id, dbbackup.MethodPhysical, "")
	data := make([]byte, 5000)
	_, _ = rand.Read(data)
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	m := dbbackup.Manifest{Format: dbbackup.ManifestFormat, BackupID: id, Method: dbbackup.MethodPhysical, ClusterID: "clu-1",
		SourceNodeUUID: "00000000-0000-4000-8000-000000000002", Object: obj, Started: ended.Add(-time.Minute), Finished: ended,
		Stream: backup.StreamSummary{KeyID: "k", PlainSize: 10, PlainMAC: "mm", CipherSize: int64(len(data)), CipherSHA256: hexSum,
			Chunks: []backup.ChunkDigest{{Index: 0, Size: 10, MAC: "bb"}}},
		Stored: backup.PutResult{Size: int64(len(data)), SHA256: hexSum}, Checkpoints: &dbbackup.Checkpoints{BackupType: "full-backuped", ToLSN: 5}}
	raw, _ := json.Marshal(m)
	ctx := context.Background()
	if err := env.be.Put(ctx, obj, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := env.be.Put(ctx, dbbackup.ManifestName(obj), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	st := env.svc.Store()
	if _, err := Put(st, CollOperations, "op-"+id, OperationRecord{ID: "op-" + id, Kind: dbbackup.OpBackup, Swarm: "sw1", Cluster: "clu-1",
		View: &runner.View{ID: "op-" + id, Status: runner.StatusSucceeded}, RequestedAt: ended.Add(-time.Minute), EndedAt: ended}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, CollBackups, id, BackupRecord{ID: id, Cluster: "clu-1", Swarm: "sw1", OpID: "op-" + id,
		Method: dbbackup.MethodPhysical, Objects: []string{obj}, StartedAt: ended.Add(-time.Minute)}, nil); err != nil {
		t.Fatal(err)
	}
	return obj
}

type notes struct {
	mu sync.Mutex
	ev []notify.Event
}

func (n *notes) Notify(_ context.Context, e notify.Event) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ev = append(n.ev, e)
	return true
}

func (n *notes) kinds() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, e := range n.ev {
		out = append(out, e.Kind)
	}
	return out
}

func waitCopy(t *testing.T, env *backupEnv, id string, want string) BackupCopyRecord {
	t.Helper()
	var last BackupCopyRecord
	for range 400 {
		r, ok, err := Get[BackupCopyRecord](env.svc.Store(), CollBackupCopies, id, Consistent())
		if err == nil && ok {
			last = r.Value
			if last.Status == want {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("copy %s: status %q (error %q), want %s", id, last.Status, last.Error, want)
	return last
}

func copyEnv(t *testing.T) (*backupEnv, *backup.MemBackend, *notes) {
	t.Helper()
	env := newBackupEnv(t, true)
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	dr := backup.NewMemBackend()
	n := &notes{}
	env.b.Targets = map[string]backup.Backend{"dr": dr}
	env.b.Keys = backup.StaticKeys{CurrentID: "dk1", Keys: map[string][]byte{"dk1": k}}
	env.b.Notify = n.Notify
	return env, dr, n
}

func TestBackupCopyToTargetThenApprovalGatedProposal(t *testing.T) {
	env, dr, _ := copyEnv(t)
	ctx := context.Background()
	obj := env.completedBackupWithData(t, "b1", env.clock.Now().Add(-time.Hour))

	if _, err := env.b.StartBackupCopy(ctx, nobody, "clu-1", api.BackupCopyInput{Backup: "b1", Target: "dr"}); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized copy: %v", err)
	}
	if _, err := env.b.StartBackupCopy(ctx, adminSubject, "clu-1", api.BackupCopyInput{Backup: "b1", Target: "nope"}); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("unknown target: %v", err)
	}
	if _, err := env.b.StartBackupCopy(ctx, adminSubject, "clu-1", api.BackupCopyInput{Backup: "nope", Target: "dr"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown backup: %v", err)
	}
	cp, err := env.b.StartBackupCopy(ctx, adminSubject, "clu-1", api.BackupCopyInput{Backup: "b1", Target: "dr"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitCopy(t, env, cp.ID, CopySucceeded)
	if len(done.Records) != 1 || done.Records[0].SourceSwarm != "sw1" || done.By != adminSubject.ID {
		t.Fatalf("record %+v", done)
	}
	if _, _, err := dbbackup.VerifyTransfer(ctx, dr, obj, env.b.Keys, true); err != nil {
		t.Fatalf("copy does not verify: %v", err)
	}
	list, err := env.b.BackupCopies(ctx)
	if err != nil || len(list) != 1 || list[0].Status != CopySucceeded {
		t.Fatalf("list %v %+v", err, list)
	}

	// The proposal needs the restore permission, a known target Swarm and an unused cluster name.
	in := api.RestoreProposalInput{TargetSwarm: "sw1", TargetCluster: "restored"}
	if _, err := env.b.ProposeCopyRestore(ctx, nobody, cp.ID, in); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized proposal: %v", err)
	}
	if _, err := env.b.ProposeCopyRestore(ctx, adminSubject, cp.ID, api.RestoreProposalInput{TargetSwarm: "ghost", TargetCluster: "restored"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown swarm: %v", err)
	}
	if _, err := env.b.ProposeCopyRestore(ctx, adminSubject, cp.ID, api.RestoreProposalInput{TargetSwarm: "sw1", TargetCluster: "shop"}); !errors.Is(err, api.ErrClusterExists) {
		t.Fatalf("existing cluster name: %v", err)
	}
	before, _ := env.svc.Store().RawList(CollClusters)
	p, err := env.b.ProposeCopyRestore(ctx, adminSubject, cp.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.ApprovalRequired || p.AutoPromotion || !strings.HasPrefix(p.Digest, "sha256:") || p.SourceCluster != "clu-1" || p.Copy != cp.ID {
		t.Fatalf("proposal %+v", p)
	}
	after, _ := env.svc.Store().RawList(CollClusters)
	ops, _ := env.svc.Store().RawList(CollOperations)
	if len(before) != len(after) {
		t.Fatal("a proposal changed the clusters")
	}
	for _, d := range ops {
		if strings.Contains(string(d.Value), "restore") && strings.Contains(string(d.Value), "restored") {
			t.Fatal("a proposal started an operation")
		}
	}

	// A tampered copy no longer yields a proposal.
	if err := dr.Put(ctx, dbbackup.ManifestName(obj), strings.NewReader("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := env.b.ProposeCopyRestore(ctx, adminSubject, cp.ID, in); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("tampered copy: %v", err)
	}
}

func TestBackupCopyFailureIsRecordedAndNotified(t *testing.T) {
	env, dr, n := copyEnv(t)
	ctx := context.Background()
	obj := env.completedBackupWithData(t, "b1", env.clock.Now().Add(-time.Hour))
	raw, _ := readBackend(env.be, obj)
	raw[7] ^= 0xff
	if err := env.be.Put(ctx, obj, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	cp, err := env.b.StartBackupCopy(ctx, adminSubject, "clu-1", api.BackupCopyInput{Backup: "b1", Target: "dr"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitCopy(t, env, cp.ID, CopyFailed)
	if !strings.Contains(done.Error, "does not match the manifest") {
		t.Fatalf("error %q", done.Error)
	}
	if objs, _ := dr.List(ctx, ""); len(objs) != 0 {
		t.Fatalf("target holds %+v", objs)
	}
	for range 100 {
		if len(n.kinds()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if k := n.kinds(); len(k) != 1 || k[0] != KindBackupCopyFailed {
		t.Fatalf("notifications %v", k)
	}
	if _, err := env.b.ProposeCopyRestore(ctx, adminSubject, cp.ID, api.RestoreProposalInput{TargetSwarm: "sw1", TargetCluster: "restored"}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("proposal from a failed copy: %v", err)
	}
}

func TestInterruptedCopyIsMarkedFailedAndRetentionRemovesCopies(t *testing.T) {
	env, dr, n := copyEnv(t)
	ctx := context.Background()
	// A "running" record without a goroutine of this leadership.
	rec := BackupCopyRecord{ID: "cp-old", Cluster: "clu-1", Swarm: "sw1", Backup: "b0", Target: "dr", Status: CopyRunning, StartedAt: env.clock.Now()}
	if _, err := Put(env.svc.Store(), CollBackupCopies, rec.ID, rec, nil); err != nil {
		t.Fatal(err)
	}
	env.b.failInterruptedCopies(ctx)
	r, _, _ := Get[BackupCopyRecord](env.svc.Store(), CollBackupCopies, "cp-old")
	if r.Value.Status != CopyFailed || len(n.kinds()) != 1 {
		t.Fatalf("interrupted copy %+v, notifications %v", r.Value, n.kinds())
	}

	// Retention coupling: two backups copied, KeepLast 1 deletes the older set and its copy.
	var objs []string
	for i, id := range []string{"b1", "b2"} {
		objs = append(objs, env.completedBackupWithData(t, id, env.clock.Now().Add(-time.Duration(2-i)*time.Hour)))
		cp, err := env.b.StartBackupCopy(ctx, adminSubject, "clu-1", api.BackupCopyInput{Backup: id, Target: "dr"})
		if err != nil {
			t.Fatal(err)
		}
		waitCopy(t, env, cp.ID, CopySucceeded)
	}
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Schedule: "0 3 * * *", Retention: &api.BackupRetention{KeepLast: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := env.b.ApplyRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dbbackup.VerifyTransfer(ctx, dr, objs[1], env.b.Keys, true); err != nil {
		t.Fatalf("copy of the kept set: %v", err)
	}
	if l, _ := dr.List(ctx, objs[0]); len(l) != 0 {
		t.Fatalf("copy of the deleted set remains: %+v", l)
	}
	list, _ := env.b.BackupCopies(ctx)
	for _, c := range list {
		if c.Backup == "b1" {
			t.Fatalf("copy of a deleted set still listed: %+v", c)
		}
	}
}

func readBackend(be backup.Backend, name string) ([]byte, error) {
	rc, err := be.Get(context.Background(), name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(rc)
	return buf.Bytes(), err
}
