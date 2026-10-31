// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Point-in-time recovery (ADR 0031). A cluster created with the binary log
// on is backed up as usual; the archive job copies its closed binlog files
// next to the newest full backup, and a restore to a time is an inspection
// restore of the newest backup before that time plus a replay of the archive
// into the same new, isolated instance. Nothing here writes to production.

var _ api.PointInTimeRestores = (*Backups)(nil)

// maxPITRCandidates bounds how many backups a proposal examines.
const maxPITRCandidates = 50

// completedPhysical returns the cluster's completed, not deleted physical
// backups, newest first.
func (b *Backups) completedPhysical(clusterID string) ([]BackupRecord, error) {
	recs, err := List[BackupRecord](b.store(), CollBackups, Consistent())
	if err != nil {
		return nil, err
	}
	type done struct {
		rec BackupRecord
		at  time.Time
	}
	var out []done
	for _, r := range recs {
		v := r.Value
		if v.Cluster != clusterID || v.Method != dbbackup.MethodPhysical || len(v.Objects) == 0 || !v.DeletedAt.IsZero() {
			continue
		}
		op, ok, err := Get[OperationRecord](b.store(), CollOperations, v.OpID)
		if err != nil {
			return nil, err
		}
		if !ok || op.Value.View == nil || op.Value.View.Status != runner.StatusSucceeded {
			continue
		}
		out = append(out, done{v, op.Value.EndedAt})
	}
	slices.SortFunc(out, func(a, b done) int { return b.at.Compare(a.at) })
	recs2 := make([]BackupRecord, len(out))
	for i, d := range out {
		recs2[i] = d.rec
	}
	return recs2, nil
}

// pitrChoice is the outcome of looking for a restore base for a time.
type pitrChoice struct {
	rec      BackupRecord
	base     dbbackup.Manifest
	archives []dbbackup.Manifest
	segments int
	coverage dbbackup.BinlogCoverage
	// err is why the time is not covered (wraps dbbackup.ErrPITRNotCovered),
	// nil when the archive reaches it.
	err error
}

func notCoveredf(f string, a ...any) error {
	return fmt.Errorf("%w: %s", dbbackup.ErrPITRNotCovered, fmt.Sprintf(f, a...))
}

// pitrChoice picks the newest backup that ended before at and whose archive
// reaches at. When none does, the outcome describes the newest backup before
// at, so the reason names what is missing there.
func (b *Backups) pitrChoice(ctx context.Context, clusterID string, at time.Time) (pitrChoice, error) {
	if b.Backend == nil {
		return pitrChoice{}, fmt.Errorf("%w: backup backend required", ErrSettingsIncomplete)
	}
	cands, err := b.completedPhysical(clusterID)
	if err != nil {
		return pitrChoice{}, err
	}
	var first *pitrChoice
	for _, rec := range cands[:min(len(cands), maxPITRCandidates)] {
		m, err := b.manifest(ctx, rec.Objects[0])
		if errors.Is(err, api.ErrNotFound) {
			continue // retention removed it; its record is marked on the next pass
		}
		if err != nil {
			return pitrChoice{}, err
		}
		if m.Finished.After(at) {
			continue
		}
		c := pitrChoice{rec: rec, base: m}
		if m.BinlogBase == nil {
			c.err = notCoveredf("backup %s has no binlog position (the binary log was off when it ran; create the cluster with binlog)", rec.ID)
		} else {
			if c.archives, err = dbbackup.ListBinlogArchives(ctx, b.Backend, m.ClusterID, m.BackupID); err != nil {
				return pitrChoice{}, err
			}
			if c.coverage, c.err = dbbackup.CoverageOf(m, c.archives); c.err == nil {
				var segs []dbbackup.Manifest
				segs, c.err = dbbackup.ResolvePITR(m, c.archives, at)
				c.segments = len(segs)
			}
		}
		if c.err == nil {
			return c, nil
		}
		if first == nil {
			first = &c
		}
	}
	if first != nil {
		return *first, nil
	}
	return pitrChoice{err: notCoveredf("no completed backup ended before %s", at.UTC().Format(time.RFC3339))}, nil
}

