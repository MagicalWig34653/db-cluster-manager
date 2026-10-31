// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Backup retention (B01) on top of dbbackup.RetentionSelect.
//
// After every scheduler pass the leader applies each cluster's retention
// policy to its complete backup sets (backups whose operation succeeded).
// It always keeps the newest set, the newest set with a passed restore
// verification, and sets an inspection session still uses; a cluster with
// an unfinished operation (a running backup or restore) is skipped for the
// pass. Deletion removes the manifest before the stream (an interrupted
// deletion leaves an incomplete backup, never a manifest without data) and
// then marks the backup record deleted; the record stays for the history.
// Failed backups' partial objects are not touched (they have no manifest
// and are ignored by restores).

// ApplyRetention applies every cluster's retention policy once.
func (b *Backups) ApplyRetention(ctx context.Context) error {
	if b.Backend == nil {
		return nil // nothing can be deleted without the backend
	}
	pols, err := List[BackupPolicyRecord](b.store(), CollBackupPolicies, Consistent())
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range pols {
		r := p.Value.Policy.Retention
		if r == nil || r.KeepLast < 1 {
			continue
		}
		if err := b.retainCluster(ctx, p.Value.Cluster, backup.Policy{KeepLast: r.KeepLast, KeepDaily: r.KeepDaily, KeepWeekly: r.KeepWeekly}); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", p.Value.Cluster, err))
		}
	}
	return errors.Join(errs...)
}

// backupSets returns the complete, not deleted backup sets of a cluster.
func (b *Backups) backupSets(clusterID string) ([]dbbackup.BackupSet, map[string]Versioned[BackupRecord], error) {
	recs, err := List[BackupRecord](b.store(), CollBackups, Consistent())
	if err != nil {
		return nil, nil, err
	}
	verified := map[string]bool{}
	reports, err := List[dbbackup.VerifyReport](b.store(), CollVerifications)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range reports {
		if r.Value.Passed {
			verified[r.Value.BackupID] = true
		}
	}
	pinned := map[string]bool{}
	insp, err := List[InspectionRecord](b.store(), CollInspections)
	if err != nil {
		return nil, nil, err
	}
	for _, ir := range insp {
		if ir.Value.Cluster != clusterID {
			continue
		}
		done := false
		if ir.Value.TeardownOp != "" {
			if op, ok, _ := Get[OperationRecord](b.store(), CollOperations, ir.Value.TeardownOp); ok && op.Value.View != nil && op.Value.View.Status == runner.StatusSucceeded {
				done = true
			}
		}
		if !done {
			pinned[ir.Value.Session.BackupID] = true
		}
	}
	var sets []dbbackup.BackupSet
	byID := map[string]Versioned[BackupRecord]{}
	for _, r := range recs {
		br := r.Value
		if br.Cluster != clusterID || !br.DeletedAt.IsZero() {
			continue
		}
		op, ok, err := Get[OperationRecord](b.store(), CollOperations, br.OpID)
		if err != nil {
			return nil, nil, err
		}
		if !ok || op.Value.View == nil || op.Value.View.Status != runner.StatusSucceeded {
			continue
		}
		created := op.Value.EndedAt
		if created.IsZero() {
			created = br.StartedAt
		}
		sets = append(sets, dbbackup.BackupSet{BackupID: br.ID, ClusterID: clusterID, Created: created, Objects: br.Objects,
			Verified: verified[br.ID], Pinned: pinned[br.ID]})
		byID[br.ID] = r
	}
	return sets, byID, nil
}

func (b *Backups) retainCluster(ctx context.Context, clusterID string, pol backup.Policy) error {
	if busy, err := b.Ops.unfinishedFor(clusterID); err != nil || busy != "" {
		return err
	}
	sets, byID, err := b.backupSets(clusterID)
	if err != nil {
		return err
	}
	_, remove := dbbackup.RetentionSelect(sets, pol)
	if len(remove) == 0 {
		return nil
	}
	// The binlog archives of a removed backup go with it, before it.
	logs, err := b.binlogSets(ctx, clusterID, remove)
	if err != nil {
		return err
	}
	err = dbbackup.ApplyRetention(ctx, b.Backend, append(slices.Clone(logs), remove...))
	// Mark only the sets whose manifests are gone; a failed deletion is
	// retried on the next pass.
	var removed []string
	for _, s := range remove {
		if len(s.Objects) > 0 {
			if rc, gerr := b.Backend.Get(ctx, dbbackup.ManifestName(s.Objects[0])); gerr == nil {
				_ = rc.Close()
				continue
			}
		}
		rec := byID[s.BackupID]
		v := rec.Value
		v.DeletedAt = b.now()
		if _, perr := Put(b.store(), CollBackups, s.BackupID, v, IfVersion(rec.Version)); perr != nil {
			err = errors.Join(err, perr)
			continue
		}
		removed = append(removed, s.BackupID)
		// Copies of a deleted set go with it (retention coupling).
		err = errors.Join(err, b.removeCopiesOf(ctx, s.BackupID))
	}
	if len(removed) > 0 {
		c, _, _ := Get[ClusterRecord](b.store(), CollClusters, clusterID)
		b.Ops.Svc.recordEvent(c.Value.Swarm, clusterID, api.SeverityInfo,
			fmt.Sprintf("retention deleted %d backup set(s) of %s: %v", len(removed), c.Value.Name, removed))
	}
	return err
}
