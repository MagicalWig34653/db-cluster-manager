// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/orchestrate"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// Operation kinds of service plans.
const (
	OpServiceCreate = "service/create"
	OpServiceDelete = "service/delete"
)

// ServiceBindings supply the facts a service plan does not contain.
type ServiceBindings struct {
	OpID           string
	InstallationID string
	// VolumeBase is the agent's volume base; a volume root is VolumeBase/<volume>.
	VolumeBase string
	// TokenIDs maps a volume name to the single-use new-identity token the
	// manager issued for its init-identity step.
	TokenIDs map[string]string
	// StepTimeout and MaxRetries apply to every step.
	StepTimeout time.Duration
	MaxRetries  int
	// ServiceHost is the host ID of the agent that runs Swarm service
	// commands (a Swarm manager node). Empty keeps the step's own host.
	ServiceHost string
}

// FromServicePlan converts a service plan into an Operation. It is pure
// data conversion. A plan with blockers is refused (service.ErrNotExecutable)
// so it can be previewed but never started; delivery of configuration files
// has no agent command and is refused too.
func FromServicePlan(p service.Plan, b ServiceBindings) (Operation, error) {
	if !p.Executable() {
		return Operation{}, fmt.Errorf("%w: %s", service.ErrNotExecutable, strings.Join(p.Blockers, "; "))
	}
	var kind string
	switch p.Kind {
	case service.PlanCreate:
		kind = OpServiceCreate
	case service.PlanDelete:
		kind = OpServiceDelete
	default:
		return Operation{}, fmt.Errorf("%w: service plan kind %q", ErrInvalidOperation, p.Kind)
	}
	if b.OpID == "" || b.InstallationID == "" || b.VolumeBase == "" || p.ServiceID == "" {
		return Operation{}, fmt.Errorf("%w: OpID, InstallationID, VolumeBase and plan ServiceID required", ErrInvalidOperation)
	}
	op := Operation{ID: b.OpID, Kind: kind, Locks: []string{"service/" + p.ServiceID}}
	for _, ps := range p.Steps {
		s, err := convertServiceStep(p, ps, b)
		if err != nil {
			return Operation{}, fmt.Errorf("step %s: %w", ps.ID, err)
		}
		op.Steps = append(op.Steps, s)
	}
	if err := op.Validate(); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func convertServiceStep(p service.Plan, ps service.Step, b ServiceBindings) (Step, error) {
	s := Step{ID: ps.ID, Target: ps.HostID, Timeout: b.StepTimeout, MaxRetries: b.MaxRetries, Note: ps.Note}
	for _, a := range ps.After {
		s.Preconditions = append(s.Preconditions, Condition{Kind: CondStepSucceeded, StepID: a})
	}
	root := func() string { return filepath.Join(b.VolumeBase, ps.Volume) }
	identity := func() agent.Identity {
		return agent.Identity{InstallationID: b.InstallationID, ClusterID: p.ServiceID, NodeUUID: ps.NodeUUID}
	}
	var args any
	switch ps.Kind {
	case service.StepCreateVolume:
		s.Command, args = KindCreateVolume, CreateVolumeArgs{Volume: ps.Volume, VolumeRoot: root(),
			Labels: map[string]string{agent.LabelCluster: p.ServiceID, agent.LabelNode: ps.NodeUUID}}
	case service.StepInitIdentity:
		tok := b.TokenIDs[ps.Volume]
		if tok == "" {
			return Step{}, fmt.Errorf("%w: no identity token for volume %s", ErrInvalidOperation, ps.Volume)
		}
		s.Command, args = agent.KindInitIdentity, agent.InitIdentityArgs{VolumeRoot: root(),
			Token: agent.NewIdentityToken{TokenID: tok, Identity: identity()}}
	case service.StepCreateService:
		if ps.Spec == nil {
			return Step{}, fmt.Errorf("%w: create-service without spec", ErrInvalidOperation)
		}
		spec, err := orchestrate.ServiceToAgentSpec(*ps.Spec)
		if err != nil {
			return Step{}, err
		}
		s.Command, args = agent.KindCreateService, spec
	case service.StepRemoveService:
		s.Command, args = agent.KindRemoveClusterService, agent.RemoveClusterServiceArgs{ServiceName: ps.ServiceName, ClusterID: ps.ServiceID}
	case service.StepRemoveVolume:
		s.Command, args = agent.KindRemoveVolume, agent.RemoveVolumeArgs{VolumeID: ps.Volume, VolumeRoot: root(),
			Expected: identity(), MissingOK: true, AllowUnmarked: true}
		s.RetryCodes = []agent.Code{agent.CodeVolumeInUse}
	default:
		return Step{}, fmt.Errorf("%w: step kind %q has no agent command", ErrInvalidOperation, ps.Kind)
	}
	switch ps.Kind {
	case service.StepCreateService, service.StepRemoveService:
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