// restoreBase returns the backup an inspection restore starts from and, for
// a point-in-time restore, the replay request. A time the archive does not
// cover is an error that names the reason.
func (b *Backups) restoreBase(ctx context.Context, clusterID string, in api.InspectionInput) (BackupRecord, dbbackup.Manifest, *dbbackup.PITRRequest, error) {
	if in.PointInTime == nil {
		br, err := b.chooseBackup(clusterID, in.Backup)
		if err != nil {
			return BackupRecord{}, dbbackup.Manifest{}, nil, err
		}
		m, err := b.manifest(ctx, br.Objects[0])
		return br, m, nil, err
	}
	ch, err := b.pitrChoice(ctx, clusterID, *in.PointInTime)
	if err == nil {
		err = ch.err
	}
	if err != nil {
		return BackupRecord{}, dbbackup.Manifest{}, nil, err
	}
	return ch.rec, ch.base, &dbbackup.PITRRequest{At: *in.PointInTime, Archives: ch.archives}, nil
}

// ProposePointInTime implements api.PointInTimeRestores: whether the cluster
// can be restored to at, and from which backup. Read-only; it needs the
// read permission on the cluster's backups.
func (b *Backups) ProposePointInTime(ctx context.Context, actor authz.Subject, clusterID string, at time.Time) (api.PointInTimeProposal, error) {
	c, err := b.clusterRecord(clusterID)
	if err != nil {
		return api.PointInTimeProposal{}, MapCommandError(err)
	}
	if err := b.Ops.authorize(actor, authz.ActionRead, backupRes(c.Swarm, c.ID)); err != nil {
		return api.PointInTimeProposal{}, MapCommandError(err)
	}
	ch, err := b.pitrChoice(ctx, c.ID, at)
	if err != nil {
		return api.PointInTimeProposal{}, MapCommandError(err)
	}
	p := api.PointInTimeProposal{Cluster: c.ID, At: at, ApprovalRequired: true}
	if ch.rec.ID != "" {
		p.Backup, p.BackupFinishedAt = ch.rec.ID, ch.base.Finished
	}
	if !ch.coverage.From.IsZero() {
		p.Coverage = &api.PITRCoverage{Segments: ch.coverage.Segments, From: ch.coverage.From, Until: ch.coverage.Until, Gap: ch.coverage.Gap}
	}
	switch {
	case ch.err == nil:
		p.Covered, p.Segments = true, ch.segments
	case errors.Is(ch.err, dbbackup.ErrPITRNotCovered):
		p.Reason = strings.TrimPrefix(ch.err.Error(), dbbackup.ErrPITRNotCovered.Error()+": ")
	default:
		return api.PointInTimeProposal{}, MapCommandError(ch.err)
	}
	return p, nil
}

// StartBinlogArchive implements api.PointInTimeRestores: the archive job for
// the newest completed backup with a binlog position (backup permission,
// audited).
func (b *Backups) StartBinlogArchive(ctx context.Context, actor authz.Subject, clusterID string) (api.OperationRef, error) {
	rec, err := b.startBinlogArchive(ctx, actor, clusterID)
	if err != nil {
		return api.OperationRef{}, MapCommandError(err)
	}
	return api.OperationRef{ID: rec.ID, Kind: rec.Kind}, nil
}

