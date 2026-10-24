// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
)

const (
	KindCreateConfig Kind = "create-config"
	KindRemoveConfig Kind = "remove-config"
	CodeConfigInUse  Code = "config-in-use"
)

// ConfigSpec carries non-secret file data. Secret content is refused before
// effects and must instead be supplied through a Swarm secret reference.
type ConfigSpec struct {
	Name   string            `json:"name"`
	Data   []byte            `json:"data"`
	Labels map[string]string `json:"labels,omitempty"`
	Secret bool              `json:"secret,omitempty"`
}

type RemoveConfigArgs struct {
	Config    string `json:"config"`
	ClusterID string `json:"cluster_id"`
}

const labelConfigDigest = "dbcm.config-sha256"

func (e *Executor) createConfig(ctx context.Context, cmd Command, spec ConfigSpec) Result {
	if v, ok := spec.Labels[LabelInstallation]; ok && v != e.cfg.InstallationID {
		return outcome(ErrNotOwned)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(spec.Data))
	obj, err := e.docker.Inspect(ctx, KindConfig, spec.Name)
	if err == nil {
		if err := e.owner.Check(obj, KindConfig); err != nil {
			return outcome(err)
		}
		if obj.Labels[LabelCluster] != spec.Labels[LabelCluster] || obj.Labels[LabelNode] != spec.Labels[LabelNode] || obj.Labels[labelConfigDigest] != digest {
			return Result{Status: StatusRefused, Code: CodeImmutable, Message: "config name already holds a different specification"}
		}
		return succeeded(map[string]string{"id": obj.ID})
	}
	if !errors.Is(err, ErrObjectNotFound) {
		return configOutcome(err)
	}
	spec.Labels = maps.Clone(spec.Labels)
	spec.Labels[LabelInstallation], spec.Labels[LabelOpID], spec.Labels[LabelStepID] = e.cfg.InstallationID, cmd.OpID, cmd.StepID
	spec.Labels[labelConfigDigest] = digest
	id, err := e.docker.CreateConfig(ctx, spec)
	if err != nil {
		return configOutcome(err)
	}
	e.allow(id)
	return succeeded(map[string]string{"id": id})
}

// Daemon errors can echo request data. Never retain their text for configs.
func configOutcome(err error) Result {
	r := outcome(err)
	r.Message = "Docker config request failed"
	return r
}

func (e *Executor) removeConfig(ctx context.Context, a RemoveConfigArgs) Result {
	obj, err := e.docker.Inspect(ctx, KindConfig, a.Config)
	if errors.Is(err, ErrObjectNotFound) {
		return succeeded(map[string]string{"removed": "absent"})
	}
	if err != nil {
		return configOutcome(err)
	}
	if err := e.owner.Check(obj, KindConfig); err != nil {
		return outcome(err)
	}
	if obj.Labels[LabelCluster] != a.ClusterID {
		return outcome(ErrNotOwned)
	}
	if err := e.docker.RemoveConfig(ctx, obj.ID); err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			e.forget(obj.ID)
			return succeeded(map[string]string{"removed": obj.ID})
		}
		if errors.Is(err, ErrConflict) {
			return Result{Status: StatusFailed, Code: CodeConfigInUse, Message: "config is still in use"}
		}
		return configOutcome(err)
	}
	e.forget(obj.ID)
	return succeeded(map[string]string{"removed": obj.ID})
}
