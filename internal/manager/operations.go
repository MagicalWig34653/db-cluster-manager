// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Operations glue: user commands that start runner operations.
//
// Every command checks the actor's permission through the Authorizer (which
// audits state-changing decisions and fails closed without audit), runs on
// the current leadership only, stores the operation definition and the
// desired state in one replicated transaction before the runner journals
// it, and executes it in a goroutine bound to the leadership. A new leader
// takes it over from the stored definition.
//
// Safety rules kept here:
//   - Replacement is planned by galera.PlanReplacement, which refuses with
//     galera.ErrRequiresGuidedRecovery unless quorum and a verified donor are
//     observed on fresh data. That error is returned unchanged; there is no
//     override.
//   - Full-outage recovery is diagnosis only (read-only). Its plan is never
//     turned into an operation here (runner.FromGaleraPlan refuses it too).

// Errors of Operations.
var (
	// ErrSettingsIncomplete: installation ID or volume base not configured.
	ErrSettingsIncomplete = errors.New("manager: settings incomplete (installationId and volumeBase required)")
	// ErrClusterExists: a cluster with that name exists in the Swarm.
	ErrClusterExists = errors.New("manager: cluster name already used in this swarm")
	// ErrMissingServiceID: the failed slot's service ID is not inventoried.
	ErrMissingServiceID = errors.New("manager: service ID of the slot is not known")
)

// Operations starts and manages operations on behalf of users.
type Operations struct {
	Svc        *Service
	Authorizer *authz.Authorizer
	// StepTimeout and MaxRetries are passed to every planned step.
	StepTimeout time.Duration
	MaxRetries  int
	// Swarms, if set, resolves the operator-provided objects an operation
	// references so the agents can be told to allow them (allowrefs.go).
	Swarms *Swarms
	// Notify, if set, receives the recovery notifications (monitoring
	// dispatcher); events are recorded either way.
	Notify func(ctx context.Context, e notify.Event) bool
}

// ErrNoServiceHost: no enrolled Swarm manager node with an enrolled agent
// can run the service commands of an operation.
var ErrNoServiceHost = errors.New("manager: no enrolled Swarm manager node with an agent identity")

// serviceHost picks the agent that runs Swarm service commands: an enrolled,
// ready Swarm manager node whose agent has enrolled its identity (the
// lowest node ID, so the choice is stable).
func (o *Operations) serviceHost(swarm string) (string, error) {
	hosts, err := List[HostRecord](o.Svc.store, CollHosts, Consistent())
	if err != nil {
		return "", err
	}
	var ids []string
	for _, h := range hosts {
		v := h.Value
		if v.Swarm == swarm && v.Enrollment == EnrollEnrolled && v.Role == "manager" && v.State == "ready" && v.AgentSerial != "" {
			ids = append(ids, v.NodeID)
		}
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("%w in swarm %s", ErrNoServiceHost, swarm)
	}
	slices.Sort(ids)
	return ids[0], nil
}

// serviceHostFor returns the service host in production (Swarms set) and
// "" (each step on its node's host) for unit tests without a Swarm
// connector.
func (o *Operations) serviceHostFor(swarm string) (string, error) {
	if o.Swarms == nil {
		return "", nil
	}
	return o.serviceHost(swarm)
}

// nodeHosts returns the host IDs of nodes.
func nodeHosts(nodes []galera.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.HostID)
	}
	return out
}

// clusterRefs are the operator-provided objects a cluster's services use.
func clusterRefs(c galera.Cluster) objectRefs {
	return objectRefs{Networks: []string{c.Network}, Secrets: append([]string{c.RootPasswordSecret}, c.TLS.Secrets...)}
}

func (o *Operations) leadership() (*Leadership, error) {
	l := o.Svc.Current()
	if l == nil || !l.Valid() {
		return nil, ErrNotLeader
	}
	return l, nil
}

func (o *Operations) authorize(actor authz.Subject, act authz.Action, r authz.Resource) error {
	d := o.Authorizer.Authorize(actor, act, r)
	if !d.Allowed {
		return &authz.DeniedError{Decision: d}
	}
	return nil
}

func clusterRes(swarm, cluster string) authz.Resource {
	return authz.Resource{Type: authz.ResourceCluster, Scope: authz.ClusterScope(swarm, cluster)}
}

