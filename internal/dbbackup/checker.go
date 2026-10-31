// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// JobStatus is the report a job writes to the backend when it ends, as
// StatusName(key): key is the object for backup jobs and the job name for
// restore jobs. A backup is complete only when its manifest exists; the
// status lets a gate stop waiting for a failed job.
type JobStatus struct {
	Job       string    `json:"job"`
	BackupID  string    `json:"backup_id"`
	Succeeded bool      `json:"succeeded"`
	Error     string    `json:"error,omitempty"`
	Finished  time.Time `json:"finished"`
	// StreamVerified: every chunk digest and the totals matched the manifest.
	StreamVerified bool `json:"stream_verified,omitempty"`
}

// ApprovalStore looks up explicit approvals.
type ApprovalStore interface {
	Approval(id string) (Approval, bool)
}

// Checker evaluates the gate conditions of this package. Conditions of
// other kinds (node-synced, quorum, ...) belong to the cluster checker the
// manager composes with this one; Checker reports them as unmet.
type Checker struct {
	Backend backup.Backend
	// Nodes returns the observed node states of a cluster (CondSourceEligible).
	Nodes     func(ctx context.Context) ([]NodeState, error)
	Policy    SourcePolicy
	Approvals ApprovalStore
	// SessionReport returns the verification report of a session.
	SessionReport func(sessionID string) (VerifyReport, bool)
}

var _ runner.Checker = (*Checker)(nil)

const maxMetaObject = 16 << 20

func (c *Checker) readJSON(ctx context.Context, name string, v any) (bool, error) {
	rc, err := c.Backend.Get(ctx, name)
	if errors.Is(err, backup.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxMetaObject))
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

// Check implements runner.Checker.
func (c *Checker) Check(ctx context.Context, _ string, _ runner.Step, cond runner.Condition) (bool, string, error) {
	switch cond.Kind {
	case CondSourceEligible:
		if c.Nodes == nil {
			return false, "no node observer", nil
		}
		nodes, err := c.Nodes(ctx)
		if err != nil {
			return false, "", err
		}
		p := c.Policy
		if cond.ClusterSize > 0 {
			p.ClusterSize = cond.ClusterSize
		}
		if err := CheckSource(nodes, cond.Target, p); err != nil {
			return false, err.Error(), nil
		}
		return true, "", nil
	case CondBackupComplete:
		return c.backupComplete(ctx, cond.Target)
	case CondRestoreComplete:
		var st JobStatus
		ok, err := c.readJSON(ctx, StatusName(cond.Target), &st)
		switch {
		case err != nil:
			return false, "", err
		case !ok:
			return false, "restore job has not reported yet", nil
		case !st.Succeeded:
			return false, "restore job failed: " + st.Error, runner.ErrConditionFatal
		case !st.StreamVerified:
			return false, "restore job did not verify the stream digests", runner.ErrConditionFatal
		}
		return true, "", nil
	case CondApproved:
		if c.Approvals == nil {
			return false, "no approval store", nil
		}
		a, ok := c.Approvals.Approval(cond.Target)
		if !ok || a.ID != cond.Target || a.By == "" {
			return false, "approval " + cond.Target + " not recorded", nil
		}
		return true, "", nil
	case CondRestoreVerified:
		if c.SessionReport == nil {
			return false, "no verification source", nil
		}
		r, ok := c.SessionReport(cond.Target)
		switch {
		case !ok:
			return false, "not verified yet", nil
		case !r.Passed:
			return false, "verification failed", nil
		}
		return true, "", nil
	}
	return false, fmt.Sprintf("condition %s is not handled by dbbackup", cond.Kind), nil
}

func (c *Checker) backupComplete(ctx context.Context, object string) (bool, string, error) {
	var st JobStatus
	if ok, err := c.readJSON(ctx, StatusName(object), &st); err != nil {
		return false, "", err
	} else if ok && !st.Succeeded {
		return false, "backup job failed: " + st.Error, runner.ErrConditionFatal
	}
	rc, err := c.Backend.Get(ctx, ManifestName(object))
	if errors.Is(err, backup.ErrNotFound) {
		return false, "manifest not written yet", nil
	}
	if err != nil {
		return false, "", err
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxMetaObject))
	_ = rc.Close()
	if err != nil {
		return false, "", err
	}
	m, err := DecodeManifest(data)
	if err != nil {
		return false, err.Error(), nil
	}
	if m.Object != object {
		return false, fmt.Sprintf("manifest names object %s", m.Object), nil
	}
	objs, err := c.Backend.List(ctx, object)
	if err != nil {
		return false, "", err
	}
	for _, o := range objs {
		if o.Name == object {
			if o.Size != m.Stored.Size {
				return false, fmt.Sprintf("stored object has %d bytes, manifest %d", o.Size, m.Stored.Size), nil
			}
			return true, "", nil
		}
	}
	return false, "manifest present but stream object missing", nil
}
