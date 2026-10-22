// SPDX-License-Identifier: AGPL-3.0-only

package swarm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// Agent deployment.
//
// One global service per installation runs the agent on every node that
// carries the enrollment label AgentLabel(installation)=="true"; enrolling a
// host therefore means setting that label, and revoking it means removing
// the label (Swarm then stops the task there). Only dbcm.* labels are
// touched (A10).
//
// LAB-GRADE, stated plainly: the agent binary only serves its
// unauthenticated lab transport on a loopback address inside its task, so a
// manager on another host cannot reach it, and there is no agent identity,
// enrollment secret or rotation. The service spec below is what the
// manager can create today; the authenticated manager-agent transport is
// open design work (ARCHITEKTUR, ADR 0003/0004).

// Labels and defaults of the agent service.
const (
	LabelRole        = "dbcm.role"
	RoleAgent        = "agent"
	AgentListen      = "127.0.0.1:7070"
	AgentStateDir    = "/var/lib/dbcm-agent"
	DockerSocket     = "/var/run/docker.sock"
	agentLabelSuffix = ".enrolled"
)

var installationRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ErrInvalidDeployment is returned for an invalid AgentDeployment.
var ErrInvalidDeployment = errors.New("swarm: invalid agent deployment")

// AgentLabel is the node label key that enrolls a host for an installation.
func AgentLabel(installation string) string { return "dbcm.agent." + installation + agentLabelSuffix }

// AgentServiceName is the agent service name of an installation.
func AgentServiceName(installation string) string { return "dbcm-agent-" + installation }

// AgentDeployment describes the agent service of one installation.
type AgentDeployment struct {
	InstallationID string
	Image          string // pin by digest
	VolumeBase     string // host directory of database volumes
	// Enrolled are the node IDs that should run an agent; Revoked lose it.
	Enrolled []string
	Revoked  []string
}

// AgentSpec returns the global service spec of the deployment.
func AgentSpec(d AgentDeployment) agent.ServiceSpec {
	labels := map[string]string{agent.LabelInstallation: d.InstallationID, LabelRole: RoleAgent}
	return agent.ServiceSpec{
		Name:   AgentServiceName(d.InstallationID),
		Image:  d.Image,
		Labels: labels,
		Args: []string{"-listen", AgentListen, "-docker", "unix://" + DockerSocket, "-state", AgentStateDir,
			"-installation", d.InstallationID, "-volume-base", d.VolumeBase},
		Mounts: []agent.Mount{
			{Type: "bind", Source: DockerSocket, Target: DockerSocket},
			{Type: "bind", Source: AgentStateDir, Target: AgentStateDir},
			{Type: "bind", Source: d.VolumeBase, Target: d.VolumeBase},
		},
		Constraints:   []string{"node.labels." + AgentLabel(d.InstallationID) + "==true"},
		RestartPolicy: &agent.RestartPolicy{Condition: "any"},
	}
}

// DeployResult reports what DeployAgents did.
type DeployResult struct {
	ServiceID string `json:"serviceId"`
	Created   bool   `json:"created"` // false: the service already existed
	// Labeled and Unlabeled list nodes whose label was set or removed;
	// Failed maps node IDs to the error of their label change.
	Labeled   []string          `json:"labeled"`
	Unlabeled []string          `json:"unlabeled"`
	Failed    map[string]string `json:"failed,omitempty"`
}

// DeployAgents sets or removes the enrollment labels and creates the agent
// service if no service of this installation with the agent role exists.
// It is idempotent. An existing service is not updated (image changes are
// a separate, not yet implemented upgrade operation).
func DeployAgents(ctx context.Context, c Connector, d AgentDeployment) (DeployResult, error) {
	if !installationRe.MatchString(d.InstallationID) || d.Image == "" || d.VolumeBase == "" {
		return DeployResult{}, fmt.Errorf("%w: installation ID (lowercase DNS label), image and volume base required", ErrInvalidDeployment)
	}
	for _, id := range d.Enrolled {
		if slices.Contains(d.Revoked, id) {
			return DeployResult{}, fmt.Errorf("%w: node %s both enrolled and revoked", ErrInvalidDeployment, id)
		}
	}
	res := DeployResult{Failed: map[string]string{}}
	key := AgentLabel(d.InstallationID)
	for _, id := range d.Enrolled {
		if err := c.SetNodeLabel(ctx, id, key, "true"); err != nil {
			res.Failed[id] = err.Error()
			continue
		}
		res.Labeled = append(res.Labeled, id)
	}
	for _, id := range d.Revoked {
		if err := c.SetNodeLabel(ctx, id, key, ""); err != nil {
			res.Failed[id] = err.Error()
			continue
		}
		res.Unlabeled = append(res.Unlabeled, id)
	}
	existing, err := c.ListByLabel(ctx, agent.KindService, agent.LabelInstallation, d.InstallationID)
	if err != nil {
		return res, fmt.Errorf("swarm: list services: %w", err)
	}
	for _, o := range existing {
		if o.Labels[LabelRole] == RoleAgent {
			res.ServiceID = o.ID
			return res, nil
		}
	}
	id, err := c.CreateGlobalService(ctx, AgentSpec(d))
	if err != nil {
		return res, fmt.Errorf("swarm: create agent service: %w", err)
	}
	res.ServiceID, res.Created = id, true
	return res, nil
}