// CreateClusterRequest is a topology request for a new MariaDB Galera
// cluster.
type CreateClusterRequest struct {
	Swarm              string                 `json:"swarm"`
	Name               string                 `json:"name"`
	Size               int                    `json:"size"`
	Image              string                 `json:"image"`
	Network            string                 `json:"network"`
	RootPasswordSecret string                 `json:"rootPasswordSecret"`
	GcacheSize         string                 `json:"gcacheSize,omitempty"`
	Binlog             bool                   `json:"binlog,omitempty"`
	Resources          galera.Resources       `json:"resources"`
	Placement          galera.PlacementPolicy `json:"placement"`
	AutoReplace        bool                   `json:"autoReplace,omitempty"`
}

// enrolledHosts returns the planner view of a Swarm's enrolled hosts with
// the demand of managed database nodes already placed on them.
func (o *Operations) enrolledHosts(swarm string) ([]galera.Host, error) {
	hosts, err := List[HostRecord](o.Svc.store, CollHosts, Consistent())
	if err != nil {
		return nil, err
	}
	clusters, err := List[ClusterRecord](o.Svc.store, CollClusters)
	if err != nil {
		return nil, err
	}
	alloc := map[string]galera.ResourceAmount{}
	for _, c := range clusters {
		for _, n := range c.Value.Nodes {
			d := n.Resources.Demand()
			a := alloc[n.HostID]
			a.MilliCPU += d.MilliCPU
			a.MemoryBytes += d.MemoryBytes
			alloc[n.HostID] = a
		}
	}
	var out []galera.Host
	for _, h := range hosts {
		v := h.Value
		if v.Swarm != swarm || v.Enrollment != EnrollEnrolled {
			continue
		}
		out = append(out, galera.Host{
			ID: v.NodeID, Name: v.Hostname, Ready: v.State == "ready", Active: v.Availability == "active", Spare: v.Spare,
			Capacity:  galera.ResourceAmount{MilliCPU: v.NanoCPUs / 1e6, MemoryBytes: v.MemoryBytes},
			Allocated: alloc[v.NodeID],
		})
	}
	return out, nil
}

func (o *Operations) settings() (Settings, error) {
	st, ok, err := Get[Settings](o.Svc.store, CollSettings, SettingsKey, Consistent())
	if err != nil {
		return Settings{}, err
	}
	if !ok || st.Value.InstallationID == "" || st.Value.VolumeBase == "" {
		return Settings{}, ErrSettingsIncomplete
	}
	return st.Value, nil
}

func tokens(nodes []galera.Node, slots ...string) map[string]string {
	out := map[string]string{}
	for _, n := range nodes {
		if len(slots) == 0 || slices.Contains(slots, n.Slot) {
			out[n.Slot] = newID("tok")
		}
	}
	return out
}

// CreateCluster plans a new cluster (galera.PlanCreate), converts it into
// an operation (runner.FromGaleraPlan) and starts it.
func (o *Operations) CreateCluster(actor authz.Subject, req CreateClusterRequest) (OperationRecord, error) {
	rec, _, err := o.createCluster(actor, req)
	return rec, err
}

// preparedCluster is a planned cluster creation that is not stored yet.
type preparedCluster struct {
	clusterID string
	plan      galera.Plan
	op        runner.Operation
}

