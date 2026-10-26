// SPDX-License-Identifier: AGPL-3.0-only

// Package provision builds and executes reviewed, idempotent host bootstrap
// plans. No command includes a private key or cluster join token.
package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

type Platform string

const (
	Swarm      Platform = "swarm"
	Kubernetes Platform = "kubernetes"
)

var ErrInvalid = errors.New("provision: invalid request")
var ErrConflict = errors.New("provision: plan ID already used")
var ErrExistingCluster = errors.New("provision: host already belongs to a cluster")
var ErrPreflight = errors.New("provision: host preflight failed")

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$`)

// Host is a public host identity. Known host keys and private keys are kept
// separately, encrypted in the manager store.
type Host struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	User    string `json:"user"`
	Role    string `json:"role"`
}

type Request struct {
	ID       string   `json:"id"`
	Platform Platform `json:"platform"`
	Hosts    []Host   `json:"hosts"`
}

// Check is a sanitized observation. ClusterID is returned for a foreign
// membership; it is never accepted as a reason to adopt a host.
type Check struct {
	HostID           string `json:"hostId"`
	OS               string `json:"os"`
	PortsAvailable   bool   `json:"portsAvailable"`
	TimeSynchronized bool   `json:"timeSynchronized"`
	DiskBytes        uint64 `json:"diskBytes"`
	ClusterID        string `json:"clusterId,omitempty"`
	Details          string `json:"details,omitempty"`
}

type Step struct {
	ID      string `json:"id"`
	HostID  string `json:"hostId"`
	Action  string `json:"action"`
	Command string `json:"command"`
}

type Plan struct {
	ID       string   `json:"id"`
	Platform Platform `json:"platform"`
	Hosts    []Host   `json:"hosts"`
	Checks   []Check  `json:"checks"`
	Steps    []Step   `json:"steps"`
	Digest   string   `json:"digest"`
	// Scripts are per-host shell scripts. The caller supplies the join token
	// when exporting a join script; it is never baked into this document.
	Scripts map[string]string `json:"scripts"`
}

func (r Request) Validate(allowKubernetes bool) error {
	if !safeName.MatchString(r.ID) || len(r.Hosts) == 0 || len(r.Hosts) > 16 ||
		(r.Platform != Swarm && (r.Platform != Kubernetes || !allowKubernetes)) {
		return ErrInvalid
	}
	seen := map[string]bool{}
	managers := 0
	for _, h := range r.Hosts {
		if !safeName.MatchString(h.ID) || !safeName.MatchString(h.Address) || !safeName.MatchString(h.User) || seen[h.ID] ||
			(h.Role != "manager" && h.Role != "worker") {
			return ErrInvalid
		}
		seen[h.ID] = true
		if h.Role == "manager" {
			managers++
		}
	}
	if r.Hosts[0].Role != "manager" || managers%2 == 0 {
		return fmt.Errorf("%w: first host must be manager and manager count must be odd", ErrInvalid)
	}
	return nil
}

// Build requires one passing, fresh preflight per requested host. The digest
// covers public identity, observations, exact commands and scripts.
func Build(req Request, checks []Check, allowKubernetes bool) (Plan, error) {
	if err := req.Validate(allowKubernetes); err != nil {
		return Plan{}, err
	}
	if len(checks) != len(req.Hosts) {
		return Plan{}, ErrPreflight
	}
	p := Plan{ID: req.ID, Platform: req.Platform, Hosts: req.Hosts, Checks: checks, Scripts: map[string]string{}}
	for i, h := range req.Hosts {
		c := checks[i]
		if c.HostID != h.ID || c.ClusterID != "" {
			if c.ClusterID != "" {
				return Plan{}, ErrExistingCluster
			}
			return Plan{}, ErrPreflight
		}
		if c.OS != "linux" || !c.PortsAvailable || !c.TimeSynchronized || c.DiskBytes < 10<<30 {
			return Plan{}, ErrPreflight
		}
		act := "join-" + h.Role
		if i == 0 {
			act = "init"
		}
		cmd := command(req.Platform, act)
		if cmd == "" {
			return Plan{}, ErrInvalid
		}
		if req.Platform == Swarm && act == "init" {
			cmd = "ADVERTISE_ADDRESS=" + shellQuote(h.Address) + "; export ADVERTISE_ADDRESS; " + cmd
		}
		p.Steps = append(p.Steps, Step{ID: fmt.Sprintf("host-%02d", i+1), HostID: h.ID, Action: act, Command: cmd})
		p.Scripts[h.ID] = script(req.Platform, req.ID, h.ID, cmd)
	}
	b, err := json.Marshal(struct {
		ID       string
		Platform Platform
		Hosts    []Host
		Checks   []Check
		Steps    []Step
		Scripts  map[string]string
	}{p.ID, p.Platform, p.Hosts, p.Checks, p.Steps, p.Scripts})
	if err != nil {
		return Plan{}, err
	}
	sum := sha256.Sum256(b)
	p.Digest = hex.EncodeToString(sum[:])
	return p, nil
}

func script(platform Platform, planID, hostID, cmd string) string {
	marker := `"$HOME/.local/state/cluster-manager/provision/` + planID + `"`
	base := "#!/bin/sh\nset -eu\n# cluster-manager plan " + planID + " / " + hostID + "\nmarker=" + marker + "\n"
	if platform == Swarm {
		base += `state=$(docker info --format '{{.Swarm.LocalNodeState}}')
