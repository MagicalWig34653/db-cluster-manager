// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"fmt"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// LabAgentDirectory is a runner.AgentClient that resolves a step target
// (a Swarm node ID) to the enrolled host record and sends the command to
// the agent's lab HTTP endpoint (agent.LabClient).
//
// LAB-GRADE: the endpoint is plain HTTP without TLS and without manager or
// agent authentication; the agent identity is only the node ID in the
// record. Agent enrollment with real identities (mTLS candidate, ADR 0003/
// ARCHITEKTUR) is not designed yet. Use only on loopback or isolated lab
// networks.
type LabAgentDirectory struct {
	Store *Store
	HTTP  *http.Client
}

var _ runner.AgentClient = LabAgentDirectory{}

// Send implements runner.AgentClient. A target without an enrolled host
// record or agent URL is refused without sending (agent.ErrInvalidCommand).
func (d LabAgentDirectory) Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error) {
	host, err := d.lookup(target)
	if err != nil {
		return agent.Result{}, err
	}
	return (&agent.LabClient{BaseURL: host.AgentURL, HTTP: d.HTTP}).Handle(ctx, cmd)
}

func (d LabAgentDirectory) lookup(target string) (HostRecord, error) {
	hosts, err := List[HostRecord](d.Store, CollHosts)
	if err != nil {
		return HostRecord{}, err
	}
	for _, h := range hosts {
		if h.Value.NodeID != target {
			continue
		}
		if h.Value.Enrollment != EnrollEnrolled || h.Value.AgentURL == "" {
			return HostRecord{}, fmt.Errorf("%w: host %s is not enrolled or has no agent endpoint", agent.ErrInvalidCommand, target)
		}
		return h.Value, nil
	}
	return HostRecord{}, fmt.Errorf("%w: unknown host %s", agent.ErrInvalidCommand, target)
}
