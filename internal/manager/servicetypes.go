// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// The galera service type and service updates (W07; fakes only).
//
// A galera service is a cluster record. Every galera request through the
// service API runs the unchanged cluster flow: creation is
// Operations.createCluster (the same prepareCreateCluster as POST
// /clusters, the same authorization on the cluster resource, no added or
// removed step), deletion is Operations.DeleteCluster with its approval.
// Replacement, recovery, maintenance and backups stay on the cluster
// routes; updates and size changes are refused (service.ErrUpdateUnsupported,
// service.ErrGaleraScale), because the generic update strategy would not
// keep the node-by-node wait-synced gates.

// ErrNoPreviousSpec: a rollback was requested but no earlier spec is kept.
var ErrNoPreviousSpec = errors.New("manager: the service has no previous spec to roll back to")

// galeraServiceInfo lists a cluster as a service of the galera type.
func galeraServiceInfo(c ClusterRecord) api.ServiceInfo {
	spec, _ := json.Marshal(service.GaleraSpec{Size: c.Size, Image: c.Spec.Image, Network: c.Spec.Network,
		RootPasswordSecret: c.Spec.RootPasswordSecret, GcacheSize: c.Spec.GcacheSize, Binlog: c.Spec.Binlog, Resources: c.Resources,
		Placement: c.Placement, AutoReplace: c.AutoReplace})
	si := api.ServiceInfo{ID: c.ID, Swarm: c.Swarm, Name: c.Name, Type: service.TypeGalera, Spec: spec,
		CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt, CreateOperation: c.CreateOp, DeleteOperation: c.DeleteOp}
	h := &api.ServiceHealth{State: string(api.StateUnknown), Desired: len(c.Nodes)}
	if o := c.Observed; o != nil {
		h.State, h.ObservedAt = string(o.State), o.At
		if o.Detail != "" {
			h.Reasons = []string{o.Detail}
		}
	}
	for _, n := range c.Nodes {
		if obs, ok := c.Health[n.Slot]; ok && obs.Health.Healthy() {
			h.Running++
		}
	}
	si.Health = h
	return si
}

func (s *Services) isCluster(id string) bool {
	_, ok, err := Get[ClusterRecord](s.Ops.Svc.store, CollClusters, id)
	return err == nil && ok
}

// galeraRequest is the cluster creation request of a galera service input.
func galeraRequest(in api.ServiceInput) (CreateClusterRequest, error) {
	g, err := service.DecodeGaleraSpec(in.Spec)
	if err != nil {
		return CreateClusterRequest{}, err
	}
	return CreateClusterRequest{Swarm: in.Swarm, Name: in.Name, Size: g.Size, Image: g.Image, Network: g.Network,
		RootPasswordSecret: g.RootPasswordSecret, GcacheSize: g.GcacheSize, Binlog: g.Binlog, Resources: g.Resources,
		Placement: g.Placement, AutoReplace: g.AutoReplace}, nil
}

func (s *Services) previewGalera(actor authz.Subject, in api.ServiceInput) (api.ServicePlan, error) {
	req, err := galeraRequest(in)
	if err != nil {
		return api.ServicePlan{}, err
	}
	pc, err := s.Ops.prepareCreateCluster(actor, req)
	if err != nil {
		return api.ServicePlan{}, err
	}
	p := toAPIPlan(service.DescribeGaleraPlan(service.Context{Name: in.Name}, pc.plan))
	// The cluster, operation and token IDs of a preview are not reused.
	return withAgentSteps(p, pc.op, true), nil
}

func (s *Services) createGalera(actor authz.Subject, in api.ServiceInput) (api.ServiceStarted, error) {
	req, err := galeraRequest(in)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	rec, plan, err := s.Ops.createCluster(actor, req)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	p := toAPIPlan(service.DescribeGaleraPlan(service.Context{Name: in.Name}, plan))
	return api.ServiceStarted{Service: rec.Cluster, Operation: api.OperationRef{ID: rec.ID, Kind: rec.Kind},
		Plan: withAgentSteps(p, rec.Definition, false)}, nil
}

// clusterDeletePlan describes a cluster deletion operation.
func clusterDeletePlan(c ClusterRecord, op runner.Operation, provisional bool) api.ServicePlan {
	p := api.ServicePlan{Kind: string(service.PlanDelete), Type: service.TypeGalera, Name: c.Name, RequiresApproval: true,
		Executable: true, Warnings: []string{"the node volumes are removed only after the agent verified their identity; see the agent steps"}}
	var b strings.Builder
	fmt.Fprintf(&b, "delete galera service %q (id %s)\n", c.Name, c.ID)
	for i, st := range op.Steps {
		kind := string(st.Command)
		if kind == "" {
			kind = "gate"
		}
		ps := api.PlanStep{ID: st.ID, Kind: kind, Slot: st.Target, Note: st.Note}
		for _, cond := range st.Preconditions {
			ps.Conditions = append(ps.Conditions, conditionText(cond))
		}
		p.Steps = append(p.Steps, ps)
		fmt.Fprintf(&b, "%d. %s: %s\n", i+1, kind, st.Note)
	}
	p.Text = b.String()
	return withAgentSteps(p, op, provisional)
}

