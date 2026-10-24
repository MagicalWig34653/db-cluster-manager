// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"path"
)

// SFTPDataDir is where the data volume of an SFTP service is mounted.
const SFTPDataDir = "/data"

// sftpPort is the container port of the SSH server.
const sftpPort = 22

// SFTPSpec is the desired state of an SFTP server.
//
// Image contract (a proposal; any image that follows it works, none is a
// dependency): the container reads the login list from DBCM_SFTP_USERS_FILE
// (the plan artifact sftp-users.json, delivered as a Swarm config once the
// operator created it, see the guide), the passwords and public keys from
// the named secrets below /run/secrets, and the optional host key from
// DBCM_SFTP_HOST_KEY_FILE. Image candidates for the first lab run:
// atmoz/sftp (needs a thin wrapper for the file contract) or an own image.
type SFTPSpec struct {
	StorageBase
	// PublishedPort is the port on the Swarm for SSH; 0 keeps the service
	// internal (reachable on the overlay network only).
	PublishedPort int `json:"publishedPort,omitempty"`
	// PortMode is "host" (default; the port is open on the pinned host
	// only) or "ingress".
	PortMode string        `json:"portMode,omitempty"`
	Users    []StorageUser `json:"users"`
	// HostKeySecret names a Swarm secret with the SSH host key. Without it
	// the image creates a key on every start and clients see a changed host
	// key.
	HostKeySecret string `json:"hostKeySecret,omitempty"`
	// UsersConfig names an existing Swarm config that holds the
	// sftp-users.json artifact of the plan. The agent mounts it at
	// /etc/dbcm/sftp-users.json after its ownership guard accepted the
	// config. Without it the plan warns that nothing delivers the file.
	UsersConfig string `json:"usersConfig,omitempty"`
}

// SFTP is the SFTP service type.
type SFTP struct{}

var _ Type = SFTP{}

// Name implements Type.
func (SFTP) Name() string { return TypeSFTP }

func decodeSFTP(raw json.RawMessage) (SFTPSpec, error) {
	var s SFTPSpec
	if err := decodeStrict(raw, &s); err != nil {
		return SFTPSpec{}, err
	}
	return s, s.validate()
}

// Validate implements Type.
func (SFTP) Validate(spec json.RawMessage) error {
	_, err := decodeSFTP(spec)
	return err
}

func (s SFTPSpec) validate() error {
	var p problems
	s.StorageBase.validate(&p)
	validPublishedPort(&p, "ssh", s.PublishedPort, s.PortMode)
	if s.PublishedPort == 0 && s.PortMode != "" {
		p.addf("portMode needs a publishedPort")
	}
	validateUsers(&p, s.Users, true)
	if s.HostKeySecret != "" && !identRe.MatchString(s.HostKeySecret) {
		p.addf("hostKeySecret name %q is invalid", s.HostKeySecret)
	}
	if s.UsersConfig != "" && !identRe.MatchString(s.UsersConfig) {
		p.addf("usersConfig name %q is invalid", s.UsersConfig)
	}
	return p.err()
}

func (s SFTPSpec) secrets() []string {
	secrets := userSecrets(s.Users)
	if s.HostKeySecret != "" {
		secrets = append(secrets, s.HostKeySecret)
	}
	return sortedUnique(secrets)
}

func (s SFTPSpec) container() ContainerSpec {
	c := s.StorageBase.container(SFTPDataDir)
	c.Secrets = s.secrets()
	c.Env = []EnvVar{{Name: "DBCM_SFTP_DATA_DIR", Value: SFTPDataDir}, {Name: "DBCM_SFTP_USERS_FILE", Value: SFTPUsersFile}}
	if s.HostKeySecret != "" {
		c.Env = append(c.Env, EnvVar{Name: "DBCM_SFTP_HOST_KEY_FILE", Value: path.Join(SecretsDir, s.HostKeySecret)})
	}
	return c
}

// SFTPUsersFile is where the users artifact is expected in the container.
const SFTPUsersFile = "/etc/dbcm/sftp-users.json"

func (s SFTPSpec) port() Port {
	mode := s.PortMode
	if mode == "" {
		mode = "host"
	}
	return Port{Target: sftpPort, Published: s.PublishedPort, Protocol: "tcp", Mode: mode}
}

// Plan implements Type.
func (SFTP) Plan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	s, err := decodeSFTP(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	pt := s.port()
	p, err := storagePlan(in, TypeSFTP, s.container(), func(sp *ServiceSpec) {
		sp.Ports = []Port{pt}
		if s.UsersConfig != "" {
			sp.Configs = []ConfigRef{{Config: s.UsersConfig, Target: SFTPUsersFile}}
		}
	})
	if err != nil {
		return Plan{}, err
	}
	users, err := jsonArtifact("sftp-users.json", userViews(s.Users))
	if err != nil {
		return Plan{}, err
	}
	ports, err := portArtifact(*p.Steps[len(p.Steps)-1].Spec)
	if err != nil {
		return Plan{}, err
	}
	p.Artifacts = append(p.Artifacts, users, ports)
	p.Warnings = append(p.Warnings, storageBackupWarning)
	if s.HostKeySecret == "" {
		p.Warnings = append(p.Warnings, "no hostKeySecret: the SSH host key changes whenever the task is recreated and clients will warn about it")
	}
	passwordOnly := false
	for _, u := range s.Users {
		if u.AuthorizedKeysSecret == "" {
			passwordOnly = true
		}
	}
	if passwordOnly {
		p.Warnings = append(p.Warnings, "at least one user logs in with a password only; prefer public keys and rate-limit the published port")
	}
	if s.PublishedPort == 0 {
		p.Warnings = append(p.Warnings, "no published port: the server is reachable on the attached networks only")
	} else if s.PublishedPort == sftpPort {
		p.Warnings = append(p.Warnings, "port 22 of the host may already belong to the host's own SSH server; publish another port or check the host")
	}
	if s.UsersConfig == "" {
		p.Warnings = append(p.Warnings, "no usersConfig: nothing delivers the artifact sftp-users.json to "+SFTPUsersFile+"; create the Swarm config from it and name it in usersConfig (the agent has no create-config command yet)")
	}
	return p, nil
}

// DeletePlan implements Type.
func (SFTP) DeletePlan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	s, err := decodeSFTP(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	return s.container().deletePlan(in, TypeSFTP), nil
}

// Health implements Type.
func (SFTP) Health(in HealthInput) (Health, error) {
	if _, err := decodeSFTP(in.Spec); err != nil {
		return Health{}, err
	}
	return observationHealth(in.Observed), nil
}
