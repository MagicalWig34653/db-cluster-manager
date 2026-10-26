// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Cross-cluster and cross-Swarm backup copies (B05).
//
// A leader copies a completed backup of a cluster from the primary backup
// backend to a named target (Backups.Targets: configured in the manager
// configuration, for example the storage another Swarm's cluster uses).
// The copy is verified against the manifest while it streams and signed
// with the data keyring (dbbackup.CopyBackup). It runs in the background of
// the leadership; a copy that a leader change interrupted is marked failed
// on the next scheduler pass and can be started again (it is idempotent).
//
// A copy never changes a cluster. Restoring from it is a separate,
// explicit step: ProposeCopyRestore verifies the copy deeply and returns a
// digest-bound proposal for a NEW cluster; nothing is promoted, routed or
// started by this file.
//
// Retention coupling: when retention deletes a backup set of the source
// cluster, the copies of that set on targets are deleted with it (their
// transfer record first, then manifest and stream). The newest set and the
// newest verified set are never selected, so they keep their copies.

// CollBackupCopies holds BackupCopyRecord values; key: copy ID.
const CollBackupCopies = "backup-copies"

// Copy states.
const (
	CopyRunning   = "running"
	CopySucceeded = "succeeded"
	CopyFailed    = "failed"
)

// KindBackupCopyFailed is the notification kind of a failed copy.
const KindBackupCopyFailed = "backup.copy_failed"

// BackupCopyRecord is one copy of a backup to a target.
type BackupCopyRecord struct {
	ID         string                    `json:"id"`
	Cluster    string                    `json:"cluster"`
	Swarm      string                    `json:"swarm"`
	Backup     string                    `json:"backup"`
	Target     string                    `json:"target"`
	Status     string                    `json:"status"`
	Error      string                    `json:"error,omitempty"`
	Objects    []string                  `json:"objects"`
	Records    []dbbackup.TransferRecord `json:"records,omitempty"`
	By         string                    `json:"by"`
	StartedAt  time.Time                 `json:"startedAt"`
	FinishedAt time.Time                 `json:"finishedAt,omitzero"`
	// DeletedAt is set when retention removed the copy's objects.
	DeletedAt time.Time `json:"deletedAt,omitzero"`
}

func (r BackupCopyRecord) view() api.BackupCopy {
	return api.BackupCopy{ID: r.ID, Cluster: r.Cluster, Swarm: r.Swarm, Backup: r.Backup, Target: r.Target, Status: r.Status,
		Error: r.Error, Objects: slices.Clone(r.Objects), By: r.By, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
}

// transferState is the in-memory part of the copy service.
type transferState struct {
	mu     sync.Mutex
	active map[string]bool
}

func (t *transferState) set(id string, on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		t.active = map[string]bool{}
	}
	if on {
		t.active[id] = true
	} else {
		delete(t.active, id)
	}
}

func (t *transferState) has(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active[id]
}

func (b *Backups) notify(ctx context.Context, sev notify.Severity, kind, resource, msg string, details map[string]string) {
	if b.Notify == nil {
		return
	}
	b.Notify(ctx, notify.Event{Time: b.now(), Severity: sev, Kind: kind, Resource: resource, Message: msg, Details: details})
}

// BackupCopies implements api.BackupTransfers.
func (b *Backups) BackupCopies(context.Context) ([]api.BackupCopy, error) {
	recs, err := List[BackupCopyRecord](b.store(), CollBackupCopies, Consistent())
	if err != nil {
		return nil, MapCommandError(err)
	}
	out := make([]api.BackupCopy, 0, len(recs))
	for _, r := range recs {
		if r.Value.DeletedAt.IsZero() {
			out = append(out, r.Value.view())
		}
	}
	slices.SortFunc(out, func(a, c api.BackupCopy) int { return c.StartedAt.Compare(a.StartedAt) })
	return out, nil
}

// BackupCopy implements api.BackupTransfers.
func (b *Backups) BackupCopy(_ context.Context, id string) (api.BackupCopy, error) {
	r, ok, err := Get[BackupCopyRecord](b.store(), CollBackupCopies, id, Consistent())
	if err != nil {
		return api.BackupCopy{}, MapCommandError(err)
	}
	if !ok || !r.Value.DeletedAt.IsZero() {
		return api.BackupCopy{}, fmt.Errorf("%w: backup copy %q", api.ErrNotFound, id)
	}
	return r.Value.view(), nil
}

