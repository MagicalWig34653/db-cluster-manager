// SPDX-License-Identifier: AGPL-3.0-only

package provision

// Fresh-server bootstrap (X18, ADR 0027). A bootstrap plan turns a freshly
// installed Linux server into a managed host: baseline hardening, container
// runtime, agents, cluster membership and optional capabilities, as an ordered
// list of reviewed shell scripts. Every script is idempotent, has a read-only
// check, a cleanup script and a digest. Nothing here opens a connection; the
// plan generator is a pure function of the request, the preflight observation
// and the environment.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Errors of the bootstrap flow.
var (
	ErrUnsupportedOS      = errors.New("provision: operating system is not supported")
	ErrHostKeyMismatch    = errors.New("provision: SSH host key fingerprint does not match the confirmed one")
	ErrBootstrapBlocked   = errors.New("provision: bootstrap plan is blocked by preflight findings")
	ErrNotApproved        = errors.New("provision: bootstrap plan is not approved")
	ErrDigest             = errors.New("provision: digest does not match the reviewed plan")
	ErrBootstrapNotFound  = errors.New("provision: bootstrap not found")
	ErrBootstrapState     = errors.New("provision: bootstrap is in the wrong state for this action")
	ErrCredentialsExpired = errors.New("provision: credentials were wiped; supply them again")
	ErrNoHook             = errors.New("provision: bootstrap step needs a backend that is not configured")
)

// DockerKeyFingerprint is the fingerprint of Docker's official package
// signing key (docs.docker.com, "Install Docker Engine"). The runtime step
// downloads the key and refuses to continue unless this fingerprint matches.
// This is the key of the apt repositories (Debian, Ubuntu). Checked against
// a live download on Debian 12 in lab E17.
const DockerKeyFingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"

// DockerRPMKeyFingerprint is the fingerprint of the key that signs Docker's
// rpm repository (download.docker.com/linux/centos/gpg). It is a different
// key from the apt one: using the apt fingerprint for the dnf step made every
// AlmaLinux bootstrap fail with "key fingerprint mismatch" (lab E17). Checked
// against a live download on AlmaLinux 9.8.
const DockerRPMKeyFingerprint = "060A61C51B558A7F742B77AAC52FEB6B621E9F35"

const (
	bootstrapStateDir = "/var/lib/dbcm-bootstrap"
	lockdownConf      = "/etc/ssh/sshd_config.d/00-dbcm-lockdown.conf"
	revertPidFile     = "/run/dbcm-ssh-revert.pid"
	revertSeconds     = 180
	// NativeAgentPort is the mTLS port of the native agent (patch.DefaultListen).
	NativeAgentPort = 7445
)

// OSSupport is one row of the support matrix.
type OSSupport struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Versions       []string `json:"versions"`
	PackageManager string   `json:"packageManager"`
	Init           string   `json:"init"`
	// Docker names where the engine comes from on this system.
	Docker string `json:"docker"`
	// NativeAgent, Firewall and Unattended tell which opt-in features exist.
	NativeAgent bool   `json:"nativeAgent"`
	Firewall    string `json:"firewall,omitempty"`
	Unattended  string `json:"unattended,omitempty"`
}

var supportMatrix = []OSSupport{
	{ID: "debian", Name: "Debian", Versions: []string{"12", "13"}, PackageManager: "apt", Init: "systemd", Docker: "download.docker.com/linux/debian (apt)", NativeAgent: true, Firewall: "ufw", Unattended: "unattended-upgrades"},
	{ID: "ubuntu", Name: "Ubuntu", Versions: []string{"22.04", "24.04"}, PackageManager: "apt", Init: "systemd", Docker: "download.docker.com/linux/ubuntu (apt)", NativeAgent: true, Firewall: "ufw", Unattended: "unattended-upgrades"},
	{ID: "almalinux", Name: "AlmaLinux", Versions: []string{"9", "10"}, PackageManager: "dnf", Init: "systemd", Docker: "download.docker.com/linux/centos (dnf)", NativeAgent: true, Firewall: "firewalld", Unattended: "dnf-automatic"},
	{ID: "rocky", Name: "Rocky Linux", Versions: []string{"9", "10"}, PackageManager: "dnf", Init: "systemd", Docker: "download.docker.com/linux/centos (dnf)", NativeAgent: true, Firewall: "firewalld", Unattended: "dnf-automatic"},
	{ID: "alpine", Name: "Alpine Linux", Versions: []string{"3.19", "3.20", "3.21", "3.22"}, PackageManager: "apk", Init: "openrc", Docker: "Alpine community repository (apk, signed by the Alpine keys)"},
}

