// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Cluster deletion (also the discard of a failed create).
//
// Lab E4 found that a failed create kept its cluster record, so its name and
// planned capacity stayed taken and its half-created services, jobs and
// volumes stayed behind. DeleteCluster removes, through the agents and
// their ownership guard, exactly the objects the installation created for
// the cluster:
//
//   - each node's database service, found by name and checked against the
//     cluster label on the agent (remove-cluster-service; missing counts as
//     removed, so no recorded service ID is needed after a failed create);
//   - the job services of backups that did not finish (remove-job);
//   - the inspection instances of the cluster that were not torn down,
//     with their manager-created init secrets (as in the expiry teardown);
//   - each node volume, including the new volume of an unfinished
//     replacement, after the agent verified its identity marker; a volume
//     that was never created or never got its marker (and holds no file)
//     counts as removed; Docker's "volume in use" while a task stops is
//     retried (RetryCodes).
//
// Operator-provided objects (the network, the root password, TLS, backup
// and account secrets) are never removed. Docker removes a local bind
// volume without deleting its host directory, so the data directory below
// the volume base stays on the host. Backups in the backend are kept.
//
// The deletion is destructive: it needs the delete permission on the
// cluster (only the administrator preset grants it), and every removal
// waits for an approval bound to the digest of the planned definition,
// like a restore. When the operation succeeded, the cluster record and its
// backup policy are deleted, which releases the name and the capacity.

// OpClusterDelete is the operation kind of a cluster deletion.
const OpClusterDelete = api.OperationKindClusterDelete

// ClusterDeletion is a started deletion that waits for its approval.
type ClusterDeletion struct {
	Operation OperationRecord
	Digest    string
}

// DeleteCluster plans the deletion, stores it with its approval digest and
// starts it; the operation pauses before its first removal until the
// approval is recorded (POST /operations/{id}/approve). Failed operations
// of the cluster are aborted first so they release their locks; an
// unfinished one refuses the deletion (busy).
func (o *Operations) DeleteCluster(actor authz.Subject, clusterID string) (ClusterDeletion, error) {
	l, err := o.leadership()
	if err != nil {
		return ClusterDeletion{}, err
	}
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, clusterID, Consistent())
	if err != nil {
		return ClusterDeletion{}, err
	}
	if !ok {
		return ClusterDeletion{}, fmt.Errorf("%w: cluster %q", api.ErrNotFound, clusterID)
	}
	c := cur.Value
	if err := o.authorize(actor, authz.ActionDelete, clusterRes(c.Swarm, c.ID)); err != nil {
		return ClusterDeletion{}, err
	}
	st, err := o.settings()
	if err != nil {
		return ClusterDeletion{}, err
	}
	ops, err := List[OperationRecord](o.Svc.store, CollOperations, Consistent())
	if err != nil {
		return ClusterDeletion{}, err
	}
	var failed []string
	var clusterOps []OperationRecord
	for _, r := range ops {
		v := r.Value
		if v.Cluster != c.ID {
			continue
		}
		clusterOps = append(clusterOps, v)
		switch {
		case v.EndedAt.IsZero() && v.View != nil && v.View.Status == runner.StatusStopped && len(v.View.Steps) == 0:
			// never started
		case v.EndedAt.IsZero():
			return ClusterDeletion{}, fmt.Errorf("%w: operation %s is not finished; abort it first", runner.ErrBusy, v.ID)
		case v.View != nil && v.View.Status == runner.StatusFailed:
			failed = append(failed, v.ID)
		}
	}
	svcHost, err := o.serviceHostFor(c.Swarm)
	if err != nil {
		return ClusterDeletion{}, err
	}
	opID, approvalID := newID("op"), newID("apr")
	op, sessions, err := o.planClusterDelete(opID, c, st, svcHost, clusterOps)
	if err != nil {
		return ClusterDeletion{}, err
	}
	requireApproval(&op, approvalID)
	if err := op.Validate(); err != nil {
		return ClusterDeletion{}, err
	}
	digest, err := DefinitionDigest(op)
	if err != nil {
		return ClusterDeletion{}, err
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: c.Swarm, Cluster: c.ID, RequestedBy: actor.ID,
		RequestedAt: o.Svc.cfg.Now(), Definition: op, Nodes: c.Nodes, ApprovalID: approvalID, ApprovalDigest: digest}
	opDoc, err := PutOp(CollOperations, opID, rec, IfVersion(0))
	if err != nil {
		return ClusterDeletion{}, err
	}
	marked := c
	marked.DeleteOp = opID
	clDoc, err := PutOp(CollClusters, c.ID, marked, IfVersion(cur.Version))
	if err != nil {
		return ClusterDeletion{}, err
	}
	docs := []metastore.DocOp{opDoc, clDoc}
	for _, s := range sessions {
		ir := s.Value
		ir.TeardownOp = opID
		d, err := PutOp(CollInspections, s.Key, ir, IfVersion(s.Version))
		if err != nil {
			return ClusterDeletion{}, err
		}
		docs = append(docs, d)
	}
	if _, err := o.Svc.store.Txn(docs...); err != nil {
		return ClusterDeletion{}, err
	}
	// Failed operations keep their runner locks until aborted (lab E4 F10);
	// the deletion needs the cluster lock.
	for _, id := range failed {
		if err := l.Runner.Abort(id); err != nil && !errors.Is(err, runner.ErrUnknownOperation) && !errors.Is(err, runner.ErrFinished) {
			o.Svc.log.Warn("manager: failed operation not aborted before the deletion", "op", id, "err", err)
		} else if v, err := l.Runner.Snapshot(id); err == nil {
			_ = o.Svc.saveView(v)
		}
	}
	o.Svc.recordEvent(c.Swarm, c.ID, api.SeverityWarning, fmt.Sprintf("%s requested deletion of cluster %s, operation %s (waits for approval)",
		actor.ID, c.Name, opID))
	// The cluster record is deleted when the operation is first stored as
	// succeeded (Service.saveView -> finishClusterDelete).
	o.start(l, op, nil)
	return ClusterDeletion{Operation: rec, Digest: digest}, nil
}

