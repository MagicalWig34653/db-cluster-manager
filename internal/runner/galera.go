// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/orchestrate"
)

// Agent command kinds and argument types that galera plans use beyond the
// generic ones. They are implemented by internal/agent; the names stay here
// as aliases for the plan translation.
const (
	// KindCreateVolume creates the node's local volume (args: CreateVolumeArgs).
	KindCreateVolume = agent.KindCreateVolume
	// KindUpdateService replaces an owned service's spec (args: UpdateServiceArgs).
	KindUpdateService = agent.KindUpdateService
)

// CreateVolumeArgs are the arguments of KindCreateVolume.
type CreateVolumeArgs = agent.CreateVolumeArgs

// UpdateServiceArgs are the arguments of KindUpdateService. The service is
// named, because a service created earlier in the same operation has no ID
// at planning time.
type UpdateServiceArgs = agent.UpdateServiceArgs

// GaleraBindings supply the facts a plan does not contain.
type GaleraBindings struct {
	OpID           string
	InstallationID string
	// VolumeBase is the agent's volume base; a volume root is VolumeBase/<volume>.
	VolumeBase string
	// TokenIDs maps a slot to the single-use new-identity token the manager
	// issued for its init-identity step.
	TokenIDs map[string]string
	// ServiceIDs maps a slot to the Swarm ID of its existing service
	// (needed for remove-service).
	ServiceIDs map[string]string
	// StepTimeout and MaxRetries apply to every step.
	StepTimeout time.Duration
	MaxRetries  int
	// ServiceHost is the host ID of the agent that runs the Swarm service
	// commands (create-, update- and remove-service): a Swarm manager node,
	// because only managers accept service changes. Lab E4: a worker's
	// agent cannot create services and does not even see the overlay
	// network. Empty keeps each step on its node's host (a Swarm whose
	// hosts are all managers).
	ServiceHost string
	// ApprovalID binds explicit recovery and cluster operation plans to
	// CondApproved. Required whenever the plan contains an approval gate.
	ApprovalID string
}

// Condition kinds produced from galera preconditions (evaluated by a Checker).
const (
	CondNodeSynced    = string(galera.PreNodeSynced)
	CondQuorum        = string(galera.PreQuorum)
	CondApproved      = string(galera.PreApproved)
	CondNoNodeRunning = string(galera.PreNoNodeRunning)
)

