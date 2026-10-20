// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// objectID is the accepted shape of an object ID or name: Docker IDs are
// hex or base-36, names match [a-zA-Z0-9][a-zA-Z0-9_.-]*. Refusing anything
// else keeps "", "." and ".." and path separators out of request paths.
var objectID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,255}$`)

func objectPath(prefix, id string) (string, error) {
	if !objectID.MatchString(id) {
		return "", fmt.Errorf("%w: object ID %q", ErrInvalidArgument, id)
	}
	return prefix + url.PathEscape(id), nil
}

// RemoveVolume calls DELETE /volumes/{name} without force. A volume in use
// answers 409 and matches ErrConflict.
func (c *Client) RemoveVolume(ctx context.Context, id string) error {
	p, err := objectPath("/volumes/", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, p, nil, nil, nil, true)
}

// Inspect returns the object of the given kind. For volumes the ID is the
// volume name. A missing object matches agent.ErrObjectNotFound.
func (c *Client) Inspect(ctx context.Context, kind agent.ObjectKind, id string) (agent.Object, error) {
	if kind == agent.KindService {
		return c.inspectService(ctx, id)
	}
	ep, ok := endpoints[kind]
	if !ok {
		return agent.Object{}, fmt.Errorf("%w: object kind %q", ErrInvalidArgument, kind)
	}
	p, err := objectPath(ep.inspect, id)
	if err != nil {
		return agent.Object{}, err
	}
	if kind == agent.KindContainer {
		p += "/json"
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, p, nil, nil, &raw, false); err != nil {
		return agent.Object{}, err
	}
	return ep.decode(kind, raw)
}

// ListByLabel lists objects of kind whose label key equals value, using the
// daemon's "label=key=value" filter. Results are re-checked client-side, so
// a daemon that ignores or widens the filter cannot widen the result.
func (c *Client) ListByLabel(ctx context.Context, kind agent.ObjectKind, key, value string) ([]agent.Object, error) {
	if key == "" || strings.ContainsAny(key, "=") {
		return nil, fmt.Errorf("%w: label key %q", ErrInvalidArgument, key)
	}
	f, err := json.Marshal(map[string][]string{"label": {key + "=" + value}})
	if err != nil {
		return nil, err
	}
	var objs []agent.Object
	if kind == agent.KindService {
		objs, err = c.listServices(ctx, string(f))
	} else {
		objs, err = c.listOther(ctx, kind, string(f))
	}
	if err != nil {
		return nil, err
	}
	out := objs[:0]
	for _, o := range objs {
		if v, ok := o.Labels[key]; ok && v == value {
			out = append(out, o)
		}
	}
	return out, nil
}

func (c *Client) listOther(ctx context.Context, kind agent.ObjectKind, filters string) ([]agent.Object, error) {
	ep, ok := endpoints[kind]
	if !ok {
		return nil, fmt.Errorf("%w: object kind %q", ErrInvalidArgument, kind)
	}
	q := url.Values{"filters": {filters}}
	if kind == agent.KindContainer {
		q.Set("all", "1")
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, ep.list, q, nil, &raw, false); err != nil {
		return nil, err
	}
	return ep.decodeList(kind, raw)
}

// endpoint describes the inspect/list paths and the JSON shapes of one
// non-service kind (API v1.47: Volume and VolumeListResponse,
// ContainerInspectResponse and ContainerSummary, Network, Secret, Config).
type endpoint struct {
	inspect, list string
	decode        func(agent.ObjectKind, json.RawMessage) (agent.Object, error)
	decodeList    func(agent.ObjectKind, json.RawMessage) ([]agent.Object, error)
}

var endpoints = map[agent.ObjectKind]endpoint{
	agent.KindVolume: {inspect: "/volumes/", list: "/volumes",
		decode: decodeOne[wireVolume],
		decodeList: func(k agent.ObjectKind, raw json.RawMessage) ([]agent.Object, error) {
			var r struct {
				Volumes []wireVolume `json:"Volumes"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, fmt.Errorf("dockerclient: decode volume list: %w", err)
			}
			return toObjects(k, r.Volumes), nil
		}},
	agent.KindContainer: {inspect: "/containers/", list: "/containers/json",
		decode: decodeOne[wireContainer], decodeList: decodeList[wireContainerSummary]},
	agent.KindNetwork: {inspect: "/networks/", list: "/networks",
		decode: decodeOne[wireNetwork], decodeList: decodeList[wireNetwork]},
	agent.KindSecret: {inspect: "/secrets/", list: "/secrets",
		decode: decodeOne[wireSwarmObject], decodeList: decodeList[wireSwarmObject]},
	agent.KindConfig: {inspect: "/configs/", list: "/configs",
		decode: decodeOne[wireSwarmObject], decodeList: decodeList[wireSwarmObject]},
}

