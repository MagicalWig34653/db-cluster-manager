// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
)

// FTPDataDir is where the data volume of an FTP service is mounted.
const FTPDataDir = "/data"

// FTP modes.
const (
	// FTPExplicitTLS is FTPS with AUTH TLS on the control port (default).
	FTPExplicitTLS = "ftps-explicit"
	// FTPImplicitTLS is FTPS with TLS from the first byte (port 990).
	FTPImplicitTLS = "ftps-implicit"
	// FTPPlain is FTP without TLS; credentials travel in clear text.
	FTPPlain = "ftp"
)

// maxPassivePorts bounds the passive range. The Swarm publishes every port
// of the range as an entry of the service endpoint, which gets slow and
// heavy for large ranges.
const maxPassivePorts = 100

// PassiveRange is the range of passive data ports. Passive FTP tells the
// client the port number, so the published port must equal the container
// port; the plan publishes Min..Max one to one.
type PassiveRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// FTPSpec is the desired state of an FTP or FTPS server.
//
// Image contract (a proposal; no image is a dependency): the container
// reads DBCM_FTP_* variables (mode, control port, passive range and
// address, TLS files) and the user secrets below /run/secrets. Candidates
// for the first lab run are vsftpd- or proftpd-based images with a thin
// wrapper for the contract.
type FTPSpec struct {
	StorageBase
	// Mode is "ftps-explicit" (default), "ftps-implicit" or "ftp".
	Mode string `json:"mode,omitempty"`
	// AllowPlaintext must be true for mode "ftp"; clear-text FTP is never
	// the silent default.
	AllowPlaintext bool `json:"allowPlaintext,omitempty"`
	// ControlPort is the published control port (default 21, or 990 for
	// implicit TLS).
	ControlPort int `json:"controlPort,omitempty"`
	// Passive is the passive data port range, published one to one.
	Passive PassiveRange `json:"passive"`
	// PassiveAddress is the address the server announces to clients: the
	// host name or IP that clients reach the pinned host on. Required, as
	// a server behind the Swarm cannot know it.
	PassiveAddress string `json:"passiveAddress"`
	// PortMode is "host" (default: the ports are open on the pinned host
	// only, which is what the passive address names) or "ingress".
	PortMode string `json:"portMode,omitempty"`
	// CertSecret and KeySecret name the Swarm secrets of the TLS
	// certificate chain and private key (FTPS modes).
	CertSecret string        `json:"certSecret,omitempty"`
	KeySecret  string        `json:"keySecret,omitempty"`
	Users      []StorageUser `json:"users"`
}

// FTP is the FTP/FTPS service type.
type FTP struct{}

var _ Type = FTP{}

// Name implements Type.
func (FTP) Name() string { return TypeFTP }

func decodeFTP(raw json.RawMessage) (FTPSpec, error) {
	var f FTPSpec
	if err := decodeStrict(raw, &f); err != nil {
		return FTPSpec{}, err
	}
	return f, f.validate()
}

// Validate implements Type.
func (FTP) Validate(spec json.RawMessage) error {
	_, err := decodeFTP(spec)
	return err
}

func (f FTPSpec) mode() string {
	if f.Mode == "" {
		return FTPExplicitTLS
	}
	return f.Mode
}

func (f FTPSpec) controlPort() int {
	switch {
	case f.ControlPort != 0:
		return f.ControlPort
	case f.mode() == FTPImplicitTLS:
		return 990
	}
	return 21
}

func (f FTPSpec) tls() bool { return f.mode() != FTPPlain }

func (f FTPSpec) validate() error {
	var p problems
	f.StorageBase.validate(&p)
	switch f.mode() {
	case FTPExplicitTLS, FTPImplicitTLS:
		if f.CertSecret == "" || f.KeySecret == "" {
			p.addf("mode %s needs certSecret and keySecret", f.mode())
		}
		if f.AllowPlaintext {
			p.addf("allowPlaintext applies to mode ftp only")
		}
	case FTPPlain:
		if !f.AllowPlaintext {
			p.addf("mode ftp sends credentials in clear text: set allowPlaintext to accept that, or use ftps-explicit")
		}
		if f.CertSecret != "" || f.KeySecret != "" {
			p.addf("certSecret and keySecret are not used without TLS")
		}
	default:
		p.addf("mode %q (want ftps-explicit, ftps-implicit or ftp)", f.Mode)
	}
	for _, s := range []string{f.CertSecret, f.KeySecret} {
		if s != "" && !identRe.MatchString(s) {
			p.addf("TLS secret name %q is invalid", s)
		}
	}
	if f.CertSecret != "" && f.CertSecret == f.KeySecret {
		p.addf("certSecret and keySecret must be different secrets")
	}
	if c := f.controlPort(); c < 1 || c > 65535 {
		p.addf("controlPort %d out of range", c)
	}
	validPublishedPort(&p, "ftp", f.controlPort(), f.PortMode)
	r := f.Passive
	switch {
	case r.Min < 1024 || r.Max > 65535:
		p.addf("passive range %d-%d must lie within 1024..65535", r.Min, r.Max)
	case r.Max < r.Min:
		p.addf("passive range %d-%d is empty", r.Min, r.Max)
	case r.Max-r.Min+1 > maxPassivePorts:
		p.addf("passive range %d-%d has %d ports; at most %d are published", r.Min, r.Max, r.Max-r.Min+1, maxPassivePorts)
	case f.controlPort() >= r.Min && f.controlPort() <= r.Max:
		p.addf("the control port %d lies inside the passive range %d-%d", f.controlPort(), r.Min, r.Max)
	}
	if f.PassiveAddress == "" || !validHostOrIP(f.PassiveAddress) {
		p.addf("passiveAddress %q must be an IP address or a lower-case host name", f.PassiveAddress)
	}
	validateUsers(&p, f.Users, false)
	return p.err()
}