func (s *Services) previewGaleraDelete(actor authz.Subject, id string) (api.ServicePlan, error) {
	pd, err := s.Ops.prepareClusterDelete(actor, id)
	if err != nil {
		return api.ServicePlan{}, err
	}
	return clusterDeletePlan(pd.cur.Value, pd.op, true), nil
}

func (s *Services) deleteGalera(actor authz.Subject, id string) (api.ServiceStarted, error) {
	cur, ok, err := Get[ClusterRecord](s.Ops.Svc.store, CollClusters, id)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	if !ok {
		return api.ServiceStarted{}, fmt.Errorf("%w: service %q", api.ErrNotFound, id)
	}
	d, err := s.Ops.DeleteCluster(actor, id)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	return api.ServiceStarted{Service: id, Operation: api.OperationRef{ID: d.Operation.ID, Kind: d.Operation.Kind},
		Approval: api.OperationApproval{Digest: d.Digest}, Plan: clusterDeletePlan(cur.Value, d.Operation.Definition, false)}, nil
}

// Updates.

func toUpdateStrategy(in api.ServiceUpdateStrategy) service.UpdateStrategy {
	s := service.UpdateStrategy{Order: in.Order, Parallelism: in.Parallelism, DelaySeconds: in.DelaySeconds,
		MonitorSeconds: in.MonitorSeconds, MaxFailureRatio: in.MaxFailureRatio, OnFailure: in.OnFailure}
	if c := in.Canary; c != nil {
		s.Canary = &service.Canary{Replicas: c.Replicas, HoldSeconds: c.HoldSeconds}
	}
	return s
}

// planUpdate plans an update (or the rollback) of a stored service.
func (s *Services) planUpdate(actor authz.Subject, id string, in api.ServiceUpdateInput) (service.Plan, Versioned[ServiceRecord], json.RawMessage, Settings, error) {
	o := s.Ops
	if s.isCluster(id) {
		return service.Plan{}, Versioned[ServiceRecord]{}, nil, Settings{}, fmt.Errorf("%w: %q; galera nodes are updated node by node through the cluster flows",
			service.ErrUpdateUnsupported, service.TypeGalera)
	}
	cur, ok, err := Get[ServiceRecord](o.Svc.store, CollServices, id, Consistent())
	if err != nil {
		return service.Plan{}, cur, nil, Settings{}, err
	}
	if !ok {
		return service.Plan{}, cur, nil, Settings{}, fmt.Errorf("%w: service %q", api.ErrNotFound, id)
	}
	r := cur.Value
	if err := o.authorize(actor, authz.ActionUpdate, serviceRes(r.Swarm, r.ID)); err != nil {
		return service.Plan{}, cur, nil, Settings{}, err
	}
	st, err := o.settings()
	if err != nil {
		return service.Plan{}, cur, nil, Settings{}, err
	}
	ty, err := s.registry().Get(r.Type)
	if err != nil {
		return service.Plan{}, cur, nil, Settings{}, err
	}
	target := in.Spec
	if in.Rollback {
		if len(r.PreviousSpec) == 0 {
			return service.Plan{}, cur, nil, Settings{}, ErrNoPreviousSpec
		}
		target = r.PreviousSpec
	}
	hosts, err := o.serviceHosts(r.Swarm)
	if err != nil {
		return service.Plan{}, cur, nil, Settings{}, err
	}
	plan, err := service.UpdatePlan(ty, service.UpdateInput{
		Context: service.Context{InstallationID: st.InstallationID, ServiceID: r.ID, Name: r.Name},
		Current: r.Spec, Spec: target, Strategy: toUpdateStrategy(in.Strategy), Hosts: hosts,
	})
	return plan, cur, slices.Clone(target), st, err
}

// PreviewServiceUpdate implements api.Services.
func (s *Services) PreviewServiceUpdate(_ context.Context, actor authz.Subject, id string, in api.ServiceUpdateInput) (api.ServicePlan, error) {
	plan, cur, _, st, err := s.planUpdate(actor, id, in)
	if err != nil {
		return api.ServicePlan{}, MapCommandError(err)
	}
	out := toAPIPlan(plan)
	if plan.Executable() {
		op, err := s.createOperation(plan, cur.Value.Swarm, st, previewID("op"), previewID("apr"), func() string { return previewID("tok") })
		if err != nil {
			return api.ServicePlan{}, MapCommandError(err)
		}
		out = withAgentSteps(out, op, true)
	}
	return out, nil
}

