// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Inventory implements api.Inventory and api.Activity from the replicated
// store. Reads are local to this replica. A state is only reported when an
// observation exists and is younger than StaleAfter; everything else is
// api.StateUnknown. Nothing here invents a healthy state.
type Inventory struct {
	Store *Store
	// StaleAfter bounds the age of observations (default 2 minutes).
	StaleAfter time.Duration
	Now        func() time.Time
	// MaxEvents bounds the returned events, newest kept (default 500).
	MaxEvents int
}

var (
	_ api.Inventory = Inventory{}
	_ api.Activity  = Inventory{}
)

func (inv Inventory) now() time.Time {
	if inv.Now != nil {
		return inv.Now()
	}
	return time.Now()
}

func (inv Inventory) fresh(at time.Time) bool {
	limit := inv.StaleAfter
	if limit <= 0 {
		limit = 2 * time.Minute
	}
	d := inv.now().Sub(at)
	return !at.IsZero() && d >= 0 && d <= limit
}

func (inv Inventory) observed(o *Observation) (api.State, time.Time) {
	if o == nil || !inv.fresh(o.At) {
		if o != nil {
			return api.StateUnknown, o.At
		}
		return api.StateUnknown, time.Time{}
	}
	switch o.State {
	case api.StateHealthy, api.StateDegraded, api.StateUnavailable:
		return o.State, o.At
	}
	return api.StateUnknown, o.At
}

// Swarms implements api.Inventory.
func (inv Inventory) Swarms(context.Context) ([]api.Swarm, error) {
	recs, err := List[SwarmRecord](inv.Store, CollSwarms)
	if err != nil {
		return nil, err
	}
	out := make([]api.Swarm, 0, len(recs))
	for _, r := range recs {
		st, at := inv.observed(r.Value.Observed)
		out = append(out, api.Swarm{ID: r.Value.ID, Name: r.Value.Name, State: st, ObservedAt: at})
	}
	return out, nil
}

func nodeState(h NodeObservation, fresh bool) api.State {
	if !fresh {
		return api.StateUnknown
	}
	switch h.Health.State {
	case galera.HealthHealthy:
		return api.StateHealthy
	case galera.HealthDegraded:
		return api.StateDegraded
	case galera.HealthUnhealthy:
		return api.StateUnavailable
	}
	return api.StateUnknown
}

// nodeRole maps wsrep_local_state_comment of a fresh observation.
func nodeRole(h NodeObservation, fresh bool) string {
	if !fresh || h.Health.State == galera.HealthUnknown {
		return api.NodeRoleUnknown
	}
	switch ls := h.Health.LocalState; {
	case ls == "Synced":
		return api.NodeRoleSynced
	case strings.HasPrefix(ls, "Donor"):
		return api.NodeRoleDonor
	case strings.HasPrefix(ls, "Joining"):
		return api.NodeRoleJoining
	case ls == "Joined":
		return api.NodeRoleJoined
	case ls == "Initialized":
		return api.NodeRoleInitialized
	}
	return api.NodeRoleUnknown
}

// DefaultDBPort is the MariaDB port of every node service (the Galera spec
// does not change it).
const DefaultDBPort = 3306

func (inv Inventory) cluster(r ClusterRecord, idx backupIndex) api.Cluster {
	st, at := inv.observed(r.Observed)
	c := api.Cluster{ID: r.ID, Swarm: r.Swarm, Name: r.Name, Engine: r.Engine, State: st, ObservedAt: at, Nodes: []api.ClusterNode{},
		LastBackupAt: idx.lastAt[r.ID], LastVerifiedRestoreAt: idx.verified[r.ID], AutomationPolicy: "manual"}
	if r.AutoReplace {
		c.AutomationPolicy = "auto-replace"
	}
	// No proxy is deployed yet: the configured endpoints are the node
	// services on the cluster network.
	for _, n := range r.Nodes {
		c.Endpoints = append(c.Endpoints, api.Endpoint{Name: n.Slot, Host: galera.ServiceName(r.Name, n.Slot), Port: DefaultDBPort})
	}
	for _, n := range r.Nodes {
		h, ok := r.Health[n.Slot]
		fresh := ok && inv.fresh(h.ObservedAt)
		c.Nodes = append(c.Nodes, api.ClusterNode{ID: n.Slot, Host: n.HostID, State: nodeState(h, fresh),
			Role: nodeRole(h, fresh), ObservedAt: h.ObservedAt})
	}
	return c
}

// Clusters implements api.Inventory.
func (inv Inventory) Clusters(context.Context) ([]api.Cluster, error) {
	recs, err := List[ClusterRecord](inv.Store, CollClusters)
	if err != nil {
		return nil, err
	}
	idx, err := loadBackupIndex(inv.Store)
	if err != nil {
		return nil, err
	}
	out := make([]api.Cluster, 0, len(recs))
	for _, r := range recs {
		out = append(out, inv.cluster(r.Value, idx))
	}
	return out, nil
}