func (b *Backups) completedBackupRecord(clusterID, id string) (BackupRecord, error) {
	r, ok, err := Get[BackupRecord](b.store(), CollBackups, id, Consistent())
	if err != nil {
		return BackupRecord{}, err
	}
	if !ok || r.Value.Cluster != clusterID || !r.Value.DeletedAt.IsZero() {
		return BackupRecord{}, fmt.Errorf("%w: backup %q of cluster %q", api.ErrNotFound, id, clusterID)
	}
	op, ok, err := Get[OperationRecord](b.store(), CollOperations, r.Value.OpID, Consistent())
	if err != nil {
		return BackupRecord{}, err
	}
	if !ok || op.Value.View == nil || op.Value.View.Status != runner.StatusSucceeded || len(r.Value.Objects) == 0 {
		return BackupRecord{}, fmt.Errorf("%w: backup %q is not complete", ErrConflict, id)
	}
	return r.Value, nil
}

// StartBackupCopy implements api.BackupTransfers. It needs the backup
// permission on the cluster's backups (audited).
func (b *Backups) StartBackupCopy(_ context.Context, actor authz.Subject, clusterID string, in api.BackupCopyInput) (api.BackupCopy, error) {
	cp, err := b.startBackupCopy(actor, clusterID, in)
	return cp, MapCommandError(err)
}

func (b *Backups) startBackupCopy(actor authz.Subject, clusterID string, in api.BackupCopyInput) (api.BackupCopy, error) {
	l, err := b.Ops.leadership()
	if err != nil {
		return api.BackupCopy{}, err
	}
	c, err := b.clusterRecord(clusterID)
	if err != nil {
		return api.BackupCopy{}, err
	}
	if err := b.Ops.authorize(actor, authz.ActionBackup, backupRes(c.Swarm, c.ID)); err != nil {
		return api.BackupCopy{}, err
	}
	dst, ok := b.Targets[in.Target]
	switch {
	case b.Backend == nil || b.Keys == nil:
		return api.BackupCopy{}, fmt.Errorf("%w: backup backend and data keyring required for copies", ErrSettingsIncomplete)
	case !ok:
		return api.BackupCopy{}, fmt.Errorf("%w: unknown backup target %q", dbbackup.ErrInvalidRequest, in.Target)
	}
	br, err := b.completedBackupRecord(clusterID, in.Backup)
	if err != nil {
		return api.BackupCopy{}, err
	}
	// One running copy per backup and target.
	recs, err := List[BackupCopyRecord](b.store(), CollBackupCopies, Consistent())
	if err != nil {
		return api.BackupCopy{}, err
	}
	for _, r := range recs {
		if r.Value.Backup == br.ID && r.Value.Target == in.Target && r.Value.Status == CopyRunning && b.xfer.has(r.Value.ID) {
			return r.Value.view(), nil
		}
	}
	rec := BackupCopyRecord{ID: newID("cp"), Cluster: c.ID, Swarm: c.Swarm, Backup: br.ID, Target: in.Target, Status: CopyRunning,
		Objects: slices.Clone(br.Objects), By: actor.ID, StartedAt: b.now()}
	if _, err := Put(b.store(), CollBackupCopies, rec.ID, rec, IfVersion(0)); err != nil {
		return api.BackupCopy{}, err
	}
	b.xfer.set(rec.ID, true)
	b.Ops.Svc.recordEvent(c.Swarm, c.ID, api.SeverityInfo, fmt.Sprintf("%s started copy %s of backup %s of %s to target %s", actor.ID, rec.ID, br.ID, c.Name, in.Target))
	src := b.Backend
	if !l.Go(func(ctx context.Context) {
		defer b.xfer.set(rec.ID, false)
		b.runCopy(ctx, rec, c, src, dst)
	}) {
		b.xfer.set(rec.ID, false)
		return api.BackupCopy{}, ErrNotLeader
	}
	return rec.view(), nil
}