if [ "$state" != inactive ]; then
  [ -f "$marker" ] && [ "$(cat "$marker")" = "$(docker info --format '{{.Swarm.Cluster.ID}}')" ] || { echo 'existing foreign cluster' >&2; exit 1; }
  exit 0
fi
` + cmd + `
mkdir -p "$(dirname "$marker")"
docker info --format '{{.Swarm.Cluster.ID}}' > "$marker"
`
	} else {
		base += `if [ -f /etc/rancher/k3s/k3s.yaml ] || [ -f /etc/rancher/k3s/k3s-agent.yaml ] || systemctl is-active --quiet k3s-agent 2>/dev/null; then
  [ -f "$marker" ] && [ "$(cat "$marker")" = k3s ] || { echo 'existing foreign cluster' >&2; exit 1; }
  exit 0
fi
` + cmd + `
mkdir -p "$(dirname "$marker")"
printf '%s\n' k3s > "$marker"
`
	}
	return base
}

func command(platform Platform, action string) string {
	var cmd string
	switch platform {
	case Swarm:
		switch action {
		case "init":
			cmd = `if [ "$(docker info --format '{{.Swarm.LocalNodeState}}')" = inactive ]; then : "${ADVERTISE_ADDRESS:?advertise address required}"; docker swarm init --advertise-addr "$ADVERTISE_ADDRESS"; fi`
		case "join-manager", "join-worker":
			cmd = `if [ "$(docker info --format '{{.Swarm.LocalNodeState}}')" = inactive ]; then : "${JOIN_TOKEN:?join token required}"; : "${MANAGER_ADDRESS:?manager address required}"; docker swarm join --token "$JOIN_TOKEN" "$MANAGER_ADDRESS"; fi`
		}
	case Kubernetes:
		switch action {
		case "init":
			cmd = `if [ ! -f /etc/rancher/k3s/k3s.yaml ]; then : "${K3S_INSTALL_SCRIPT:?reviewed k3s install script required}"; sh "$K3S_INSTALL_SCRIPT" server --cluster-init; fi`
		case "join-manager", "join-worker":
			cmd = `if [ ! -f /etc/rancher/k3s/k3s.yaml ] && [ ! -f /etc/rancher/k3s/k3s-agent.yaml ]; then : "${K3S_INSTALL_SCRIPT:?reviewed k3s install script required}"; : "${K3S_TOKEN:?join token required}"; : "${K3S_URL:?server URL required}"; sh "$K3S_INSTALL_SCRIPT" `
			if action == "join-manager" {
				cmd += `server; fi`
			} else {
				cmd += `agent; fi`
			}
		}
	}
	return strings.TrimSpace(cmd)
}
