// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"fmt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"net/http"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
)

// PatchAgentDirectory is the manager's patch.HostAgent: it reaches the
// native patch agent of an enrolled host over the same mTLS transport and
// CA as the main agent, on the patch agent's own port. The host ID of the
// patch service is the node ID of the enrolled host record; the agent must
// present its native identity (URI SAN) and current, non-revoked certificate.
type PatchAgentDirectory struct {
	// Dir resolves host addresses and supplies the PKI; its Port is the patch
	// agent's listen port (default 7445 is applied by the caller).
	Dir *AgentDirectory

	mu      sync.Mutex
	clients map[string]*http.Client // key: node ID + "@" + base URL
}

var _ patch.HostAgent = (*PatchAgentDirectory)(nil)

func (d *PatchAgentDirectory) client(nodeID, base string) (*http.Client, error) {
	key := nodeID + "@" + base
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.clients[key]; ok {
		return c, nil
	}
	ca, err := d.Dir.PKI.CA()
	if err != nil {
		return nil, err
	}
	inst, err := d.Dir.PKI.installation()
	if err != nil {
		return nil, err
	}
	tc, err := agent.ClientTLSConfig(agent.MTLSClientConfig{
		Certificate:   d.Dir.PKI.ManagerClientCertificate,
		RootCAs:       ca.Pool(),
		AgentIdentity: certs.RuntimeAgentURI(inst, nodeID, string(agentrt.Native)),
		CurrentSerial: func() (string, error) { return d.Dir.currentSerial(nodeID, agentrt.Native) },
		Revoked:       d.Dir.PKI.Revoked,
		Now:           d.Dir.PKI.Now,
	})
	if err != nil {
		return nil, err
	}
	timeout := d.Dir.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute // package installations are slow
	}
	c := &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: tc.Clone(), DisableKeepAlives: true,
		TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if d.clients == nil {
		d.clients = map[string]*http.Client{}
	}
	d.clients[key] = c
	return c, nil
}

// Do implements patch.HostAgent.
func (d *PatchAgentDirectory) Do(ctx context.Context, hostID string, c patch.AgentCommand) (patch.AgentResult, error) {
	// Patch needs a native agent (ADR 0024). A host whose reports name no
	// native patch capability is refused here, never routed elsewhere.
	if _, err := d.Dir.Route(hostID, agentrt.PatchKindPrefix+c.Kind); err != nil {
		return patch.AgentResult{}, fmt.Errorf("%w: %v", patch.ErrNotFound, err)
	}
	base, err := d.Dir.resolve(hostID, agentrt.Native)
	if err != nil {
		return patch.AgentResult{}, fmt.Errorf("%w: %v", patch.ErrNotFound, err)
	}
	cl, err := d.client(hostID, base)
	if err != nil {
		return patch.AgentResult{}, fmt.Errorf("patch: agent transport for %s: %w", hostID, err)
	}
	return patch.HTTPHostAgent{Client: cl, URL: func(string) (string, bool) { return base, true },
		OnInfo: func(id string, info agentrt.Info, present bool, err error) {
			d.Dir.NoteReport(id, agentrt.Native, info, present, err)
		}}.Do(ctx, hostID, c)
}