func (b *Backups) runCopy(ctx context.Context, rec BackupCopyRecord, c ClusterRecord, src, dst backup.Backend) {
	var errs []error
	for _, obj := range rec.Objects {
		tr, err := dbbackup.CopyBackup(ctx, src, dst, dbbackup.TransferRequest{Object: obj, SourceSwarm: c.Swarm,
			SourceInstallation: b.installation(), By: rec.By, Keys: b.Keys, Now: b.now})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", obj, err))
			break
		}
		rec.Records = append(rec.Records, tr)
	}
	rec.FinishedAt = b.now()
	if err := errors.Join(errs...); err != nil {
		rec.Status, rec.Error = CopyFailed, err.Error()
	} else {
		rec.Status = CopySucceeded
	}
	// The record is written under a context that survives the end of the
	// leadership only for this final write; a lost write leaves "running",
	// which the next scheduler pass turns into "failed".
	if _, _, err := Update(b.store(), CollBackupCopies, rec.ID, func(cur BackupCopyRecord, _ bool) (BackupCopyRecord, error) {
		return rec, nil
	}); err != nil {
		b.log("manager: backup copy result not stored", "copy", rec.ID, "err", err)
		return
	}
	if rec.Status == CopySucceeded {
		b.Ops.Svc.recordEvent(c.Swarm, c.ID, api.SeverityInfo, fmt.Sprintf("copy %s of backup %s of %s to target %s verified", rec.ID, rec.Backup, c.Name, rec.Target))
		return
	}
	msg := fmt.Sprintf("copy %s of backup %s of %s to target %s failed: %s", rec.ID, rec.Backup, c.Name, rec.Target, rec.Error)
	b.Ops.Svc.recordEvent(c.Swarm, c.ID, api.SeverityWarning, msg)
	b.notify(ctx, notify.SeverityWarning, KindBackupCopyFailed, "swarm/"+c.Swarm+"/cluster/"+c.ID, msg,
		map[string]string{"swarm": c.Swarm, "cluster": c.ID, "backup": rec.Backup, "target": rec.Target, "copy": rec.ID})
}

func (b *Backups) installation() string {
	if st, err := b.Ops.settings(); err == nil {
		return st.InstallationID
	}
	return ""
}

// failInterruptedCopies marks copies that no goroutine of this leadership
// runs any more as failed. The scheduler calls it on every pass.
func (b *Backups) failInterruptedCopies(ctx context.Context) {
	recs, err := List[BackupCopyRecord](b.store(), CollBackupCopies, Consistent())
	if err != nil {
		return
	}
	for _, r := range recs {
		v := r.Value
		if v.Status != CopyRunning || b.xfer.has(v.ID) {
			continue
		}
		v.Status, v.Error, v.FinishedAt = CopyFailed, "interrupted (manager leadership changed); start the copy again", b.now()
		if _, err := Put(b.store(), CollBackupCopies, v.ID, v, IfVersion(r.Version)); err != nil {
			continue
		}
		msg := fmt.Sprintf("copy %s of backup %s to target %s was interrupted", v.ID, v.Backup, v.Target)
		b.Ops.Svc.recordEvent(v.Swarm, v.Cluster, api.SeverityWarning, msg)
		b.notify(ctx, notify.SeverityWarning, KindBackupCopyFailed, "swarm/"+v.Swarm+"/cluster/"+v.Cluster, msg,
			map[string]string{"swarm": v.Swarm, "cluster": v.Cluster, "backup": v.Backup, "target": v.Target, "copy": v.ID})
	}
}

// ProposeCopyRestore implements api.BackupTransfers: a read-only proposal
// to restore a verified copy into a new cluster of targetSwarm. It needs
// the restore permission on the source cluster's backups (audited) and
// changes nothing; approving and executing are separate explicit steps.
func (b *Backups) ProposeCopyRestore(ctx context.Context, actor authz.Subject, copyID string, in api.RestoreProposalInput) (api.RestoreProposal, error) {
	p, err := b.proposeCopyRestore(ctx, actor, copyID, in)
	return p, MapCommandError(err)
}

