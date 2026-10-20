// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// Wire types for the Swarm service endpoints (API v1.47 definitions
// ServiceSpec, TaskSpec, TaskSpec.ContainerSpec, Mount, Limit,
// ResourceObject, ServiceCreateResponse, Service).

type wireServiceSpec struct {
	Name         string            `json:"Name"`
	Labels       map[string]string `json:"Labels,omitempty"`
	TaskTemplate wireTaskSpec      `json:"TaskTemplate"`
	Mode         wireServiceMode   `json:"Mode"`
}

type wireTaskSpec struct {
	ContainerSpec wireContainerSpec  `json:"ContainerSpec"`
	Resources     *wireResources     `json:"Resources,omitempty"`
	RestartPolicy *wireRestartPolicy `json:"RestartPolicy,omitempty"`
	Placement     *wirePlacement     `json:"Placement,omitempty"`
}

type wireContainerSpec struct {
	Image   string            `json:"Image"`
	Labels  map[string]string `json:"Labels,omitempty"`
	Command []string          `json:"Command,omitempty"`
	Args    []string          `json:"Args,omitempty"`
	Env     []string          `json:"Env,omitempty"`
	Mounts  []wireMount       `json:"Mounts,omitempty"`
}

type wireMount struct {
	Target   string `json:"Target"`
	Source   string `json:"Source,omitempty"`
	Type     string `json:"Type"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

type wireResources struct {
	Limits       *wireLimit    `json:"Limits,omitempty"`
	Reservations *wireResource `json:"Reservations,omitempty"`
}

type wireLimit struct {
	NanoCPUs    int64 `json:"NanoCPUs,omitempty"`
	MemoryBytes int64 `json:"MemoryBytes,omitempty"`
	Pids        int64 `json:"Pids,omitempty"`
}

type wireResource struct {
	NanoCPUs    int64 `json:"NanoCPUs,omitempty"`
	MemoryBytes int64 `json:"MemoryBytes,omitempty"`
}

type wireRestartPolicy struct {
	Condition   string `json:"Condition,omitempty"`
	Delay       int64  `json:"Delay,omitempty"`
	MaxAttempts int64  `json:"MaxAttempts,omitempty"`
	Window      int64  `json:"Window,omitempty"`
}

type wirePlacement struct {
	Constraints []string `json:"Constraints,omitempty"`
}

type wireServiceMode struct {
	Replicated *wireReplicated `json:"Replicated,omitempty"`
}

type wireReplicated struct {
	Replicas uint64 `json:"Replicas"` // always sent: 0 is meaningful, the default is 1
}

type wireService struct {
	ID      string `json:"ID"`
	Version struct {
		Index uint64 `json:"Index"`
	} `json:"Version"`
	Spec struct {
		Name         string            `json:"Name"`
		Labels       map[string]string `json:"Labels"`
		TaskTemplate struct {
			ContainerSpec struct {
				Mounts []wireMount `json:"Mounts"`
			} `json:"ContainerSpec"`
		} `json:"TaskTemplate"`
	} `json:"Spec"`
}

// toWireSpec converts an agent spec. Service labels are also set on the
// container spec so the service's containers carry the ownership labels.
func toWireSpec(s agent.ServiceSpec) wireServiceSpec {
	w := wireServiceSpec{
		Name:   s.Name,
		Labels: maps.Clone(s.Labels),
		TaskTemplate: wireTaskSpec{ContainerSpec: wireContainerSpec{
			Image:   s.Image,
			Labels:  maps.Clone(s.Labels),
			Command: s.Command,
			Args:    s.Args,
			Env:     s.Env,
		}},
		Mode: wireServiceMode{Replicated: &wireReplicated{Replicas: s.Replicas}},
	}
	for _, m := range s.Mounts {
		w.TaskTemplate.ContainerSpec.Mounts = append(w.TaskTemplate.ContainerSpec.Mounts,
			wireMount{Target: m.Target, Source: m.Source, Type: m.Type, ReadOnly: m.ReadOnly})
	}
	if len(s.Constraints) > 0 {
		w.TaskTemplate.Placement = &wirePlacement{Constraints: s.Constraints}
	}
	if rp := s.RestartPolicy; rp != nil {
		w.TaskTemplate.RestartPolicy = &wireRestartPolicy{Condition: rp.Condition, Delay: rp.DelayNS, MaxAttempts: rp.MaxAttempts, Window: rp.WindowNS}
	}
	if r := s.Resources; r != nil {
		wr := &wireResources{}
		if l := r.Limits; l != nil {
			wr.Limits = &wireLimit{NanoCPUs: l.NanoCPUs, MemoryBytes: l.MemoryBytes, Pids: l.Pids}
		}
		if rv := r.Reservations; rv != nil {
			wr.Reservations = &wireResource{NanoCPUs: rv.NanoCPUs, MemoryBytes: rv.MemoryBytes}
		}
		w.TaskTemplate.Resources = wr
	}
	return w
}

func validateSpec(s agent.ServiceSpec) error {
	if s.Name == "" || s.Image == "" {
		return fmt.Errorf("%w: service spec needs name and image", ErrInvalidArgument)
	}
	for _, m := range s.Mounts {
		if m.Target == "" {
			return fmt.Errorf("%w: mount without target", ErrInvalidArgument)
		}
		switch m.Type {
		case "bind", "volume":
		default:
			return fmt.Errorf("%w: mount type %q (want bind or volume)", ErrInvalidArgument, m.Type)
		}
	}
	if rp := s.RestartPolicy; rp != nil {
		switch rp.Condition {
		case "", "none", "on-failure", "any":
		default:
			return fmt.Errorf("%w: restart condition %q", ErrInvalidArgument, rp.Condition)
		}
	}
	return nil
}

// CreateService calls POST /services/create and returns the new service ID.
// A 409 (name conflict) matches ErrConflict.
func (c *Client) CreateService(ctx context.Context, spec agent.ServiceSpec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	var resp struct {
		ID       string   `json:"ID"`
		Warnings []string `json:"Warnings"`
	}
	if err := c.do(ctx, http.MethodPost, "/services/create", nil, toWireSpec(spec), &resp, true); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", fmt.Errorf("%w: dockerclient: create service %q: response has no ID", agent.ErrOutcomeUnknown, spec.Name)
	}
	return resp.ID, nil
}

// RemoveService calls DELETE /services/{id}.
func (c *Client) RemoveService(ctx context.Context, id string) error {
	p, err := objectPath("/services/", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, p, nil, nil, nil, true)
}

// ScaleService reads the service (GET /services/{id}) and posts its full
// spec back with a new replica count (POST /services/{id}/update?version=N).
// The spec is round-tripped as raw JSON so fields this client does not model
// are preserved. Only replicated services can be scaled.
func (c *Client) ScaleService(ctx context.Context, id string, replicas uint64) error {
	p, err := objectPath("/services/", id)
	if err != nil {
		return err
	}
	var svc struct {
		ID      string `json:"ID"`
		Version struct {
			Index uint64 `json:"Index"`
		} `json:"Version"`
		Spec map[string]json.RawMessage `json:"Spec"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, nil, &svc, false); err != nil {
		return err
	}
	var mode map[string]json.RawMessage
	if raw, ok := svc.Spec["Mode"]; ok {
		if err := json.Unmarshal(raw, &mode); err != nil {
			return fmt.Errorf("dockerclient: service %q mode: %w", id, err)
		}
	}
	rawRep, ok := mode["Replicated"]
	if !ok {
		return fmt.Errorf("%w: service %q is not a replicated service", ErrInvalidArgument, id)
	}
	var rep map[string]json.RawMessage
	if err := json.Unmarshal(rawRep, &rep); err != nil || rep == nil {
		rep = map[string]json.RawMessage{}
	}
	rep["Replicas"] = json.RawMessage(strconv.FormatUint(replicas, 10))
	if mode["Replicated"], err = json.Marshal(rep); err != nil {
		return err
	}
	if svc.Spec["Mode"], err = json.Marshal(mode); err != nil {
		return err
	}
	q := url.Values{"version": {strconv.FormatUint(svc.Version.Index, 10)}}
	var resp struct {
		Warnings []string `json:"Warnings"`
	}
	return c.do(ctx, http.MethodPost, p+"/update", q, svc.Spec, &resp, true)
}

func (c *Client) inspectService(ctx context.Context, id string) (agent.Object, error) {
	p, err := objectPath("/services/", id)
	if err != nil {
		return agent.Object{}, err
	}
	var s wireService
	if err := c.do(ctx, http.MethodGet, p, nil, nil, &s, false); err != nil {
		return agent.Object{}, err
	}
	return serviceObject(s), nil
}

func (c *Client) listServices(ctx context.Context, filters string) ([]agent.Object, error) {
	var list []wireService
	if err := c.do(ctx, http.MethodGet, "/services", url.Values{"filters": {filters}}, nil, &list, false); err != nil {
		return nil, err
	}
	out := make([]agent.Object, 0, len(list))
	for _, s := range list {
		out = append(out, serviceObject(s))
	}
	return out, nil
}

func serviceObject(s wireService) agent.Object {
	o := agent.Object{Kind: agent.KindService, ID: s.ID, Name: s.Spec.Name, Labels: s.Spec.Labels}
	for _, m := range s.Spec.TaskTemplate.ContainerSpec.Mounts {
		o.Mounts = append(o.Mounts, agent.Mount{Type: m.Type, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return o
}
