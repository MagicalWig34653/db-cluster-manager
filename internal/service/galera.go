// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// TypeGalera is the registry name of the MariaDB Galera type.
const TypeGalera = "galera"

// Galera expresses a MariaDB Galera cluster as a service type without
// changing its safety logic. Every plan comes from internal/galera
// unchanged: creation from galera.PlanCreate, single-node replacement from
// galera.PlanReplacement (quorum and verified-donor checks on fresh
// observations, galera.ErrRequiresGuidedRecovery without override),
// full-outage recovery from galera.RecoveryPlan (always waits for an
// approval), host maintenance from galera.MaintenancePlan. The type only
// decodes the spec, passes the inputs through and describes the resulting
// plan in the framework's step list; the plan itself travels in
// Plan.Galera. Execution stays with the cluster flows (manager
// Operations.CreateCluster, ReplaceNode, RecoverOutage, DeleteCluster and
// runner.FromGaleraPlan); runner.FromServicePlan refuses delegated plans.
//
// Things the type deliberately does not do: change the cluster size (no
// planner exists; ErrGaleraScale), update the image through the generic
// update strategy (Galera updates go node by node behind wait-synced
// gates; the type is not an Updater), or delete from the spec alone (the
// deletion needs the inventoried service IDs and inspection sessions, so
// the cluster deletion plans it; ErrDelegated).
type Galera struct {
	// NewUUID is passed to the galera planner; nil uses random UUIDs.
	NewUUID func() (string, error)
}

var _ Type = Galera{}

// Errors of the galera type.
var (
	// ErrDelegated: the request is planned by a cluster flow, not by the
	// service type.
	ErrDelegated = errors.New("service: planned by the Galera cluster flow")
	// ErrGaleraScale: there is no planner for changing a cluster's size.
	ErrGaleraScale = errors.New("service: changing the size of a Galera cluster is not supported")
)

// GaleraSpec is the desired state of a Galera cluster. It has the fields of
// the cluster creation request (POST /clusters) other than swarm and name.
type GaleraSpec struct {
	Size               int                    `json:"size"`
	Image              string                 `json:"image"`
	Network            string                 `json:"network"`
	RootPasswordSecret string                 `json:"rootPasswordSecret"`
	GcacheSize         string                 `json:"gcacheSize,omitempty"`
	Resources          galera.Resources       `json:"resources"`
	Placement          galera.PlacementPolicy `json:"placement"`
	AutoReplace        bool                   `json:"autoReplace,omitempty"`
}

// DecodeGaleraSpec decodes a galera spec strictly. Planning validates the
// rest (internal/galera).
func DecodeGaleraSpec(raw json.RawMessage) (GaleraSpec, error) {
	var g GaleraSpec
	if err := decodeStrict(raw, &g); err != nil {
		return GaleraSpec{}, err
	}
	var p problems
	if g.Size < galera.MinClusterSize {
		p.addf("size %d below the minimum %d", g.Size, galera.MinClusterSize)
	}
	if g.Image == "" || g.Network == "" || g.RootPasswordSecret == "" {
		p.addf("image, network and rootPasswordSecret are required")
	}
	if err := g.Resources.Validate(); err != nil {
		p.addf("%v", err)
	}
	return g, p.err()
}

// Name implements Type.
func (Galera) Name() string { return TypeGalera }

// Validate implements Type.
func (Galera) Validate(spec json.RawMessage) error {
	_, err := DecodeGaleraSpec(spec)
	return err
}

// GaleraInput is the full input of a cluster creation plan: the facts the
// generic PlanInput has no field for (host capacity in galera units,
// configured database accounts).
type GaleraInput struct {
	Context
	Spec     json.RawMessage
	Hosts    []galera.Host
	Accounts galera.Accounts
}

// Topology returns exactly the galera.PlanCreate input the manager's
// cluster creation builds from the same request.
func (g Galera) Topology(in GaleraInput) (galera.Topology, galera.PlacementPolicy, error) {
	if err := in.check(); err != nil {
		return galera.Topology{}, galera.PlacementPolicy{}, err
	}
	s, err := DecodeGaleraSpec(in.Spec)
	if err != nil {
		return galera.Topology{}, galera.PlacementPolicy{}, err
	}
	return galera.Topology{
		Cluster: galera.Cluster{InstallationID: in.InstallationID, ClusterID: in.ServiceID, Name: in.Name, Image: s.Image,
			Network: s.Network, RootPasswordSecret: s.RootPasswordSecret, GcacheSize: s.GcacheSize, Accounts: in.Accounts},
		Size: s.Size, Resources: s.Resources, NewUUID: g.NewUUID,
	}, s.Placement, nil
}

// PlanCluster delegates to galera.PlanCreate.
func (g Galera) PlanCluster(in GaleraInput) (galera.Plan, error) {
	topo, pol, err := g.Topology(in)
	if err != nil {
		return galera.Plan{}, err
	}
	return galera.PlanCreate(topo, in.Hosts, pol)
}

// PlanReplacement delegates to galera.PlanReplacement. A refusal
// (galera.ErrRequiresGuidedRecovery, ErrReplacementPaused, ...) is
// returned unchanged.
func (Galera) PlanReplacement(st galera.ClusterState, slot string, pol galera.ReplacementPolicy) (galera.Plan, error) {
	return galera.PlanReplacement(st, slot, pol)
}