func (b *Backups) proposeCopyRestore(ctx context.Context, actor authz.Subject, copyID string, in api.RestoreProposalInput) (api.RestoreProposal, error) {
	cr, ok, err := Get[BackupCopyRecord](b.store(), CollBackupCopies, copyID, Consistent())
	if err != nil {
		return api.RestoreProposal{}, err
	}
	if !ok || !cr.Value.DeletedAt.IsZero() {
		return api.RestoreProposal{}, fmt.Errorf("%w: backup copy %q", api.ErrNotFound, copyID)
	}
	cp := cr.Value
	if err := b.Ops.authorize(actor, authz.ActionRestore, backupRes(cp.Swarm, cp.Cluster)); err != nil {
		return api.RestoreProposal{}, err
	}
	dst, ok := b.Targets[cp.Target]
	if !ok || b.Keys == nil {
		return api.RestoreProposal{}, fmt.Errorf("%w: backup target %q or the data keyring is not configured", ErrSettingsIncomplete, cp.Target)
	}
	if cp.Status != CopySucceeded || len(cp.Objects) == 0 {
		return api.RestoreProposal{}, fmt.Errorf("%w: copy %s is %s", ErrConflict, cp.ID, cp.Status)
	}
	if _, ok, err := Get[SwarmRecord](b.store(), CollSwarms, in.TargetSwarm, Consistent()); err != nil {
		return api.RestoreProposal{}, err
	} else if !ok {
		return api.RestoreProposal{}, fmt.Errorf("%w: target Swarm %q", api.ErrNotFound, in.TargetSwarm)
	}
	inv, err := productionInventory(b.store())
	if err != nil {
		return api.RestoreProposal{}, err
	}
	if slices.Contains(inv.ClusterNames, in.TargetCluster) {
		return api.RestoreProposal{}, fmt.Errorf("%w: a cluster named %q exists; a restore always creates a new one", ErrClusterExists, in.TargetCluster)
	}
	p, err := dbbackup.ProposeTransferRestore(ctx, dst, b.Keys, cp.Objects[0], in.TargetSwarm, in.TargetCluster)
	if err != nil {
		if errors.Is(err, dbbackup.ErrNotTransferred) || errors.Is(err, dbbackup.ErrTransferMismatch) {
			return api.RestoreProposal{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return api.RestoreProposal{}, err
	}
	b.Ops.Svc.recordEvent(cp.Swarm, cp.Cluster, api.SeverityWarning, fmt.Sprintf("%s requested a restore proposal of copy %s (backup %s) into new cluster %s of Swarm %s; nothing is changed until it is approved",
		actor.ID, cp.ID, cp.Backup, in.TargetCluster, in.TargetSwarm))
	return api.RestoreProposal{Copy: cp.ID, BackupID: p.BackupID, SourceCluster: p.SourceClusterID, SourceSwarm: p.SourceSwarm,
		ManifestSHA256: p.ManifestSHA256, ObjectSHA256: p.ObjectSHA256, CopiedBy: p.CopiedBy, CopiedAt: p.CopiedAt,
		TargetSwarm: p.TargetSwarm, TargetCluster: p.TargetCluster, ApprovalRequired: p.ApprovalRequired, AutoPromotion: p.AutoPromotion,
		Steps: p.Steps, Digest: p.Digest}, nil
}

// removeCopiesOf deletes the copies of a backup that retention removed.
// The transfer record goes first, so a half-deleted copy never verifies.
func (b *Backups) removeCopiesOf(ctx context.Context, backupID string) error {
	recs, err := List[BackupCopyRecord](b.store(), CollBackupCopies, Consistent())
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range recs {
		v := r.Value
		if v.Backup != backupID || !v.DeletedAt.IsZero() || v.Status == CopyRunning {
			continue
		}
		dst, ok := b.Targets[v.Target]
		if !ok {
			continue
		}
		failed := false
		for _, o := range v.Objects {
			for _, n := range []string{dbbackup.TransferName(o), dbbackup.ManifestName(o), o} {
				if err := dst.Delete(ctx, n); err != nil && !errors.Is(err, backup.ErrNotFound) {
					errs = append(errs, fmt.Errorf("copy %s: delete %s: %w", v.ID, n, err))
					failed = true
				}
			}
		}
		if failed {
			continue
		}
		v.DeletedAt = b.now()
		if _, err := Put(b.store(), CollBackupCopies, v.ID, v, IfVersion(r.Version)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