// finishClusterDelete deletes the cluster record and its backup policy
// after a deletion succeeded, which releases the name and the capacity. A
// record whose DeleteOp is another operation is kept.
func (s *Service) finishClusterDelete(rec OperationRecord) {
	cur, ok, err := Get[ClusterRecord](s.store, CollClusters, rec.Cluster, Consistent())
	if err != nil || !ok {
		return
	}
	if cur.Value.DeleteOp != rec.ID {
		s.log.Warn("manager: cluster record not deleted: it names another deletion", "cluster", rec.Cluster, "op", rec.ID)
		return
	}
	if err := Delete(s.store, CollClusters, rec.Cluster, IfVersion(cur.Version)); err != nil {
		s.log.Warn("manager: cluster record not deleted after the deletion operation", "cluster", rec.Cluster, "err", err)
		return
	}
	_ = Delete(s.store, CollBackupPolicies, rec.Cluster, nil)
	s.recordEvent(rec.Swarm, rec.Cluster, api.SeverityWarning, fmt.Sprintf("cluster %s deleted; name and capacity released (operation %s)",
		cur.Value.Name, rec.ID))
}

// requireApproval adds the approval condition to every destructive command
// step (every kind that is not idempotent).
func requireApproval(op *runner.Operation, approvalID string) {
	for i := range op.Steps {
		if s := op.Steps[i]; s.Command != "" && !runner.Idempotent(s.Command) {
			op.Steps[i].Preconditions = append(op.Steps[i].Preconditions, runner.Condition{Kind: dbbackup.CondApproved, Target: approvalID})
		}
	}
}

