// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Guided host maintenance (A08) on top of galera.MaintenancePlan.
//
// Entering maintenance marks the host (which pauses node replacement on
// it) and starts, per cluster with a node on the host, an operation that
// stops those nodes gracefully after checking that the rest of the
// cluster is Synced. Nothing is started when any affected cluster refuses
// (not a majority outside the host, an unfinished operation, missing
// service IDs). The host is "ready" once every maintenance operation
// succeeded; the operator then patches or reboots it. Leaving maintenance
// starts the nodes again after an identity check and waits until each is
// Synced. The manager never reboots a host and never changes its Swarm
// availability.

// HostMaintenanceRecord is the maintenance state stored on a host.
type HostMaintenanceRecord struct {
	Since      time.Time `json:"since"`
	By         string    `json:"by"`
	Reason     string    `json:"reason,omitempty"`
	Operations []string  `json:"operations,omitempty"`
}

// ErrInMaintenance / ErrNotInMaintenance: the host's maintenance state
// does not allow the request.
var (
	ErrInMaintenance    = errors.New("manager: host is already in maintenance")
	ErrNotInMaintenance = errors.New("manager: host is not in maintenance")
)

func hostRes(swarm, nodeID string) authz.Resource {
	return authz.Resource{Type: authz.ResourceHost, Scope: authz.SwarmScope(swarm), ID: nodeID}
}

type maintPlan struct {
	cluster ClusterRecord
	plan    galera.Plan
	err     error
}

// maintenancePlans plans every cluster of the Swarm with a node on the
// host.
func (o *Operations) maintenancePlans(swarm, nodeID string, end bool) ([]maintPlan, error) {
	clusters, err := List[ClusterRecord](o.Svc.store, CollClusters, Consistent())
	if err != nil {
		return nil, err
	}
	var out []maintPlan
	for _, c := range clusters {
		cv := c.Value
		if cv.Swarm != swarm || !slices.ContainsFunc(cv.Nodes, func(n galera.Node) bool { return n.HostID == nodeID }) {
			continue
		}
		var p galera.Plan
		if end {
			p, err = galera.MaintenanceEndPlan(cv.Spec, cv.Nodes, nodeID)
		} else {
			p, err = galera.MaintenancePlan(cv.Spec, cv.Nodes, nodeID)
		}
		if err == nil {
			for _, n := range cv.Nodes {
				if n.HostID == nodeID && cv.ServiceID[n.Slot] == "" {
					err = fmt.Errorf("%w: %s/%s", ErrMissingServiceID, cv.ID, n.Slot)
				}
			}
		}
		if err == nil {
			if busy, berr := o.unfinishedFor(cv.ID); berr != nil {
				return nil, berr
			} else if busy != "" {
				err = fmt.Errorf("%w: operation %s is not finished", runner.ErrBusy, busy)
			}
		}
		out = append(out, maintPlan{cluster: cv, plan: p, err: err})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cluster.ID < out[j].cluster.ID })
	return out, nil
}

func describeCondition(pc galera.Precondition) string {
	switch pc.Kind {
	case galera.PreNodeSynced:
		return fmt.Sprintf("%s is Synced in the Primary component at cluster size %d", pc.Slot, pc.ClusterSize)
	case galera.PreStepSucceeded:
		return "step " + pc.StepID + " succeeded"
	}
	return string(pc.Kind)
}

func previewOf(nodeID string, end bool, plans []maintPlan) api.MaintenancePreview {
	pv := api.MaintenancePreview{Host: nodeID, End: end, Safe: true}
	for _, mp := range plans {
		cp := api.ClusterMaintenancePlan{Cluster: mp.cluster.ID, Name: mp.cluster.Name}
		for _, n := range mp.cluster.Nodes {
			if n.HostID == nodeID {
				cp.Slots = append(cp.Slots, n.Slot)
			}
		}
		if mp.err != nil {
			cp.Refused = mp.err.Error()
			pv.Safe = false
		} else {
			cp.Warnings = mp.plan.Warnings
			for _, s := range mp.plan.Steps {
				ps := api.PlanStep{ID: s.ID, Kind: string(s.Kind), Slot: s.Slot, Note: s.Note}
				for _, pc := range s.Preconditions {
					ps.Conditions = append(ps.Conditions, describeCondition(pc))
				}
				if s.Kind == galera.StepWaitSynced {
					ps.Conditions = append(ps.Conditions, fmt.Sprintf("%s is Synced at cluster size %d", s.Slot, s.ClusterSize))
				}
				cp.Steps = append(cp.Steps, ps)
			}
		}
		pv.Clusters = append(pv.Clusters, cp)
	}
	return pv
}

