// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/orchestrate"
)

// Proposed agent command kinds that galera plans need but internal/agent does
// not implement yet. The real agent executor rejects them as
// agent.ErrInvalidCommand, which the runner records as a Failed step (no
// retry, no effect). They exist so plans translate completely and the
// contract gap stays visible.
const (
	// KindCreateVolume creates the local named volume (args: CreateVolumeArgs).
	KindCreateVolume agent.Kind = "create-volume"
	// KindUpdateService replaces an owned service's spec (args: UpdateServiceArgs).
	KindUpdateService agent.Kind = "update-service"
)

// CreateVolumeArgs are the arguments of KindCreateVolume.
type CreateVolumeArgs struct {
	Volume     string `json:"volume"`
	VolumeRoot string `json:"volume_root"`
}

// UpdateServiceArgs are the arguments of KindUpdateService. The service is
// named, because a service created earlier in the same operation has no ID
// at planning time.
type UpdateServiceArgs struct {
	ServiceName string            `json:"service_name"`
	Spec        agent.ServiceSpec `json:"spec"`
}

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
}

// Condition kinds produced from galera preconditions (evaluated by a Checker).
const (
	CondNodeSynced    = string(galera.PreNodeSynced)
	CondQuorum        = string(galera.PreQuorum)
	CondApproved      = string(galera.PreApproved)
	CondNoNodeRunning = string(galera.PreNoNodeRunning)
)

// FromGaleraPlan converts a create or replacement plan into an Operation.
// It is pure data conversion: wait-synced and verify-donor become gates
// whose node-synced condition a Checker evaluates, retain-volume becomes a
// gate without conditions (the old volume is deliberately left alone), and
// the remaining steps become agent commands targeted at the step's host.
// Full-outage recovery plans are refused: they need explicit approval and a
// separate guided flow.
func FromGaleraPlan(p galera.Plan, b GaleraBindings) (Operation, error) {
	switch p.Kind {
	case galera.PlanCreateCluster, galera.PlanReplaceNode:
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
		if pc.Kind == galera.PreStepSucceeded {
			c = Condition{Kind: CondStepSucceeded, StepID: pc.StepID}
		}
		s.Preconditions = append(s.Preconditions, c)
	}
	root := func() string { return filepath.Join(b.VolumeBase, gs.Volume) }
	var args any
	switch gs.Kind {
	case galera.StepCreateVolume:
		s.Command, args = KindCreateVolume, CreateVolumeArgs{Volume: gs.Volume, VolumeRoot: root()}
	case galera.StepInitIdentity:
		tok := b.TokenIDs[gs.Slot]
		if tok == "" {
			return Step{}, fmt.Errorf("%w: no identity token for slot %s", ErrInvalidOperation, gs.Slot)
		}
		s.Command, args = agent.KindInitIdentity, agent.InitIdentityArgs{VolumeRoot: root(), Token: agent.NewIdentityToken{
			TokenID: tok, Identity: agent.Identity{InstallationID: b.InstallationID, ClusterID: p.ClusterID, NodeUUID: gs.NodeUUID}}}
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
	case galera.StepRemoveService:
		id := b.ServiceIDs[gs.Slot]
		if id == "" {
			return Step{}, fmt.Errorf("%w: no service ID for slot %s", ErrInvalidOperation, gs.Slot)
		}
		s.Command, args = agent.KindRemoveService, agent.ServiceArgs{ServiceID: id}
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
