// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Guided Galera recovery from the zero cluster UUID (ADR 0029, owner
// decision 2026-10-30, lab E12).
//
// Two journaled operations share one pre-recovery snapshot name:
//
//  1. galera/zero-uuid-prepare: approval, stop every node, verify identities,
//     snapshot every data directory. Nothing is written to the data.
//  2. galera/zero-uuid-recovery: planned by the ZeroUUIDReconciler from the
//     positions read on the stopped nodes (wsrep-recover): safe_to_bootstrap
//     on the unique highest-seqno node, bootstrap with a new state UUID, the
//     others rejoin by SST, steady-state arguments, state check.
//
// Both wait for the digest-bound approval of the existing approval flow
// (administrator, recover permission) unless the cluster's autoRecover
// policy approves: the prepare phase loses nothing (stop and copy), the
// recovery phase only when every position is readable and the highest
// seqno is unique. Every run notifies before and after.

// Notification kinds of the zero-UUID recovery.
const (
	KindRecoveryPlanned   = "recovery.planned"
	KindRecoveryNeedsHand = "recovery.needs_approval"
	KindRecoveryEnded     = "recovery.ended"
)

// zeroUUIDCooldown stops the autoRecover policy from starting the next run
// right after a failed one.
const zeroUUIDCooldown = time.Hour

// AutoApprover is the approver recorded for the autoRecover policy.
const AutoApprover = "policy:autoRecover"

func isZeroUUIDKind(k string) bool {
	return k == api.OperationKindZeroUUIDPrepare || k == api.OperationKindZeroUUIDRecovery
}

func (o *Operations) notify(ctx context.Context, sev notify.Severity, kind, swarm, cluster, msg string, details map[string]string) {
	if o.Notify == nil {
		return
	}
	d := map[string]string{"swarm": swarm, "cluster": cluster}
	for k, v := range details {
		d[k] = v
	}
	o.Notify(ctx, notify.Event{Time: o.Svc.cfg.Now(), Severity: sev, Kind: kind,
		Resource: "swarm/" + swarm + "/cluster/" + cluster, Message: msg, Details: d})
}

// zeroUUIDEligibleState reports whether the stored health allows starting a
// zero-UUID recovery: no node was seen in a Primary component with a real
// state UUID.
func zeroUUIDEligibleState(c ClusterRecord) error {
	for slot, h := range c.Health {
		if h.Health.ReportsPrimary() && h.Health.StateUUID != galera.ZeroUUID {
			return fmt.Errorf("%w: %s is a member of a Primary component with a valid state UUID", galera.ErrNotFullOutage, slot)
		}
	}
	return nil
}

// StartZeroUUIDRecovery plans the prepare phase of the zero-UUID recovery
// (stop, verify, snapshot) and starts it waiting for an approval. It needs
// operate permission on the cluster; approving needs recover.
func (o *Operations) StartZeroUUIDRecovery(ctx context.Context, actor authz.Subject, clusterID string) (OperationRecord, error) {
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, clusterID, Consistent())
	if err != nil {
		return OperationRecord{}, err
	}
	if !ok {
		return OperationRecord{}, fmt.Errorf("%w: cluster %q", api.ErrNotFound, clusterID)
	}
	if err := o.authorize(actor, authz.ActionOperate, clusterRes(cur.Value.Swarm, clusterID)); err != nil {
		return OperationRecord{}, err
	}
	return o.startPrepare(ctx, cur.Value, actor.ID, false)
}

