// SPDX-License-Identifier: AGPL-3.0-only

package provision

// In-memory simulated host for the bootstrap tests (X18, ADR 0027). It
// implements CommandRunner and HostKeyProbe without any socket. It does not
// interpret shell: it recognizes the step marker line of every generated
// script and applies a state transition for that step, so the tests check the
// plan, the executor, the journal and the secret handling, not the shell
// text. The scripts themselves are only syntax-checked (sh -n).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

var simMarker = regexp.MustCompile(`(?m)^# dbcm-bootstrap (\S+)(?: (\S+) (\S+))?$`)

type simCall struct {
	User, Step, Kind, Cmd, Stdin string
}

type simHost struct {
	mu sync.Mutex

	osID, version, arch string
	cpus                int
	mem, disk           uint64
	timeSynced          bool
	busy                []string
	other               []string

	keyFP    string
	rootKey  string
	rootPass string
	svcName  string
	dedPub   string // authorized_keys line installed for the service user

	// state
	rootLogin    bool
	basePkgs     bool
	chrony       bool
	svcUser      bool
	sudoers      bool
	docker       bool
	swarm        string // inactive | active
	k3s          bool
	firewall     bool
	autoUpdates  bool
	restic       bool
	agent        bool
	agentMarker  bool
	revertArmed  bool
	dedBroken    bool
	rootDisabled bool

	failOnce                    map[string]bool
	calls                       []simCall
	hostKeyRejects, authRejects int
}

func newSimHost(osID, version string) *simHost {
	return &simHost{
		osID: osID, version: version, arch: "x86_64", cpus: 4, mem: 4 << 30, disk: 40 << 30,
		timeSynced: true, keyFP: "SHA256:SIMULATEDHOSTKEYFINGERPRINT0000000000000", rootKey: "root-private-key-SECRET",
		svcName: "dbcm", swarm: "inactive", rootLogin: true, failOnce: map[string]bool{},
	}
}

func (s *simHost) knownHosts() string { return "sim.example.test ssh-ed25519 " + s.keyFP + "\n" }

func (s *simHost) rootCred() Credential {
	return Credential{PrivateKey: s.rootKey, KnownHosts: s.knownHosts()}
}

// Probe implements HostKeyProbe.
func (s *simHost) Probe(context.Context, Host) (HostKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return HostKey{Type: "ssh-ed25519", Fingerprint: s.keyFP, KnownHosts: s.knownHosts()}, nil
}

func (s *simHost) stepCalls(step, kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.Step == step && c.Kind == kind {
			n++
		}
	}
	return n
}

func (s *simHost) mutatingCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.Kind == "run" {
			n++
		}
	}
	return n
}

func (s *simHost) fireRevert() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revertArmed {
		s.rootLogin, s.rootDisabled, s.revertArmed = true, false, false
	}
}

func (s *simHost) authenticate(h Host, c Credential) error {
	switch {
	case h.User == "root":
		if !s.rootLogin {
			return errors.New("sim: root login refused")
		}
		if (c.PrivateKey != "" && c.PrivateKey == s.rootKey) || (c.Password != "" && c.Password == s.rootPass) {
			return nil
		}
	case h.User == s.svcName && s.dedPub != "" && !s.dedBroken:
		signer, err := ssh.ParsePrivateKey([]byte(c.PrivateKey))
		if err != nil {
			return errors.New("sim: bad private key")
		}
		if strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) == s.dedPub {
			return nil
		}
	}
	return errors.New("sim: authentication failed")
}

// Run implements CommandRunner.
func (s *simHost) Run(_ context.Context, h Host, c Credential, cmd, stdin string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.Contains(c.KnownHosts, s.keyFP) {
		s.hostKeyRejects++
		return "", errors.New("sim: host key mismatch")
	}
	if err := s.authenticate(h, c); err != nil {
		s.authRejects++
		return "", err
	}
	m := simMarker.FindStringSubmatch(cmd)
	if m == nil {
		return "", errors.New("sim: unknown command")
	}
	call := simCall{User: h.User, Cmd: cmd, Stdin: stdin}
	if m[1] == "preflight" {
		call.Step, call.Kind = "preflight", "check"
		s.calls = append(s.calls, call)
		return s.preflight(), nil
	}
	step, kind := m[2], m[3]
	call.Step, call.Kind = step, kind
	s.calls = append(s.calls, call)
	switch kind {
	case "check":
		if s.done(step) {
			return "dbcm:done=yes\n", nil
		}
		return "dbcm:done=no\n", nil
	case "run":
		if s.failOnce[step] {
			delete(s.failOnce, step)
			s.apply(step, stdin, true)
			return "", errors.New("sim: remote command failed")
		}
		changed := !s.done(step) || step == "lockdown-disable-root" || step == "lockdown-confirm"
		out, err := s.apply(step, stdin, false)
		if err != nil {
			return "", err
		}
		if changed {
			return out + "dbcm:result=changed\n", nil
		}
		return out + "dbcm:result=unchanged\n", nil
	}
	return "", errors.New("sim: unsupported kind")
}