func (f FTPSpec) secrets() []string {
	s := userSecrets(f.Users)
	for _, x := range []string{f.CertSecret, f.KeySecret} {
		if x != "" {
			s = append(s, x)
		}
	}
	return sortedUnique(s)
}

func (f FTPSpec) container() ContainerSpec {
	c := f.StorageBase.container(FTPDataDir)
	c.Secrets = f.secrets()
	c.Env = []EnvVar{
		{Name: "DBCM_FTP_MODE", Value: f.mode()},
		{Name: "DBCM_FTP_DATA_DIR", Value: FTPDataDir},
		{Name: "DBCM_FTP_USERS_FILE", Value: "/etc/dbcm/ftp-users.json"},
		{Name: "DBCM_FTP_CONTROL_PORT", Value: strconv.Itoa(f.controlPort())},
		{Name: "DBCM_FTP_PASSIVE_MIN", Value: strconv.Itoa(f.Passive.Min)},
		{Name: "DBCM_FTP_PASSIVE_MAX", Value: strconv.Itoa(f.Passive.Max)},
		{Name: "DBCM_FTP_PASSIVE_ADDRESS", Value: f.PassiveAddress},
	}
	if f.tls() {
		c.Env = append(c.Env,
			EnvVar{Name: "DBCM_FTP_TLS_CERT_FILE", Value: path.Join(SecretsDir, f.CertSecret)},
			EnvVar{Name: "DBCM_FTP_TLS_KEY_FILE", Value: path.Join(SecretsDir, f.KeySecret)})
	}
	return c
}

func (f FTPSpec) ports() []Port {
	mode := f.PortMode
	if mode == "" {
		mode = "host"
	}
	ps := []Port{{Target: f.controlPort(), Published: f.controlPort(), Protocol: "tcp", Mode: mode}}
	for n := f.Passive.Min; n <= f.Passive.Max; n++ {
		ps = append(ps, Port{Target: n, Published: n, Protocol: "tcp", Mode: mode})
	}
	return ps
}

// Plan implements Type.
func (FTP) Plan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	f, err := decodeFTP(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	p, err := storagePlan(in, TypeFTP, f.container(), func(sp *ServiceSpec) { sp.Ports = f.ports() })
	if err != nil {
		return Plan{}, err
	}
	users, err := jsonArtifact("ftp-users.json", userViews(f.Users))
	if err != nil {
		return Plan{}, err
	}
	ports, err := portArtifact(*p.Steps[len(p.Steps)-1].Spec)
	if err != nil {
		return Plan{}, err
	}
	p.Artifacts = append(p.Artifacts, users, ports)
	deliverConfig(&p, in.Context, "ftp-users.json", "/etc/dbcm/ftp-users.json")
	p.Warnings = append(p.Warnings, storageBackupWarning)
	n := f.Passive.Max - f.Passive.Min + 1
	p.Warnings = append(p.Warnings, fmt.Sprintf("passive FTP publishes %d data ports plus the control port %d on the Swarm; the firewall in front of the host must allow %d-%d and %d",
		n, f.controlPort(), f.Passive.Min, f.Passive.Max, f.controlPort()))
	if n > 20 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("a passive range of %d ports makes the service endpoint large; 10-20 ports are usually enough for a small server", n))
	}
	if f.PortMode == "ingress" {
		p.Warnings = append(p.Warnings, "with the ingress routing mesh every node answers on the FTP ports, but the announced passive address must then reach a node that forwards to the task; prefer host mode on the pinned host")
	}
	if !f.tls() {
		p.Warnings = append(p.Warnings, "plain FTP: passwords and data cross the network unencrypted")
	}
	if f.mode() == FTPExplicitTLS {
		p.Warnings = append(p.Warnings, "explicit FTPS: the server must refuse logins before AUTH TLS (image setting), the plan cannot enforce it")
	}
	return p, nil
}

// DeletePlan implements Type.
func (FTP) DeletePlan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	f, err := decodeFTP(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	p := f.container().deletePlan(in, TypeFTP)
	removeManagedConfig(&p, in.Context, "ftp-users.json")
	return p, nil
}

// Health implements Type.
func (FTP) Health(in HealthInput) (Health, error) {
	if _, err := decodeFTP(in.Spec); err != nil {
		return Health{}, err
	}
	return observationHealth(in.Observed), nil
}
