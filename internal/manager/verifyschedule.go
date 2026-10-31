// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Scheduled restore verification (B04).
//
// A backup that was never restored is a hope, not a backup. A policy with
// Verify.Enabled makes the leader restore the newest completed physical
// backup into an isolated inspection instance on the schedule (the same
// plan, operation and agent commands as a manual inspection: wsrep off,
// separate network, production accounts locked, expiry), let the
// restore-verified gate probe it, and tear it down again at once.
//
// Approval: enabling the verification needs the restore permission (the
// manager checks it when the policy is stored) and is the explicit
// approval of every scheduled inspection of that policy; the approval
// record names the policy and the person. Scheduled runs never restore
// into a production identity, name, network or volume (the plan refuses),
// never promote anything and never touch the cluster. A full restore into
// a new cluster is not scheduled and stays an approval-gated person's
// decision.
//
// A verification that fails (the gate records a failed report), ends
// without success, exceeds MaxMinutes, or cannot be started records an
// event and sends a notification of KindRestoreVerifyFailed. One run per
// cluster at a time; a run that does not end in time is aborted and its
// instance is torn down by TeardownExpired.

// CollVerifyRuns holds VerifyRunRecord values; key: cluster ID. Only the
// newest run of a cluster is kept.
const CollVerifyRuns = "restore-verify-runs"

// KindRestoreVerifyFailed is the notification kind of a failed scheduled
// restore verification; KindRestoreVerifyPassed is informational.
const (
	KindRestoreVerifyFailed = "backup.restore_verification_failed"
	KindRestoreVerifyPassed = "backup.restore_verification_passed"
)

const (
	defaultVerifyMinutes = 60
	maxVerifyMinutes     = 240
)

// Run states of a scheduled verification.
const (
	VerifyRunning = "running"
	VerifyPassed  = "passed"
	VerifyFailed  = "failed"
)

// VerifyRunRecord is the newest scheduled verification of a cluster.
type VerifyRunRecord struct {
	Cluster      string    `json:"cluster"`
	Swarm        string    `json:"swarm"`
	Backup       string    `json:"backup,omitempty"`
	Session      string    `json:"session,omitempty"`
	OpID         string    `json:"opId,omitempty"`
	ScheduledFor time.Time `json:"scheduledFor"`
	StartedAt    time.Time `json:"startedAt"`
	Deadline     time.Time `json:"deadline"`
	Status       string    `json:"status"`
	Reason       string    `json:"reason,omitempty"`
	FinishedAt   time.Time `json:"finishedAt,omitzero"`
}

func (b *Backups) verifyEvent(ctx context.Context, run VerifyRunRecord, name string, passed bool, reason string) {
	sev, nsev, kind := api.SeverityWarning, notify.SeverityCritical, KindRestoreVerifyFailed
	// A run that never started has no backup yet (lab E14: the text showed an empty ID).
	what := "a backup"
	if run.Backup != "" {
		what = "backup " + run.Backup
	}
	msg := fmt.Sprintf("scheduled restore verification of %s of cluster %s failed: %s", what, name, reason)
	if passed {
		sev, nsev, kind = api.SeverityInfo, notify.SeverityInfo, KindRestoreVerifyPassed
		msg = fmt.Sprintf("scheduled restore verification of %s of cluster %s passed", what, name)
	}
	b.Ops.Svc.recordEvent(run.Swarm, run.Cluster, sev, msg)
	b.notify(ctx, nsev, kind, "swarm/"+run.Swarm+"/cluster/"+run.Cluster, msg,
		map[string]string{"swarm": run.Swarm, "cluster": run.Cluster, "backup": run.Backup, "session": run.Session, "op": run.OpID})
}

// finishVerify stores the outcome, announces it and lets the inspection
// instance expire now, so TeardownExpired removes it on this pass or the
// next.
func (b *Backups) finishVerify(ctx context.Context, key string, version uint64, run VerifyRunRecord, status, reason string) {
	run.Status, run.Reason, run.FinishedAt = status, reason, b.now()
	if _, err := Put(b.store(), CollVerifyRuns, key, run, IfVersion(version)); err != nil {
		return // another pass or leader handled it
	}
	name := run.Cluster
	if c, ok, _ := Get[ClusterRecord](b.store(), CollClusters, run.Cluster); ok {
		name = c.Value.Name
	}
	b.verifyEvent(ctx, run, name, status == VerifyPassed, reason)
	if run.Session != "" {
		_, _, _ = Update(b.store(), CollInspections, run.Session, func(ir InspectionRecord, exists bool) (InspectionRecord, error) {
			if exists && ir.Session.ExpiresAt.After(b.now()) {
				ir.Session.ExpiresAt = b.now()
			}
			return ir, nil
		})
	}
}

