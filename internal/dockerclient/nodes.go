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
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// Swarm node and global-service endpoints used by the manager (not by the
// agent): GET /nodes, GET+POST /nodes/{id}/update for one label, POST
// /services/create in global mode. Wire types follow the API v1.47
// definitions Node, NodeSpec, NodeDescription and EngineDescription.

// Node is the subset of a Swarm node the manager inventories.
type Node struct {
	ID            string            `json:"id"`
	Hostname      string            `json:"hostname"`
	Role          string            `json:"role"`         // "manager" or "worker"
	Availability  string            `json:"availability"` // "active", "pause" or "drain"
	State         string            `json:"state"`        // "ready", "down", "unknown", "disconnected"
	Addr          string            `json:"addr,omitempty"`
	Leader        bool              `json:"leader,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"` // node spec labels (user-settable)
	EngineVersion string            `json:"engineVersion,omitempty"`
	OS            string            `json:"os,omitempty"`
	Arch          string            `json:"arch,omitempty"`
	NanoCPUs      int64             `json:"nanoCpus,omitempty"`
	MemoryBytes   int64             `json:"memoryBytes,omitempty"`
}

type wireNode struct {
	ID      string `json:"ID"`
	Version struct {
		Index uint64 `json:"Index"`
	} `json:"Version"`
	Spec struct {
		Labels       map[string]string `json:"Labels"`
		Role         string            `json:"Role"`
		Availability string            `json:"Availability"`
	} `json:"Spec"`
	Description struct {
		Hostname string `json:"Hostname"`
		Platform struct {
			Architecture string `json:"Architecture"`
			OS           string `json:"OS"`
		} `json:"Platform"`
		Resources struct {
			NanoCPUs    int64 `json:"NanoCPUs"`
			MemoryBytes int64 `json:"MemoryBytes"`
		} `json:"Resources"`
		Engine struct {
			EngineVersion string `json:"EngineVersion"`
		} `json:"Engine"`
	} `json:"Description"`
	Status struct {
		State string `json:"State"`
		Addr  string `json:"Addr"`
	} `json:"Status"`
	ManagerStatus *struct {
		Leader bool `json:"Leader"`
	} `json:"ManagerStatus"`
}

func (w wireNode) node() Node {
	n := Node{
		ID: w.ID, Hostname: w.Description.Hostname, Role: w.Spec.Role, Availability: w.Spec.Availability,
		State: w.Status.State, Addr: w.Status.Addr, Labels: maps.Clone(w.Spec.Labels),
		EngineVersion: w.Description.Engine.EngineVersion, OS: w.Description.Platform.OS,
		Arch: w.Description.Platform.Architecture, NanoCPUs: w.Description.Resources.NanoCPUs,
		MemoryBytes: w.Description.Resources.MemoryBytes,
	}
	if w.ManagerStatus != nil {
		n.Leader = w.ManagerStatus.Leader
	}
	return n
}

// Nodes calls GET /nodes. It needs a connection to a Swarm manager.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var list []wireNode
	if err := c.do(ctx, http.MethodGet, "/nodes", nil, nil, &list, false); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(list))
	for _, w := range list {
		out = append(out, w.node())
	}
	return out, nil
}

// SetNodeLabel sets (value != "") or removes (value == "") one label of a
// node's spec. It reads the node (GET /nodes/{id}) and posts its spec back
// as raw JSON with only that label changed (POST /nodes/{id}/update
// ?version=N), so every other field and label is preserved. A concurrent
// change fails with a 500 "update out of sequence" error from the daemon.
// Only keys under the "dbcm." prefix may be changed: the manager must not
// modify labels it does not own (A10).
func (c *Client) SetNodeLabel(ctx context.Context, id, key, value string) error {
	if !strings.HasPrefix(key, "dbcm.") || len(key) <= len("dbcm.") {
		return fmt.Errorf("%w: node label %q is not a dbcm.* label", ErrInvalidArgument, key)
	}
	p, err := objectPath("/nodes/", id)
	if err != nil {
		return err
	}
	var n struct {
		Version struct {
			Index uint64 `json:"Index"`
		} `json:"Version"`
		Spec map[string]json.RawMessage `json:"Spec"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, nil, &n, false); err != nil {
		return err
	}
	if n.Spec == nil {
		return fmt.Errorf("dockerclient: node %q has no spec", id)
	}
	labels := map[string]string{}
	if raw, ok := n.Spec["Labels"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &labels); err != nil {
			return fmt.Errorf("dockerclient: node %q labels: %w", id, err)
		}
	}
	if value == "" {
		delete(labels, key)
	} else {
		labels[key] = value
	}
	if n.Spec["Labels"], err = json.Marshal(labels); err != nil {
		return err
	}
	q := url.Values{"version": {strconv.FormatUint(n.Version.Index, 10)}}
	return c.do(ctx, http.MethodPost, p+"/update", q, n.Spec, nil, true)
}

// CreateGlobalService creates a service in global mode (one task per
// eligible node), as used for the agent service. spec.Replicas is ignored.
// The same validation as CreateService applies.
func (c *Client) CreateGlobalService(ctx context.Context, spec agent.ServiceSpec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	secrets, err := c.resolveSecrets(ctx, spec.Secrets)
	if err != nil {
		return "", err
	}
	w := toWireSpec(spec, secrets)
	w.Mode = wireServiceMode{Global: &struct{}{}}
	var resp struct {
		ID string `json:"ID"`
	}
	if err := c.do(ctx, http.MethodPost, "/services/create", nil, w, &resp, true); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", fmt.Errorf("%w: dockerclient: create service %q: response has no ID", agent.ErrOutcomeUnknown, spec.Name)
	}
	return resp.ID, nil
}
