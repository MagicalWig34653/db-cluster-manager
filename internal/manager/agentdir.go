// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"net"
	"slices"
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
	// NativePort is the listen port of the native agent (default 7445). The
	// native agent uses the same enrollment identity as the container agent.
	NativePort int

	mu      sync.Mutex
	clients map[string]*agent.Client // key: node ID + "@" + address
}

var _ runner.AgentClient = (*AgentDirectory)(nil)

// Resolve returns the container agent's https base URL for a node.
func (d *AgentDirectory) Resolve(nodeID string) (string, error) {
	return d.resolve(nodeID, agentrt.Container)
}

func (d *AgentDirectory) resolve(nodeID string, rt agentrt.Runtime) (string, error) {
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
		if rt == agentrt.Native {
			port = d.NativePort
			if port <= 0 {
				port = 7445
			}
		}
		return "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
	}
	return "", fmt.Errorf("%w: unknown host %s", agent.ErrInvalidCommand, nodeID)
}

func (d *AgentDirectory) client(nodeID, base string, rt agentrt.Runtime) (*agent.Client, error) {
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
	c.OnResponse = func(v string) { d.noteSeen(nodeID, v) }
	c.OnInfo = func(info agentrt.Info, present bool, err error) { d.NoteReport(nodeID, rt, info, present, err) }
	if d.clients == nil {
		d.clients = map[string]*agent.Client{}
	}
	d.clients[key] = c
	return c, nil
}

// legacyReports is what a host without any report is assumed to run: an
// agent built before ADR 0024 existed only as the container agent. It has
// no restic capability claim, so restic stays refused until the agent
// reports.
var legacyReports = map[agentrt.Runtime]agentrt.Info{
	agentrt.Container: {Runtime: agentrt.Container, Capabilities: []agentrt.Capability{agentrt.DockerObjects, agentrt.GaleraSteps}},
}

func (d *AgentDirectory) hostRecord(nodeID string) (HostRecord, error) {
	hosts, err := List[HostRecord](d.Store, CollHosts)
	if err != nil {
		return HostRecord{}, err
	}
	for _, h := range hosts {
		if h.Value.NodeID == nodeID {
			return h.Value, nil
		}
	}
	return HostRecord{}, fmt.Errorf("%w: unknown host %s", agent.ErrInvalidCommand, nodeID)
}

// Route picks the agent runtime that executes a step type on a host. It
// uses only the explicit table of internal/agentrt and the host's stored
// reports; it refuses with a reason when the required runtime is absent.
func (d *AgentDirectory) Route(nodeID, kind string) (agentrt.Runtime, error) {
	req, ok := agentrt.RequirementFor(kind)
	if !ok {
		return "", fmt.Errorf("%w: command %q has no runtime mapping", agent.ErrInvalidCommand, kind)
	}
	h, err := d.hostRecord(nodeID)
	if err != nil {
		return "", err
	}
	rt, err := routeHost(h, req)
	if err != nil {
		return "", fmt.Errorf("%w: host %s: %v", agent.ErrInvalidCommand, nodeID, err)
	}
	return rt, nil
}

// routeHost applies a requirement to the stored reports of one host.
func routeHost(h HostRecord, req agentrt.Requirement) (agentrt.Runtime, error) {
	if h.AgentReportError != "" {
		return "", fmt.Errorf("an agent sent an inconsistent runtime report (%s)", h.AgentReportError)
	}
	have := h.AgentReports
	if len(have) == 0 {
		have = legacyReports
	}
	return agentrt.Select(req, have)
}

// Send implements runner.AgentClient. The step type decides the runtime
// (Route); a host that cannot be resolved or lacks the required runtime is
// refused without sending (agent.ErrInvalidCommand). The agent's answer must
// name the runtime that was selected.
func (d *AgentDirectory) Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error) {
	rt, err := d.Route(target, string(cmd.Kind))
	if err != nil {
		return agent.Result{}, err
	}
	base, err := d.resolve(target, rt)
	if err != nil {
		return agent.Result{}, err
	}
	c, err := d.client(target, base, rt)
	if err != nil {
		return agent.Result{}, fmt.Errorf("%w: %v", agent.ErrInvalidCommand, err)
	}
	res, err := c.Handle(ctx, cmd)
	if h, herr := d.hostRecord(target); herr == nil && h.AgentReportError != "" {
		return res, fmt.Errorf("%w: host %s: an agent sent an inconsistent runtime report (%s)", agent.ErrInvalidCommand, target, h.AgentReportError)
	}
	if err == nil {
		switch res.Runtime {
		case "":
			res.Runtime = rt // agent built before ADR 0024
		case rt:
		default:
			return res, fmt.Errorf("%w: host %s: step routed to the %s agent was answered by a %s agent", agent.ErrInvalidCommand, target, rt, res.Runtime)
		}
	}
	return res, err
}

// NoteReport stores the runtime report an agent sent with an answer. An
// inconsistent report (a container agent claiming native capabilities and
// the like) is recorded as AgentReportError and refuses the host; a valid
// report clears it. An absent report changes nothing.
func (d *AgentDirectory) NoteReport(nodeID string, rt agentrt.Runtime, info agentrt.Info, present bool, rerr error) {
	if !present {
		return
	}
	if rerr == nil && info.Runtime != rt {
		rerr = fmt.Errorf("%w: the %s endpoint answered as %s", agentrt.ErrMismatch, rt, info.Runtime)
	}
	hosts, err := List[HostRecord](d.Store, CollHosts)
	if err != nil {
		return
	}
	for _, h := range hosts {
		v := h.Value
		if v.NodeID != nodeID || v.Enrollment != EnrollEnrolled {
			continue
		}
		if rerr == nil && v.AgentReportError == "" && slices.Equal(v.AgentReports[rt].Capabilities, info.Capabilities) {
			return
		}
		_, _, _ = Update(d.Store, CollHosts, h.Key, func(cur HostRecord, exists bool) (HostRecord, error) {
			if !exists || cur.Enrollment != EnrollEnrolled {
				return cur, errSkipSeen
			}
			if rerr != nil {
				cur.AgentReportError = fmt.Sprintf("%s endpoint: %v", rt, rerr)
				return cur, nil
			}
			cur.AgentReportError = ""
			if cur.AgentReports == nil {
				cur.AgentReports = map[agentrt.Runtime]agentrt.Info{}
			}
			cur.AgentReports[rt] = info
			return cur, nil
		})
		return
	}
}

// seenInterval bounds how often a host record is rewritten for "last seen".
const seenInterval = time.Minute

var errSkipSeen = errors.New("manager: host no longer enrolled")

// noteSeen records that the agent of nodeID answered, with its version.
// Best effort: a failed write is ignored (it is only a display value) and a
// recent identical record is not rewritten.
func (d *AgentDirectory) noteSeen(nodeID, version string) {
	now := time.Now()
	if d.PKI != nil {
		now = d.PKI.now()
	}
	hosts, err := List[HostRecord](d.Store, CollHosts)
	if err != nil {
		return
	}
	for _, h := range hosts {
		v := h.Value
		if v.NodeID != nodeID || v.Enrollment != EnrollEnrolled {
			continue
		}
		if v.AgentVersion == version && now.Sub(v.AgentSeenAt) < seenInterval {
			return
		}
		_, _, _ = Update(d.Store, CollHosts, h.Key, func(cur HostRecord, exists bool) (HostRecord, error) {
			if !exists || cur.Enrollment != EnrollEnrolled {
				return cur, errSkipSeen
			}
			cur.AgentVersion, cur.AgentSeenAt = version, now
			return cur, nil
		})
		return
	}
}