// SupportMatrix returns a copy of the support matrix.
func SupportMatrix() []OSSupport {
	out := slices.Clone(supportMatrix)
	for i := range out {
		out[i].Versions = slices.Clone(out[i].Versions)
	}
	return out
}

// LookupSupport maps /etc/os-release ID and VERSION_ID to a matrix row.
func LookupSupport(id, version string) (OSSupport, error) {
	for _, s := range supportMatrix {
		if s.ID != id {
			continue
		}
		for _, v := range s.Versions {
			if version == v || strings.HasPrefix(version, v+".") {
				s.Versions = slices.Clone(s.Versions)
				return s, nil
			}
		}
	}
	return OSSupport{}, fmt.Errorf("%w: %s %s", ErrUnsupportedOS, id, version)
}

// ClusterChoice selects Swarm or k3s membership of the new host.
type ClusterChoice struct {
	// Platform is none, swarm or k3s. k3s stays a preview (ADR 0018).
	Platform string `json:"platform"`
	// Role is init, join-manager or join-worker.
	Role string `json:"role,omitempty"`
	// ManagerAddress is host:2377 of an existing manager for a join. The join
	// token is a write-only secret of the draft, never part of the plan.
	ManagerAddress string `json:"managerAddress,omitempty"`
}

// BootstrapProfile is the operator's choice of what the host becomes.
type BootstrapProfile struct {
	ServiceUser        string `json:"serviceUser"`
	Firewall           bool   `json:"firewall"`
	UnattendedUpgrades bool   `json:"unattendedUpgrades"`
	// DedicatedKey and DisableRootLogin are the lockdown phase. They are
	// planned here but only run after a separate approval.
	DedicatedKey     bool `json:"dedicatedKey"`
	DisableRootLogin bool `json:"disableRootLogin"`
	// Runtime is docker or none.
	Runtime        string        `json:"runtime"`
	NativeAgent    bool          `json:"nativeAgent"`
	ContainerAgent bool          `json:"containerAgent"`
	Patch          bool          `json:"patch"`
	PatchApply     bool          `json:"patchApply"`
	Restic         bool          `json:"restic"`
	ResticPaths    []string      `json:"resticPaths,omitempty"`
	Cluster        ClusterChoice `json:"cluster"`
}

