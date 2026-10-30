// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// Cross-cluster and cross-Swarm backup copies (B05).
//
// A backup is copied object by object between two backends (for example the
// target of cluster A in Swarm 1 and the target of cluster B in Swarm 2).
// The copy never decrypts the stream: it is already sealed for the recovery
// key, and the copy checks the ciphertext against the digest and size in
// the source manifest while it streams. After the stream, the unchanged
// manifest is written, and last a signed transfer record: an HMAC-SHA256
// (key derived from the manager data keyring, purpose "backup-transfer")
// over the manifest digest, the object digest and size and the provenance.
// The presence of a verifying record marks a complete, verified copy; a
// restore from a copy must verify it first (VerifyTransfer).
//
// A copy is data only. Nothing here changes a cluster, a route or a proxy:
// there is no automatic promotion. A restore from a copy always goes into
// a NEW cluster through ProposeTransferRestore, whose digest-bound
// proposal a person approves explicitly (RestoreProposal.Approve) before
// ClusterRestorePlan accepts the request (TransferRestoreRequest).
//
// Limits: the signature binds the copy to keys of this installation's
// keyring; a different installation needs the same key material.
// Verification of the signature proves that a holder of the keyring made
// the record, not that the source cluster still exists.

// TransferPurpose is the signing purpose of transfer records.
const TransferPurpose = "backup-transfer"

// TransferFormat is the transfer record version.
const TransferFormat = 1

// Errors of transfers.
var (
	// ErrTransferMismatch: the copied bytes differ from the manifest.
	ErrTransferMismatch = errors.New("dbbackup: transferred object does not match the manifest")
	// ErrNotTransferred: the backend holds no verifying transfer record.
	ErrNotTransferred = errors.New("dbbackup: no verified transfer record")
)

// TransferName is the object name of the transfer record of object.
func TransferName(object string) string { return object + ".transfer.json" }

// TransferRecord is the signed payload of a transfer.
type TransferRecord struct {
	Format             int       `json:"format"`
	BackupID           string    `json:"backup_id"`
	Object             string    `json:"object"`
	SourceClusterID    string    `json:"source_cluster_id"`
	SourceSwarm        string    `json:"source_swarm,omitempty"`
	SourceInstallation string    `json:"source_installation,omitempty"`
	ManifestSHA256     string    `json:"manifest_sha256"`
	ObjectSHA256       string    `json:"object_sha256"`
	ObjectSize         int64     `json:"object_size"`
	CopiedAt           time.Time `json:"copied_at"`
	CopiedBy           string    `json:"copied_by"`
}

// TransferRequest copies one backup object between backends.
type TransferRequest struct {
	Object string
	// SourceSwarm and SourceInstallation are provenance labels.
	SourceSwarm        string
	SourceInstallation string
	By                 string
	Keys               backup.KeySource
	Now                func() time.Time
}

func sumHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func readAllLimited(ctx context.Context, be backup.Backend, name string, limit int64) ([]byte, error) {
	rc, err := be.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: %s larger than %d bytes", ErrInvalidManifest, name, limit)
	}
	return b, nil
}