// prepareCreateCluster checks, authorizes and plans a cluster creation
// without changing anything. CreateCluster and the galera service type
// (services.go) share it, so both produce the same operation.
func (o *Operations) prepareCreateCluster(actor authz.Subject, req CreateClusterRequest) (preparedCluster, error) {
	if _, ok, err := Get[SwarmRecord](o.Svc.store, CollSwarms, req.Swarm, Consistent()); err != nil {
		return preparedCluster{}, err
	} else if !ok {
		return preparedCluster{}, fmt.Errorf("%w: swarm %q", api.ErrNotFound, req.Swarm)
	}
	if err := o.authorize(actor, authz.ActionCreate, authz.Resource{Type: authz.ResourceCluster, Scope: authz.SwarmScope(req.Swarm)}); err != nil {
		return preparedCluster{}, err
	}
	st, err := o.settings()
	if err != nil {
		return preparedCluster{}, err
	}
	existing, err := List[ClusterRecord](o.Svc.store, CollClusters)
	if err != nil {
		return preparedCluster{}, err
	}
	for _, c := range existing {
		if c.Value.Swarm == req.Swarm && c.Value.Name == req.Name {
			return preparedCluster{}, fmt.Errorf("%w: %s", ErrClusterExists, req.Name)
		}
	}
	hosts, err := o.enrolledHosts(req.Swarm)
	if err != nil {
		return preparedCluster{}, err
	}
	clusterID := newID("clu")
	plan, err := galera.PlanCreate(galera.Topology{
		Cluster: galera.Cluster{InstallationID: st.InstallationID, ClusterID: clusterID, Name: req.Name, Image: req.Image,
			Network: req.Network, RootPasswordSecret: req.RootPasswordSecret, GcacheSize: req.GcacheSize, Binlog: req.Binlog,
			Accounts: st.Accounts},
		Size: req.Size, Resources: req.Resources,
	}, hosts, req.Placement)
	if err != nil {
		return preparedCluster{}, err
	}
	svcHost, err := o.serviceHostFor(req.Swarm)
	if err != nil {
		return preparedCluster{}, err
	}
	opID := newID("op")
	op, err := runner.FromGaleraPlan(plan, runner.GaleraBindings{
		OpID: opID, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase, TokenIDs: tokens(plan.Nodes),
		StepTimeout: o.StepTimeout, MaxRetries: o.MaxRetries, ServiceHost: svcHost,
	})
	if err != nil {
		return preparedCluster{}, err
	}
	if err := o.allowReferenced(&op, req.Swarm, st.InstallationID, append(nodeHosts(plan.Nodes), svcHost), clusterRefs(plan.Cluster)); err != nil {
		return preparedCluster{}, err
	}
	return preparedCluster{clusterID: clusterID, plan: plan, op: op}, nil
}

func (o *Operations) createCluster(actor authz.Subject, req CreateClusterRequest) (OperationRecord, galera.Plan, error) {
	l, err := o.leadership()
	if err != nil {
		return OperationRecord{}, galera.Plan{}, err
	}
	pc, err := o.prepareCreateCluster(actor, req)
	if err != nil {
		return OperationRecord{}, galera.Plan{}, err
	}
	clusterID, plan, op, opID := pc.clusterID, pc.plan, pc.op, pc.op.ID
	now := o.Svc.cfg.Now()
	cluster := ClusterRecord{
		ID: clusterID, Swarm: req.Swarm, Name: req.Name, Engine: "mariadb-galera", Spec: plan.Cluster, Size: req.Size,
		Resources: req.Resources, Placement: req.Placement, Nodes: plan.Nodes, LastKnownSize: req.Size,
		AutoReplace: req.AutoReplace, CreatedBy: actor.ID, CreatedAt: now, CreateOp: opID,
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: req.Swarm, Cluster: clusterID, RequestedBy: actor.ID,
		RequestedAt: now, Definition: op, Nodes: plan.Nodes}
	if err := o.commit(rec, &cluster); err != nil {
		return OperationRecord{}, galera.Plan{}, err
	}
	o.Svc.recordEvent(req.Swarm, clusterID, api.SeverityInfo, fmt.Sprintf("%s requested cluster %s (%d nodes), operation %s", actor.ID, req.Name, req.Size, opID))
	o.start(l, op, func(v runner.View) {
		ids := createdServices(plan, v)
		_, _, err := Update(o.Svc.store, CollClusters, clusterID, func(cr ClusterRecord, exists bool) (ClusterRecord, error) {
			if !exists {
				return cr, ErrNotFound
			}
			cr.ServiceID = ids
			return cr, nil
		})
		if err != nil {
			o.Svc.log.Warn("manager: service IDs not recorded", "cluster", clusterID, "err", err)
		}
	})
	return rec, plan, nil
}

// commit stores the operation record (and the cluster record, if given)
// atomically; both must not exist yet.
func (o *Operations) commit(rec OperationRecord, cluster *ClusterRecord) error {
	opDoc, err := PutOp(CollOperations, rec.ID, rec, IfVersion(0))
	if err != nil {
		return err
	}
	ops := []metastore.DocOp{opDoc}
	if cluster != nil {
		cd, err := PutOp(CollClusters, cluster.ID, cluster, IfVersion(0))
		if err != nil {
			return err
		}
		ops = append(ops, cd)
	}
	_, err = o.Svc.store.Txn(ops...)
	return err
}