type objecter interface {
	object(agent.ObjectKind) agent.Object
}

func decodeOne[T objecter](k agent.ObjectKind, raw json.RawMessage) (agent.Object, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return agent.Object{}, fmt.Errorf("dockerclient: decode %s: %w", k, err)
	}
	return v.object(k), nil
}

func decodeList[T objecter](k agent.ObjectKind, raw json.RawMessage) ([]agent.Object, error) {
	var vs []T
	if err := json.Unmarshal(raw, &vs); err != nil {
		return nil, fmt.Errorf("dockerclient: decode %s list: %w", k, err)
	}
	return toObjects(k, vs), nil
}

func toObjects[T objecter](k agent.ObjectKind, vs []T) []agent.Object {
	out := make([]agent.Object, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.object(k))
	}
	return out
}

type wireVolume struct {
	Name       string            `json:"Name"`
	Mountpoint string            `json:"Mountpoint"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
}

// object reports the host directory that holds the volume's data as the
// mountpoint. For a volume created by CreateVolume (local driver, bind
// option) that is the device directory, not the daemon's own _data path.
func (v wireVolume) object(k agent.ObjectKind) agent.Object {
	mp := v.Mountpoint
	if v.Options["type"] == "none" && v.Options["device"] != "" && strings.Contains(","+v.Options["o"]+",", ",bind,") {
		mp = v.Options["device"]
	}
	return agent.Object{Kind: k, ID: v.Name, Name: v.Name, Labels: v.Labels, Mountpoint: mp}
}

// CreateVolume calls POST /volumes/create for a local volume that bind-mounts
// the host directory spec.Device (DriverOpts type=none, o=bind, device=...).
// The daemon answers 201 with the volume, also when a volume of that name
// and the same options exists; the answer must name the requested volume.
func (c *Client) CreateVolume(ctx context.Context, spec agent.VolumeSpec) error {
	if !objectID.MatchString(spec.Name) {
		return fmt.Errorf("%w: volume name %q", ErrInvalidArgument, spec.Name)
	}
	if spec.Device == "" || !strings.HasPrefix(spec.Device, "/") {
		return fmt.Errorf("%w: volume device %q must be an absolute path", ErrInvalidArgument, spec.Device)
	}
	body := struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		DriverOpts map[string]string `json:"DriverOpts"`
		Labels     map[string]string `json:"Labels,omitempty"`
	}{
		Name: spec.Name, Driver: "local", Labels: spec.Labels,
		DriverOpts: map[string]string{"type": "none", "o": "bind", "device": spec.Device},
	}
	var resp wireVolume
	if err := c.do(ctx, http.MethodPost, "/volumes/create", nil, body, &resp, true); err != nil {
		return err
	}
	if resp.Name != spec.Name {
		return fmt.Errorf("%w: dockerclient: create volume %q: response names %q", agent.ErrOutcomeUnknown, spec.Name, resp.Name)
	}
	return nil
}

type wireContainer struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (v wireContainer) object(k agent.ObjectKind) agent.Object {
	return agent.Object{Kind: k, ID: v.ID, Name: strings.TrimPrefix(v.Name, "/"), Labels: v.Config.Labels}
}

type wireContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
}

func (v wireContainerSummary) object(k agent.ObjectKind) agent.Object {
	o := agent.Object{Kind: k, ID: v.ID, Labels: v.Labels}
	if len(v.Names) > 0 {
		o.Name = strings.TrimPrefix(v.Names[0], "/")
	}
	return o
}

type wireNetwork struct {
	ID     string            `json:"Id"`
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

func (v wireNetwork) object(k agent.ObjectKind) agent.Object {
	return agent.Object{Kind: k, ID: v.ID, Name: v.Name, Labels: v.Labels}
}

// wireSwarmObject is a Secret or Config.
type wireSwarmObject struct {
	ID   string `json:"ID"`
	Spec struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	} `json:"Spec"`
}

func (v wireSwarmObject) object(k agent.ObjectKind) agent.Object {
	return agent.Object{Kind: k, ID: v.ID, Name: v.Spec.Name, Labels: v.Spec.Labels}
}