// PlanRecovery delegates to galera.RecoveryPlan; the plan requires an
// approval and starts with the approved stop.
func (Galera) PlanRecovery(d galera.Diagnosis, c galera.Cluster, nodes []galera.Node) (galera.Plan, error) {
	return galera.RecoveryPlan(d, c, nodes)
}

// PlanMaintenance delegates to galera.MaintenancePlan (end=false) or
// galera.MaintenanceEndPlan.
func (Galera) PlanMaintenance(c galera.Cluster, nodes []galera.Node, hostID string, end bool) (galera.Plan, error) {
	if end {
		return galera.MaintenanceEndPlan(c, nodes, hostID)
	}
	return galera.MaintenancePlan(c, nodes, hostID)
}

// PlanScale refuses: no planner changes a cluster's size. Replacement
// keeps the size.
func (Galera) PlanScale(int) (galera.Plan, error) {
	return galera.Plan{}, fmt.Errorf("%w: replace failed nodes instead, or create a new cluster", ErrGaleraScale)
}

// GaleraHosts converts the framework's host view into the galera planner's.
func GaleraHosts(hs []Host) []galera.Host {
	if hs == nil {
		return nil
	}
	out := make([]galera.Host, 0, len(hs))
	for _, h := range hs {
		out = append(out, galera.Host{ID: h.ID, Name: h.Name, Ready: h.Ready, Active: h.Active, Spare: h.Spare,
			Capacity:  galera.ResourceAmount{MilliCPU: h.Capacity.MilliCPU, MemoryBytes: h.Capacity.MemoryBytes},
			Allocated: galera.ResourceAmount{MilliCPU: h.Allocated.MilliCPU, MemoryBytes: h.Allocated.MemoryBytes}})
	}
	return out
}

// Plan implements Type: the creation plan from galera.PlanCreate, described.
// Without accounts in the generic input the galera planner warns that no
// monitoring account is configured; the manager passes them through
// GaleraInput.
func (g Galera) Plan(in PlanInput) (Plan, error) {
	gp, err := g.PlanCluster(GaleraInput{Context: in.Context, Spec: in.Spec, Hosts: GaleraHosts(in.Hosts)})
	if err != nil {
		return Plan{}, err
	}
	return DescribeGaleraPlan(in.Context, gp), nil
}

// DeletePlan implements Type. The deletion of a cluster is planned by the
// cluster deletion flow from the inventory, not from the spec.
func (Galera) DeletePlan(PlanInput) (Plan, error) {
	return Plan{}, fmt.Errorf("%w: delete the cluster (DELETE /clusters/{id})", ErrDelegated)
}

// Health implements Type from the Swarm task view of the node services.
// The database verdict (wsrep state, quorum) is the cluster health the
// manager's health collector derives; this is only the task view.
func (Galera) Health(in HealthInput) (Health, error) {
	if _, err := DecodeGaleraSpec(in.Spec); err != nil {
		return Health{}, err
	}
	h := observationHealth(in.Observed)
	if h.State == HealthHealthy {
		h.Reasons = append(h.Reasons, "task view only; the wsrep cluster health is reported on the cluster")
	}
	return h, nil
}

// DescribeGaleraPlan describes a galera plan in the framework's terms. The
// plan itself is attached unchanged.
func DescribeGaleraPlan(ctx Context, gp galera.Plan) Plan {
	kind := PlanKind(gp.Kind)
	if gp.Kind == galera.PlanCreateCluster {
		kind = PlanCreate
	}
	p := Plan{Kind: kind, Type: TypeGalera, ServiceID: gp.ClusterID, Name: gp.Cluster.Name, RequiresApproval: true,
		Warnings: append([]string(nil), gp.Warnings...)}
	if p.Name == "" {
		p.Name = ctx.Name
	}
	for _, gs := range gp.Steps {
		s := Step{ID: gs.ID, Kind: StepKind(gs.Kind), HostID: gs.HostID, Volume: gs.Volume, NodeUUID: gs.NodeUUID, Note: gs.Note}
		for _, pc := range gs.Preconditions {
			switch pc.Kind {
			case galera.PreStepSucceeded:
				s.After = append(s.After, pc.StepID)
			case galera.PreNodeSynced:
				s.Conditions = append(s.Conditions, fmt.Sprintf("node %s Synced and Primary at cluster size %d", pc.Slot, pc.ClusterSize))
			case galera.PreQuorum:
				s.Conditions = append(s.Conditions, fmt.Sprintf("quorum: at least %d nodes in the Primary component", pc.ClusterSize))
			case galera.PreApproved:
				s.Conditions = append(s.Conditions, "approved by a user")
			case galera.PreNoNodeRunning:
				s.Conditions = append(s.Conditions, "no database task of the cluster runs")
			default:
				s.Conditions = append(s.Conditions, string(pc.Kind))
			}
		}
		switch gs.Kind {
		case galera.StepWaitSynced:
			s.Conditions = append(s.Conditions, fmt.Sprintf("node %s Synced and Primary at cluster size %d", gs.Slot, gs.ClusterSize))
		case galera.StepRemoveService, galera.StepStopAll, galera.StepMarkSafe:
			s.Destructive = true
		}
		if s.Note == "" {
			s.Note = strings.TrimSpace(fmt.Sprintf("%s %s", gs.Kind, gs.Slot))
			if gs.Slot != "" {
				s.Note = fmt.Sprintf("%s for node %s", gs.Kind, gs.Slot)
			}
		}
		if gs.Spec != nil {
			s.ServiceName = gs.Spec.Name
		}
		p.Steps = append(p.Steps, s)
	}
	cp := gp
	p.Galera = &cp
	return p
}
