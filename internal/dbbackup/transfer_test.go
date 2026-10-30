// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

func transferKeys(t *testing.T) backup.StaticKeys {
	t.Helper()
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return backup.StaticKeys{CurrentID: "dk1", Keys: map[string][]byte{"dk1": k}}
}

const xferObject = "prod-b1.xbs.dbcms"

func copyReq(keys backup.KeySource) TransferRequest {
	return TransferRequest{Object: xferObject, SourceSwarm: "swarm-a", SourceInstallation: testInst, By: "alice", Keys: keys,
		Now: func() time.Time { return t0 }}
}

func TestCopyBackupAcrossBackendsAndVerify(t *testing.T) {
	ctx := context.Background()
	src, _, _ := runPhysicalBackup(t, testKey(t))
	keys := transferKeys(t)
	dir, err := backup.NewDirBackend(filepath.Join(t.TempDir(), "other-cluster"))
	if err != nil {
		t.Fatal(err)
	}
	dst := &backup.EncryptedBackend{Inner: dir, Keys: keys} // a different target type, encrypted at rest
	rec, err := CopyBackup(ctx, src, dst, copyReq(keys))
	if err != nil {
		t.Fatal(err)
	}
	if rec.SourceClusterID != testCluster || rec.SourceSwarm != "swarm-a" || rec.CopiedBy != "alice" {
		t.Fatalf("record %+v", rec)
	}
	got, m, err := VerifyTransfer(ctx, dst, xferObject, keys, true)
	if err != nil || got.ManifestSHA256 != rec.ManifestSHA256 || m.ClusterID != testCluster {
		t.Fatalf("verify: %v %+v", err, got)
	}
	// The copy is byte-identical to the source.
	a, _ := readAllBackend(t, src, xferObject)
	b, _ := readAllBackend(t, dst, xferObject)
	if !bytes.Equal(a, b) {
		t.Fatal("copied stream differs")
	}
	// Idempotent: a second copy rewrites nothing and returns the record.
	again, err := CopyBackup(ctx, src, dst, copyReq(keys))
	if err != nil || again.CopiedAt != rec.CopiedAt {
		t.Fatalf("second copy: %v %+v", err, again)
	}
}