// checkRun advances a running verification. It returns true while the run
// is still running.
func (b *Backups) checkRun(ctx context.Context, l *Leadership, v Versioned[VerifyRunRecord]) bool {
	run := v.Value
	if run.Status != VerifyRunning {
		return false
	}
	if rep, ok, err := Get[dbbackup.VerifyReport](b.store(), CollVerifications, run.Session, Consistent()); err == nil && ok {
		if rep.Value.Passed {
			b.finishVerify(ctx, v.Key, v.Version, run, VerifyPassed, "")
		} else {
			b.finishVerify(ctx, v.Key, v.Version, run, VerifyFailed, "the restored instance failed the checks")
		}
		return false
	}
	if op, ok, err := Get[OperationRecord](b.store(), CollOperations, run.OpID, Consistent()); err == nil {
		switch {
		case !ok:
			b.finishVerify(ctx, v.Key, v.Version, run, VerifyFailed, "the verification operation is gone")
			return false
		case !op.Value.EndedAt.IsZero() && op.Value.View != nil && op.Value.View.Status != runner.StatusSucceeded:
			b.finishVerify(ctx, v.Key, v.Version, run, VerifyFailed, fmt.Sprintf("operation %s ended %s", run.OpID, op.Value.View.Status))
			return false
		}
	}
	if b.now().After(run.Deadline) {
		_ = l.Runner.Abort(run.OpID)
		b.finishVerify(ctx, v.Key, v.Version, run, VerifyFailed, "not finished within the allowed time; aborted")
		return false
	}
	return true
}

// RunVerifications evaluates the verification schedules of all clusters
// and advances running verifications. The scheduler calls it on every pass
// of the leader; actor is the manager's system subject.
func (b *Backups) RunVerifications(ctx context.Context, actor authz.Subject) {
	l, err := b.Ops.leadership()
	if err != nil {
		return
	}
	pols, err := List[BackupPolicyRecord](b.store(), CollBackupPolicies, Consistent())
	if err != nil {
		return
	}
	now := b.now()
	for _, p := range pols {
		rec := p.Value
		vp := rec.Policy.Verify
		if vp == nil || !vp.Enabled {
			continue
		}
		if cur, ok, err := Get[VerifyRunRecord](b.store(), CollVerifyRuns, rec.Cluster, Consistent()); err == nil && ok && b.checkRun(ctx, l, cur) {
			continue // one run per cluster at a time
		}
		sp, err := schedulePolicy(api.BackupPolicy{Schedule: vp.Schedule, Timezone: vp.Timezone, Missed: string(dbbackup.MissedSkip)})
		if err != nil {
			continue
		}
		d, err := sp.Evaluate(rec.VerifyLast, now)
		if err != nil || d.ScheduledFor.IsZero() {
			continue
		}
		rec.VerifyLast = d.ScheduledFor
		if _, err := Put(b.store(), CollBackupPolicies, p.Key, rec, IfVersion(p.Version)); err != nil {
			continue
		}
		if !d.Run {
			continue
		}
		b.startVerify(ctx, actor, rec, d.ScheduledFor)
	}
}

func (b *Backups) startVerify(ctx context.Context, actor authz.Subject, rec BackupPolicyRecord, scheduled time.Time) {
	vp := rec.Policy.Verify
	c, err := b.clusterRecord(rec.Cluster)
	if err != nil {
		return
	}
	minutes := vp.MaxMinutes
	if minutes <= 0 {
		minutes = defaultVerifyMinutes
	}
	run := VerifyRunRecord{Cluster: c.ID, Swarm: c.Swarm, ScheduledFor: scheduled, StartedAt: b.now(),
		Deadline: b.now().Add(time.Duration(minutes) * time.Minute), Status: VerifyRunning}
	approver := fmt.Sprintf("scheduled verification enabled by %s", rec.UpdatedBy)
	// The inspection lives a little longer than the verification may take,
	// so the deadline (not the expiry) ends a stuck run.
	res, err := b.startInspectionAs(ctx, actor, api.InspectionInput{Cluster: c.ID, Host: vp.Host, TTLMinutes: minutes + 30}, approver)
	if err != nil {
		run.Status, run.Reason, run.FinishedAt = VerifyFailed, "not started: "+err.Error(), b.now()
		_, _ = Put(b.store(), CollVerifyRuns, c.ID, run, nil)
		b.verifyEvent(ctx, run, c.Name, false, run.Reason)
		return
	}
	run.Backup, run.Session, run.OpID = res.Backup, res.Session, res.ID
	if _, err := Put(b.store(), CollVerifyRuns, c.ID, run, nil); err != nil {
		b.log("manager: scheduled verification not recorded", "cluster", c.ID, "err", err)
	}
	b.Ops.Svc.recordEvent(c.Swarm, c.ID, api.SeverityInfo, fmt.Sprintf("scheduled restore verification of backup %s of %s started, operation %s", res.Backup, c.Name, res.ID))
}