var (
	serviceUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)
	managerAddrRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}:[0-9]{2,5}$`)
)

// Validate checks the profile for contradictions.
func (p BootstrapProfile) Validate() error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrInvalid, why) }
	if !serviceUserRe.MatchString(p.ServiceUser) || p.ServiceUser == "root" {
		return bad("service user")
	}
	if p.Runtime != "docker" && p.Runtime != "none" {
		return bad("runtime")
	}
	switch p.Cluster.Platform {
	case "none":
		if p.Cluster.Role != "" || p.Cluster.ManagerAddress != "" {
			return bad("cluster role without platform")
		}
	case "swarm", "k3s":
		if p.Cluster.Platform == "swarm" && p.Runtime != "docker" {
			return bad("swarm needs the docker runtime")
		}
		if p.Cluster.Platform == "k3s" && p.Runtime != "none" {
			return bad("k3s brings its own runtime; choose runtime none")
		}
		switch p.Cluster.Role {
		case "init":
			if p.Cluster.ManagerAddress != "" {
				return bad("init takes no manager address")
			}
		case "join-manager", "join-worker":
			if !managerAddrRe.MatchString(p.Cluster.ManagerAddress) {
				return bad("manager address host:port")
			}
		default:
			return bad("cluster role")
		}
	default:
		return bad("cluster platform")
	}
	if p.ContainerAgent && p.Cluster.Platform != "swarm" {
		return bad("the container agent is a Swarm service")
	}
	if (p.Patch || p.Restic) && !p.NativeAgent {
		return bad("patch and restic capabilities need the native agent")
	}
	if p.PatchApply && !p.Patch {
		return bad("patch apply needs the patch capability")
	}
	if len(p.ResticPaths) > 0 && !p.Restic {
		return bad("restic paths without the restic capability")
	}
	if p.Restic && len(p.ResticPaths) == 0 {
		return bad("restic needs at least one allowed path")
	}
	if p.DisableRootLogin && !p.DedicatedKey {
		return bad("disabling root login needs the dedicated key")
	}
	return nil
}

// BootstrapRequest is the public description of one host to bootstrap.
type BootstrapRequest struct {
	ID      string           `json:"id"`
	Host    Host             `json:"host"`
	Profile BootstrapProfile `json:"profile"`
	// HostKeyFingerprint is the operator-confirmed SSH host key. The service
	// fills it in; it is part of the approved digest.
	HostKeyFingerprint string `json:"hostKeyFingerprint,omitempty"`
}

// Validate checks the request.
func (r BootstrapRequest) Validate() error {
	if !safeName.MatchString(r.ID) || !safeName.MatchString(r.Host.ID) || !safeName.MatchString(r.Host.Address) || !safeName.MatchString(r.Host.User) {
		return ErrInvalid
	}
	return r.Profile.Validate()
}

// AgentRelease identifies the reviewed native agent binary.
type AgentRelease struct {
	Version   string `json:"version"`
	BinaryURL string `json:"binaryUrl"`
	SHA256    string `json:"sha256"`
}

// BootstrapEnv is manager-side configuration the plan needs.
type BootstrapEnv struct {
	Installation  string       `json:"installation"`
	ManagerURL    string       `json:"managerUrl"`
	CAFingerprint string       `json:"caFingerprint"`
	Release       AgentRelease `json:"release"`
}

// AgentInstallSpec is what the native agent install script generator needs.
type AgentInstallSpec struct {
	Host          Host
	Mode          string // report | apply
	ResticPaths   []string
	Release       AgentRelease
	ManagerURL    string
	CAFingerprint string
	Installation  string
}

// AgentScript is the native agent install script (reads the enrollment token
// from stdin) and its removal script. The adapter around internal/patch
// implements it; provision cannot import patch.
type AgentScript struct {
	Install string
	Cleanup string
}

// AgentScripts generates the reviewed native agent scripts.
type AgentScripts interface {
	Native(AgentInstallSpec) (AgentScript, error)
}

// BootstrapCheck is the sanitized preflight observation.
type BootstrapCheck struct {
	HostID            string   `json:"hostId"`
	OS                string   `json:"os"`
	Version           string   `json:"version"`
	Arch              string   `json:"arch"`
	CPUs              int      `json:"cpus"`
	MemBytes          uint64   `json:"memBytes"`
	DiskBytes         uint64   `json:"diskBytes"`
	TimeSynchronized  bool     `json:"timeSynchronized"`
	Systemd           bool     `json:"systemd"`
	Privilege         string   `json:"privilege"`
	PackageManager    string   `json:"packageManager"`
	Docker            string   `json:"docker"`
	ClusterID         string   `json:"clusterId,omitempty"`
	AgentPresent      bool     `json:"agentPresent"`
	AgentMarker       bool     `json:"agentMarker"`
	ResticPresent     bool     `json:"resticPresent"`
	SSHPort           int      `json:"sshPort"`
	SSHRootLogin      string   `json:"sshRootLogin"`
	SSHPasswordAuth   string   `json:"sshPasswordAuth"`
	ServiceUserExists bool     `json:"serviceUserExists"`
	BusyPorts         []string `json:"busyPorts"`
	OtherSoftware     []string `json:"otherSoftware"`
}

// BootstrapFinding is a preflight or profile finding. Severity is block, warn
// or info; a block makes the plan non-executable.
type BootstrapFinding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// BootstrapStep is one reviewed unit of work.
type BootstrapStep struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Phase is baseline, runtime, agent, cluster, capability or lockdown.
	Phase    string `json:"phase"`
	Mutating bool   `json:"mutating"`
	// RunAs is root (the login), dedicated (the service user with the
	// dedicated key) or manager (an action inside the manager, no script).
	RunAs        string `json:"runAs"`
	Unprivileged bool   `json:"unprivileged,omitempty"`
	Script       string `json:"script,omitempty"`
	ScriptDigest string `json:"scriptDigest,omitempty"`
	// Check is a read-only script printing dbcm:done=yes|no. A step that is
	// done is never run again; this is how a run resumes.
	Check   string `json:"check,omitempty"`
	Cleanup string `json:"cleanup,omitempty"`
	// Stdin names the secret delivered on standard input: enrollment or
	// join. Secrets never appear in a command line.
	Stdin         string `json:"stdin,omitempty"`
	ManagerAction string `json:"managerAction,omitempty"`
	// Gate is lockdown for steps that need a separate approval.
	Gate string `json:"gate,omitempty"`
	// Expect is the preflight's expectation: done or todo.
	Expect string `json:"expect"`
}

// BootstrapPlan is the reviewed document. Digest covers all of it;
// LockdownDigest covers the gated steps and is approved separately.
type BootstrapPlan struct {
	ID             string             `json:"id"`
	Host           Host               `json:"host"`
	Profile        BootstrapProfile   `json:"profile"`
	HostKey        string             `json:"hostKeyFingerprint,omitempty"`
	OS             OSSupport          `json:"os"`
	Check          BootstrapCheck     `json:"check"`
	Findings       []BootstrapFinding `json:"findings"`
	Blocked        bool               `json:"blocked"`
	Steps          []BootstrapStep    `json:"steps"`
	DedicatedKey   string             `json:"dedicatedPublicKey,omitempty"`
	Digest         string             `json:"digest"`
	LockdownDigest string             `json:"lockdownDigest,omitempty"`
}

// Step returns the step with the ID, or the zero value.
func (p BootstrapPlan) Step(id string) BootstrapStep {
	for _, s := range p.Steps {
		if s.ID == id {
			return s
		}
	}
	return BootstrapStep{}
}

func sum(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ComputeDigest returns the digest of the whole plan (without Digest).
func (p BootstrapPlan) ComputeDigest() string {
	p.Digest = ""
	return sum(p)
}

// ComputeLockdownDigest returns the digest of the gated steps and the
// dedicated public key, or "" when the plan has none.
func (p BootstrapPlan) ComputeLockdownDigest() string {
	var gated []BootstrapStep
	for _, s := range p.Steps {
		if s.Gate == "lockdown" {
			gated = append(gated, s)
		}
	}
	if len(gated) == 0 {
		return ""
	}
	return sum(struct {
		ID, Host, Key string
		Steps         []BootstrapStep
	}{p.ID, p.Host.ID, p.DedicatedKey, gated})
}

func scriptDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// VerifyDigests checks the plan digest, the lockdown digest and every script
// digest. A plan that fails it was changed after review.
func (p BootstrapPlan) VerifyDigests() error {
	if p.Digest == "" || p.ComputeDigest() != p.Digest || p.ComputeLockdownDigest() != p.LockdownDigest {
		return ErrDigest
	}
	for _, s := range p.Steps {
		if s.ScriptDigest != scriptDigest(s.Script) {
			return ErrDigest
		}
	}
	return nil
}

// GenerateDedicatedKey creates the ed25519 key pair for the dedicated login.
// The private key is PKCS#8 PEM; the public key is an authorized_keys line.
func GenerateDedicatedKey(comment string) (privatePEM, authorizedKey string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
	if comment != "" && regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`).MatchString(comment) {
		line += " " + comment
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), line, nil
}

