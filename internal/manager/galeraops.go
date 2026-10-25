// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// PlanClusterOperation persists a reviewable plan; all agent commands wait
// for a digest-bound approval. The definition and resulting state survive HA.
func (o *Operations) PlanClusterOperation(actor authz.Subject, id string, req galera.OperationRequest) (api.ClusterOperationPlan, error) {
	l, err := o.leadership()
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, id, Consistent())
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	if !ok {
		return api.ClusterOperationPlan{}, api.ErrNotFound
	}
	c := cur.Value
	if err = o.authorize(actor, authz.ActionOperate, clusterRes(c.Swarm, id)); err != nil {
		return api.ClusterOperationPlan{}, err
	}
	if busy, err := o.unfinishedFor(id); err != nil {
		return api.ClusterOperationPlan{}, err
	} else if busy != "" {
		return api.ClusterOperationPlan{}, runner.ErrBusy
	}
	st, err := o.settings()
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	state, err := o.clusterState(c)
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	paused, err := o.pausedHosts(c.Swarm)
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	plan, err := galera.PlanOperation(state, req, galera.ReplacementPolicy{Now: o.Svc.cfg.Now(), Placement: c.Placement, PausedHosts: paused})
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	svcHost, err := o.serviceHostFor(c.Swarm)
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	opID, approvalID := newID("op"), newID("apr")
	op, err := runner.FromGaleraPlan(plan, runner.GaleraBindings{OpID: opID, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase, TokenIDs: tokens(plan.Nodes), ServiceIDs: c.ServiceID, ServiceHost: svcHost, ApprovalID: approvalID, StepTimeout: o.StepTimeout, MaxRetries: o.MaxRetries})
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	if err = o.allowReferenced(&op, c.Swarm, st.InstallationID, append(nodeHosts(plan.Nodes), svcHost), clusterRefs(plan.Cluster)); err != nil {
		return api.ClusterOperationPlan{}, err
	}
	// Referenced-resource permission steps can precede the Galera gate. Bind
	// every command too, so even those steps cannot execute before approval.
	for i := range op.Steps {
		if op.Steps[i].Command != "" {
			op.Steps[i].Preconditions = append(op.Steps[i].Preconditions, runner.Condition{Kind: runner.CondApproved, Target: approvalID})
		}
	}
	digest, err := DefinitionDigest(op)
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: c.Swarm, Cluster: id, RequestedBy: actor.ID, RequestedAt: o.Svc.cfg.Now(), Definition: op, Nodes: operationProbeNodes(c.Nodes, plan.Nodes), GaleraPlan: &plan, ApprovalID: approvalID, ApprovalDigest: digest}
	// Serialize concurrent plan requests through the cluster document CAS.
	od, err := PutOp(CollOperations, opID, rec, IfVersion(0))
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	cd, err := PutOp(CollClusters, id, c, IfVersion(cur.Version))
	if err != nil {
		return api.ClusterOperationPlan{}, err
	}
	if _, err = o.Svc.store.Txn(od, cd); err != nil {
		return api.ClusterOperationPlan{}, err
	}
	o.Svc.recordEvent(c.Swarm, id, api.SeverityInfo, fmt.Sprintf("%s planned %s, operation %s awaits approval", actor.ID, req.Kind, opID))
	o.start(l, op, nil)
	return api.ClusterOperationPlan{ID: opID, Kind: op.Kind, Approval: api.OperationApproval{Digest: digest}, Plan: plan, AgentSteps: op.Steps}, nil
}

// saveGaleraSuccess commits desired state and completion together. This is
// shared by initial execution, approval resume and a new leader's takeover.
func (s *Service) saveGaleraSuccess(v runner.View) (bool, error) {
	r, ok, err := Get[OperationRecord](s.store, CollOperations, v.ID, Consistent())
	if err != nil {
		return false, err
	}
	if !ok || r.Value.GaleraPlan == nil {
		return false, nil
	}
	if !r.Value.EndedAt.IsZero() {
		return true, nil
	}
	c, ok, err := Get[ClusterRecord](s.store, CollClusters, r.Value.Cluster, Consistent())
	if err != nil {
		return true, err
	}
	if !ok {
		return true, ErrNotFound
	}
	rec, cl := r.Value, c.Value
	p := rec.GaleraPlan
	cl.Spec, cl.Nodes, cl.Size, cl.LastKnownSize = p.Cluster, p.Nodes, len(p.Nodes), len(p.Nodes)
	// Completed step outputs are not in the runner journal. Re-query a
	// missing create result with its original agent step ID (idempotent
	// replay), under the new leader's term, before committing success.
	if err := s.recoverGaleraResults(rec, &v); err != nil {
		return true, err
	}
	ids := createdServices(*p, v)
	if cl.ServiceID == nil {
		cl.ServiceID = map[string]string{}
	}
	for slot, id := range ids {
		cl.ServiceID[slot] = id
	}
	for slot := range cl.ServiceID {
		found := false
		for _, n := range p.Nodes {
			if n.Slot == slot {
				found = true
			}
		}
		if !found {
			delete(cl.ServiceID, slot)
			delete(cl.Health, slot)
		}
	}
	rec.View = &v
	rec.UpdatedAt = s.cfg.Now()
	rec.EndedAt = rec.UpdatedAt
	od, err := PutOp(CollOperations, rec.ID, rec, IfVersion(r.Version))
	if err != nil {
		return true, err
	}
	cd, err := PutOp(CollClusters, cl.ID, cl, IfVersion(c.Version))
	if err != nil {
		return true, err
	}
	_, err = s.store.Txn(od, cd)
	return true, err
}