// start runs op on the leadership. onSuccess runs after the operation
// succeeded (on the same leadership).
func (o *Operations) start(l *Leadership, op runner.Operation, onSuccess func(runner.View)) {
	l.Go(func(ctx context.Context) {
		v, err := l.Runner.Run(ctx, op)
		if v.ID == "" {
			// Nothing was journaled (not leader, busy, invalid). Record that
			// the operation did not start rather than leaving it pending.
			reason := "not started"
			if err != nil {
				reason = err.Error()
			}
			v = runner.View{ID: op.ID, Kind: op.Kind, Status: runner.StatusStopped, Term: l.Term, Reason: reason, Locks: op.Locks}
		}
		_ = o.Svc.saveView(v)
		if v.Status == runner.StatusSucceeded && onSuccess != nil {
			onSuccess(v)
		}
	})
}

// createdServices maps slots to the service IDs reported by the agents
// create-service results ({"id": ...}) of a finished operation.
func createdServices(plan galera.Plan, v runner.View) map[string]string {
	slotOf := map[string]string{}
	for _, st := range plan.Steps {
		if st.Kind == galera.StepCreateService {
			slotOf[st.ID] = st.Slot
		}
	}
	out := map[string]string{}
	for _, sv := range v.Steps {
		slot, ok := slotOf[sv.ID]
		if !ok || sv.LastResult == nil {
			continue
		}
		var res struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(sv.LastResult.Output, &res) == nil && res.ID != "" {
			out[slot] = res.ID
		}
	}
	return out
}

// unfinishedFor reports an operation on the cluster that has not ended.
func (o *Operations) unfinishedFor(clusterID string) (string, error) {
	recs, err := List[OperationRecord](o.Svc.store, CollOperations, Consistent())
	if err != nil {
		return "", err
	}
	for _, r := range recs {
		v := r.Value
		if v.Cluster != clusterID {
			continue
		}
		// EndedAt is set when an operation fails and stays set when a new
		// leader adopts it as paused; such an operation still holds its
		// runner locks until it is resumed or aborted.
		if !v.EndedAt.IsZero() && (v.View == nil || terminal(v.View.Status) || v.View.Status == runner.StatusStopped) {
			continue
		}
		if v.View != nil && v.View.Status == runner.StatusStopped && len(v.View.Steps) == 0 {
			continue // never started
		}
		return v.ID, nil
	}
	return "", nil
}

// clusterState builds the planner's view from a cluster record.
func (o *Operations) clusterState(c ClusterRecord) (galera.ClusterState, error) {
	hosts, err := o.enrolledHosts(c.Swarm)
	if err != nil {
		return galera.ClusterState{}, err
	}
	st := galera.ClusterState{Cluster: c.Spec, LastKnownSize: c.LastKnownSize, Hosts: hosts}
	for _, n := range c.Nodes {
		ns := galera.NodeState{Node: n}
		if h, ok := c.Health[n.Slot]; ok {
			ns.Health, ns.ObservedAt = h.Health, h.ObservedAt
		}
		st.Nodes = append(st.Nodes, ns)
	}
	return st, nil
}