// planClusterDelete builds the deletion steps (see the package comment
// above) and returns the inspection sessions it tears down.
func (o *Operations) planClusterDelete(opID string, c ClusterRecord, st Settings, svcHost string, clusterOps []OperationRecord) (runner.Operation, []Versioned[InspectionRecord], error) {
	var steps []runner.Step
	add := func(name, target string, kind agent.Kind, args any, retry ...agent.Code) error {
		raw, err := json.Marshal(args)
		if err != nil {
			return err
		}
		steps = append(steps, runner.Step{ID: fmt.Sprintf("%02d-%s", len(steps)+1, name), Target: target, Command: kind, Args: raw,
			Timeout: o.StepTimeout, MaxRetries: o.MaxRetries, RetryCodes: retry})
		return nil
	}
	svc := func(host string) string {
		if svcHost != "" {
			return svcHost
		}
		return host
	}
	hostOf := map[string]string{}
	for _, n := range c.Nodes {
		hostOf[n.Slot] = n.HostID
	}
	// Database services first: their tasks release the volumes.
	nodes := slices.Clone(c.Nodes)
	slices.SortFunc(nodes, func(a, b galera.Node) int { return strings.Compare(a.Slot, b.Slot) })
	for _, n := range nodes {
		if err := add("remove-service-"+n.Slot, svc(n.HostID), agent.KindRemoveClusterService,
			agent.RemoveClusterServiceArgs{ServiceName: galera.ServiceName(c.Name, n.Slot), ClusterID: c.ID}); err != nil {
			return runner.Operation{}, nil, err
		}
	}
	// Jobs of backups that did not succeed (a succeeded backup removed its
	// job as its last step).
	succeeded := map[string]bool{}
	for _, op := range clusterOps {
		succeeded[op.ID] = op.View != nil && op.View.Status == runner.StatusSucceeded
	}
	backups, err := List[BackupRecord](o.Svc.store, CollBackups, Consistent())
	if err != nil {
		return runner.Operation{}, nil, err
	}
	for _, b := range backups {
		br := b.Value
		if br.Cluster != c.ID || succeeded[br.OpID] || br.Method != dbbackup.MethodPhysical {
			continue
		}
		if err := add("remove-job-"+br.ID, svc(hostOf[br.Source]), agent.KindRemoveJob,
			agent.RemoveJobArgs{JobName: dbbackup.JobName(br.ID, "")}); err != nil {
			return runner.Operation{}, nil, err
		}
	}
	// Inspection instances of the cluster that were not torn down.
	locks := []string{"cluster/" + c.ID}
	insp, err := List[InspectionRecord](o.Svc.store, CollInspections, Consistent())
	if err != nil {
		return runner.Operation{}, nil, err
	}
	var sessions []Versioned[InspectionRecord]
	bk := &Backups{Ops: o}
	for _, r := range insp {
		ir := r.Value
		if ir.Cluster != c.ID || o.tornDown(ir.TeardownOp) {
			continue
		}
		var view *runner.View
		if op, ok, err := Get[OperationRecord](o.Svc.store, CollOperations, ir.OpID); err == nil && ok {
			view = op.Value.View
		}
		td, err := bk.teardownPlan(opID, ir, view, st)
		if err != nil {
			return runner.Operation{}, nil, fmt.Errorf("inspection %s: %w", ir.Session.ID, err)
		}
		prefix := fmt.Sprintf("%02d-%s-", len(steps)+1, ir.Session.ID)
		for _, s := range td.Steps {
			s.ID = prefix + s.ID
			for i, pc := range s.Preconditions {
				if pc.Kind == runner.CondStepSucceeded {
					s.Preconditions[i].StepID = prefix + pc.StepID
				}
			}
			steps = append(steps, s)
		}
		locks = append(locks, td.Locks...)
		sessions = append(sessions, r)
	}
	// Node volumes last, including volumes planned by replacements that
	// did not finish (same slot, new identity).
	vols := map[string]bool{}
	volNodes := nodes
	for _, op := range clusterOps {
		volNodes = append(volNodes, op.Nodes...)
	}
	for _, n := range volNodes {
		if n.Volume == "" || vols[n.Volume] {
			continue
		}
		vols[n.Volume] = true
		if err := add("remove-volume-"+n.Slot+"-"+n.NodeUUID[:min(8, len(n.NodeUUID))], n.HostID, agent.KindRemoveVolume, agent.RemoveVolumeArgs{
			VolumeID: n.Volume, VolumeRoot: filepath.Join(st.VolumeBase, n.Volume), MissingOK: true, AllowUnmarked: true,
			Expected: agent.Identity{InstallationID: st.InstallationID, ClusterID: c.ID, NodeUUID: n.NodeUUID},
		}, agent.CodeVolumeInUse); err != nil {
			return runner.Operation{}, nil, err
		}
	}
	op := runner.Operation{ID: opID, Kind: OpClusterDelete, Locks: locks, Steps: steps}
	return op, sessions, op.Validate()
}

// endedUnsuccessfully reports whether an operation ended failed or
// aborted.
func (o *Operations) endedUnsuccessfully(opID string) bool {
	r, ok, err := Get[OperationRecord](o.Svc.store, CollOperations, opID)
	return err == nil && ok && !r.Value.EndedAt.IsZero() && r.Value.View != nil && r.Value.View.Status != runner.StatusSucceeded
}

// tornDown reports whether a teardown operation exists and succeeded.
func (o *Operations) tornDown(opID string) bool {
	if opID == "" {
		return false
	}
	r, ok, err := Get[OperationRecord](o.Svc.store, CollOperations, opID)
	return err == nil && ok && r.Value.View != nil && r.Value.View.Status == runner.StatusSucceeded
}