// FromGaleraPlan converts a Galera plan into a fenced Operation.
// It is pure data conversion: wait-synced and verify-donor become gates
// whose node-synced condition a Checker evaluates, retain-volume becomes a
// gate without conditions (the old volume is deliberately left alone), and
// the remaining steps become agent commands targeted at the step's host.
// Recovery, scaling and rolling plans need an explicit approval binding and
// must begin with an approval gate; recovery remains a separate guided flow.
func FromGaleraPlan(p galera.Plan, b GaleraBindings) (Operation, error) {
	switch p.Kind {
	case galera.PlanScaleOut, galera.PlanScaleIn, galera.PlanRollingRestart, galera.PlanMinorUpgrade:
		if b.ApprovalID == "" || !p.RequiresApproval {
			return Operation{}, fmt.Errorf("%w: Galera operation needs approval", ErrInvalidOperation)
		}
		if len(p.Steps) == 0 || !slices.ContainsFunc(p.Steps[0].Preconditions, func(c galera.Precondition) bool { return c.Kind == galera.PreApproved }) {
			return Operation{}, fmt.Errorf("%w: missing first approval gate", ErrInvalidOperation)
		}
	case galera.PlanCreateCluster, galera.PlanReplaceNode, galera.PlanHostMaintenance, galera.PlanHostMaintenanceEnd:
	case galera.PlanFullOutageRecovery:
		// Guided recovery (A06): only with an approval to wait for, and the
		// plan must start with the approved stop.
		if b.ApprovalID == "" || !p.RequiresApproval {
			return Operation{}, fmt.Errorf("%w: full-outage recovery needs an approval ID", ErrInvalidOperation)
		}
		if len(p.Steps) == 0 || !slices.ContainsFunc(p.Steps[0].Preconditions, func(c galera.Precondition) bool { return c.Kind == galera.PreApproved }) {
			return Operation{}, fmt.Errorf("%w: full-outage recovery must start with the approved step", ErrInvalidOperation)
		}
	default:
		return Operation{}, fmt.Errorf("%w: plan kind %q is not executable by the runner", ErrInvalidOperation, p.Kind)
	}
	if b.OpID == "" || b.InstallationID == "" || b.VolumeBase == "" || p.ClusterID == "" {
		return Operation{}, fmt.Errorf("%w: OpID, InstallationID, VolumeBase and plan ClusterID required", ErrInvalidOperation)
	}
	op := Operation{ID: b.OpID, Kind: "galera/" + string(p.Kind), Locks: []string{"cluster/" + p.ClusterID}}
	slots := map[string]bool{}
	for _, gs := range p.Steps {
		s, err := convertStep(p, gs, b)
		if err != nil {
			return Operation{}, fmt.Errorf("step %s: %w", gs.ID, err)
		}
		if gs.Slot != "" && !slots[gs.Slot] {
			slots[gs.Slot] = true
			op.Locks = append(op.Locks, "node/"+p.ClusterID+"/"+gs.Slot)
		}
		op.Steps = append(op.Steps, s)
	}
	if err := op.Validate(); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func convertStep(p galera.Plan, gs galera.Step, b GaleraBindings) (Step, error) {
	s := Step{ID: gs.ID, Target: gs.HostID, Timeout: b.StepTimeout, MaxRetries: b.MaxRetries, Note: gs.Note}
	for _, pc := range gs.Preconditions {
		c := Condition{Kind: string(pc.Kind), StepID: pc.StepID, Target: pc.Slot, ClusterSize: pc.ClusterSize}
		switch pc.Kind {
		case galera.PreStepSucceeded:
			c = Condition{Kind: CondStepSucceeded, StepID: pc.StepID}
		case galera.PreApproved:
			if b.ApprovalID == "" {
				return Step{}, fmt.Errorf("%w: approval precondition without an approval ID", ErrInvalidOperation)
			}
			c = Condition{Kind: CondApproved, Target: b.ApprovalID}
		case galera.PreNoNodeRunning:
			// Evaluated by the plan's await-stopped steps (agent
			// KindAwaitStopped on a Swarm manager node): every service
			// was scaled to 0 and reported no running task. Nothing in
			// the plan scales a service up before the bootstrap.
			ids := awaitStoppedSteps(p)
			if len(ids) == 0 {
				return Step{}, fmt.Errorf("%w: no-node-running without await-stopped steps", ErrInvalidOperation)
			}
			for _, id := range ids {
				s.Preconditions = append(s.Preconditions, Condition{Kind: CondStepSucceeded, StepID: id})
			}
			continue
		}
		s.Preconditions = append(s.Preconditions, c)
	}
	root := func() string { return filepath.Join(b.VolumeBase, gs.Volume) }
	var args any
	switch gs.Kind {
	case galera.StepCreateVolume:
		labels := map[string]string{agent.LabelCluster: p.ClusterID}
		for _, n := range p.Nodes {
			if n.Slot == gs.Slot && n.Volume == gs.Volume {
				labels[agent.LabelNode] = n.NodeUUID
			}
		}
		s.Command, args = KindCreateVolume, CreateVolumeArgs{Volume: gs.Volume, VolumeRoot: root(), Labels: labels}
	case galera.StepInitIdentity:
		tok := b.TokenIDs[gs.Slot]
		if tok == "" {
			return Step{}, fmt.Errorf("%w: no identity token for slot %s", ErrInvalidOperation, gs.Slot)
		}
		s.Command, args = agent.KindInitIdentity, agent.InitIdentityArgs{VolumeRoot: root(), Token: agent.NewIdentityToken{
			TokenID: tok, Identity: agent.Identity{InstallationID: b.InstallationID, ClusterID: p.ClusterID, NodeUUID: gs.NodeUUID}}}
	case galera.StepInitVolumeDirs:
		if gs.Owner == nil {
			return Step{}, fmt.Errorf("%w: init-volume-dirs without owner", ErrInvalidOperation)
		}
		s.Command, args = agent.KindInitVolumeDirs, agent.InitVolumeDirsArgs{VolumeRoot: root(),
			Expected: agent.Identity{InstallationID: b.InstallationID, ClusterID: p.ClusterID, NodeUUID: gs.NodeUUID},
			UID:      gs.Owner.UID, GID: gs.Owner.GID}
	case galera.StepExecSQL:
		if gs.SQL == nil {
			return Step{}, fmt.Errorf("%w: exec-sql without template", ErrInvalidOperation)
		}
		// The agent connects as root with the cluster's root password secret,
		// which its own service must mount (ADR 0011).
		s.Command, args = agent.KindExecSQL, agent.ExecSQLArgs{VolumeRoot: root(),
			Expected: agent.Identity{InstallationID: b.InstallationID, ClusterID: p.ClusterID, NodeUUID: gs.NodeUUID},
			Template: string(gs.SQL.Template), User: gs.SQL.User, Host: gs.SQL.Host,
			RequireSSL: gs.SQL.RequireSSL, Logical: gs.SQL.Logical,
			PasswordSecret: gs.SQL.PasswordSecret, AdminSecret: p.Cluster.RootPasswordSecret}
	case galera.StepCreateService:
		if gs.Spec == nil {
			return Step{}, fmt.Errorf("%w: create-service without spec", ErrInvalidOperation)
		}
		s.Command, args = agent.KindCreateService, orchestrate.ToAgentSpec(*gs.Spec)
	case galera.StepUpdateService:
		if gs.Spec == nil {
			return Step{}, fmt.Errorf("%w: update-service without spec", ErrInvalidOperation)
		}
		s.Command, args = KindUpdateService, UpdateServiceArgs{ServiceName: gs.Spec.Name, Spec: orchestrate.ToAgentSpec(*gs.Spec)}
	case galera.StepAwaitUpdate:
		if gs.Spec == nil {
			return Step{}, fmt.Errorf("%w: await-update without spec", ErrInvalidOperation)
		}
		s.Command, args = agent.KindAwaitService, agent.AwaitServiceArgs{ServiceName: gs.Spec.Name}
	case galera.StepRemoveService:
		id := b.ServiceIDs[gs.Slot]
		if id == "" {
			return Step{}, fmt.Errorf("%w: no service ID for slot %s", ErrInvalidOperation, gs.Slot)
		}
		s.Command, args = agent.KindRemoveService, agent.ServiceArgs{ServiceID: id}
	case galera.StepStopAll:
		id := b.ServiceIDs[gs.Slot]
		if id == "" {
			return Step{}, fmt.Errorf("%w: no service ID for slot %s", ErrInvalidOperation, gs.Slot)
		}
		s.Command, args = agent.KindStopDB, agent.ServiceArgs{ServiceID: id}
	case galera.StepAwaitStopped, galera.StepStartNode:
		if gs.Spec == nil {
			return Step{}, fmt.Errorf("%w: %s without spec", ErrInvalidOperation, gs.Kind)
		}
		kind := agent.KindAwaitStopped
		if gs.Kind == galera.StepStartNode {
			kind = agent.KindStartService
		}
		s.Command, args = kind, agent.AwaitServiceArgs{ServiceName: gs.Spec.Name}
	case galera.StepVerifyIdentity:
		s.Command, args = agent.KindVerifyIdentity, agent.VerifyIdentityArgs{VolumeRoot: root(),
			Expected: agent.Identity{InstallationID: b.InstallationID, ClusterID: p.ClusterID, NodeUUID: gs.NodeUUID}}
	case galera.StepMarkSafe:
		if gs.Position == nil {
			return Step{}, fmt.Errorf("%w: mark-safe without the diagnosed position", ErrInvalidOperation)
		}
		ma := agent.MarkSafeArgs{VolumeRoot: root(),
			Expected: agent.Identity{InstallationID: b.InstallationID, ClusterID: p.ClusterID, NodeUUID: gs.NodeUUID},
			UUID:     gs.Position.UUID, Seqno: gs.Position.Seqno}
		if r := gs.Recovered; r != nil {
			if r.UUID != gs.Position.UUID || r.Seqno < 0 || gs.Position.Seqno >= 0 {
				return Step{}, fmt.Errorf("%w: recovered position %s:%d does not fill grastate %s:%d", ErrInvalidOperation,
					r.UUID, r.Seqno, gs.Position.UUID, gs.Position.Seqno)
			}
			seq := r.Seqno
			ma.SetSeqno = &seq
		}
		s.Command, args = agent.KindMarkSafeToBootstrap, ma
	case galera.StepWaitSynced:
		s.Preconditions = append(s.Preconditions, Condition{Kind: CondNodeSynced, Target: gs.Slot, ClusterSize: gs.ClusterSize})
	case galera.StepVerifyDonor, galera.StepRetainVolume:
		// Gates: verify-donor carries its node-synced condition from the plan.
	default:
		return Step{}, fmt.Errorf("%w: step kind %q is not supported", ErrInvalidOperation, gs.Kind)
	}
	if s.Command == "" {
		s.Target = ""
		return s, nil
	}
	switch gs.Kind {
	case galera.StepCreateService, galera.StepUpdateService, galera.StepAwaitUpdate, galera.StepRemoveService,
		galera.StepStopAll, galera.StepAwaitStopped, galera.StepStartNode:
		if b.ServiceHost != "" {
			s.Target = b.ServiceHost
		}
	}
	if s.Target == "" {
		return Step{}, fmt.Errorf("%w: step without host", ErrInvalidOperation)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return Step{}, fmt.Errorf("encode args: %w", err)
	}
	s.Args = raw
	return s, nil
}

// awaitStoppedSteps returns the IDs of a plan's await-stopped steps.
func awaitStoppedSteps(p galera.Plan) []string {
	var ids []string
	for _, st := range p.Steps {
		if st.Kind == galera.StepAwaitStopped {
			ids = append(ids, st.ID)
		}
	}
	return ids
}
