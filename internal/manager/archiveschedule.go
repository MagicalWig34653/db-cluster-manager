// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
)

// RunArchives evaluates the binary log archive schedules (policy field
// Archive) and starts the archive job of every due cluster as actor. A due
// time is recorded as handled before the job starts, whether or not it
// starts, so a refusal (no backup with a binlog position yet, another
// operation unfinished) records a warning event and is retried only at the
// next scheduled time. A missed time is skipped, not caught up: one archive
// run copies everything closed so far anyway.
func (b *Backups) RunArchives(ctx context.Context, actor authz.Subject) {
	pols, err := List[BackupPolicyRecord](b.store(), CollBackupPolicies, Consistent())
	if err != nil {
		return
	}
	now := b.now()
	for _, p := range pols {
		rec := p.Value
		ap := rec.Policy.Archive
		if ap == nil || !ap.Enabled {
			continue
		}
		sp, err := schedulePolicy(api.BackupPolicy{Schedule: ap.Schedule, Timezone: ap.Timezone, Missed: string(dbbackup.MissedSkip)})
		if err != nil {
			continue
		}
		d, err := sp.Evaluate(rec.ArchiveLast, now)
		if err != nil || d.ScheduledFor.IsZero() {
			continue
		}
		rec.ArchiveLast = d.ScheduledFor
		if _, err := Put(b.store(), CollBackupPolicies, p.Key, rec, IfVersion(p.Version)); err != nil {
			continue
		}
		if !d.Run {
			continue
		}
		c, _, _ := Get[ClusterRecord](b.store(), CollClusters, rec.Cluster)
		if _, err := b.startBinlogArchive(ctx, actor, rec.Cluster); err != nil {
			b.Ops.Svc.recordEvent(c.Value.Swarm, rec.Cluster, api.SeverityWarning, fmt.Sprintf("scheduled binary log archive of %s not started: %v", c.Value.Name, err))
		}
	}
}