func (c *Control) PlanClusterOperation(_ context.Context, actor authz.Subject, id string, req galera.OperationRequest) (api.ClusterOperationPlan, error) {
	p, err := c.Ops.PlanClusterOperation(actor, id, req)
	switch {
	case errors.Is(err, galera.ErrMajorUpgrade):
		return p, api.ErrMajorUpgradeApproval
	case errors.Is(err, galera.ErrUnsafeOperation):
		return p, api.ErrRequiresGuidedRecovery
	}
	return p, MapCommandError(err)
}

// Keep the removed slot resolvable until its initial stop has completed.
func operationProbeNodes(before, after []galera.Node) []galera.Node {
	out := append([]galera.Node(nil), after...)
	for _, n := range before {
		found := false
		for _, a := range after {
			if a.Slot == n.Slot {
				found = true
			}
		}
		if !found {
			out = append(out, n)
		}
	}
	return out
}

func (s *Service) recoverGaleraResults(rec OperationRecord, v *runner.View) error {
	l := s.Current()
	if l == nil || !l.Valid() {
		return ErrNotLeader
	}
	known := createdServices(*rec.GaleraPlan, *v)
	journal, err := (runner.MetastoreJournal{Node: s.node}).Get(rec.ID)
	if err != nil {
		return err
	}
	for _, gs := range rec.GaleraPlan.Steps {
		if gs.Kind != galera.StepCreateService || known[gs.Slot] != "" {
			continue
		}
		for _, step := range rec.Definition.Steps {
			if step.ID != gs.ID {
				continue
			}
			agentID := step.ID
			for _, js := range journal.Steps {
				if js.ID == step.ID && js.AgentStep != "" {
					agentID = js.AgentStep
				}
			}
			ctx, cancel := context.WithTimeout(l.Context(), s.cfg.BarrierTimeout)
			res, sendErr := l.Agents.Send(ctx, step.Target, agent.Command{Term: l.Term, OpID: rec.ID, StepID: agentID, Kind: step.Command, Args: step.Args})
			cancel()
			if sendErr != nil {
				return sendErr
			}
			if res.Status != agent.StatusSucceeded {
				return fmt.Errorf("manager: cannot recover create result: %s", res.Code)
			}
			var out struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(res.Output, &out) != nil || out.ID == "" {
				return fmt.Errorf("manager: create result has no service ID")
			}
			found := false
			for i := range v.Steps {
				if v.Steps[i].ID == step.ID {
					v.Steps[i].LastResult = &res
					found = true
				}
			}
			if !found {
				v.Steps = append(v.Steps, runner.StepView{ID: step.ID, LastResult: &res})
			}
		}
	}
	return nil
}

// A crash can occur after the last journal commit but before the desired
// state transaction. Such a journal is no longer listed by TakeOver.
func (s *Service) reconcileGaleraSuccesses() {
	recs, err := List[OperationRecord](s.store, CollOperations, Consistent())
	if err != nil {
		return
	}
	for _, r := range recs {
		rec := r.Value
		if rec.GaleraPlan == nil || !rec.EndedAt.IsZero() {
			continue
		}
		j, err := s.node.Journal().Get(rec.ID)
		if err != nil {
			continue
		}
		done := len(j.Steps) > 0
		for _, step := range j.Steps {
			if step.State != "succeeded" {
				done = false
			}
		}
		if done {
			v := runner.View{ID: rec.ID, Kind: rec.Kind, Status: runner.StatusSucceeded, Term: s.node.Term()}
			if err := s.saveView(v); err != nil {
				s.log.Warn("manager: Galera success reconciliation deferred", "op", rec.ID, "err", err)
			}
		}
	}
}
