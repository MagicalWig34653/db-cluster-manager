// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
)

// Ownership labels from ADR 0004 item 1, plus labels that link an object to
// the operation step that created it (for later reconciliation).
const (
	LabelInstallation = "dbcm.installation"
	LabelCluster      = "dbcm.cluster"
	LabelNode         = "dbcm.node"
	LabelOpID         = "dbcm.op"
	LabelStepID       = "dbcm.step"
)

// ObjectKind is the type of a Docker object.
type ObjectKind string

// Docker object kinds the agent knows.
const (
	KindService   ObjectKind = "service"
	KindVolume    ObjectKind = "volume"
	KindContainer ObjectKind = "container"
	KindNetwork   ObjectKind = "network"
	KindSecret    ObjectKind = "secret"
	KindConfig    ObjectKind = "config"
)

// Object is the subset of a Docker object the agent needs.
type Object struct {
	Kind       ObjectKind        `json:"kind"`
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Mountpoint string            `json:"mountpoint,omitempty"` // volumes only
	Mounts     []Mount           `json:"mounts,omitempty"`     // services only
}

// Mount is a service mount.
type Mount struct {
	Type     string `json:"type"` // "bind" or "volume"
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// ServiceSpec is the subset of a Swarm service spec the agent creates.
type ServiceSpec struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Labels      map[string]string `json:"labels,omitempty"`
	Env         []string          `json:"env,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Mounts      []Mount           `json:"mounts,omitempty"`
	Constraints []string          `json:"constraints,omitempty"`
	Replicas    uint64            `json:"replicas"`
}

// ErrObjectNotFound is returned by DockerAPI implementations when an object
// does not exist.
var ErrObjectNotFound = errors.New("agent: docker object not found")

// ErrOutcomeUnknown may be returned (wrapped) by DockerAPI implementations
// when a request may or may not have taken effect, for example after a
// timeout. The executor records such steps as StatusUnknown.
var ErrOutcomeUnknown = errors.New("agent: docker outcome unknown")

// DockerAPI is the narrow set of Docker operations the executor uses. Only a
// test fake implements it so far; the real Docker Engine client is future
// work.
type DockerAPI interface {
	CreateService(ctx context.Context, spec ServiceSpec) (id string, err error)
	RemoveService(ctx context.Context, id string) error
	ScaleService(ctx context.Context, id string, replicas uint64) error
	RemoveVolume(ctx context.Context, id string) error
	Inspect(ctx context.Context, kind ObjectKind, id string) (Object, error)
	ListByLabel(ctx context.Context, kind ObjectKind, key, value string) ([]Object, error)
}
