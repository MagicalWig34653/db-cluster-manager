// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// AgentDirectory is the production runner.AgentClient: it resolves a step
// target (a Swarm node ID) to the enrolled host record, dials the agent at
// the node's Swarm address on Port over mTLS, presents the manager's client
// certificate and accepts only the agent identity of that node (URI SAN)
// from the agent CA, rejecting revoked serials.
type AgentDirectory struct {
	Store *Store
	PKI   *AgentPKI
	// Port is the agents' listen port (default swarm.AgentPort).
	Port int
	// Timeout bounds one command (default 5 minutes).
	Timeout time.Duration

	mu      sync.Mutex
	clients map[string]*agent.Client // key: node ID + "@" + address
}

var _ runner.AgentClient = (*AgentDirectory)(nil)

// Resolve returns the agent's https base URL for a node.
func (d *AgentDirectory) Resolve(nodeID string) (string, error) {
	hosts, err := List[HostRecord](d.Store, CollHosts)
	if err != nil {
		return "", err
	}
	for _, h := range hosts {
		v := h.Value
		if v.NodeID != nodeID {
			continue
		}
		switch {
		case v.Enrollment != EnrollEnrolled:
			return "", fmt.Errorf("%w: host %s is not enrolled", agent.ErrInvalidCommand, nodeID)
		case v.AgentSerial == "":
			return "", fmt.Errorf("%w: the agent on host %s has not enrolled its identity", agent.ErrInvalidCommand, nodeID)
		}
		ip := net.ParseIP(v.Addr)
		if ip == nil || ip.IsUnspecified() {
			return "", fmt.Errorf("%w: host %s has no Swarm address (run Sync)", agent.ErrInvalidCommand, nodeID)
		}
		port := d.Port
		if port <= 0 {
			port = swarm.AgentPort
		}
		return "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
	}
	return "", fmt.Errorf("%w: unknown host %s", agent.ErrInvalidCommand, nodeID)
}

func (d *AgentDirectory) client(nodeID, base string) (*agent.Client, error) {
	key := nodeID + "@" + base
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.clients[key]; ok {
		return c, nil
	}
	ca, err := d.PKI.CA()
	if err != nil {
		return nil, err
	}
	inst, err := d.PKI.installation()
	if err != nil {
		return nil, err
	}
	c, err := agent.NewMTLSClient(base, agent.MTLSClientConfig{
		Certificate:   d.PKI.ManagerClientCertificate,
		RootCAs:       ca.Pool(),
		AgentIdentity: certs.AgentURI(inst, nodeID),
		Revoked:       d.PKI.Revoked,
		Now:           d.PKI.Now,
		Timeout:       d.Timeout,
	})
	if err != nil {
		return nil, err
	}
	if d.clients == nil {
		d.clients = map[string]*agent.Client{}
	}
	d.clients[key] = c
	return c, nil
}

// Send implements runner.AgentClient. A host that cannot be resolved is
// refused without sending (agent.ErrInvalidCommand).
func (d *AgentDirectory) Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error) {
	base, err := d.Resolve(target)
	if err != nil {
		return agent.Result{}, err
	}
	c, err := d.client(target, base)
	if err != nil {
		return agent.Result{}, fmt.Errorf("%w: %v", agent.ErrInvalidCommand, err)
	}
	return c.Handle(ctx, cmd)
}