// Cluster implements api.Inventory.
func (inv Inventory) Cluster(_ context.Context, id string) (api.Cluster, error) {
	r, ok, err := Get[ClusterRecord](inv.Store, CollClusters, id)
	if err != nil {
		return api.Cluster{}, err
	}
	if !ok {
		return api.Cluster{}, api.ErrNotFound
	}
	idx, err := loadBackupIndex(inv.Store)
	if err != nil {
		return api.Cluster{}, err
	}
	return inv.cluster(r.Value, idx), nil
}

// operationState maps a runner status. A paused operation waits for a
// resume or reconcile decision and is reported pending; a stopped one (its
// runner lost leadership) is unknown until a new leader takes it over.
func operationState(rec OperationRecord) api.OperationState {
	if rec.View == nil {
		return api.OperationPending
	}
	switch rec.View.Status {
	case runner.StatusRunning:
		return api.OperationRunning
	case runner.StatusSucceeded:
		return api.OperationSucceeded
	case runner.StatusFailed:
		return api.OperationFailed
	case runner.StatusAborted:
		// Lab E4 failcreate (A3): an operator abort was reported "failed"
		// with the reason "aborted at a safe boundary".
		return api.OperationAborted
	case runner.StatusPaused:
		return api.OperationPending
	}
	return api.OperationUnknown
}

// Operations implements api.Activity, newest first.
func (inv Inventory) Operations(context.Context) ([]api.Operation, error) {
	recs, err := List[OperationRecord](inv.Store, CollOperations)
	if err != nil {
		return nil, err
	}
	approvals, err := List[ApprovalRecord](inv.Store, CollApprovals)
	if err != nil {
		return nil, err
	}
	approved := map[string]ApprovalRecord{}
	for _, a := range approvals {
		approved[a.Key] = a.Value
	}
	out := make([]api.Operation, 0, len(recs))
	for _, r := range recs {
		v := r.Value
		op := api.Operation{ID: v.ID, Kind: v.Kind, Swarm: v.Swarm, Cluster: v.Cluster,
			State: operationState(v), StartedAt: v.RequestedAt, EndedAt: v.EndedAt}
		if v.View != nil {
			op.Reason = v.View.Reason
			op.Progress = operationProgress(*v.View, v.UpdatedAt)
			op.ExecutedBy = executedBy(*v.View)
		}
		if v.ApprovalID != "" {
			op.Approval = &api.OperationApproval{Digest: v.ApprovalDigest}
			if a, ok := approved[v.ApprovalID]; ok && a.Digest == v.ApprovalDigest {
				op.Approval.ApprovedBy, op.Approval.ApprovedAt = a.By, a.Approved
			}
		}
		out = append(out, op)
	}
	slices.SortStableFunc(out, func(a, b api.Operation) int { return b.StartedAt.Compare(a.StartedAt) })
	return out, nil
}

// operationProgress derives the step progress from a stored runner view:
// the first step that has not succeeded and the latest wait or retry entry
// of its timeline.
func operationProgress(v runner.View, updated time.Time) *api.OperationProgress {
	if len(v.Steps) == 0 {
		return nil
	}
	p := &api.OperationProgress{StepsTotal: len(v.Steps), UpdatedAt: updated}
	cur := -1
	for i, s := range v.Steps {
		if s.State == opjournal.Succeeded {
			p.StepsDone++
		} else if cur < 0 {
			cur = i
		}
	}
	if cur < 0 {
		return p
	}
	s := v.Steps[cur]
	p.Step, p.Command, p.Target, p.State, p.Attempts = s.ID, string(s.Command), s.Target, string(s.State), s.Attempts
	for _, e := range v.Timeline {
		if e.StepID != s.ID {
			continue
		}
		p.Since = e.At
		switch e.Type {
		case runner.EventWait:
			p.Polls++
			p.Waiting = e.Detail
		case runner.EventRetry:
			p.Polls++
			if p.Waiting == "" {
				p.Waiting = e.Detail
			}
		case runner.EventStart:
			p.Waiting = ""
		case runner.EventResult:
			if !strings.HasPrefix(e.Detail, string(agent.StatusSucceeded)) {
				p.LastError = runner.Redact(e.Detail)
			}
		}
	}
	return p
}

// Events implements api.Activity, newest first.
func (inv Inventory) Events(context.Context) ([]api.Event, error) {
	recs, err := List[EventRecord](inv.Store, CollEvents)
	if err != nil {
		return nil, err
	}
	limit := inv.MaxEvents
	if limit <= 0 {
		limit = 500
	}
	if len(recs) > limit {
		recs = recs[len(recs)-limit:]
	}
	out := make([]api.Event, 0, len(recs))
	for i := len(recs) - 1; i >= 0; i-- {
		e := recs[i].Value
		out = append(out, api.Event{ID: e.ID, Time: e.Time, Swarm: e.Swarm, Cluster: e.Cluster,
			Severity: e.Severity, Message: strings.TrimSpace(e.Message)})
	}
	return out, nil
}