func (o *Operations) startPrepare(ctx context.Context, c ClusterRecord, by string, auto bool) (OperationRecord, error) {
	l, err := o.leadership()
	if err != nil {
		return OperationRecord{}, err
	}
	if err := zeroUUIDEligibleState(c); err != nil {
		return OperationRecord{}, err
	}
	if busy, err := o.unfinishedFor(c.ID); err != nil {
		return OperationRecord{}, err
	} else if busy != "" {
		return OperationRecord{}, fmt.Errorf("%w: operation %s is not finished", runner.ErrBusy, busy)
	}
	for _, n := range c.Nodes {
		if c.ServiceID[n.Slot] == "" {
			return OperationRecord{}, fmt.Errorf("%w: %s/%s", ErrMissingServiceID, c.ID, n.Slot)
		}
	}
	st, err := o.settings()
	if err != nil {
		return OperationRecord{}, err
	}
	plan, err := galera.BuildZeroUUIDPrepare(c.Spec, c.Nodes)
	if err != nil {
		return OperationRecord{}, err
	}
	opID, approvalID := newID("op"), newID("apr")
	snapshot := "pre-recovery-" + opID
	svcHost, err := o.serviceHostFor(c.Swarm)
	if err != nil {
		return OperationRecord{}, err
	}
	op, err := runner.FromGaleraPlan(plan.WithSnapshot(snapshot), runner.GaleraBindings{
		OpID: opID, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase,
		ServiceIDs: c.ServiceID, StepTimeout: o.StepTimeout, MaxRetries: o.MaxRetries,
		ServiceHost: svcHost, ApprovalID: approvalID,
	})
	if err != nil {
		return OperationRecord{}, err
	}
	if err := o.allowReferenced(&op, c.Swarm, st.InstallationID, append(nodeHosts(c.Nodes), svcHost), clusterRefs(plan.Cluster)); err != nil {
		return OperationRecord{}, err
	}
	digest, err := DefinitionDigest(op)
	if err != nil {
		return OperationRecord{}, err
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: c.Swarm, Cluster: c.ID, RequestedBy: by, RequestedAt: o.Svc.cfg.Now(),
		Definition: op, Nodes: c.Nodes, ApprovalID: approvalID, ApprovalDigest: digest, Snapshot: snapshot, AutoApproved: auto}
	if err := o.commit(rec, nil); err != nil {
		return OperationRecord{}, err
	}
	if auto {
		if err := o.recordAutoApproval(rec); err != nil {
			return rec, err
		}
	}
	how := "waits for approval"
	if auto {
		how = "approved by the autoRecover policy"
	}
	msg := fmt.Sprintf("%s started the zero-UUID recovery of %s (phase 1: stop all nodes, snapshot the data directories as %s), operation %s %s",
		by, c.Name, snapshot, opID, how)
	o.Svc.recordEvent(c.Swarm, c.ID, api.SeverityCritical, msg)
	kind, sev := KindRecoveryNeedsHand, notify.SeverityCritical
	if auto {
		kind = KindRecoveryPlanned
	}
	o.notify(ctx, sev, kind, c.Swarm, c.ID, msg, map[string]string{"operation": opID, "phase": "prepare", "auto": fmt.Sprint(auto)})
	o.start(l, op, nil)
	return rec, nil
}

// recordAutoApproval stores the approval of the autoRecover policy for
// rec's exact definition digest.
func (o *Operations) recordAutoApproval(rec OperationRecord) error {
	a := ApprovalRecord{ID: rec.ApprovalID, OpID: rec.ID, Digest: rec.ApprovalDigest, By: AutoApprover, Approved: o.Svc.cfg.Now()}
	_, err := Put(o.Svc.store, CollApprovals, a.ID, a, IfVersion(0))
	return err
}