// TestCopyBackupRepairsCorruptExistingCopy: a repeated copy must not report
// success for a same-size corrupt object that an earlier copy left on the
// target (lab E14: the second copy "succeeded" in milliseconds over a copy
// with a flipped byte).
func TestCopyBackupRepairsCorruptExistingCopy(t *testing.T) {
	ctx := context.Background()
	src, _, _ := runPhysicalBackup(t, testKey(t))
	keys := transferKeys(t)
	dst := backup.NewMemBackend()
	if _, err := CopyBackup(ctx, src, dst, copyReq(keys)); err != nil {
		t.Fatal(err)
	}
	stream, _ := readAllBackend(t, dst, xferObject)
	flipped := append([]byte(nil), stream...)
	flipped[10] ^= 1
	if err := dst.Put(ctx, xferObject, bytes.NewReader(flipped)); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyBackup(ctx, src, dst, copyReq(keys)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyTransfer(ctx, dst, xferObject, keys, true); err != nil {
		t.Fatalf("copy still corrupt after a repeated copy: %v", err)
	}
}

func readAllBackend(t *testing.T, be backup.Backend, name string) ([]byte, error) {
	t.Helper()
	rc, err := be.Get(context.Background(), name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func TestCopyBackupRefusals(t *testing.T) {
	ctx := context.Background()
	src, _, _ := runPhysicalBackup(t, testKey(t))
	keys := transferKeys(t)

	// A backup without manifest is incomplete and is not copied.
	if err := src.Delete(ctx, ManifestName(xferObject)); err != nil {
		t.Fatal(err)
	}
	dst := backup.NewMemBackend()
	if _, err := CopyBackup(ctx, src, dst, copyReq(keys)); !errors.Is(err, backup.ErrNotFound) {
		t.Fatalf("incomplete source: %v", err)
	}
	if objs, _ := dst.List(ctx, ""); len(objs) != 0 {
		t.Fatalf("destination touched: %+v", objs)
	}

	// A corrupted source stream is detected against the manifest and removed again.
	src2, _, _ := runPhysicalBackup(t, testKey(t))
	raw, _ := readAllBackend(t, src2, xferObject)
	raw[len(raw)/2] ^= 0xff
	if err := src2.Put(ctx, xferObject, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	dst2 := backup.NewMemBackend()
	if _, err := CopyBackup(ctx, src2, dst2, copyReq(keys)); !errors.Is(err, ErrTransferMismatch) {
		t.Fatalf("corrupted source: %v", err)
	}
	if objs, _ := dst2.List(ctx, ""); len(objs) != 0 {
		t.Fatalf("mismatching copy left behind: %+v", objs)
	}

	for _, bad := range []TransferRequest{{Object: xferObject, By: "a"}, {Object: xferObject, Keys: keys}, {By: "a", Keys: keys}} {
		if _, err := CopyBackup(ctx, src2, dst2, bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("request %+v: %v", bad, err)
		}
	}
}

func TestVerifyTransferDetectsTampering(t *testing.T) {
	ctx := context.Background()
	src, _, _ := runPhysicalBackup(t, testKey(t))
	keys := transferKeys(t)
	dst := backup.NewMemBackend()
	if _, err := CopyBackup(ctx, src, dst, copyReq(keys)); err != nil {
		t.Fatal(err)
	}
	put := func(name string, b []byte) {
		if err := dst.Put(ctx, name, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, _ := readAllBackend(t, dst, ManifestName(xferObject))
	record, _ := readAllBackend(t, dst, TransferName(xferObject))
	stream, _ := readAllBackend(t, dst, xferObject)

	// Wrong keyring (another installation) cannot verify.
	if _, _, err := VerifyTransfer(ctx, dst, xferObject, transferKeys(t), false); !errors.Is(err, ErrNotTransferred) {
		t.Errorf("other keys: %v", err)
	}
	// Edited manifest.
	var mm Manifest
	if err := json.Unmarshal(manifest, &mm); err != nil {
		t.Fatal(err)
	}
	mm.ClusterID = "evil-prod"
	edited, _ := json.Marshal(mm)
	put(ManifestName(xferObject), edited)
	if _, _, err := VerifyTransfer(ctx, dst, xferObject, keys, false); !errors.Is(err, ErrNotTransferred) {
		t.Errorf("edited manifest: %v", err)
	}
	put(ManifestName(xferObject), manifest)
	// Edited record.
	put(TransferName(xferObject), append(record[:len(record)-4:len(record)-4], []byte(`0"}`)...))
	if _, _, err := VerifyTransfer(ctx, dst, xferObject, keys, false); !errors.Is(err, ErrNotTransferred) {
		t.Errorf("edited record: %v", err)
	}
	put(TransferName(xferObject), record)
	// Flipped stream byte: caught by the deep check only; truncation by the shallow one.
	flipped := append([]byte(nil), stream...)
	flipped[10] ^= 1
	put(xferObject, flipped)
	if _, _, err := VerifyTransfer(ctx, dst, xferObject, keys, false); err != nil {
		t.Errorf("shallow check of same-size object: %v", err)
	}
	if _, _, err := VerifyTransfer(ctx, dst, xferObject, keys, true); !errors.Is(err, ErrTransferMismatch) {
		t.Errorf("deep check of flipped object: %v", err)
	}
	put(xferObject, stream[:len(stream)-1])
	if _, _, err := VerifyTransfer(ctx, dst, xferObject, keys, false); !errors.Is(err, ErrTransferMismatch) {
		t.Errorf("truncated object: %v", err)
	}
	// A plain backup that was never transferred has no record.
	if _, _, err := VerifyTransfer(ctx, src, xferObject, keys, false); !errors.Is(err, ErrNotTransferred) {
		t.Errorf("untransferred: %v", err)
	}
}

func TestTransferRestoreNeedsExplicitApprovalAndNewIdentity(t *testing.T) {
	ctx := context.Background()
	src, _, _ := runPhysicalBackup(t, testKey(t))
	keys := transferKeys(t)
	dst := backup.NewMemBackend()
	if _, err := CopyBackup(ctx, src, dst, copyReq(keys)); err != nil {
		t.Fatal(err)
	}
	p, err := ProposeTransferRestore(ctx, dst, keys, xferObject, "swarm-b", "restored")
	if err != nil {
		t.Fatal(err)
	}
	if !p.ApprovalRequired || p.AutoPromotion || p.Digest == "" || p.SourceClusterID != testCluster {
		t.Fatalf("proposal %+v", p)
	}
	// The same input gives the same digest; another target another one.
	p2, _ := ProposeTransferRestore(ctx, dst, keys, xferObject, "swarm-b", "restored")
	p3, _ := ProposeTransferRestore(ctx, dst, keys, xferObject, "swarm-b", "other")
	if p.Digest != p2.Digest || p.Digest == p3.Digest {
		t.Fatal("digest does not bind the proposal")
	}
	_, m, _ := VerifyTransfer(ctx, dst, xferObject, keys, false)
	base := clusterRestoreRequest(t.TempDir())
	base.Source = RestoreSource{}
	// No approval, wrong digest, empty approver: refused.
	for _, c := range []struct{ digest, by string }{{"", "bob"}, {"sha256:abc", "bob"}, {p.Digest, ""}} {
		if _, err := p.Approve(c.digest, c.by, t0); !errors.Is(err, ErrApprovalRequired) {
			t.Errorf("approve %+v: %v", c, err)
		}
	}
	if _, err := TransferRestoreRequest(p, RestoreApproval{}, m, base); !errors.Is(err, ErrApprovalRequired) {
		t.Errorf("no approval: %v", err)
	}
	a, err := p.Approve(p.Digest, "bob", t0)
	if err != nil {
		t.Fatal(err)
	}
	// The approval of another proposal does not unlock this one.
	if a3, _ := p3.Approve(p3.Digest, "bob", t0); true {
		if _, err := TransferRestoreRequest(p, a3, m, base); !errors.Is(err, ErrApprovalRequired) {
			t.Errorf("foreign approval: %v", err)
		}
	}
	base.Target.Cluster.Name = "different"
	if _, err := TransferRestoreRequest(p, a, m, base); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("target name differs: %v", err)
	}
	base.Target.Cluster.Name = "restored"
	req, err := TransferRestoreRequest(p, a, m, base)
	if err != nil {
		t.Fatal(err)
	}
	req.Source.BackendSecret, req.Source.KeySecret = "backend-cfg", "restore-key"
	res, err := ClusterRestorePlan(req, Inventory{ClusterIDs: []string{"cluster-local"}, ClusterNames: []string{"local"}})
	if err != nil {
		t.Fatal(err)
	}
	requireApprovalOnDestructive(t, res.Operation, a.ID)
	// A restore into the source identity stays refused.
	req.Target.Cluster.ClusterID = testCluster
	if _, err := ClusterRestorePlan(req, Inventory{}); !errors.Is(err, ErrProductionTarget) {
		t.Errorf("restore into source identity: %v", err)
	}
}