// MaintenancePreview returns the plan without changing anything (read on
// the host).
func (o *Operations) MaintenancePreview(actor authz.Subject, swarm, nodeID string, end bool) (api.MaintenancePreview, error) {
	if err := o.authorize(actor, authz.ActionRead, hostRes(swarm, nodeID)); err != nil {
		return api.MaintenancePreview{}, err
	}
	if _, ok, err := Get[HostRecord](o.Svc.store, CollHosts, HostKey(swarm, nodeID)); err != nil {
		return api.MaintenancePreview{}, err
	} else if !ok {
		return api.MaintenancePreview{}, fmt.Errorf("%w: host %s", api.ErrNotFound, nodeID)
	}
	plans, err := o.maintenancePlans(swarm, nodeID, end)
	if err != nil {
		return api.MaintenancePreview{}, err
	}
	return previewOf(nodeID, end, plans), nil
}

// startPlans converts, commits and starts one operation per plan.
func (o *Operations) startPlans(l *Leadership, actor authz.Subject, plans []maintPlan) ([]api.OperationRef, error) {
	st, err := o.settings()
	if err != nil {
		return nil, err
	}
	var refs []api.OperationRef
	for _, mp := range plans {
		c := mp.cluster
		svcHost, err := o.serviceHostFor(c.Swarm)
		if err != nil {
			return refs, err
		}
		opID := newID("op")
		op, err := runner.FromGaleraPlan(mp.plan, runner.GaleraBindings{
			OpID: opID, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase, ServiceIDs: c.ServiceID,
			StepTimeout: o.StepTimeout, MaxRetries: o.MaxRetries, ServiceHost: svcHost,
		})
		if err != nil {
			return refs, err
		}
		if err := o.allowReferenced(&op, c.Swarm, st.InstallationID, append(nodeHosts(mp.plan.Nodes), svcHost), clusterRefs(mp.plan.Cluster)); err != nil {
			return refs, err
		}
		rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: c.Swarm, Cluster: c.ID, RequestedBy: actor.ID,
			RequestedAt: o.Svc.cfg.Now(), Definition: op, Nodes: mp.plan.Nodes}
		if err := o.commit(rec, nil); err != nil {
			return refs, err
		}
		o.start(l, op, nil)
		refs = append(refs, api.OperationRef{ID: opID, Kind: op.Kind})
	}
	return refs, nil
}

// EnterMaintenance puts a host into maintenance (operate on the host and
// on every affected cluster; audited).
func (o *Operations) EnterMaintenance(actor authz.Subject, swarm, nodeID, reason string) (api.MaintenanceStarted, error) {
	l, err := o.leadership()
	if err != nil {
		return api.MaintenanceStarted{}, err
	}
	if err := o.authorize(actor, authz.ActionOperate, hostRes(swarm, nodeID)); err != nil {
		return api.MaintenanceStarted{}, err
	}
	h, ok, err := Get[HostRecord](o.Svc.store, CollHosts, HostKey(swarm, nodeID), Consistent())
	if err != nil {
		return api.MaintenanceStarted{}, err
	}
	if !ok {
		return api.MaintenanceStarted{}, fmt.Errorf("%w: host %s", api.ErrNotFound, nodeID)
	}
	if h.Value.Maintenance != nil {
		return api.MaintenanceStarted{}, ErrInMaintenance
	}
	plans, err := o.maintenancePlans(swarm, nodeID, false)
	if err != nil {
		return api.MaintenanceStarted{}, err
	}
	pv := previewOf(nodeID, false, plans)
	for _, mp := range plans {
		if err := o.authorize(actor, authz.ActionOperate, clusterRes(mp.cluster.Swarm, mp.cluster.ID)); err != nil {
			return api.MaintenanceStarted{}, err
		}
		if mp.err != nil {
			return api.MaintenanceStarted{Host: nodeID, Preview: pv}, fmt.Errorf("cluster %s: %w", mp.cluster.Name, mp.err)
		}
	}
	// Mark first: replacement of the host's nodes is paused before any
	// node stops.
	mark := &HostMaintenanceRecord{Since: o.Svc.cfg.Now(), By: actor.ID, Reason: reason}
	if _, err := Put(o.Svc.store, CollHosts, HostKey(swarm, nodeID), withMaintenance(h.Value, mark), IfVersion(h.Version)); err != nil {
		return api.MaintenanceStarted{}, err
	}
	refs, err := o.startPlans(l, actor, plans)
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.ID)
	}
	if _, _, uerr := Update(o.Svc.store, CollHosts, HostKey(swarm, nodeID), func(cur HostRecord, exists bool) (HostRecord, error) {
		if !exists || cur.Maintenance == nil {
			return cur, ErrNotFound
		}
		cur.Maintenance.Operations = ids
		return cur, nil
	}); uerr != nil && err == nil {
		err = uerr
	}
	o.Svc.recordEvent(swarm, "", api.SeverityWarning, fmt.Sprintf("%s put host %s into maintenance (%d operation(s)); replacement on it is paused", actor.ID, nodeID, len(refs)))
	if err != nil {
		return api.MaintenanceStarted{Host: nodeID, Operations: refs, Preview: pv}, err
	}
	return api.MaintenanceStarted{Host: nodeID, Operations: refs, Preview: pv}, nil
}

