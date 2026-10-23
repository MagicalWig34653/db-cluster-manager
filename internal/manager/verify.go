// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
)

// Restore verification (ADR 0011 gate "restore-verified").
//
// The inspection plan ends with a gate on CondRestoreVerified. The routing
// checker evaluates it here: a stored report decides; without one, the
// instance is probed through the agent on its host (collect-health over
// the socket on the inspection volume, as the monitoring account the init
// file leaves unlocked) and the outcome is recorded as a VerifyReport of
// level "instance" (dbbackup.EvaluateInstance). An instance that does not
// answer yet records nothing and the gate polls again. Table contents are
// not compared (no catalog is captured yet), which the report's level
// states.

// restoreVerified evaluates CondRestoreVerified for an inspection session.
func (c *RoutingChecker) restoreVerified(ctx context.Context, session string) (bool, string, error) {
	if r, ok, err := Get[dbbackup.VerifyReport](c.Store, CollVerifications, session, Consistent()); err != nil {
		return false, "", err
	} else if ok {
		if r.Value.Passed {
			return true, "", nil
		}
		return false, "verification failed", nil
	}
	if c.Ops == nil || c.Backup == nil || c.Backup.Backend == nil {
		return false, "not verified yet (no backup backend to read the restore job status)", nil
	}
	ir, ok, err := Get[InspectionRecord](c.Store, CollInspections, session, Consistent())
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "no inspection session " + session, nil
	}
	sess := ir.Value.Session
	svc := c.Ops.Svc
	l := svc.Current()
	if l == nil || !l.Valid() {
		return false, "", ErrNotLeader
	}
	st, _, err := Get[Settings](c.Store, CollSettings, SettingsKey, Consistent())
	if err != nil {
		return false, "", err
	}
	streamOK, err := c.restoreStreamVerified(ctx, sess.JobName)
	if err != nil {
		return false, "", err
	}
	sock := st.Value.MonitorSocket
	if sock == "" {
		sock = DefaultMonitorSocket
	}
	if st.Value.VolumeBase == "" || !filepath.IsLocal(sock) {
		return false, "no socket path to probe the instance (settings)", nil
	}
	raw, err := json.Marshal(agent.CollectHealthArgs{Socket: filepath.Join(st.Value.VolumeBase, sess.Volume, sock)})
	if err != nil {
		return false, "", err
	}
	res, err := l.Agents.Send(ctx, sess.HostID, agent.Command{Term: l.Term, OpID: healthOpID, StepID: newID("v"),
		Kind: agent.KindCollectHealth, Args: raw})
	if err != nil {
		return false, "inspection instance not probed: " + err.Error(), nil
	}
	switch {
	case res.Status == agent.StatusSucceeded:
	case res.Code == agent.CodeDBUnavailable:
		return false, "inspection instance does not answer yet: " + res.Message, nil
	default:
		return false, fmt.Sprintf("agent answered %s/%s: %s", res.Status, res.Code, res.Message), nil
	}
	var out agent.HealthOutput
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return false, "", fmt.Errorf("decode health output: %w", err)
	}
	report := dbbackup.EvaluateInstance(dbbackup.Manifest{BackupID: sess.BackupID, ClusterID: ir.Value.Cluster}, session,
		dbbackup.InstanceProbe{StreamVerified: streamOK, Answered: true, Status: out.Status}, svc.cfg.Now())
	if _, err := Put(c.Store, CollVerifications, session, report, IfVersion(0)); errors.Is(err, ErrConflict) {
		return c.restoreVerified(ctx, session) // recorded meanwhile: that report decides
	} else if err != nil {
		return false, "", err
	}
	sev, word := api.SeverityInfo, "passed"
	if !report.Passed {
		sev, word = api.SeverityWarning, "failed"
	}
	svc.recordEvent(ir.Value.Swarm, ir.Value.Cluster, sev, fmt.Sprintf("restore verification of backup %s in inspection %s %s (level %s)",
		sess.BackupID, session, word, report.Level))
	if !report.Passed {
		return false, "verification failed", nil
	}
	return true, "", nil
}

// restoreStreamVerified reads the restore job's status report.
func (c *RoutingChecker) restoreStreamVerified(ctx context.Context, job string) (bool, error) {
	rc, err := c.Backup.Backend.Get(ctx, dbbackup.StatusName(job))
	if errors.Is(err, backup.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return false, err
	}
	var st dbbackup.JobStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return false, nil
	}
	return st.Succeeded && st.StreamVerified, nil
}
