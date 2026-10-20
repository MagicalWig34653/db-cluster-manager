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
// ResourceObject, NetworkAttachmentConfig, EndpointSpec,
// ServiceCreateResponse, Service).

type wireServiceSpec struct {
	Name         string            `json:"Name"`
	Labels       map[string]string `json:"Labels,omitempty"`
	TaskTemplate wireTaskSpec      `json:"TaskTemplate"`
	Mode         wireServiceMode   `json:"Mode"`
	EndpointSpec *wireEndpointSpec `json:"EndpointSpec,omitempty"`
}

type wireTaskSpec struct {
	ContainerSpec wireContainerSpec  `json:"ContainerSpec"`
	Resources     *wireResources     `json:"Resources,omitempty"`
	RestartPolicy *wireRestartPolicy `json:"RestartPolicy,omitempty"`
	Placement     *wirePlacement     `json:"Placement,omitempty"`
	// Networks in TaskSpec, not the ServiceSpec.Networks deprecated since
	// API v1.44.
	Networks []wireNetworkAttachment `json:"Networks,omitempty"`
}

type wireContainerSpec struct {
	Image           string            `json:"Image"`
	Labels          map[string]string `json:"Labels,omitempty"`
	Command         []string          `json:"Command,omitempty"`
	Args            []string          `json:"Args,omitempty"`
	Hostname        string            `json:"Hostname,omitempty"`
	Env             []string          `json:"Env,omitempty"`
	Mounts          []wireMount       `json:"Mounts,omitempty"`
	StopGracePeriod int64             `json:"StopGracePeriod,omitempty"` // nanoseconds
	Secrets         []wireSecretRef   `json:"Secrets,omitempty"`
}

// wireSecretRef is an item of TaskSpec.ContainerSpec.Secrets. The daemon
// identifies the secret by SecretID; SecretName must be its name.
type wireSecretRef struct {
	File       wireSecretFile `json:"File"`
	SecretID   string         `json:"SecretID"`
	SecretName string         `json:"SecretName"`
}

type wireSecretFile struct {
	Name string `json:"Name"`
	UID  string `json:"UID"`
	GID  string `json:"GID"`
	Mode uint32 `json:"Mode"`
}

// secretFileMode is the mode of a secret file, as the Docker CLI uses by
// default: readable, root-owned.
const secretFileMode = 0o444

type wireNetworkAttachment struct {
	Target string `json:"Target"` // network name or ID
}

type wireEndpointSpec struct {
	Mode string `json:"Mode,omitempty"`
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
			Placement *wirePlacement `json:"Placement"`
		} `json:"TaskTemplate"`
		Mode wireServiceMode `json:"Mode"`
	} `json:"Spec"`
}