// planRecovery runs the second phase for a succeeded prepare operation:
// read the positions on the stopped nodes, diagnose, plan. It is
// idempotent per prepare operation (FollowUp).
func (o *Operations) planRecovery(ctx context.Context, prep OperationRecord) error {
	l, err := o.leadership()
	if err != nil {
		return err
	}
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, prep.Cluster, Consistent())
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("%w: cluster %q", api.ErrNotFound, prep.Cluster)
		}
		return err
	}
	c := cur.Value
	ev, err := o.collectEvidence(ctx, collectSystem, c.ID, true, true)
	if err != nil {
		return err
	}
	d, err := galera.DiagnoseFullOutage(ev)
	if err != nil {
		return err
	}
	setFollowUp := func(v string) error {
		_, _, err := Update(o.Svc.store, CollOperations, prep.ID, func(r OperationRecord, ex bool) (OperationRecord, error) {
			if !ex {
				return r, ErrNotFound
			}
			r.FollowUp = v
			return r, nil
		})
		return err
	}
	if d.Candidate == "" || d.Kind != galera.PlanZeroUUIDRecovery {
		msg := fmt.Sprintf("zero-UUID recovery of %s stopped after phase 1: no safe candidate (%v). The nodes stay stopped and snapshot %s is kept; collect the evidence and decide manually",
			c.Name, d.Reasons, prep.Snapshot)
		o.Svc.recordEvent(c.Swarm, c.ID, api.SeverityCritical, msg)
		o.notify(ctx, notify.SeverityCritical, KindRecoveryNeedsHand, c.Swarm, c.ID, msg, map[string]string{"operation": prep.ID, "phase": "diagnosis"})
		return setFollowUp("none")
	}
	plan, err := galera.RecoveryPlan(d, c.Spec, c.Nodes)
	if err != nil {
		return err
	}
	st, err := o.settings()
	if err != nil {
		return err
	}
	svcHost, err := o.serviceHostFor(c.Swarm)
	if err != nil {
		return err
	}
	opID, approvalID := newID("op"), newID("apr")
	op, err := runner.FromGaleraPlan(plan.WithSnapshot(prep.Snapshot), runner.GaleraBindings{
		OpID: opID, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase,
		ServiceIDs: c.ServiceID, StepTimeout: o.StepTimeout, MaxRetries: o.MaxRetries,
		ServiceHost: svcHost, ApprovalID: approvalID,
	})
	if err != nil {
		return err
	}
	if err := o.allowReferenced(&op, c.Swarm, st.InstallationID, append(nodeHosts(c.Nodes), svcHost), clusterRefs(plan.Cluster)); err != nil {
		return err
	}
	digest, err := DefinitionDigest(op)
	if err != nil {
		return err
	}
	auto := c.AutoRecover && d.AutoEligible
	dc := d
	dc.Plan = galera.Plan{}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: c.Swarm, Cluster: c.ID, RequestedBy: prep.RequestedBy, RequestedAt: o.Svc.cfg.Now(),
		Definition: op, Nodes: c.Nodes, ApprovalID: approvalID, ApprovalDigest: digest, Snapshot: prep.Snapshot, AutoApproved: auto, Diagnosis: &dc}
	// The follow-up link and the new operation commit together, so a crash
	// between them neither loses nor duplicates the second phase.
	opDoc, err := PutOp(CollOperations, opID, rec, IfVersion(0))
	if err != nil {
		return err
	}
	pr, ok, err := Get[OperationRecord](o.Svc.store, CollOperations, prep.ID, Consistent())
	if err != nil || !ok {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	if pr.Value.FollowUp != "" {
		return nil
	}
	pv := pr.Value
	pv.FollowUp = opID
	prDoc, err := PutOp(CollOperations, prep.ID, pv, IfVersion(pr.Version))
	if err != nil {
		return err
	}
	if _, err := o.Svc.store.Txn(opDoc, prDoc); err != nil {
		return err
	}
	if auto {
		if err := o.recordAutoApproval(rec); err != nil {
			return err
		}
	}
	how := "waits for approval"
	kind := KindRecoveryNeedsHand
	if auto {
		how, kind = "approved by the autoRecover policy (all positions readable, unique highest seqno)", KindRecoveryPlanned
	} else if c.AutoRecover {
		how += " (the autoRecover policy does not apply: " + fmt.Sprint(d.Risks) + ")"
	}
	msg := fmt.Sprintf("zero-UUID recovery of %s (phase 2): bootstrap %s (local seqno %d) with a new state UUID, the other nodes rejoin by SST; snapshot %s; operation %s %s",
		c.Name, d.Candidate, seqnoOf(d, d.Candidate), prep.Snapshot, opID, how)
	o.Svc.recordEvent(c.Swarm, c.ID, api.SeverityCritical, msg)
	o.notify(ctx, notify.SeverityCritical, kind, c.Swarm, c.ID, msg,
		map[string]string{"operation": opID, "phase": "recovery", "candidate": d.Candidate, "auto": fmt.Sprint(auto)})
	o.start(l, op, nil)
	return nil
}

func seqnoOf(d galera.Diagnosis, slot string) int64 {
	for _, p := range d.Positions {
		if p.Slot == slot {
			return p.Seqno
		}
	}
	return -1
}

