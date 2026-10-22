// SPDX-License-Identifier: AGPL-3.0-only

package swarm

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
)

// Agent deployment.
//
// One global service per installation runs the agent on every node that
// carries the enrollment label AgentLabel(installation)=="true"; enrolling a
// host therefore means setting that label, and revoking it means removing
// the label (Swarm then stops the task there). Only dbcm.* labels are
// touched (A10).
//
// Transport (ADR 0010). With MTLS set (the production path) the agent runs
// in the host network namespace, so its mTLS listener on AgentPort is
// reachable at the node's Swarm address and the agent reaches a database
// port published on the host through loopback. The service mounts the
// enrollment token as a Swarm secret and passes the pinned CA fingerprint
// and the manager's enrollment URL; the agent learns its Swarm node ID from
// the {{.Node.ID}} template in DBCM_HOST_ID. Without MTLS the agent runs
// the unauthenticated lab transport on loopback inside its task
// (-insecure-lab): unreachable from other hosts and for labs only.

// Labels and defaults of the agent service.
const (
	LabelRole        = "dbcm.role"
	RoleAgent        = "agent"
	AgentListen      = "127.0.0.1:7070" // lab transport
	AgentPort        = 7443             // default mTLS port on the host
	AgentStateDir    = "/var/lib/dbcm-agent"
	DockerSocket     = "/var/run/docker.sock"
	HostNetwork      = "host"
	HostIDEnv        = "DBCM_HOST_ID"
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
	// MTLS selects the authenticated transport; nil deploys the lab
	// transport.
	MTLS *AgentMTLS
}

// AgentMTLS are the transport settings of an mTLS agent deployment.
type AgentMTLS struct {
	// ManagerURL is the enrollment endpoint (https://host:port).
	ManagerURL string
	// CAFingerprint pins the manager's internal CA (SHA-256 hex).
	CAFingerprint string
	// TokenSecret is the name of the Swarm secret that will hold Token.
	TokenSecret string
	// Token is the plain enrollment token; it is written into TokenSecret
	// when the service is created and appears nowhere else in the spec.
	// It is required only when the service does not exist yet.
	Token []byte
	// Port is the agent's listen port on the host (default AgentPort).
	Port int
	// MonitorUser and MonitorSecret (an existing Swarm secret holding the
	// password) give the agent its database monitoring credential for
	// collect-health; both empty leaves health collection unconfigured.
	MonitorUser   string
	MonitorSecret string
}

func (m *AgentMTLS) port() int {
	if m.Port > 0 {
		return m.Port
	}
	return AgentPort
}

// AgentSpec returns the global service spec of the deployment.
func AgentSpec(d AgentDeployment) agent.ServiceSpec {
	labels := map[string]string{agent.LabelInstallation: d.InstallationID, LabelRole: RoleAgent}
	common := []string{"-docker", "unix://" + DockerSocket, "-state", AgentStateDir,
		"-installation", d.InstallationID, "-volume-base", d.VolumeBase}
	spec := agent.ServiceSpec{
		Name:   AgentServiceName(d.InstallationID),
		Image:  d.Image,
		Labels: labels,
		Mounts: []agent.Mount{
			{Type: "bind", Source: DockerSocket, Target: DockerSocket},
			{Type: "bind", Source: AgentStateDir, Target: AgentStateDir},
			{Type: "bind", Source: d.VolumeBase, Target: d.VolumeBase},
		},
		Constraints:   []string{"node.labels." + AgentLabel(d.InstallationID) + "==true"},
		RestartPolicy: &agent.RestartPolicy{Condition: "any"},
	}
	m := d.MTLS
	if m == nil {
		spec.Args = append([]string{"-insecure-lab", "-listen", AgentListen}, common...)
		return spec
	}
	spec.Args = append([]string{"-listen", "0.0.0.0:" + strconv.Itoa(m.port()),
		"-manager-url", m.ManagerURL, "-ca-fingerprint", m.CAFingerprint,
		"-enroll-token-file", "/run/secrets/" + m.TokenSecret}, common...)
	spec.Secrets = []string{m.TokenSecret}
	if m.MonitorUser != "" && m.MonitorSecret != "" {
		spec.Args = append(spec.Args, "-monitor-user", m.MonitorUser, "-monitor-password-file", "/run/secrets/"+m.MonitorSecret)
		spec.Secrets = append(spec.Secrets, m.MonitorSecret)
	}
	spec.Env = []string{HostIDEnv + "={{.Node.ID}}"}
	spec.Networks = []string{HostNetwork}
	return spec
}

func (d AgentDeployment) validate() error {
	if !installationRe.MatchString(d.InstallationID) || d.Image == "" || d.VolumeBase == "" {
		return fmt.Errorf("%w: installation ID (lowercase DNS label), image and volume base required", ErrInvalidDeployment)
	}
	for _, id := range d.Enrolled {
		if slices.Contains(d.Revoked, id) {
			return fmt.Errorf("%w: node %s both enrolled and revoked", ErrInvalidDeployment, id)
		}
	}
	if m := d.MTLS; m != nil {
		u, err := url.Parse(m.ManagerURL)
		switch {
		case err != nil || u.Scheme != "https" || u.Host == "":
			return fmt.Errorf("%w: manager URL %q must be https://host:port", ErrInvalidDeployment, m.ManagerURL)
		case !strings.HasPrefix(m.TokenSecret, "dbcm-"):
			return fmt.Errorf("%w: token secret name must start with dbcm-", ErrInvalidDeployment)
		case m.Port < 0 || m.Port > 65535:
			return fmt.Errorf("%w: port %d", ErrInvalidDeployment, m.Port)
		case (m.MonitorUser == "") != (m.MonitorSecret == ""):
			return fmt.Errorf("%w: monitor user and secret go together", ErrInvalidDeployment)
		}
		if _, err := certs.NormalizeFingerprint(m.CAFingerprint); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidDeployment, err)
		}
	}
	return nil
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
	// TokenSecret is set when an enrollment token secret was created.
	TokenSecret string `json:"tokenSecret,omitempty"`
}

// DeployAgents sets or removes the enrollment labels and creates the agent
// service if no service of this installation with the agent role exists.
// It is idempotent. An existing service is not updated (image changes are
// a separate, not yet implemented upgrade operation).
func DeployAgents(ctx context.Context, c Connector, d AgentDeployment) (DeployResult, error) {
	if err := d.validate(); err != nil {
		return DeployResult{}, err
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
	if m := d.MTLS; m != nil {
		if len(m.Token) == 0 {
			return res, fmt.Errorf("%w: the agent service must be created with an enrollment token", ErrInvalidDeployment)
		}
		labels := map[string]string{agent.LabelInstallation: d.InstallationID, LabelRole: RoleAgent}
		if _, err := c.CreateSecret(ctx, m.TokenSecret, labels, m.Token); err != nil {
			return res, fmt.Errorf("swarm: create enrollment token secret: %w", err)
		}
		res.TokenSecret = m.TokenSecret
	}
	id, err := c.CreateGlobalService(ctx, AgentSpec(d))
	if err != nil {
		return res, fmt.Errorf("swarm: create agent service: %w", err)
	}
	res.ServiceID, res.Created = id, true
	return res, nil
}