func withMaintenance(h HostRecord, m *HostMaintenanceRecord) HostRecord {
	h.Maintenance = m
	return h
}

// LeaveMaintenance starts the host's cluster nodes again and clears the
// maintenance mark (operate on the host and the clusters; audited).
func (o *Operations) LeaveMaintenance(actor authz.Subject, swarm, nodeID string) (api.MaintenanceStarted, error) {
	l, err := o.leadership()
	if err != nil {
		return api.MaintenanceStarted{}, err
	}
	if err := o.authorize(actor, authz.ActionOperate, hostRes(swarm, nodeID)); err != nil {
		return api.MaintenanceStarted{}, err
	}
	h, ok, err := Get[HostRecord](o.Svc.store, CollHosts, HostKey(swarm, nodeID), Consistent())
	if err != nil {
		return api.MaintenanceStarted{}, err
	}
	if !ok {
		return api.MaintenanceStarted{}, fmt.Errorf("%w: host %s", api.ErrNotFound, nodeID)
	}
	if h.Value.Maintenance == nil {
		return api.MaintenanceStarted{}, ErrNotInMaintenance
	}
	plans, err := o.maintenancePlans(swarm, nodeID, true)
	if err != nil {
		return api.MaintenanceStarted{}, err
	}
	pv := previewOf(nodeID, true, plans)
	for _, mp := range plans {
		if err := o.authorize(actor, authz.ActionOperate, clusterRes(mp.cluster.Swarm, mp.cluster.ID)); err != nil {
			return api.MaintenanceStarted{}, err
		}
		if mp.err != nil {
			return api.MaintenanceStarted{Host: nodeID, Preview: pv}, fmt.Errorf("cluster %s: %w", mp.cluster.Name, mp.err)
		}
	}
	refs, err := o.startPlans(l, actor, plans)
	if err != nil {
		return api.MaintenanceStarted{Host: nodeID, Operations: refs, Preview: pv}, err
	}
	if _, err := Put(o.Svc.store, CollHosts, HostKey(swarm, nodeID), withMaintenance(h.Value, nil), IfVersion(h.Version)); err != nil {
		return api.MaintenanceStarted{Host: nodeID, Operations: refs, Preview: pv}, err
	}
	o.Svc.recordEvent(swarm, "", api.SeverityInfo, fmt.Sprintf("%s ended maintenance of host %s (%d operation(s) start its nodes)", actor.ID, nodeID, len(refs)))
	return api.MaintenanceStarted{Host: nodeID, Operations: refs, Preview: pv}, nil
}

// pausedHosts lists the hosts of a Swarm in maintenance.
func (o *Operations) pausedHosts(swarm string) ([]string, error) {
	hosts, err := List[HostRecord](o.Svc.store, CollHosts)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, h := range hosts {
		if h.Value.Swarm == swarm && h.Value.Maintenance != nil {
			out = append(out, h.Value.NodeID)
		}
	}
	return out, nil
}

// maintenanceView is the API view of a host's maintenance with readiness.
func (o *Operations) maintenanceView(m *HostMaintenanceRecord) *api.HostMaintenance {
	if m == nil {
		return nil
	}
	v := &api.HostMaintenance{Since: m.Since, By: m.By, Reason: m.Reason, Operations: append([]string{}, m.Operations...), Ready: true}
	for _, id := range m.Operations {
		rec, ok, err := Get[OperationRecord](o.Svc.store, CollOperations, id)
		if err != nil || !ok || rec.Value.View == nil || rec.Value.View.Status != runner.StatusSucceeded {
			v.Ready = false
		}
	}
	return v
}

// MaintenancePlan implements api.Maintenance.
func (c *Control) MaintenancePlan(_ context.Context, actor authz.Subject, swarm, nodeID string, end bool) (api.MaintenancePreview, error) {
	pv, err := c.Ops.MaintenancePreview(actor, swarm, nodeID, end)
	return pv, MapCommandError(err)
}

// EnterMaintenance implements api.Maintenance.
func (c *Control) EnterMaintenance(_ context.Context, actor authz.Subject, swarm, nodeID, reason string) (api.MaintenanceStarted, error) {
	out, err := c.Ops.EnterMaintenance(actor, swarm, nodeID, reason)
	return out, MapCommandError(err)
}

// LeaveMaintenance implements api.Maintenance.
func (c *Control) LeaveMaintenance(_ context.Context, actor authz.Subject, swarm, nodeID string) (api.MaintenanceStarted, error) {
	out, err := c.Ops.LeaveMaintenance(actor, swarm, nodeID)
	return out, MapCommandError(err)
}