// ReplaceNode starts the replacement of a failed node. It never bypasses
// the quorum rules: galera.ErrRequiresGuidedRecovery (and every other
// planning refusal) is returned unchanged and nothing is started.
func (o *Operations) ReplaceNode(actor authz.Subject, clusterID, slot string) (OperationRecord, error) {
	l, err := o.leadership()
	if err != nil {
		return OperationRecord{}, err
	}
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, clusterID, Consistent())
	if err != nil {
		return OperationRecord{}, err
	}
	if !ok {
		return OperationRecord{}, fmt.Errorf("%w: cluster %q", api.ErrNotFound, clusterID)
	}
	c := cur.Value
	if err := o.authorize(actor, authz.ActionOperate, clusterRes(c.Swarm, c.ID)); err != nil {
		return OperationRecord{}, err
	}
	if busy, err := o.unfinishedFor(c.ID); err != nil {
		return OperationRecord{}, err
	} else if busy != "" {
		return OperationRecord{}, fmt.Errorf("%w: operation %s is not finished", runner.ErrBusy, busy)
	}
	st, err := o.settings()
	if err != nil {
		return OperationRecord{}, err
	}
	state, err := o.clusterState(c)
	if err != nil {
		return OperationRecord{}, err
	}
	paused, err := o.pausedHosts(c.Swarm)
	if err != nil {
		return OperationRecord{}, err
	}
	plan, err := galera.PlanReplacement(state, slot, galera.ReplacementPolicy{Placement: c.Placement, Now: o.Svc.cfg.Now(), PausedHosts: paused})
	if err != nil {
		return OperationRecord{}, err
	}
	if c.ServiceID[slot] == "" {
		return OperationRecord{}, fmt.Errorf("%w: %s/%s", ErrMissingServiceID, c.ID, slot)
	}
	svcHost, err := o.serviceHostFor(c.Swarm)
	if err != nil {
		return OperationRecord{}, err
	}
	opID := newID("op")
	op, err := runner.FromGaleraPlan(plan, runner.GaleraBindings{
		OpID: opID, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase,
		TokenIDs: tokens(plan.Nodes, slot), ServiceIDs: map[string]string{slot: c.ServiceID[slot]},
		StepTimeout: o.StepTimeout, MaxRetries: o.MaxRetries, ServiceHost: svcHost,
	})
	if err != nil {
		return OperationRecord{}, err
	}
	if err := o.allowReferenced(&op, c.Swarm, st.InstallationID, append(nodeHosts(plan.Nodes), svcHost), clusterRefs(plan.Cluster)); err != nil {
		return OperationRecord{}, err
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: c.Swarm, Cluster: c.ID, RequestedBy: actor.ID,
		RequestedAt: o.Svc.cfg.Now(), Definition: op, Nodes: plan.Nodes}
	if err := o.commit(rec, nil); err != nil {
		return OperationRecord{}, err
	}
	o.Svc.recordEvent(c.Swarm, c.ID, api.SeverityWarning, fmt.Sprintf("%s requested replacement of %s/%s, operation %s", actor.ID, c.Name, slot, opID))
	nodes := plan.Nodes
	o.start(l, op, func(v runner.View) {
		created := createdServices(plan, v)
		// The new identity becomes the desired state only after success.
		_, _, err := Update(o.Svc.store, CollClusters, c.ID, func(cr ClusterRecord, exists bool) (ClusterRecord, error) {
			if !exists {
				return cr, ErrNotFound
			}
			cr.Nodes = nodes
			if cr.ServiceID == nil {
				cr.ServiceID = map[string]string{}
			}
			delete(cr.ServiceID, slot)
			for k, id := range created {
				cr.ServiceID[k] = id
			}
			delete(cr.Health, slot)
			return cr, nil
		})
		if err != nil {
			o.Svc.log.Warn("manager: cluster record not updated after replacement", "cluster", c.ID, "err", err)
		}
	})
	return rec, nil
}

// DiagnoseOutage evaluates recovery evidence of a cluster without a
// Primary component (A06). It is read-only: it needs read permission,
// starts nothing and returns advice whose plan always requires approval.
// Collecting the evidence (grastate.dat, wsrep-recover) from the agents is
// not wired yet; the caller supplies it.
func (o *Operations) DiagnoseOutage(actor authz.Subject, clusterID string, evidence []galera.NodeInfo) (galera.Diagnosis, error) {
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, clusterID)
	if err != nil {
		return galera.Diagnosis{}, err
	}
	if !ok {
		return galera.Diagnosis{}, fmt.Errorf("%w: cluster %q", api.ErrNotFound, clusterID)
	}
	if d := o.Authorizer.Check(actor, authz.ActionRead, clusterRes(cur.Value.Swarm, clusterID)); !d.Allowed {
		return galera.Diagnosis{}, &authz.DeniedError{Decision: d}
	}
	return galera.DiagnoseFullOutage(evidence)
}

// Resume continues a paused or adopted operation on the current leadership.
func (o *Operations) Resume(actor authz.Subject, opID string) error {
	l, err := o.leadership()
	if err != nil {
		return err
	}
	rec, ok, err := Get[OperationRecord](o.Svc.store, CollOperations, opID, Consistent())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: operation %q", api.ErrNotFound, opID)
	}
	if err := o.authorize(actor, authz.ActionOperate, clusterRes(rec.Value.Swarm, rec.Value.Cluster)); err != nil {
		return err
	}
	l.Go(func(ctx context.Context) {
		v, err := l.Runner.Resume(ctx, opID)
		if err != nil && v.ID == "" {
			return
		}
		_ = o.Svc.saveView(v)
	})
	return nil
}