func canonicalAuthorizedKey(line string) (string, error) {
	k, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || k.Type() != ssh.KeyAlgoED25519 {
		return "", fmt.Errorf("%w: dedicated public key", ErrInvalid)
	}
	out := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	if comment != "" && regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`).MatchString(comment) {
		out += " " + comment
	}
	return out, nil
}

// NeededPorts lists the ports the profile will use on the host, as
// "port/proto". SSH is not included.
func NeededPorts(p BootstrapProfile) []string {
	var out []string
	if p.Cluster.Platform == "swarm" {
		if p.Cluster.Role != "join-worker" {
			out = append(out, "2377/tcp")
		}
		out = append(out, "7946/tcp", "7946/udp", "4789/udp")
	}
	if p.NativeAgent {
		out = append(out, fmt.Sprintf("%d/tcp", NativeAgentPort))
	}
	return out
}

// BuildBootstrapPlan builds the reviewed plan from a request, a preflight
// observation, the environment and the agent script generator. It returns a
// plan even when preflight findings block it, so the operator can see why;
// ErrExistingCluster is the exception because such a host is never planned.
func BuildBootstrapPlan(req BootstrapRequest, chk BootstrapCheck, env BootstrapEnv, agents AgentScripts, dedicatedPub string) (BootstrapPlan, error) {
	if err := req.Validate(); err != nil {
		return BootstrapPlan{}, err
	}
	if chk.HostID != req.Host.ID {
		return BootstrapPlan{}, ErrPreflight
	}
	if chk.ClusterID != "" {
		return BootstrapPlan{}, ErrExistingCluster
	}
	p := req.Profile
	plan := BootstrapPlan{ID: req.ID, Host: req.Host, Profile: p, HostKey: req.HostKeyFingerprint, Check: chk, Findings: []BootstrapFinding{}}
	find := func(sev, code, msg string) {
		plan.Findings = append(plan.Findings, BootstrapFinding{Severity: sev, Code: code, Message: msg})
		if sev == "block" {
			plan.Blocked = true
		}
	}
	if p.DedicatedKey {
		k, err := canonicalAuthorizedKey(dedicatedPub)
		if err != nil {
			return BootstrapPlan{}, err
		}
		plan.DedicatedKey = k
	}
	sup, err := LookupSupport(chk.OS, chk.Version)
	if err != nil {
		find("block", "os-unsupported", fmt.Sprintf("%s %s is not in the support matrix (Debian, Ubuntu, AlmaLinux, Rocky, Alpine).", chk.OS, chk.Version))
		return seal(plan), nil
	}
	plan.OS = sup
	if chk.Arch != "amd64" && chk.Arch != "arm64" {
		find("block", "arch-unsupported", "Only amd64 and arm64 hosts are supported, found "+chk.Arch+".")
	}
	if chk.Privilege != "root" && chk.Privilege != "sudo" {
		find("block", "privilege", "The login is neither root nor allowed to run sudo without a password.")
	}
	if chk.MemBytes < 1<<30 {
		find("block", "memory-low", "At least 1 GiB of memory is required.")
	} else if chk.MemBytes < 2<<30 && p.Cluster.Platform != "none" {
		find("warn", "memory-small", "Less than 2 GiB of memory is thin for a cluster member.")
	}
	if chk.DiskBytes < 10<<30 {
		find("block", "disk-low", "At least 10 GiB of free disk space is required.")
	}
	if chk.CPUs < 2 {
		find("warn", "cpu-low", "A single CPU leaves little room for containers.")
	}
	if len(chk.BusyPorts) > 0 {
		find("block", "ports-busy", "Ports already in use: "+strings.Join(chk.BusyPorts, ", ")+".")
	}
	if !chk.Systemd && sup.Init == "systemd" {
		find("block", "systemd-missing", "This system does not run systemd.")
	}
	if !chk.TimeSynchronized {
		find("info", "time-sync-added", "The clock is not synchronized; a time synchronization step (chrony) is part of the plan.")
	}
	if chk.AgentPresent && !chk.AgentMarker && p.NativeAgent {
		find("block", "agent-present", "A native agent is already installed here and was not installed by this tool; remove it or choose a different profile.")
	}
	if chk.Docker != "" && chk.Docker != "none" && p.Runtime == "docker" {
		find("warn", "docker-existing", "Docker engine "+chk.Docker+" is already installed. It is kept as it is (not upgraded, not reconfigured).")
	}
	if len(chk.OtherSoftware) > 0 {
		find("warn", "software-existing", "Existing software found: "+strings.Join(chk.OtherSoftware, ", ")+". This host is not fresh; nothing of it is changed.")
	}
	if chk.SSHRootLogin == "yes" || chk.SSHPasswordAuth == "yes" {
		find("info", "ssh-audit", "sshd currently allows root login ("+chk.SSHRootLogin+") and password authentication ("+chk.SSHPasswordAuth+"). Nothing is changed unless the separately approved lockdown phase is selected.")
	}
	if sup.ID == "alpine" {
		if p.NativeAgent {
			find("block", "native-agent-unsupported-os", "The native agent needs systemd; Alpine uses OpenRC.")
		}
		if p.Firewall {
			find("block", "firewall-unsupported-os", "The firewall option supports ufw and firewalld only.")
		}
		if p.UnattendedUpgrades {
			find("block", "unattended-unsupported-os", "Unattended upgrades are not available for Alpine.")
		}
	}
	if p.UnattendedUpgrades && p.Patch {
		find("warn", "unattended-with-patch", "Unattended upgrades and the patch capability both change packages; prefer one of them.")
	}
	if p.Firewall && p.Runtime == "docker" {
		find("warn", "firewall-docker", "Docker publishes container ports through its own firewall rules, which can bypass ufw and firewalld rules.")
	}
	if p.Cluster.Platform == "k3s" {
		find("block", "k3s-disabled", "k3s execution over SSH stays disabled (ADR 0018); the step is a preview only.")
	}
	if p.Cluster.Platform == "swarm" && p.Cluster.Role != "init" {
		find("info", "join", "The join token is a write-only secret of this draft and is delivered on standard input.")
	}
	if p.NativeAgent && sup.NativeAgent && agents == nil {
		return BootstrapPlan{}, ErrNoHook
	}
	plan.Steps, err = buildSteps(req, plan, env, agents)
	if err != nil {
		return BootstrapPlan{}, err
	}
	return seal(plan), nil
}

func seal(p BootstrapPlan) BootstrapPlan {
	if p.Steps == nil {
		p.Steps = []BootstrapStep{}
	}
	for i := range p.Steps {
		p.Steps[i].ScriptDigest = scriptDigest(p.Steps[i].Script)
	}
	p.LockdownDigest = p.ComputeLockdownDigest()
	p.Digest = p.ComputeDigest()
	return p
}