// toWireSpec converts an agent spec. Service labels are also set on the
// container spec so the service's containers carry the ownership labels.
// secrets are the resolved s.Secrets in the same order.
func toWireSpec(s agent.ServiceSpec, secrets []agent.Object) wireServiceSpec {
	w := wireServiceSpec{
		Name:   s.Name,
		Labels: maps.Clone(s.Labels),
		TaskTemplate: wireTaskSpec{ContainerSpec: wireContainerSpec{
			Image:           s.Image,
			Labels:          maps.Clone(s.Labels),
			Command:         s.Command,
			Args:            s.Args,
			Hostname:        s.Hostname,
			Env:             s.Env,
			StopGracePeriod: s.StopGracePeriodNS,
		}},
		Mode: wireServiceMode{Replicated: &wireReplicated{Replicas: s.Replicas}},
	}
	for _, n := range s.Networks {
		w.TaskTemplate.Networks = append(w.TaskTemplate.Networks, wireNetworkAttachment{Target: n})
	}
	if s.EndpointMode != "" {
		w.EndpointSpec = &wireEndpointSpec{Mode: s.EndpointMode}
	}
	for _, sec := range secrets {
		w.TaskTemplate.ContainerSpec.Secrets = append(w.TaskTemplate.ContainerSpec.Secrets, wireSecretRef{
			File:     wireSecretFile{Name: sec.Name, UID: "0", GID: "0", Mode: secretFileMode},
			SecretID: sec.ID, SecretName: sec.Name,
		})
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
	switch s.EndpointMode {
	case "", agent.EndpointModeVIP, agent.EndpointModeDNSRR:
	default:
		return fmt.Errorf("%w: endpoint mode %q", ErrInvalidArgument, s.EndpointMode)
	}
	if s.StopGracePeriodNS < 0 {
		return fmt.Errorf("%w: negative stop grace period", ErrInvalidArgument)
	}
	for _, refs := range [][]string{s.Networks, s.Secrets} {
		for _, r := range refs {
			if !objectID.MatchString(r) {
				return fmt.Errorf("%w: network or secret reference %q", ErrInvalidArgument, r)
			}
		}
	}
	return nil
}

// resolveSecrets inspects each referenced secret: the API needs both its ID
// and its name, and the name is also the file name below /run/secrets.
// Two references to the same file name are refused.
func (c *Client) resolveSecrets(ctx context.Context, refs []string) ([]agent.Object, error) {
	out := make([]agent.Object, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		obj, err := c.Inspect(ctx, agent.KindSecret, ref)
		if err != nil {
			return nil, fmt.Errorf("dockerclient: secret %q: %w", ref, err)
		}
		if obj.ID == "" || obj.Name == "" {
			return nil, fmt.Errorf("dockerclient: secret %q: response lacks ID or name", ref)
		}
		if seen[obj.Name] {
			return nil, fmt.Errorf("%w: secret %q referenced twice", ErrInvalidArgument, obj.Name)
		}
		seen[obj.Name] = true
		out = append(out, obj)
	}
	return out, nil
}

// CreateService calls POST /services/create and returns the new service ID.
// Referenced secrets are inspected first (GET /secrets/{id}); a missing one
// fails before anything is created. A 409 (name conflict) matches
// ErrConflict.
func (c *Client) CreateService(ctx context.Context, spec agent.ServiceSpec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	secrets, err := c.resolveSecrets(ctx, spec.Secrets)
	if err != nil {
		return "", err
	}
	var resp struct {
		ID       string   `json:"ID"`
		Warnings []string `json:"Warnings"`
	}
	if err := c.do(ctx, http.MethodPost, "/services/create", nil, toWireSpec(spec, secrets), &resp, true); err != nil {
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

// shape lists the keys of a JSON object this client models (and therefore
// replaces on update) and, for nested objects, their own shapes.
type shape struct {
	keys []string
	sub  map[string]*shape
}

// specShape is the part of the Swarm ServiceSpec that toWireSpec produces.
// Everything else in a service's current spec (UpdateConfig, RollbackConfig,
// LogDriver, ForceUpdate, endpoint ports, ...) is preserved on update.
var specShape = &shape{
	keys: []string{"Name", "Labels", "Mode"},
	sub: map[string]*shape{
		"TaskTemplate": {
			keys: []string{"Resources", "RestartPolicy", "Placement", "Networks"},
			sub: map[string]*shape{"ContainerSpec": {keys: []string{
				"Image", "Labels", "Command", "Args", "Hostname", "Env", "Mounts", "StopGracePeriod", "Secrets"}}},
		},
		"EndpointSpec": {keys: []string{"Mode"}},
	},
}

func toRawMap(v any) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// mergeShape returns cur with the modeled keys of want applied: modeled keys
// absent from want are removed, nested objects are merged, other keys of cur
// stay.
func mergeShape(cur, want map[string]json.RawMessage, sh *shape) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(cur)+len(want))
	for k, v := range cur {
		out[k] = v
	}
	for _, k := range sh.keys {
		delete(out, k)
	}
	for k, v := range want {
		sub, nested := sh.sub[k]
		var curSub map[string]json.RawMessage
		if nested {
			if raw, ok := cur[k]; ok {
				if err := json.Unmarshal(raw, &curSub); err != nil {
					curSub = nil
				}
			}
		}
		if !nested || curSub == nil {
			out[k] = v
			continue
		}
		var wantSub map[string]json.RawMessage
		if err := json.Unmarshal(v, &wantSub); err != nil {
			return nil, err
		}
		merged, err := mergeShape(curSub, wantSub, sub)
		if err != nil {
			return nil, err
		}
		if out[k], err = json.Marshal(merged); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpdateService replaces the modeled parts of a service's spec. It reads the
// service (GET /services/{id}) for its version index, merges the new spec
// into the current one so fields this client does not model are kept, and
// posts the result (POST /services/{id}/update?version=N). A concurrent
// change makes the daemon reject the stale version; the caller sees the
// error and retries with a fresh read. Referenced secrets are inspected
// first, as in CreateService.
func (c *Client) UpdateService(ctx context.Context, id string, spec agent.ServiceSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	p, err := objectPath("/services/", id)
	if err != nil {
		return err
	}
	secrets, err := c.resolveSecrets(ctx, spec.Secrets)
	if err != nil {
		return err
	}
	var svc struct {
		Version struct {
			Index uint64 `json:"Index"`
		} `json:"Version"`
		Spec map[string]json.RawMessage `json:"Spec"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, nil, &svc, false); err != nil {
		return err
	}
	if svc.Spec == nil {
		return fmt.Errorf("dockerclient: service %q: response has no spec", id)
	}
	want, err := toRawMap(toWireSpec(spec, secrets))
	if err != nil {
		return err
	}
	merged, err := mergeShape(svc.Spec, want, specShape)
	if err != nil {
		return fmt.Errorf("dockerclient: merge spec of service %q: %w", id, err)
	}
	q := url.Values{"version": {strconv.FormatUint(svc.Version.Index, 10)}}
	var resp struct {
		Warnings []string `json:"Warnings"`
	}
	return c.do(ctx, http.MethodPost, p+"/update", q, merged, &resp, true)
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
	if p := s.Spec.TaskTemplate.Placement; p != nil {
		o.Constraints = p.Constraints
	}
	if r := s.Spec.Mode.Replicated; r != nil {
		o.Replicas = r.Replicas
	}
	return o
}