// collectSystem is the internal subject of trusted evidence collection.
var collectSystem = authz.Subject{ID: "manager", Kind: authz.SubjectSystem}

// SetAutoRecover switches the per-cluster autoRecover policy. It needs the
// recover permission on the cluster (administrators) and is audited.
func (o *Operations) SetAutoRecover(actor authz.Subject, clusterID string, enabled bool) (bool, error) {
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, clusterID, Consistent())
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%w: cluster %q", api.ErrNotFound, clusterID)
	}
	if err := o.authorize(actor, authz.ActionRecover, clusterRes(cur.Value.Swarm, clusterID)); err != nil {
		return false, err
	}
	if _, _, err := Update(o.Svc.store, CollClusters, clusterID, func(c ClusterRecord, ex bool) (ClusterRecord, error) {
		if !ex {
			return c, ErrNotFound
		}
		c.AutoRecover = enabled
		return c, nil
	}); err != nil {
		return false, err
	}
	o.Svc.recordEvent(cur.Value.Swarm, clusterID, api.SeverityWarning, fmt.Sprintf("%s set autoRecover of %s to %v", actor.ID, cur.Value.Name, enabled))
	return enabled, nil
}

// ZeroUUIDStatus describes the newest recovery run of a cluster.
func (o *Operations) ZeroUUIDStatus(actor authz.Subject, clusterID string) (api.ZeroUUIDRecoveryStatus, error) {
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, clusterID)
	if err != nil {
		return api.ZeroUUIDRecoveryStatus{}, err
	}
	if !ok {
		return api.ZeroUUIDRecoveryStatus{}, fmt.Errorf("%w: cluster %q", api.ErrNotFound, clusterID)
	}
	if d := o.Authorizer.Check(actor, authz.ActionRead, clusterRes(cur.Value.Swarm, clusterID)); !d.Allowed {
		return api.ZeroUUIDRecoveryStatus{}, &authz.DeniedError{Decision: d}
	}
	recs, err := List[OperationRecord](o.Svc.store, CollOperations)
	if err != nil {
		return api.ZeroUUIDRecoveryStatus{}, err
	}
	approvals, err := List[ApprovalRecord](o.Svc.store, CollApprovals)
	if err != nil {
		return api.ZeroUUIDRecoveryStatus{}, err
	}
	approved := map[string]ApprovalRecord{}
	for _, a := range approvals {
		approved[a.Key] = a.Value
	}
	out := api.ZeroUUIDRecoveryStatus{AutoRecover: cur.Value.AutoRecover}
	newest := map[string]OperationRecord{}
	for _, r := range recs {
		v := r.Value
		if v.Cluster != clusterID || !isZeroUUIDKind(v.Kind) {
			continue
		}
		if p, ok := newest[v.Kind]; !ok || v.RequestedAt.After(p.RequestedAt) {
			newest[v.Kind] = v
		}
	}
	phase := func(v OperationRecord) *api.RecoveryPhase {
		a, ok := approved[v.ApprovalID]
		isApproved := ok && a.Digest == v.ApprovalDigest
		p := &api.RecoveryPhase{ID: v.ID, Kind: v.Kind, State: string(operationState(v)),
			AwaitingApproval: !isApproved && v.EndedAt.IsZero(), Approval: api.OperationApproval{Digest: v.ApprovalDigest},
			Auto: v.AutoApproved}
		if isApproved {
			p.Approval.ApprovedBy, p.Approval.ApprovedAt = a.By, a.Approved
		}
		if d := v.Diagnosis; d != nil {
			p.Candidate = d.Candidate
			p.Diagnosis = &api.Diagnosis{Candidate: d.Candidate, Positions: d.Positions, Reasons: d.Reasons, Risks: d.Risks,
				RequiresApproval: true, RecoveryKind: string(d.Kind), AutoEligible: d.AutoEligible}
		}
		return p
	}
	if v, ok := newest[api.OperationKindZeroUUIDPrepare]; ok {
		out.Prepare, out.Snapshot = phase(v), v.Snapshot
	}
	if v, ok := newest[api.OperationKindZeroUUIDRecovery]; ok {
		out.Recovery = phase(v)
		if out.Snapshot == "" {
			out.Snapshot = v.Snapshot
		}
	}
	return out, nil
}

