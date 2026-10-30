// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Guided full-outage recovery (A06).
//
// The manager never starts it by itself. A user requests it for the
// bootstrap candidate the read-only diagnosis proposed; the manager
// collects the evidence again, refuses unless the fresh diagnosis proposes
// the same candidate, and stores the plan as an operation whose first step
// waits for an explicit approval bound to the digest of that exact
// definition (approvals.go). Until a user approves, nothing is stopped,
// marked or started. The agent re-checks the candidate's grastate.dat
// position before it sets safe_to_bootstrap, so evidence that changed after
// the approval stops the operation instead of bootstrapping a stale node.

// ErrCandidateMismatch: the requested bootstrap candidate is not the one
// the fresh diagnosis proposes.
var ErrCandidateMismatch = errors.New("manager: requested bootstrap candidate differs from the diagnosis")

// RecoverOutage plans the guided recovery of a cluster without a Primary
// component and starts it waiting for approval. candidate must equal the
// diagnosis candidate (the user confirms what they saw). It needs operate
// permission on the cluster; approving needs the separate recover
// permission on the cluster (api.ApprovalPermission).
func (o *Operations) RecoverOutage(ctx context.Context, actor authz.Subject, clusterID, candidate string) (OperationRecord, galera.Diagnosis, error) {
	l, err := o.leadership()
	if err != nil {
		return OperationRecord{}, galera.Diagnosis{}, err
	}
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, clusterID, Consistent())
	if err != nil {
		return OperationRecord{}, galera.Diagnosis{}, err
	}
	if !ok {
		return OperationRecord{}, galera.Diagnosis{}, fmt.Errorf("%w: cluster %q", api.ErrNotFound, clusterID)
	}
	c := cur.Value
	if err := o.authorize(actor, authz.ActionOperate, clusterRes(c.Swarm, c.ID)); err != nil {
		return OperationRecord{}, galera.Diagnosis{}, err
	}
	if busy, err := o.unfinishedFor(c.ID); err != nil {
		return OperationRecord{}, galera.Diagnosis{}, err
	} else if busy != "" {
		return OperationRecord{}, galera.Diagnosis{}, fmt.Errorf("%w: operation %s is not finished", runner.ErrBusy, busy)
	}
	st, err := o.settings()
	if err != nil {
		return OperationRecord{}, galera.Diagnosis{}, err
	}
	// Positions after a crash come from mariadbd --wsrep-recover (lab E6:
	// a SIGKILL of all nodes left seqno -1 in every grastate.dat).
	d, _, err := o.DiagnoseOutageRecovering(ctx, actor, clusterID)
	if err != nil {
		return OperationRecord{}, galera.Diagnosis{}, err
	}
	if d.Candidate == "" {
		return OperationRecord{}, d, fmt.Errorf("%w: %v", galera.ErrNoCandidate, d.Reasons)
	}
	if candidate != d.Candidate {
		return OperationRecord{}, d, fmt.Errorf("%w: requested %q, diagnosis proposes %q", ErrCandidateMismatch, candidate, d.Candidate)
	}
	for _, n := range c.Nodes {
		if c.ServiceID[n.Slot] == "" {
			return OperationRecord{}, d, fmt.Errorf("%w: %s/%s", ErrMissingServiceID, c.ID, n.Slot)
		}
	}
	plan, err := galera.RecoveryPlan(d, c.Spec, c.Nodes)
	if err != nil {
		return OperationRecord{}, d, err
	}
	opID, approvalID := newID("op"), newID("apr")
	snapshot := "pre-recovery-" + opID
	plan = plan.WithSnapshot(snapshot)
	svcHost, err := o.serviceHostFor(c.Swarm)
	if err != nil {
		return OperationRecord{}, d, err
	}
	op, err := runner.FromGaleraPlan(plan, runner.GaleraBindings{
		OpID: opID, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase,
		ServiceIDs: c.ServiceID, StepTimeout: o.StepTimeout, MaxRetries: o.MaxRetries,
		ServiceHost: svcHost, ApprovalID: approvalID,
	})
	if err != nil {
		return OperationRecord{}, d, err
	}
	// The allow-objects steps are part of the approved definition.
	if err := o.allowReferenced(&op, c.Swarm, st.InstallationID, append(nodeHosts(c.Nodes), svcHost), clusterRefs(plan.Cluster)); err != nil {
		return OperationRecord{}, d, err
	}
	digest, err := DefinitionDigest(op)
	if err != nil {
		return OperationRecord{}, d, err
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: c.Swarm, Cluster: c.ID, RequestedBy: actor.ID,
		RequestedAt: o.Svc.cfg.Now(), Definition: op, Nodes: c.Nodes, ApprovalID: approvalID, ApprovalDigest: digest, Snapshot: snapshot}
	if err := o.commit(rec, nil); err != nil {
		return OperationRecord{}, d, err
	}
	o.Svc.recordEvent(c.Swarm, c.ID, api.SeverityCritical, fmt.Sprintf("%s requested guided full-outage recovery of %s bootstrapping %s, operation %s (waits for approval)",
		actor.ID, c.Name, d.Candidate, opID))
	o.start(l, op, nil)
	return rec, d, nil
}
