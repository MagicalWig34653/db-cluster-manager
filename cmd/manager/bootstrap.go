// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// imageComposerConfig is the "manager.imageComposer" section (X17, ADR 0026).
// The composer is off unless "enabled" is true. It generates Dockerfiles from
// a validated catalog recipe and starts builds through the image build
// service, so it needs manager.imageBuild and auth.enabled.
type imageComposerConfig struct {
	Enabled bool `json:"enabled"`
}

func (c *imageComposerConfig) on() bool { return c != nil && c.Enabled }

func (c *imageComposerConfig) validate(m *managerConfig, authEnabled bool) error {
	if !c.on() {
		return nil
	}
	if m.ImageBuild == nil {
		return errors.New("manager.imageComposer needs manager.imageBuild (composed images are built by the image build service)")
	}
	if !authEnabled {
		return errors.New("manager.imageComposer needs auth.enabled")
	}
	return nil
}

// bootstrapConfig is the "manager.bootstrap" section (X18, ADR 0027/0028).
// The fresh-server bootstrap is off unless "enabled" is true. It needs
// auth.enabled, the mTLS agent transport (the native agent it installs
// enrolls there) and the reviewed native agent release.
type bootstrapConfig struct {
	Enabled bool `json:"enabled"`
	// AgentRelease identifies the native agent binary the bootstrap installs
	// on a host: HTTPS URL for the host's architecture and its SHA-256.
	AgentRelease *agentReleaseConfig `json:"agentRelease,omitempty"`
	// SweepSeconds is how often expired credentials are deleted and failed
	// deletions are retried (default 60, minimum 15).
	SweepSeconds int `json:"sweepSeconds,omitempty"`
}

type agentReleaseConfig struct {
	Version   string `json:"version"`
	BinaryURL string `json:"binaryUrl"`
	SHA256    string `json:"sha256"`
}

const defaultBootstrapSweep = 60

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (c *bootstrapConfig) on() bool { return c != nil && c.Enabled }

func (c *bootstrapConfig) sweepEvery() time.Duration {
	if c.SweepSeconds > 0 {
		return time.Duration(c.SweepSeconds) * time.Second
	}
	return defaultBootstrapSweep * time.Second
}

func (c *bootstrapConfig) validate(m *managerConfig, authEnabled bool) error {
	if !c.on() {
		return nil
	}
	if c.SweepSeconds != 0 && c.SweepSeconds < 15 {
		return errors.New("manager.bootstrap.sweepSeconds must be at least 15")
	}
	if m.AgentTransport == nil {
		return errors.New("manager.bootstrap needs manager.agentTransport (the installed native agent enrolls there)")
	}
	if !authEnabled {
		return errors.New("manager.bootstrap needs auth.enabled")
	}
	return c.releaseErr()
}

// releaseErr checks the reviewed native agent release.
func (c *bootstrapConfig) releaseErr() error {
	r := c.AgentRelease
	if r == nil {
		return errors.New("manager.bootstrap.agentRelease is required (version, binaryUrl, sha256 of the reviewed native agent)")
	}
	if r.Version == "" {
		return errors.New("manager.bootstrap.agentRelease.version is required")
	}
	if u, err := url.Parse(r.BinaryURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("manager.bootstrap.agentRelease.binaryUrl must be an https URL")
	}
	if !sha256HexRe.MatchString(r.SHA256) {
		return errors.New("manager.bootstrap.agentRelease.sha256 must be 64 lowercase hex characters")
	}
	return nil
}

// Test seams: the SSH runner and the host key probe reach real hosts.
var (
	newSSHRunner    = func() provision.CommandRunner { return provision.SSHRunner{} }
	newHostKeyProbe = func() provision.HostKeyProbe { return provision.SSHHostKeyProbe{} }
)

// leaderTerm is the term of this replica's current leadership, 0 when it is
// not the leader.
func (rt *managedRuntime) leaderTerm() uint64 {
	l := rt.svc.Current()
	if l == nil || !l.Valid() {
		return 0
	}
	return l.Term
}

// newPatchInstaller builds the SSH installer of OS patch management on the
// sealed draft store and the enrollment token source.
func (rt *managedRuntime) newPatchInstaller() *patch.Installer {
	st := rt.svc.Store()
	return &patch.Installer{
		Runner: newSSHRunner(),
		Store:  manager.PatchInstallSealer{Store: st, Keys: rt.keys},
		Token:  manager.HostEnrollmentTokens{PKI: rt.pki}.InstallToken,
	}
}

// newBootstrapService builds the fresh-server bootstrap of one leadership:
// durable sealed drafts in the replicated metastore, the enrollment token
// source, the container agent deployment through the Swarm code, and a runner
// that works only while this replica leads (term fencing).
func (rt *managedRuntime) newBootstrapService(sw *manager.Swarms) *provision.BootstrapService {
	st := rt.svc.Store()
	cfg, at := rt.bootstrapCfg, rt.agentTransport
	deps := provision.BootstrapDeps{
		Runner:  newSSHRunner(),
		Tokens:  manager.HostEnrollmentTokens{PKI: rt.pki},
		Agents:  patch.BootstrapAgentScripts{},
		Manager: manager.BootstrapManagerActions{Swarms: sw, Store: st, Keys: rt.keys, Actor: collect.SystemSubject},
		Journal: opjournal.New(),
	}
	svc := provision.NewBootstrapService(deps, provision.BootstrapEnv{}, newHostKeyProbe(), patch.BootstrapAgentScripts{})
	svc.Store = manager.BootstrapDraftStore{Store: st, Keys: rt.keys}
	svc.Term = rt.leaderTerm
	svc.EnvFunc = func() (provision.BootstrapEnv, error) {
		inst, err := rt.pki.Installation()
		if err != nil {
			return provision.BootstrapEnv{}, errors.New("installation settings are incomplete: " + err.Error())
		}
		fp, err := rt.pki.CAFingerprint()
		if err != nil {
			return provision.BootstrapEnv{}, err
		}
		return provision.BootstrapEnv{Installation: inst, ManagerURL: at.ManagerURL, CAFingerprint: fp,
			Release: provision.AgentRelease{Version: cfg.AgentRelease.Version, BinaryURL: cfg.AgentRelease.BinaryURL, SHA256: cfg.AgentRelease.SHA256}}, nil
	}
	return svc
}

// pollBootstrap is the scheduler's run: delete expired credentials and retry
// failed deletions. It runs on the leader only.
func (rt *managedRuntime) pollBootstrap(ctx context.Context) error {
	// Drafts hold sealed credentials: they expire even when the module is
	// off, so there is no module check here.
	if svc := rt.bootstrap.Load(); svc != nil {
		svc.Sweep()
	}
	if in := rt.installer.Load(); in != nil {
		_, err := in.Sweep(ctx)
		return err
	}
	return nil
}