// UpdateService implements api.Services: the update waits for an approval
// bound to its digest like create and delete.
func (s *Services) UpdateService(_ context.Context, actor authz.Subject, id string, in api.ServiceUpdateInput) (api.ServiceStarted, error) {
	started, err := s.update(actor, id, in)
	return started, MapCommandError(err)
}

func (s *Services) update(actor authz.Subject, id string, in api.ServiceUpdateInput) (api.ServiceStarted, error) {
	o := s.Ops
	l, err := o.leadership()
	if err != nil {
		return api.ServiceStarted{}, err
	}
	plan, cur, target, st, err := s.planUpdate(actor, id, in)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	r := cur.Value
	if r.DeleteOp != "" {
		return api.ServiceStarted{}, fmt.Errorf("%w: the service is being deleted (operation %s)", ErrConflict, r.DeleteOp)
	}
	ops, err := List[OperationRecord](o.Svc.store, CollOperations, Consistent())
	if err != nil {
		return api.ServiceStarted{}, err
	}
	for _, x := range ops {
		v := x.Value
		if v.Service == id && v.EndedAt.IsZero() && !(v.View != nil && v.View.Status == runner.StatusStopped && len(v.View.Steps) == 0) {
			return api.ServiceStarted{}, fmt.Errorf("%w: operation %s is not finished", runner.ErrBusy, v.ID)
		}
	}
	opID, approvalID := newID("op"), newID("apr")
	op, err := s.createOperation(plan, r.Swarm, st, opID, approvalID, func() string { return newID("tok") })
	if err != nil {
		return api.ServiceStarted{}, err
	}
	digest, err := DefinitionDigest(op)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: r.Swarm, Service: id, RequestedBy: actor.ID, RequestedAt: o.Svc.cfg.Now(),
		Definition: op, ApprovalID: approvalID, ApprovalDigest: digest}
	opDoc, err := PutOp(CollOperations, opID, rec, IfVersion(0))
	if err != nil {
		return api.ServiceStarted{}, err
	}
	marked := r
	marked.UpdateOp, marked.PendingSpec = opID, target
	svcDoc, err := PutOp(CollServices, id, marked, IfVersion(cur.Version))
	if err != nil {
		return api.ServiceStarted{}, err
	}
	if _, err := o.Svc.store.Txn(opDoc, svcDoc); err != nil {
		return api.ServiceStarted{}, err
	}
	what := "update"
	if in.Rollback {
		what = "rollback"
	}
	o.Svc.recordEvent(r.Swarm, "", api.SeverityInfo, fmt.Sprintf("%s requested %s of service %s, operation %s (waits for approval)", actor.ID, what, r.Name, opID))
	o.start(l, op, nil)
	return api.ServiceStarted{Service: id, Operation: api.OperationRef{ID: opID, Kind: op.Kind},
		Approval: api.OperationApproval{Digest: digest}, Plan: withAgentSteps(toAPIPlan(plan), op, false)}, nil
}

// finishServiceUpdate records the end of an update (from saveView, also
// for resumed runs). After success the pending spec becomes the desired
// spec and the old one the rollback target; otherwise the stored spec is
// kept. A failed update may have left Swarm on the previous spec (Swarm
// rollback) or paused; the event says so and the health collector reports
// the observed state.
func (s *Service) finishServiceUpdate(rec OperationRecord, succeeded bool) {
	var name string
	_, _, err := Update(s.store, CollServices, rec.Service, func(cur ServiceRecord, exists bool) (ServiceRecord, error) {
		if !exists {
			return cur, ErrNotFound
		}
		if cur.UpdateOp != rec.ID {
			return cur, errSkip
		}
		name = cur.Name
		if succeeded {
			cur.PreviousSpec, cur.Spec = cur.Spec, cur.PendingSpec
		}
		cur.UpdateOp, cur.PendingSpec = "", nil
		return cur, nil
	})
	if errors.Is(err, errSkip) || errors.Is(err, ErrNotFound) {
		return
	}
	if err != nil {
		if !errors.Is(err, metastore.ErrNotLeader) {
			s.log.Warn("manager: service record not updated after the update operation", "service", rec.Service, "err", err)
		}
		return
	}
	if !succeeded {
		s.recordEvent(rec.Swarm, "", api.SeverityWarning, fmt.Sprintf("update of service %s did not succeed (operation %s); the stored spec is unchanged, "+
			"Swarm may have rolled back or paused the update; check the service health and use the rollback plan if needed", name, rec.ID))
	}
}

var errSkip = errors.New("manager: skip")