func (b *Backups) startBinlogArchive(ctx context.Context, actor authz.Subject, clusterID string) (OperationRecord, error) {
	l, err := b.Ops.leadership()
	if err != nil {
		return OperationRecord{}, err
	}
	c, err := b.clusterRecord(clusterID)
	if err != nil {
		return OperationRecord{}, err
	}
	if err := b.Ops.authorize(actor, authz.ActionBackup, backupRes(c.Swarm, c.ID)); err != nil {
		return OperationRecord{}, err
	}
	st, err := b.Ops.settings()
	if err != nil {
		return OperationRecord{}, err
	}
	if err := st.Backup.backupReady(); err != nil {
		return OperationRecord{}, err
	}
	if b.Backend == nil {
		return OperationRecord{}, fmt.Errorf("%w: backup backend required", ErrSettingsIncomplete)
	}
	if busy, err := b.Ops.unfinishedFor(c.ID); err != nil {
		return OperationRecord{}, err
	} else if busy != "" {
		return OperationRecord{}, fmt.Errorf("%w: operation %s is not finished", runner.ErrBusy, busy)
	}
	cands, err := b.completedPhysical(c.ID)
	if err != nil {
		return OperationRecord{}, err
	}
	var base dbbackup.Manifest
	for _, r := range cands[:min(len(cands), maxPITRCandidates)] {
		m, err := b.manifest(ctx, r.Objects[0])
		if errors.Is(err, api.ErrNotFound) {
			continue
		}
		if err != nil {
			return OperationRecord{}, err
		}
		if m.BinlogBase != nil {
			base = m
			break
		}
	}
	if base.BackupID == "" {
		return OperationRecord{}, fmt.Errorf("%w: no completed backup with a binlog position (create the cluster with binlog and take a backup)", dbbackup.ErrNoSource)
	}
	nodes, err := b.nodeStates(c)
	if err != nil {
		return OperationRecord{}, err
	}
	svcHost, err := b.Ops.serviceHostFor(c.Swarm)
	if err != nil {
		return OperationRecord{}, err
	}
	opID, now := newID("op"), b.now()
	op, src, err := dbbackup.BinlogArchivePlan(dbbackup.BinlogArchiveRequest{
		OpID: opID, Base: base, InstallationID: st.InstallationID, ClusterName: c.Name, JobImage: st.Backup.JobImage,
		Network: c.Spec.Network, DBCredentialSecret: st.Backup.DBCredentialSecret, BackendSecret: st.Backup.BackendSecret,
		RecipientKey: st.Backup.RecipientKey, VolumeBase: st.VolumeBase, Job: st.Backup.job(), ClusterSize: c.Size,
		StepTimeout: b.Ops.StepTimeout, MaxRetries: b.Ops.MaxRetries, ServiceHost: svcHost,
	}, nodes)
	if err != nil {
		return OperationRecord{}, err
	}
	if err := b.Ops.allowReferenced(&op, c.Swarm, st.InstallationID, []string{src.HostID, svcHost},
		objectRefs{Networks: []string{c.Spec.Network}, Secrets: []string{st.Backup.DBCredentialSecret, st.Backup.BackendSecret}}); err != nil {
		return OperationRecord{}, err
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: c.Swarm, Cluster: c.ID, RequestedBy: actor.ID, RequestedAt: now, Definition: op, Nodes: c.Nodes}
	if err := b.commit(rec); err != nil {
		return OperationRecord{}, err
	}
	b.Ops.Svc.recordEvent(c.Swarm, c.ID, api.SeverityInfo, fmt.Sprintf("%s started the binlog archive of %s for backup %s from %s, operation %s",
		actor.ID, c.Name, base.BackupID, src.Slot, opID))
	b.Ops.start(l, op, nil)
	return rec, nil
}

// binlogSets are the archive objects of the given full backups as retention
// sets, so retention deletes them together with their base. They are found
// by name, not by manifest: a damaged archive must not keep its base alive.
func (b *Backups) binlogSets(ctx context.Context, clusterID string, bases []dbbackup.BackupSet) ([]dbbackup.BackupSet, error) {
	var out []dbbackup.BackupSet
	for _, base := range bases {
		objs, err := b.Backend.List(ctx, dbbackup.BinlogArchivePrefix(clusterID, base.BackupID))
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, o := range objs {
			name := strings.TrimSuffix(o.Name, ".manifest.json")
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, dbbackup.BackupSet{BackupID: name, BaseBackupID: base.BackupID, ClusterID: clusterID, Objects: []string{name}})
		}
	}
	return out, nil
}
