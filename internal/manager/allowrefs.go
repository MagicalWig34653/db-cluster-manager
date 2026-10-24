// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Operator-provided objects (the cluster network, password and backup
// secrets) are referenced by the services and jobs the agents create. An
// agent only touches objects whose ID is in its allow-list (ADR 0004: the
// allow-list is the agent's view of the manager's inventory; labels alone
// are not trusted). Lab E4 showed that nothing put these objects into the
// allow-list: the first create-service was refused with not-owned. The
// manager therefore resolves the references in the Swarm (they must carry
// the installation label) and prepends one allow-objects step per target
// host to the operation.

// ErrUnknownReference: a referenced network or secret does not exist in the
// Swarm with the installation label.
var ErrUnknownReference = errors.New("manager: referenced network or secret not found with the installation label")

// objectRefs are the names (or IDs) of referenced objects.
type objectRefs struct {
	Networks, Secrets, Configs []string
}

// resolveRefs returns the IDs of refs in the Swarm, each carrying the
// installation label.
func (s *Swarms) resolveRefs(ctx context.Context, swarmID, inst string, refs objectRefs) ([]string, error) {
	if s.Dial == nil {
		return nil, errNoDial
	}
	sw, ok, err := Get[SwarmRecord](s.Svc.store, CollSwarms, swarmID, Consistent())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: swarm %q", api.ErrNotFound, swarmID)
	}
	conn, err := s.Dial(sw.Value.DockerHost)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, k := range []struct {
		kind agent.ObjectKind
		refs []string
	}{{agent.KindNetwork, refs.Networks}, {agent.KindSecret, refs.Secrets}, {agent.KindConfig, refs.Configs}} {
		if len(k.refs) == 0 {
			continue
		}
		objs, err := conn.ListByLabel(ctx, k.kind, agent.LabelInstallation, inst)
		if err != nil {
			return nil, fmt.Errorf("swarm: list %ss: %w", k.kind, err)
		}
		for _, r := range k.refs {
			i := slices.IndexFunc(objs, func(o agent.Object) bool { return o.ID == r || o.Name == r })
			if i < 0 {
				return nil, fmt.Errorf("%w: %s %q", ErrUnknownReference, k.kind, r)
			}
			if !slices.Contains(ids, objs[i].ID) {
				ids = append(ids, objs[i].ID)
			}
		}
	}
	return ids, nil
}

// allowSteps returns one allow-objects step per distinct host.
func allowSteps(hosts, ids []string, timeout time.Duration, retries int) ([]runner.Step, error) {
	raw, err := json.Marshal(agent.AllowObjectsArgs{IDs: ids})
	if err != nil {
		return nil, err
	}
	var out []runner.Step
	seen := map[string]bool{}
	for _, h := range hosts {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, runner.Step{ID: fmt.Sprintf("00-allow-objects-%d", len(out)+1), Command: agent.KindAllowObjects,
			Target: h, Args: raw, Timeout: timeout, MaxRetries: retries,
			Note: "allow the operator-provided network and secrets this operation references"})
	}
	return out, nil
}

// allowReferenced prepends the allow-objects steps to op. Without a Swarm
// connector (unit tests with fake agents) it changes nothing.
func (o *Operations) allowReferenced(op *runner.Operation, swarmID, inst string, hosts []string, refs objectRefs) error {
	if o.Swarms == nil || o.Swarms.Dial == nil {
		return nil
	}
	sw := *o.Swarms
	sw.Svc = o.Svc
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ids, err := sw.resolveRefs(ctx, swarmID, inst, refs)
	if err != nil || len(ids) == 0 {
		return err
	}
	steps, err := allowSteps(hosts, ids, o.StepTimeout, o.MaxRetries)
	if err != nil {
		return err
	}
	op.Steps = append(steps, op.Steps...)
	return op.Validate()
}