func (s *simHost) preflight() string {
	var b strings.Builder
	p := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, v) }
	yn := func(v bool) string {
		if v {
			return "yes"
		}
		return "no"
	}
	p("os", s.osID)
	p("version", s.version)
	p("arch", s.arch)
	p("cpus", fmt.Sprint(s.cpus))
	p("mem", fmt.Sprint(s.mem))
	p("disk", fmt.Sprint(s.disk))
	p("time", yn(s.timeSynced))
	p("systemd", yn(s.osID != "alpine"))
	p("priv", "root")
	pm := map[string]string{"debian": "apt", "ubuntu": "apt", "almalinux": "dnf", "rocky": "dnf", "alpine": "apk"}[s.osID]
	p("pkg", pm)
	if s.docker {
		p("docker", "27.3.1")
	} else {
		p("docker", "none")
	}
	if s.swarm == "active" {
		p("cluster", "swarm")
	} else if s.k3s {
		p("cluster", "k3s")
	} else {
		p("cluster", "none")
	}
	p("agent", map[bool]string{true: "present", false: "absent"}[s.agent])
	p("agentmarker", yn(s.agentMarker))
	p("restic", yn(s.restic))
	p("sshport", "22")
	p("rootlogin", "yes")
	p("pwauth", "yes")
	p("svcuser", yn(s.svcUser))
	if len(s.busy) == 0 {
		p("busyports", "none")
	} else {
		p("busyports", strings.Join(s.busy, ","))
	}
	if len(s.other) == 0 {
		p("other", "none")
	} else {
		p("other", strings.Join(s.other, ","))
	}
	return b.String()
}

func (s *simHost) done(step string) bool {
	switch step {
	case "base-packages":
		return s.basePkgs
	case "time-sync":
		return s.chrony
	case "service-user":
		return s.svcUser && s.sudoers
	case "sshd-audit":
		return false
	case "firewall":
		return s.firewall
	case "unattended-upgrades":
		return s.autoUpdates
	case "runtime-docker":
		return s.docker
	case "restic":
		return s.restic
	case "agent-native":
		return s.agent && s.agentMarker
	case "cluster-swarm":
		return s.swarm == "active"
	case "lockdown-install-key":
		return s.dedPub != ""
	case "lockdown-disable-root":
		return s.rootDisabled
	}
	return false
}

var pubInScript = regexp.MustCompile(`(?m)^(ssh-ed25519 \S+)`)

func (s *simHost) apply(step, stdin string, partial bool) (string, error) {
	switch step {
	case "base-packages":
		s.basePkgs = true
	case "time-sync":
		s.chrony, s.timeSynced = true, true
	case "service-user":
		s.svcUser = true
		if !partial {
			s.sudoers = true
		}
	case "sshd-audit":
		return "dbcm:note=PermitRootLogin yes\ndbcm:note=PasswordAuthentication yes\n", nil
	case "firewall":
		s.firewall = !partial
	case "unattended-upgrades":
		s.autoUpdates = true
	case "runtime-docker":
		if partial {
			return "", nil // repository key added, engine not installed
		}
		s.docker = true
	case "restic":
		s.restic = true
	case "agent-native":
		if strings.TrimSpace(stdin) == "" {
			return "", errors.New("sim: enrollment token missing on stdin")
		}
		s.agent = true
		s.agentMarker = !partial
	case "cluster-swarm":
		if !s.docker {
			return "", errors.New("sim: docker missing")
		}
		s.swarm = "active"
	case "lockdown-install-key":
		if !s.svcUser {
			return "", errors.New("sim: service user missing")
		}
		m := pubInScript.FindStringSubmatch(s.lastScript())
		if m == nil {
			return "", errors.New("sim: no public key in script")
		}
		s.dedPub = m[1]
	case "lockdown-verify-key", "lockdown-store-key":
		return "", nil
	case "lockdown-disable-root":
		s.rootLogin, s.rootDisabled, s.revertArmed = false, true, true
	case "lockdown-confirm":
		s.revertArmed = false
	}
	return "", nil
}

func (s *simHost) lastScript() string { return s.calls[len(s.calls)-1].Cmd }

// simTokens is a TokenSource and ManagerActions double.
type simTokens struct {
	mu     sync.Mutex
	issued []string
}

func (t *simTokens) EnrollToken(_ context.Context, h Host, runtime string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tok := "enroll-token-SECRET-" + h.ID + "-" + runtime
	t.issued = append(t.issued, tok)
	return tok, nil
}

type simManager struct {
	mu       sync.Mutex
	deploys  []string
	stored   []Credential
	storeErr error
}

func (m *simManager) DeployContainerAgent(_ context.Context, h Host) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deploys = append(m.deploys, h.ID)
	return nil
}

func (m *simManager) StoreDedicatedKey(_ context.Context, _ Host, c Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.storeErr != nil {
		return m.storeErr
	}
	m.stored = append(m.stored, c)
	return nil
}

// simAgents is an AgentScripts double (the real adapter wraps internal/patch).
type simAgents struct{}

func (simAgents) Native(spec AgentInstallSpec) (AgentScript, error) {
	if spec.Release.SHA256 == "" {
		return AgentScript{}, errors.New("sim: no release")
	}
	return AgentScript{
		Install: "IFS= read -r TOKEN\n[ -n \"$TOKEN\" ] || exit 1\necho install agent " + spec.Mode + "\n",
		Cleanup: "echo remove agent\n",
	}, nil
}
