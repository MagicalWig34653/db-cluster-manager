// SPDX-License-Identifier: AGPL-3.0-only

// Package swarm connects the manager to a Docker Swarm: node inventory with
// roles, availability, labels, resources and engine versions; a Docker
// compatibility check against the maintained matrix in compat.json (A09);
// and the deployment of the agent as one global service constrained to
// enrolled hosts (P05, ADR 0004 item 6).
//
// The connection is the Docker Engine API through internal/dockerclient.
// Secure remote connection settings (P04: TLS, credentials) are not
// designed; a tcp:// endpoint without TLS is lab-only. Tested only against
// a fake daemon and an in-memory fake Connector, never a real Swarm.
package swarm

import (
	"context"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dockerclient"
)

// Connector is the narrow Docker API of one Swarm manager endpoint that
// the manager uses. *dockerclient.Client implements it.
type Connector interface {
	Version(ctx context.Context) (dockerclient.VersionInfo, error)
	Nodes(ctx context.Context) ([]dockerclient.Node, error)
	// SetNodeLabel changes one dbcm.* node label ("" removes it).
	SetNodeLabel(ctx context.Context, nodeID, key, value string) error
	CreateGlobalService(ctx context.Context, spec agent.ServiceSpec) (string, error)
	// CreateSecret creates a dbcm-* Swarm secret.
	CreateSecret(ctx context.Context, name string, labels map[string]string, data []byte) (string, error)
	ListByLabel(ctx context.Context, kind agent.ObjectKind, key, value string) ([]agent.Object, error)
}

var _ Connector = (*dockerclient.Client)(nil)

// Dial returns a Connector for a Docker host URL.
func Dial(dockerHost string) (*dockerclient.Client, error) {
	return dockerclient.New(dockerclient.Options{Host: dockerHost})
}

// Inventory is one observation of a Swarm.
type Inventory struct {
	Engine   dockerclient.VersionInfo `json:"engine"`
	Nodes    []dockerclient.Node      `json:"nodes"`
	Findings []Finding                `json:"findings"`
}

// Observe reads the endpoint's version and the Swarm's nodes and checks
// every node's engine version against m.
func Observe(ctx context.Context, c Connector, m Matrix) (Inventory, error) {
	v, err := c.Version(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("swarm: version: %w", err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("swarm: nodes: %w", err)
	}
	inv := Inventory{Engine: v, Nodes: nodes}
	for _, n := range nodes {
		inv.Findings = append(inv.Findings, m.Check(n.ID, n.EngineVersion))
	}
	return inv, nil
}