// streamDigest reads object fully and returns its SHA-256 (hex) and size.
func streamDigest(ctx context.Context, be backup.Backend, name string) (string, int64, error) {
	rc, err := be.Get(ctx, name)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// CopyBackup copies a complete backup object (stream, manifest, signed
// record) from src to dst. It is idempotent: when dst already holds a
// transfer record for the same manifest whose object verifies against the
// manifest digest, nothing is rewritten; a corrupt existing copy is replaced. A
// source without a valid manifest (an incomplete backup) is refused, and a
// stream whose bytes differ from the manifest is deleted from dst again.
func CopyBackup(ctx context.Context, src, dst backup.Backend, req TransferRequest) (TransferRecord, error) {
	if req.Keys == nil || req.Object == "" || req.By == "" {
		return TransferRecord{}, fmt.Errorf("%w: object, actor and signing keys required", ErrInvalidRequest)
	}
	now := req.Now
	if now == nil {
		now = time.Now
	}
	raw, err := readAllLimited(ctx, src, ManifestName(req.Object), 16<<20)
	if err != nil {
		return TransferRecord{}, fmt.Errorf("dbbackup: source manifest of %s: %w", req.Object, err)
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		return TransferRecord{}, err
	}
	if m.Object != req.Object {
		return TransferRecord{}, fmt.Errorf("%w: manifest names object %q", ErrInvalidManifest, m.Object)
	}
	// Deep check: a same-size corrupt object left by an earlier copy must be
	// rewritten, not reported as copied (lab E14).
	if rec, _, err := VerifyTransfer(ctx, dst, req.Object, req.Keys, true); err == nil {
		if old, merr := readAllLimited(ctx, dst, ManifestName(req.Object), 16<<20); merr == nil && bytes.Equal(old, raw) {
			return rec, nil
		}
	}
	rc, err := src.Get(ctx, req.Object)
	if err != nil {
		return TransferRecord{}, fmt.Errorf("dbbackup: source object %s: %w", req.Object, err)
	}
	defer rc.Close()
	h := sha256.New()
	cr := &countReader{r: io.TeeReader(rc, h)}
	if err := dst.Put(ctx, req.Object, cr); err != nil {
		return TransferRecord{}, fmt.Errorf("dbbackup: write copy of %s: %w", req.Object, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.Stored.SHA256 || cr.n != m.Stored.Size {
		_ = dst.Delete(ctx, req.Object)
		return TransferRecord{}, fmt.Errorf("%w: %s (digest %s, %d bytes; manifest %s, %d bytes)", ErrTransferMismatch, req.Object, got, cr.n, m.Stored.SHA256, m.Stored.Size)
	}
	if err := dst.Put(ctx, ManifestName(req.Object), bytes.NewReader(raw)); err != nil {
		return TransferRecord{}, err
	}
	rec := TransferRecord{Format: TransferFormat, BackupID: m.BackupID, Object: m.Object, SourceClusterID: m.ClusterID,
		SourceSwarm: req.SourceSwarm, SourceInstallation: req.SourceInstallation, ManifestSHA256: sumHex(raw),
		ObjectSHA256: m.Stored.SHA256, ObjectSize: m.Stored.Size, CopiedAt: now().UTC(), CopiedBy: req.By}
	payload, err := json.Marshal(rec)
	if err != nil {
		return TransferRecord{}, err
	}
	signed, err := backup.Sign(req.Keys, TransferPurpose, payload)
	if err != nil {
		return TransferRecord{}, err
	}
	enc, err := backup.EncodeSigned(signed)
	if err != nil {
		return TransferRecord{}, err
	}
	if err := dst.Put(ctx, TransferName(req.Object), bytes.NewReader(enc)); err != nil {
		return TransferRecord{}, err
	}
	return rec, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// VerifyTransfer checks the transfer record of object on be: the HMAC, the
// manifest digest and fields, and the stored object's size. With deep it
// also re-reads the object and compares its SHA-256 with the manifest
// (needed before a restore; shallow suffices for listings).
func VerifyTransfer(ctx context.Context, be backup.Backend, object string, keys backup.KeySource, deep bool) (TransferRecord, Manifest, error) {
	fail := func(f string, a ...any) (TransferRecord, Manifest, error) {
		return TransferRecord{}, Manifest{}, fmt.Errorf("%w: %s", ErrNotTransferred, fmt.Sprintf(f, a...))
	}
	enc, err := readAllLimited(ctx, be, TransferName(object), 1<<20)
	if errors.Is(err, backup.ErrNotFound) {
		return fail("%s has no transfer record", object)
	}
	if err != nil {
		return TransferRecord{}, Manifest{}, err
	}
	signed, err := backup.DecodeSigned(enc)
	if err != nil {
		return fail("%v", err)
	}
	payload, err := signed.Verify(keys, TransferPurpose)
	if err != nil {
		return fail("%v", err)
	}
	var rec TransferRecord
	if err := json.Unmarshal(payload, &rec); err != nil || rec.Format != TransferFormat || rec.Object != object {
		return fail("record does not describe %s", object)
	}
	raw, err := readAllLimited(ctx, be, ManifestName(object), 16<<20)
	if err != nil {
		return fail("manifest: %v", err)
	}
	if sumHex(raw) != rec.ManifestSHA256 {
		return fail("manifest differs from the signed digest")
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		return TransferRecord{}, Manifest{}, err
	}
	if m.Object != object || m.BackupID != rec.BackupID || m.ClusterID != rec.SourceClusterID ||
		m.Stored.SHA256 != rec.ObjectSHA256 || m.Stored.Size != rec.ObjectSize {
		return fail("manifest fields differ from the signed record")
	}
	if deep {
		d, n, err := streamDigest(ctx, be, object)
		if err != nil {
			return fail("object: %v", err)
		}
		if d != m.Stored.SHA256 || n != m.Stored.Size {
			return TransferRecord{}, Manifest{}, fmt.Errorf("%w: %s", ErrTransferMismatch, object)
		}
		return rec, m, nil
	}
	infos, err := be.List(ctx, object)
	if err != nil {
		return TransferRecord{}, Manifest{}, err
	}
	for _, i := range infos {
		if i.Name == object {
			if i.Size != m.Stored.Size {
				return TransferRecord{}, Manifest{}, fmt.Errorf("%w: %s has %d bytes, manifest says %d", ErrTransferMismatch, object, i.Size, m.Stored.Size)
			}
			return rec, m, nil
		}
	}
	return fail("object %s is missing", object)
}

// RestoreProposal is a read-only, digest-bound proposal to restore a copied
// backup into a new cluster. Computing it changes nothing.
type RestoreProposal struct {
	Format          int       `json:"format"`
	BackupID        string    `json:"backup_id"`
	Object          string    `json:"object"`
	SourceClusterID string    `json:"source_cluster_id"`
	SourceSwarm     string    `json:"source_swarm,omitempty"`
	ManifestSHA256  string    `json:"manifest_sha256"`
	ObjectSHA256    string    `json:"object_sha256"`
	CopiedBy        string    `json:"copied_by"`
	CopiedAt        time.Time `json:"copied_at"`
	TargetSwarm     string    `json:"target_swarm"`
	TargetCluster   string    `json:"target_cluster"`
	// ApprovalRequired is always true; AutoPromotion is always false. They
	// are part of the digest so a client cannot present an older shape.
	ApprovalRequired bool     `json:"approval_required"`
	AutoPromotion    bool     `json:"auto_promotion"`
	Steps            []string `json:"steps"`
	Digest           string   `json:"digest"`
}

// ProposeTransferRestore verifies the copy deeply and returns the proposal.
// The target is the name of the new cluster and the Swarm to create it in.
func ProposeTransferRestore(ctx context.Context, be backup.Backend, keys backup.KeySource, object, targetSwarm, targetCluster string) (RestoreProposal, error) {
	if targetSwarm == "" || targetCluster == "" {
		return RestoreProposal{}, fmt.Errorf("%w: target Swarm and cluster name required", ErrInvalidRequest)
	}
	rec, m, err := VerifyTransfer(ctx, be, object, keys, true)
	if err != nil {
		return RestoreProposal{}, err
	}
	if m.Method != MethodPhysical {
		return RestoreProposal{}, fmt.Errorf("%w: only a physical backup restores into a new cluster, got %s", ErrInvalidRequest, m.Method)
	}
	p := RestoreProposal{Format: 1, BackupID: m.BackupID, Object: object, SourceClusterID: m.ClusterID, SourceSwarm: rec.SourceSwarm,
		ManifestSHA256: rec.ManifestSHA256, ObjectSHA256: rec.ObjectSHA256, CopiedBy: rec.CopiedBy, CopiedAt: rec.CopiedAt,
		TargetSwarm: targetSwarm, TargetCluster: targetCluster, ApprovalRequired: true,
		Steps: []string{
			"A person approves this proposal by its digest.",
			"A new cluster " + targetCluster + " with a new identity is planned in Swarm " + targetSwarm + "; the source cluster and every route to it stay untouched.",
			"The restore job reads the verified copy, extracts it into the first node and bootstraps a new Galera history.",
			"The new cluster is inspected and verified; clients keep using the source cluster until a person switches them.",
		}}
	b, err := json.Marshal(p)
	if err != nil {
		return RestoreProposal{}, err
	}
	p.Digest = "sha256:" + sumHex(b)
	return p, nil
}

// RestoreApproval is the explicit approval of one proposal.
type RestoreApproval struct {
	ID         string    `json:"id"`
	Digest     string    `json:"digest"`
	ApprovedBy string    `json:"approved_by"`
	ApprovedAt time.Time `json:"approved_at"`
}

// Approve records approvedBy's approval when digest equals the proposal's
// digest. An empty or different digest, or an empty approver, is refused
// with ErrApprovalRequired.
func (p RestoreProposal) Approve(digest, approvedBy string, now time.Time) (RestoreApproval, error) {
	if p.Digest == "" || digest != p.Digest || approvedBy == "" || !p.ApprovalRequired || p.AutoPromotion {
		return RestoreApproval{}, fmt.Errorf("%w: the approval must carry the digest of the proposal", ErrApprovalRequired)
	}
	return RestoreApproval{ID: "apr-" + p.Digest[len("sha256:"):][:16], Digest: digest, ApprovedBy: approvedBy, ApprovedAt: now.UTC()}, nil
}

// TransferRestoreRequest turns an approved proposal into a
// ClusterRestoreRequest. base carries the infrastructure choices (target
// topology, hosts, bindings, job image); this function sets the source and
// the approval and refuses anything that does not match the proposal.
func TransferRestoreRequest(p RestoreProposal, a RestoreApproval, m Manifest, base ClusterRestoreRequest) (ClusterRestoreRequest, error) {
	if a.Digest != p.Digest || a.ApprovedBy == "" || a.ID == "" {
		return ClusterRestoreRequest{}, ErrApprovalRequired
	}
	if m.BackupID != p.BackupID || m.Object != p.Object || m.ClusterID != p.SourceClusterID || m.Stored.SHA256 != p.ObjectSHA256 {
		return ClusterRestoreRequest{}, fmt.Errorf("%w: manifest is not the one the proposal covers", ErrInvalidRequest)
	}
	if base.Target.Cluster.Name != p.TargetCluster {
		return ClusterRestoreRequest{}, fmt.Errorf("%w: target cluster %q differs from the proposal's %q", ErrInvalidRequest, base.Target.Cluster.Name, p.TargetCluster)
	}
	base.ApprovalID = a.ID
	base.Source.Manifest = m
	base.Source.ClusterID = p.SourceClusterID
	return base, nil
}