// ZeroUUIDReconciler drives the zero-UUID recovery on the leader: it plans
// the second phase after the first, starts runs for clusters whose
// autoRecover policy is on, and notifies when a run ended.
type ZeroUUIDReconciler struct {
	Ops      *Operations
	Interval time.Duration // default 10s
	// MaxAge is how old a zero-UUID observation may be to start an
	// automatic run (default 2 minutes).
	MaxAge time.Duration
}

// Name implements Reconciler.
func (ZeroUUIDReconciler) Name() string { return "zero-uuid-recovery" }

// Run implements Reconciler.
func (r ZeroUUIDReconciler) Run(ctx context.Context, l *Leadership) error {
	iv := r.Interval
	if iv <= 0 {
		iv = 10 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := r.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
			l.svc.log.Warn("zero-uuid recovery reconcile incomplete", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// ReconcileOnce is deterministic given the stored records.
func (r ZeroUUIDReconciler) ReconcileOnce(ctx context.Context) error {
	o := r.Ops
	recs, err := List[OperationRecord](o.Svc.store, CollOperations, Consistent())
	if err != nil {
		return err
	}
	var errs []error
	started := map[string]time.Time{}
	for _, e := range recs {
		v := e.Value
		if !isZeroUUIDKind(v.Kind) {
			continue
		}
		if v.RequestedAt.After(started[v.Cluster]) {
			started[v.Cluster] = v.RequestedAt
		}
		if v.Kind == api.OperationKindZeroUUIDPrepare && v.FollowUp == "" && v.View != nil && v.View.Status == runner.StatusSucceeded {
			if err := o.planRecovery(ctx, v); err != nil {
				errs = append(errs, err)
			}
		}
		if !v.EndedAt.IsZero() && !v.EndNotified {
			errs = append(errs, r.notifyEnd(ctx, v))
		}
	}
	// Opt-in automatic runs.
	clusters, err := List[ClusterRecord](o.Svc.store, CollClusters, Consistent())
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	maxAge := r.MaxAge
	if maxAge <= 0 {
		maxAge = 2 * time.Minute
	}
	now := o.Svc.cfg.Now()
	for _, ce := range clusters {
		c := ce.Value
		if !c.AutoRecover || now.Sub(started[c.ID]) < zeroUUIDCooldown && !started[c.ID].IsZero() {
			continue
		}
		zero := false
		for _, h := range c.Health {
			if h.Health.ReasonCode == galera.ReasonZeroUUIDPrimary && now.Sub(h.ObservedAt) <= maxAge {
				zero = true
			}
		}
		if !zero {
			continue
		}
		if _, err := o.startPrepare(ctx, c, AutoApprover, true); err != nil && !errors.Is(err, runner.ErrBusy) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r ZeroUUIDReconciler) notifyEnd(ctx context.Context, v OperationRecord) error {
	o := r.Ops
	status := runner.Status("")
	if v.View != nil {
		status = v.View.Status
	}
	sev := notify.SeverityInfo
	if status != runner.StatusSucceeded {
		sev = notify.SeverityCritical
	}
	msg := fmt.Sprintf("zero-UUID recovery operation %s (%s) %s; snapshot %s", v.ID, v.Kind, status, v.Snapshot)
	if v.Kind == api.OperationKindZeroUUIDRecovery && status == runner.StatusSucceeded {
		msg += "; all nodes Synced with a new state UUID"
	}
	if v.Kind == api.OperationKindZeroUUIDPrepare && status == runner.StatusSucceeded && v.FollowUp == "none" {
		msg += "; no recovery was planned"
	}
	o.notify(ctx, sev, KindRecoveryEnded, v.Swarm, v.Cluster, msg, map[string]string{"operation": v.ID, "status": string(status)})
	_, _, err := Update(o.Svc.store, CollOperations, v.ID, func(r OperationRecord, ex bool) (OperationRecord, error) {
		if !ex {
			return r, ErrNotFound
		}
		r.EndNotified = true
		return r, nil
	})
	return err
}
