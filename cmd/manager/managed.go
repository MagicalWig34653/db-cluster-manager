// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imageupdate"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/managertls"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/modules"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// managerConfig is the optional "manager" section. With it the manager
// keeps its state in the replicated store of internal/manager instead of
// memory.
type managerConfig struct {
	// TLS configures the public API/UI listener, separately from peer/agent mTLS.
	TLS *tlsConfig `json:"tls,omitempty"`
	// NodeID defaults to "manager-1".
	NodeID string `json:"nodeId"`
	// DataDir holds the metastore and (by default) the data key file.
	DataDir string `json:"dataDir"`
	// Bind is the metastore TCP address (no TLS: loopback or isolated lab
	// networks only); Advertise is the address peers dial.
	Bind      string `json:"bind"`
	Advertise string `json:"advertise"`
	// Peers is the initial voting membership including this node; empty
	// means single-instance mode.
	Peers     []peerConfig `json:"peers"`
	Bootstrap bool         `json:"bootstrap"`
	// KeyFile holds the 32-byte data key for secrets at rest; default
	// <dataDir>/data.key, created if missing. Without Keyring every replica
	// needs the same file (copied out of band).
	KeyFile string `json:"keyFile"`
	// Keyring distributes the data key through the replicated keyring
	// instead (manager/keyring.go): each replica keeps only its identity in
	// <dataDir>/replica.key; an existing KeyFile is adopted as the first
	// key when the keyring is bootstrapped. Other replicas are registered
	// through PUT /settings/replicas/{id}/data-key-recipient.
	Keyring bool `json:"keyring"`
	// PeerTLS makes the metastore transport mutual TLS 1.3 between
	// replicas with certificates of a peer CA (manager -issue-peer-certs).
	PeerTLS *peerTLSConfig `json:"peerTLS,omitempty"`
	// APIForwardBind is the mTLS listener used only by other manager replicas.
	APIForwardBind string `json:"apiForwardBind,omitempty"`
	// LabAgents routes agent commands to each enrolled host's lab HTTP
	// endpoint (unauthenticated; lab only). Without it agent commands are
	// refused.
	LabAgents bool `json:"labAgents"`
	// AutoResume resumes taken-over operations on a new leader.
	AutoResume bool `json:"autoResume"`
	// AgentTransport enables the authenticated mTLS agent transport with
	// enrollment (ADR 0010). It excludes LabAgents.
	AgentTransport *agentTransportConfig `json:"agentTransport,omitempty"`
	// Monitoring configures the monitoring service (metric store, alerts,
	// notifications). Default: enabled with <dataDir>/monitoring.
	Monitoring *monitoringConfig `json:"monitoring,omitempty"`
	// Backup configures the backup subsystem on the manager side.
	Backup *backupConfig `json:"backup,omitempty"`
	// ImageBuild enables the image build service (X08, proposal). The
	// registry endpoints work without it; builds need it.
	ImageBuild *imageBuildConfig `json:"imageBuild,omitempty"`
	// Settings are installation settings the leader merges into the
	// replicated settings on every leadership start (no API sets them yet).
	Settings *settingsConfig `json:"settings,omitempty"`
	// Vuln enables the vulnerability response (X13, ADR 0015); see vuln.go.
	// It needs auth.enabled and contacts nothing unless feeds are listed.
	Vuln *vulnConfig `json:"vuln,omitempty"`
	// Patch enables OS patch management (X15, ADR 0023); off unless
	// "enabled" is true. See patch.go.
	Patch *patchConfig `json:"patch,omitempty"`
	// ImageUpdate enables the image update service (X16, ADR 0025); off
	// unless "enabled" is true. See imageupdate.go.
	ImageUpdate *imageUpdateConfig `json:"imageUpdate,omitempty"`
	// ImageComposer enables the image composer (X17, ADR 0026); off unless
	// "enabled" is true. Needs imageBuild. See bootstrap.go.
	ImageComposer *imageComposerConfig `json:"imageComposer,omitempty"`
	// ServerBootstrap enables the fresh-server bootstrap (X18, ADR 0027 and
	// 0028); off unless "enabled" is true. It is not called "bootstrap"
	// because that key already starts the metastore cluster. See bootstrap.go.
	ServerBootstrap *bootstrapConfig `json:"serverBootstrap,omitempty"`
	// ProxyWriters enables the replicated writer-choice protocol.
	ProxyWriters []proxyWriterConfig `json:"proxyWriters,omitempty"`
	// ExperimentalProvisioning and ExperimentalKubernetesProvisioning are the
	// initial state of the "provisioning" and "kubernetes" modules.
	ExperimentalProvisioning           bool `json:"experimentalProvisioning,omitempty"`
	ExperimentalKubernetesProvisioning bool `json:"experimentalKubernetesProvisioning,omitempty"`
}

// imageBuildConfig is the "manager.imageBuild" section. Presence enables
// the build service; the builder runs `<dockerBinary> buildx build`.
type imageBuildConfig struct {
	// DockerBinary defaults to "docker".
	DockerBinary string `json:"dockerBinary"`
	// WorkDir holds temporary build contexts and docker configs (absolute);
	// empty selects the OS temporary directory.
	WorkDir string `json:"workDir"`
	// MaxLogBytes caps the retained job log (default 262144, minimum 1024).
	MaxLogBytes int `json:"maxLogBytes"`
}

func (c *imageBuildConfig) validate() error {
	if c.WorkDir != "" && !filepath.IsAbs(c.WorkDir) {
		return errors.New("manager.imageBuild.workDir must be an absolute path")
	}
	if c.MaxLogBytes != 0 && c.MaxLogBytes < 1024 {
		return errors.New("manager.imageBuild.maxLogBytes must be at least 1024")
	}
	if strings.ContainsAny(c.DockerBinary, " \t\n") {
		return errors.New("manager.imageBuild.dockerBinary must be a single path or command name")
	}
	return nil
}

// builder returns the docker builder for this configuration.
func (c *imageBuildConfig) builder(r imagebuild.CommandRunner) *imagebuild.DockerBuilder {
	return &imagebuild.DockerBuilder{Binary: c.DockerBinary, Runner: r, WorkDir: c.WorkDir}
}

// settingsConfig is the "manager.settings" section (manager.ConfiguredSettings).
type settingsConfig struct {
	InstallationID string          `json:"installationId"`
	VolumeBase     string          `json:"volumeBase"`
	AgentImage     string          `json:"agentImage"`
	MonitorSocket  string          `json:"monitorSocket"`
	Accounts       *accountsConfig `json:"accounts,omitempty"`
	Backup         *backupSettings `json:"backup,omitempty"`
}

// accountsConfig names the database accounts of new clusters (galera.Accounts).
type accountsConfig struct {
	MonitorUser   string `json:"monitorUser"`
	MonitorSecret string `json:"monitorSecret"`
	BackupUser    string `json:"backupUser"`
	BackupSecret  string `json:"backupSecret"`
	BackupLogical bool   `json:"backupLogical"`
}

// backupSettings mirrors manager.BackupSettings.
type backupSettings struct {
	JobImage           string `json:"jobImage"`
	BackendSecret      string `json:"backendSecret"`
	DBCredentialSecret string `json:"dbCredentialSecret"`
	RecipientKey       string `json:"recipientKey"`
	KeySecret          string `json:"keySecret"`
	InspectionNetwork  string `json:"inspectionNetwork"`
	// TargetSecrets maps a manager.backup.targets name to the Swarm secret
	// with that target's backend configuration (restore of a copy).
	TargetSecrets map[string]string `json:"targetSecrets"`
}

func (c *settingsConfig) configured() manager.ConfiguredSettings {
	out := manager.ConfiguredSettings{InstallationID: c.InstallationID, VolumeBase: c.VolumeBase,
		AgentImage: c.AgentImage, MonitorSocket: c.MonitorSocket}
	if a := c.Accounts; a != nil {
		out.Accounts = &galera.Accounts{MonitorUser: a.MonitorUser, MonitorSecret: a.MonitorSecret,
			BackupUser: a.BackupUser, BackupSecret: a.BackupSecret, BackupLogical: a.BackupLogical}
	}
	if b := c.Backup; b != nil {
		out.Backup = &manager.BackupSettings{JobImage: b.JobImage, BackendSecret: b.BackendSecret,
			DBCredentialSecret: b.DBCredentialSecret, RecipientKey: b.RecipientKey, KeySecret: b.KeySecret,
			InspectionNetwork: b.InspectionNetwork, TargetSecrets: b.TargetSecrets}
	}
	return out
}

// monitoringConfig is the "manager.monitoring" section.
type monitoringConfig struct {
	// Disabled turns off metric collection, alerting and metric queries
	// (dashboards and the notification settings still work).
	Disabled bool `json:"disabled"`
	// DataDir holds the metric store; default <manager.dataDir>/monitoring.
	DataDir string `json:"dataDir"`
}

// backupConfig is the "manager.backup" section.
type backupConfig struct {
	// BackendConfigFile is a file with the same JSON as the jobs' backend
	// secret (dbbackup.BackendConfig). The manager reads manifests and
	// job status reports through it; without it backup gates stay unmet
	// and restores are refused (settings_incomplete).
	BackendConfigFile string `json:"backendConfigFile"`
	// ScheduleIntervalSeconds is how often the leader evaluates backup
	// schedules and inspection expiry (default 60, minimum 10).
	ScheduleIntervalSeconds int `json:"scheduleIntervalSeconds"`
	// Targets are further named backup targets (name to backend config
	// file, same JSON as backendConfigFile) that backups can be copied to,
	// for example the storage of a cluster in another Swarm. A backend
	// config may carry an "encryption" section (client-side encryption).
	Targets map[string]string `json:"targets"`
	// MetastoreTarget names the target of backups of the manager's own
	// metastore; empty uses the main backend.
	MetastoreTarget string `json:"metastoreTarget"`
	// MetastoreBackupHours, if positive, backs up the manager's own
	// metastore that often (needs backup.recipientKey in the settings).
	MetastoreBackupHours int `json:"metastoreBackupHours"`
	// MetastoreBackupKeep is the number of metastore backups kept
	// (default 14).
	MetastoreBackupKeep int `json:"metastoreBackupKeep"`
}

var targetNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (b *backupConfig) validate() error {
	if b.ScheduleIntervalSeconds != 0 && b.ScheduleIntervalSeconds < 10 {
		return errors.New("manager.backup.scheduleIntervalSeconds must be at least 10")
	}
	if b.BackendConfigFile != "" && !filepath.IsAbs(b.BackendConfigFile) {
		return errors.New("manager.backup.backendConfigFile must be an absolute path")
	}
	for name, file := range b.Targets {
		if !targetNamePattern.MatchString(name) || !filepath.IsAbs(file) {
			return fmt.Errorf("manager.backup.targets[%q]: needs a lower-case name and an absolute backend config file", name)
		}
	}
	if b.MetastoreTarget != "" {
		if _, ok := b.Targets[b.MetastoreTarget]; !ok {
			return fmt.Errorf("manager.backup.metastoreTarget %q is not in targets", b.MetastoreTarget)
		}
	}
	if b.MetastoreBackupHours < 0 || b.MetastoreBackupKeep < 0 {
		return errors.New("manager.backup.metastoreBackupHours and metastoreBackupKeep must not be negative")
	}
	return nil
}

// targetFile is the backend config file of a named target; the receiver may be nil.
func (b *backupConfig) targetFile(name string) (string, bool) {
	if b == nil {
		return "", false
	}
	f, ok := b.Targets[name]
	return f, ok
}

// openTargets reads and opens the named copy targets (no network I/O).
func (b *backupConfig) openTargets() (map[string]backup.Backend, error) {
	if b == nil || len(b.Targets) == 0 {
		return nil, nil
	}
	out := make(map[string]backup.Backend, len(b.Targets))
	for name, file := range b.Targets {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("manager.backup.targets[%q]: %w", name, err)
		}
		c, err := dbbackup.ParseBackendConfig(raw)
		if err != nil {
			return nil, fmt.Errorf("manager.backup.targets[%q]: %w", name, err)
		}
		if out[name], err = c.Open(); err != nil {
			return nil, fmt.Errorf("manager.backup.targets[%q]: %w", name, err)
		}
	}
	return out, nil
}

// openBackend reads and opens the configured backend (no network I/O).
func (b *backupConfig) openBackend() (backup.Backend, error) {
	if b == nil || b.BackendConfigFile == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(b.BackendConfigFile)
	if err != nil {
		return nil, fmt.Errorf("manager.backup.backendConfigFile: %w", err)
	}
	c, err := dbbackup.ParseBackendConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("manager.backup.backendConfigFile: %w", err)
	}
	be, err := c.Open()
	if err != nil {
		return nil, fmt.Errorf("manager.backup.backendConfigFile: %w", err)
	}
	return be, nil
}

// agentTransportConfig configures the mTLS agent transport.
type agentTransportConfig struct {
	// EnrollListen is the enrollment endpoint's TLS listen address.
	EnrollListen string `json:"enrollListen"`
	// ManagerURL is how agents reach EnrollListen (https://host:port).
	ManagerURL string `json:"managerUrl"`
	// ServerDNS/ServerIPs are extra SANs of the enrollment certificate.
	ServerDNS []string `json:"serverDns"`
	ServerIPs []string `json:"serverIps"`
	// AgentPort is the agents' listen port on their hosts (default 7443).
	AgentPort int `json:"agentPort"`
	// AllowAnySource disables the check that an enrollment request comes
	// from the claimed host's Swarm address (needed behind NAT only).
	AllowAnySource bool `json:"allowAnySource"`
	// MonitorUser and MonitorSecret (existing Swarm secret) give agents the
	// database monitoring credential for health collection.
	MonitorUser   string `json:"monitorUser"`
	MonitorSecret string `json:"monitorSecret"`
	// DBSecrets are further existing Swarm secrets the agent service mounts
	// (cluster root password, backup account password) so that exec-sql can
	// read them. Mounted only when the agent service is created.
	DBSecrets []string `json:"dbSecrets"`
}

func (a *agentTransportConfig) validate() ([]net.IP, error) {
	if a.EnrollListen == "" {
		return nil, errors.New("manager.agentTransport.enrollListen is required")
	}
	if u, err := url.Parse(a.ManagerURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("manager.agentTransport.managerUrl must be https://host:port")
	}
	var ips []net.IP
	for _, s := range a.ServerIPs {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("manager.agentTransport.serverIps: %q is not an IP", s)
		}
		ips = append(ips, ip)
	}
	if u, _ := url.Parse(a.ManagerURL); u != nil {
		if ip := net.ParseIP(u.Hostname()); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

type peerConfig struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	APIAddress string `json:"apiAddress,omitempty"`
}

func (m *managerConfig) validate() error {
	if err := m.TLS.validate(); err != nil {
		return err
	}
	if m.DataDir == "" {
		return errors.New("manager.dataDir is required")
	}
	if m.Bind == "" {
		return errors.New("manager.bind is required")
	}
	if m.AgentTransport != nil && m.LabAgents {
		return errors.New("manager.labAgents and manager.agentTransport exclude each other")
	}
	if m.Monitoring != nil && m.Monitoring.DataDir != "" && !filepath.IsAbs(m.Monitoring.DataDir) {
		return errors.New("manager.monitoring.dataDir must be an absolute path")
	}
	if m.Backup != nil {
		if err := m.Backup.validate(); err != nil {
			return err
		}
	}
	if m.Settings != nil && m.Settings.Backup != nil {
		for name := range m.Settings.Backup.TargetSecrets {
			if _, ok := m.Backup.targetFile(name); !ok {
				return fmt.Errorf("manager.settings.backup.targetSecrets[%q] names no manager.backup.targets entry", name)
			}
		}
	}
	if m.Vuln != nil {
		if err := m.Vuln.validate(); err != nil {
			return err
		}
	}
	if m.ImageBuild != nil {
		if err := m.ImageBuild.validate(); err != nil {
			return err
		}
	}
	if err := validateProxyWriters(m.ProxyWriters); err != nil {
		return err
	}
	if m.PeerTLS != nil && m.PeerTLS.Renewal != nil {
		r := m.PeerTLS.Renewal
		if r.CAFile == "" || r.PassphraseFile == "" || r.BeforeDays < 0 || r.BeforeDays > 365 ||
			r.ValidityDays < 0 || r.ValidityDays > 3650 || r.IntervalHours < 0 || r.IntervalHours > 168 || r.before() >= r.validity() {
			return errors.New("manager.peerTLS.renew needs caFile, passphraseFile and a renewal window shorter than validity")
		}
	}
	return m.validatePeers()
}

// validatePeers checks the replicated mode: this node is one of the peers
// (lab E4 re-run B: a quoting error gave every replica a node ID outside the
// peer list, and no replica ever led), IDs and addresses are unique, and the
// node's own entry is the address it advertises.
func (m *managerConfig) validatePeers() error {
	if m.APIForwardBind != "" {
		if m.PeerTLS == nil || len(m.Peers) < 3 {
			return errors.New("manager.apiForwardBind needs peerTLS and at least three peers")
		}
		if _, _, err := net.SplitHostPort(m.APIForwardBind); err != nil {
			return fmt.Errorf("manager.apiForwardBind: %w", err)
		}
		apiAddrs := map[string]bool{}
		for _, p := range m.Peers {
			if _, _, err := net.SplitHostPort(p.APIAddress); err != nil {
				return fmt.Errorf("manager.peers[%s].apiAddress: %w", p.ID, err)
			}
			if apiAddrs[p.APIAddress] {
				return fmt.Errorf("manager.peers repeat apiAddress %q", p.APIAddress)
			}
			apiAddrs[p.APIAddress] = true
		}
	} else {
		for _, p := range m.Peers {
			if p.APIAddress != "" {
				return errors.New("manager.peers[].apiAddress needs manager.apiForwardBind")
			}
		}
	}
	if len(m.Peers) == 0 {
		return nil
	}
	nodeID := m.NodeID
	if nodeID == "" {
		nodeID = "manager-1"
	}
	ids, addrs := map[string]bool{}, map[string]bool{}
	var self *peerConfig
	for i, p := range m.Peers {
		if p.ID == "" || p.Address == "" {
			return fmt.Errorf("manager.peers[%d] needs an id and an address", i)
		}
		if ids[p.ID] || addrs[p.Address] {
			return fmt.Errorf("manager.peers repeat id %q or address %q", p.ID, p.Address)
		}
		ids[p.ID], addrs[p.Address] = true, true
		if p.ID == nodeID {
			self = &m.Peers[i]
		}
	}
	if self == nil {
		return fmt.Errorf("manager.nodeId %q is not in manager.peers", nodeID)
	}
	adv := m.Advertise
	if adv == "" {
		adv = m.Bind
	}
	if self.Address != adv {
		return fmt.Errorf("manager.peers entry of %q has address %q, but this replica advertises %q", nodeID, self.Address, adv)
	}
	if m.APIForwardBind != "" {
		bindHost, bindPort, _ := net.SplitHostPort(m.APIForwardBind)
		apiHost, apiPort, _ := net.SplitHostPort(self.APIAddress)
		if bindPort != apiPort || (bindHost != "" && bindHost != "0.0.0.0" && bindHost != "::" && bindHost != apiHost) {
			return fmt.Errorf("manager.peers entry of %q has apiAddress %q, but this replica binds %q", nodeID, self.APIAddress, m.APIForwardBind)
		}
	}
	return nil
}

func (m *managerConfig) monitoringDir() string {
	switch {
	case m.Monitoring != nil && m.Monitoring.Disabled:
		return ""
	case m.Monitoring != nil && m.Monitoring.DataDir != "":
		return m.Monitoring.DataDir
	}
	return filepath.Join(m.DataDir, "monitoring")
}

func (m *managerConfig) scheduleInterval() time.Duration {
	if m.Backup != nil && m.Backup.ScheduleIntervalSeconds > 0 {
		return time.Duration(m.Backup.ScheduleIntervalSeconds) * time.Second
	}
	return time.Minute
}

// managedRuntime runs a manager.Service and serves the API of the current
// leadership. On a replica that does not lead, only health and version are
// answered; every other API request gets 503 with the known leader in the
// X-DCM-Leader header (no request forwarding).
type managedRuntime struct {
	acme         *managertls.Manager
	svc          *manager.Service
	handler      atomic.Pointer[http.Handler]
	stack        atomic.Pointer[manager.AuthStack]
	proto        http.Handler
	proxyWriters map[string]proxyWriterConfig

	// Command services of the current leadership's API.
	keys        manager.KeyProvider
	keyring     *manager.Keyring  // nil without manager.keyring
	pki         *manager.AgentPKI // nil without manager.agentTransport
	swarms      *manager.Swarms
	monitoring  *manager.Monitoring
	backend     backup.Backend
	targets     map[string]backup.Backend
	backupCfg   *backupConfig
	backups     atomic.Pointer[manager.Backups]
	hostSources atomic.Pointer[manager.HostSources]

	// imageRunner executes docker for image builds.
	imageCfg    *imageBuildConfig // nil: builds disabled
	imageRunner imagebuild.CommandRunner
	images      atomic.Pointer[api.ImageBuildBackend]

	// vulnCfg is nil when the vulnerability response is disabled; vuln is
	// the current leadership's service.
	vulnCfg *vulnConfig
	vuln    atomic.Pointer[vuln.Service]
	// The optional services are the current leadership's.
	patchCfg       *patchConfig
	imageUpdateCfg *imageUpdateConfig
	imageUpdate    atomic.Pointer[imageupdate.Service]
	patch          atomic.Pointer[patch.Service]
	installer      atomic.Pointer[patch.Installer]
	bootstrap      atomic.Pointer[provision.BootstrapService]
	bootstrapCfg   *bootstrapConfig
	agentTransport *agentTransportConfig
	// modules is the current leadership's module registry; nil without one,
	// so nothing optional runs on a replica that does not lead.
	modules        atomic.Pointer[modules.Registry]
	moduleDefaults map[string]bool

	enroll           *http.Server
	forwardServer    *http.Server
	forwardTLS       *metastore.PeerTLS
	forwardPeers     map[string]peerConfig
	forwardTransport func(peerConfig) http.RoundTripper // tests replace network transport
	renewStop        context.CancelFunc
	renewWG          sync.WaitGroup

	mu      sync.Mutex
	initErr error
}

// newImageRunner returns the command runner of the image builder. Tests
// replace it to avoid running docker.
var newImageRunner = func() imagebuild.CommandRunner { return imagebuild.ExecRunner{} }

func newManagedRuntime(cfg fileConfig, log *slog.Logger, tokenOut io.Writer) (*managedRuntime, error) {
	mc := cfg.Manager
	if err := mc.validate(); err != nil {
		return nil, err
	}
	var pkCfg passkey.Config
	if cfg.Auth.Enabled {
		pkCfg = passkey.Config{RPID: cfg.Auth.RPID, RPDisplayName: cfg.Auth.RPDisplayName, Origins: cfg.Auth.Origins}
		if err := pkCfg.Validate(); err != nil {
			return nil, fmt.Errorf("auth config: %w", err)
		}
	}
	nodeID := mc.NodeID
	if nodeID == "" {
		nodeID = "manager-1"
	}
	keys, keyring, err := mc.dataKeys(nodeID)
	if err != nil {
		return nil, err
	}
	scfg := manager.Config{
		NodeID: nodeID, DataDir: filepath.Join(mc.DataDir, "metastore"), BindAddr: mc.Bind, AdvertiseAddr: mc.Advertise,
		Bootstrap: mc.Bootstrap, AutoResume: mc.AutoResume, Logger: log, Keyring: keyring,
	}
	var peerStore atomic.Pointer[manager.Store]
	if mc.PeerTLS != nil {
		if _, err := mc.PeerTLS.renewOnce(context.Background(), time.Now); err != nil {
			log.Error("manager: initial peer certificate renewal failed", "err", err)
		}
		if scfg.PeerTLS, err = mc.PeerTLS.load(); err != nil {
			return nil, err
		}
	}
	if scfg.PeerTLS != nil {
		scfg.PeerTLS.Revoked = func(c *x509.Certificate) bool { st := peerStore.Load(); return st == nil || manager.PeerRevoked(st, c) }
		scfg.PeerTLS.PeerAllowed = func(id string) bool { st := peerStore.Load(); return st != nil && manager.PeerAllowed(st, id) }
	}
	for _, p := range mc.Peers {
		scfg.Peers = append(scfg.Peers, metastore.Peer{ID: p.ID, Address: p.Address})
	}
	backend, err := mc.Backup.openBackend()
	if err != nil {
		return nil, err
	}
	targets, err := mc.Backup.openTargets()
	if err != nil {
		return nil, err
	}
	if mc.Vuln != nil && !cfg.Auth.Enabled {
		return nil, errors.New("manager.vuln needs auth.enabled")
	}
	if err := mc.Patch.validate(mc, cfg.Auth.Enabled); err != nil {
		return nil, err
	}
	if err := mc.ImageUpdate.validate(cfg.Auth.Enabled); err != nil {
		return nil, err
	}
	if err := mc.ImageComposer.validate(mc, cfg.Auth.Enabled); err != nil {
		return nil, err
	}
	if err := mc.ServerBootstrap.validate(mc, cfg.Auth.Enabled); err != nil {
		return nil, err
	}
	// Every optional feature is built when its configuration allows it; the
	// module registry (internal/modules) decides whether it runs. The
	// sections only supply the default of a module nobody switched yet.
	patchCfg, iuCfg, bootCfg := orDefault(mc.Patch), orDefault(mc.ImageUpdate), orDefault(mc.ServerBootstrap)
	rt := &managedRuntime{bootstrapCfg: bootCfg, moduleDefaults: mc.moduleDefaults(), agentTransport: mc.AgentTransport, patchCfg: patchCfg, imageUpdateCfg: iuCfg, proto: api.NewHandler(), keys: keys, backend: backend, targets: targets, backupCfg: mc.Backup, imageCfg: mc.ImageBuild, imageRunner: newImageRunner(), vulnCfg: mc.Vuln,
		proxyWriters: map[string]proxyWriterConfig{},
		forwardTLS:   scfg.PeerTLS, forwardPeers: map[string]peerConfig{}}
	for _, w := range mc.ProxyWriters {
		rt.proxyWriters[w.ClusterID] = w
	}
	for _, p := range mc.Peers {
		rt.forwardPeers[p.ID] = p
	}
	rt.setHandler(rt.notLeader())
	swarms := &manager.Swarms{Matrix: swarm.DefaultMatrix(), Dial: func(h string) (swarm.Connector, error) { return swarm.Dial(h) }}
	rt.swarms = swarms
	scfg.Reconcilers = []manager.Reconciler{
		manager.DenyListCompactor{},
		manager.SyncReconciler{Swarms: swarms},
		manager.DriftDetector{Swarms: swarms},
		manager.BackupScheduler{Backups: rt.backups.Load, Interval: mc.scheduleInterval()},
		manager.HostSourceScheduler{Sources: rt.hostSources.Load, Interval: mc.scheduleInterval()},
		// Service health from the Swarm task state; transitions go to the
		// monitoring dispatcher like node health (rt.monitoring is set
		// before Start).
		manager.ServiceHealthCollector{Swarms: swarms, Notify: func(ctx context.Context, e notify.Event) bool {
			if rt.monitoring == nil {
				return false
			}
			return rt.monitoring.Notify(ctx, e)
		}},
	}
	// The pollers skip while their module is off.
	if mc.Vuln != nil {
		scfg.Reconcilers = append(scfg.Reconcilers, manager.VulnScheduler{Poll: rt.pollVuln, Interval: mc.Vuln.pollInterval(), Logf: log.Warn})
	}
	scfg.Reconcilers = append(scfg.Reconcilers,
		manager.PatchScheduler{Poll: rt.pollPatch, Interval: patchCfg.interval(), Logf: log.Warn},
		// Expired sealed credentials of bootstrap and SSH install drafts.
		manager.DraftSweeper{Poll: rt.pollBootstrap, Interval: bootCfg.sweepEvery(), Logf: log.Warn},
		manager.ImageUpdateScheduler{Poll: rt.pollImageUpdate, Interval: iuCfg.interval(), Logf: log.Warn})
	if keyring != nil {
		rt.keyring = keyring
		scfg.Reconcilers = append(scfg.Reconcilers, manager.KeyringReconciler{Keyring: keyring})
	}
	var denyDistributor *manager.DenyListDistributor
	if mc.AgentTransport != nil {
		// Revoked serials (renewals, revoked hosts and manager
		// certificates) reach every enrolled agent.
		denyDistributor = &manager.DenyListDistributor{}
		scfg.Reconcilers = append(scfg.Reconcilers, denyDistributor)
	}
	// Every condition goes through the routing checker: approvals from the
	// replicated store, backup conditions through the backend, cluster
	// conditions through live agent probes. Anything unavailable fails
	// closed.
	checker := &manager.RoutingChecker{}
	if backend != nil {
		checker.Backup = &dbbackup.Checker{Backend: backend}
		checker.Targets = targets
	}
	scfg.Checker = checker
	// Zero-UUID recovery (ADR 0029): plans phase 2, runs the opt-in
	// autoRecover policy, notifies; Ops is set once the Service exists.
	zeroRec := &manager.ZeroUUIDReconciler{}
	scfg.Reconcilers = append(scfg.Reconcilers, zeroRec)
	var probe manager.AgentHealthProbe
	agents := mc.LabAgents || mc.AgentTransport != nil
	if agents {
		// Live preconditions and periodic health go through the agents.
		checker.Cluster = &galeraprobe.Checker{Prober: &probe, Topology: &probe}
		// Node outages and recoveries go to the monitoring dispatcher (UI
		// inbox and configured channels); rt.monitoring is set before Start.
		scfg.Reconcilers = append(scfg.Reconcilers, manager.HealthCollector{Notify: func(ctx context.Context, e notify.Event) bool {
			if rt.monitoring == nil {
				return false
			}
			return rt.monitoring.Notify(ctx, e)
		}})
	}
	var autoWriterRoutes []manager.ProxyWriterRoute
	for _, w := range mc.ProxyWriters {
		if !w.AutoFailover {
			continue
		}
		ids := make([]string, 0, len(w.Members))
		for _, m := range w.Members {
			ids = append(ids, m.ID)
		}
		autoWriterRoutes = append(autoWriterRoutes, manager.ProxyWriterRoute{Cluster: w.ClusterID, InitialWriter: w.InitialWriter, Candidates: slices.Clone(w.Candidates), Members: ids})
	}
	if len(autoWriterRoutes) > 0 {
		scfg.Reconcilers = append(scfg.Reconcilers, manager.ProxyWriterReconciler{Routes: autoWriterRoutes})
	}
	var serverIPs []net.IP
	if at := mc.AgentTransport; at != nil {
		if serverIPs, err = at.validate(); err != nil {
			return nil, err
		}
	}
	svc, err := manager.New(scfg)
	if err != nil {
		return nil, err
	}
	peerStore.Store(svc.Store())
	probe.Svc = svc
	// Swarms lets the checker observe services for the update health gate.
	checker.Store, checker.Ops = svc.Store(), &manager.Operations{Svc: svc, Swarms: swarms}
	zeroRec.Ops = &manager.Operations{Svc: svc, Swarms: swarms, MaxRetries: operationRetries, Notify: func(ctx context.Context, e notify.Event) bool {
		if rt.monitoring == nil {
			return false
		}
		return rt.monitoring.Notify(ctx, e)
	}}
	monCfg := manager.MonitoringConfig{DataDir: mc.monitoringDir(), Checker: rt, Keys: keys, Logger: log}
	if agents {
		monCfg.Status = manager.AgentStatusSource{Svc: svc}
	}
	if rt.monitoring, err = manager.NewMonitoring(svc, monCfg); err != nil {
		_ = svc.Close()
		return nil, err
	}
	var agentClient runner.AgentClient
	switch {
	case mc.LabAgents:
		// Agents are only configurable before Start; the directory reads
		// host records from the service's own store.
		agentClient = manager.LabAgentDirectory{Store: svc.Store()}
	case mc.AgentTransport != nil:
		at := mc.AgentTransport
		pki := &manager.AgentPKI{Store: svc.Store(), Keys: keys, ReplicaID: nodeID, ServerDNS: at.ServerDNS, ServerIPs: serverIPs,
			RequireSourceAddr: !at.AllowAnySource}
		rt.pki = pki
		dir := &manager.AgentDirectory{Store: svc.Store(), PKI: pki, Port: at.AgentPort}
		dir.NativePort = patchCfg.port() // restic may run on the native agent (ADR 0024)
		denyDistributor.Native = &manager.PatchAgentDirectory{Dir: dir}
		agentClient = dir
		swarms.Transport = &manager.AgentTransport{PKI: pki, ManagerURL: at.ManagerURL, Port: at.AgentPort,
			MonitorUser: at.MonitorUser, MonitorSecret: at.MonitorSecret, DBSecrets: at.DBSecrets}
		es := &enroll.Server{Authority: pki, Registry: pki, Log: log, Installation: ""}
		if rt.enroll, err = startEnrollment(at.EnrollListen, es, pki, log); err != nil {
			_ = svc.Close()
			return nil, err
		}
	}
	agentClient = manager.ProvisionAgentClient{Next: agentClient, Store: svc.Store(), Keys: keys, Commands: provision.SSHRunner{}}
	agentClient = manager.HostSourceSSHClient{Next: agentClient, Store: svc.Store(), Keys: keys, Commands: provision.SSHRunner{}}
	if agentClient != nil {
		svc.SetAgents(agentClient)
	}
	rt.svc = svc
	if mc.TLS != nil && mc.TLS.ACME != nil {
		rt.acme, err = managertls.New(*mc.TLS.ACME, manager.ACMEStorage{Store: svc.Store(), Keys: keys}, nil)
		if err != nil {
			if rt.enroll != nil {
				_ = rt.enroll.Close()
			}
			_ = svc.Close()
			return nil, err
		}
	}
	svc.OnLeadership(func(l *manager.Leadership) {
		if l == nil {
			rt.stack.Store(nil)
			rt.modules.Store(nil)
			rt.backups.Store(nil)
			rt.hostSources.Store(nil)
			rt.vuln.Store(nil)
			rt.patch.Store(nil)
			rt.installer.Store(nil)
			rt.bootstrap.Store(nil)
			rt.imageUpdate.Store(nil)
			rt.setHandler(rt.notLeader())
			return
		}
		if mc.TLS != nil && mc.TLS.ACME != nil {
			// The worker has a fresh instance and a storage fence for this term.
			worker, err := managertls.New(*mc.TLS.ACME, manager.ACMEStorage{Store: svc.Store(), Keys: keys, CanWrite: l.Valid}, nil)
			if err == nil {
				l.Go(func(ctx context.Context) {
					worker.Run(ctx, func(err error) { log.Error("manager: ACME issuance/renewal failed", "err", err) })
				})
			}
		}
		if mc.Settings != nil {
			changed, err := manager.ApplyConfiguredSettings(svc.Store(), mc.Settings.configured())
			if err != nil {
				log.Error("manager: configured settings not applied", "err", err)
			} else if changed {
				log.Info("manager: configured settings applied")
			}
		}
		if !cfg.Auth.Enabled {
			rt.setHandler(rt.proto)
			return
		}
		h, st, err := rt.leaderAPI(pkCfg, keys, log, tokenOut)
		if err != nil {
			log.Error("manager: cannot serve the API on this leader", "err", err)
			rt.mu.Lock()
			rt.initErr = err
			rt.mu.Unlock()
			return
		}
		rt.stack.Store(st)
		rt.setHandler(h)
	})
	if mc.APIForwardBind != "" {
		ln, err := net.Listen("tcp", mc.APIForwardBind)
		if err != nil {
			if rt.enroll != nil {
				_ = rt.enroll.Close()
			}
			_ = svc.Close()
			return nil, fmt.Errorf("manager API forwarding listen: %w", err)
		}
		rt.forwardServer = &http.Server{Handler: rt, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := rt.forwardServer.Serve(tls.NewListener(ln, rt.forwardTLS.ServerConfig())); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("manager API forwarding listener stopped", "err", err)
			}
		}()
	}
	svc.Start()
	if mc.PeerTLS != nil && mc.PeerTLS.Renewal != nil {
		ctx, cancel := context.WithCancel(context.Background())
		rt.renewStop = cancel
		rt.renewWG.Add(1)
		go func() {
			defer rt.renewWG.Done()
			t := time.NewTicker(mc.PeerTLS.Renewal.interval())
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if _, err := mc.PeerTLS.renewOnce(ctx, time.Now); err != nil {
						log.Error("manager: peer certificate renewal failed", "err", err)
					}
				}
			}
		}()
	}
	return rt, nil
}

func (rt *managedRuntime) setHandler(h http.Handler) { rt.handler.Store(&h) }

// ServeHTTP implements http.Handler.
func (rt *managedRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Proxy-writer endpoints authenticate with their own tokens and answer
	// on any replica that can reach the leader; they are routed first.
	if strings.HasPrefix(r.URL.Path, "/api/v1/proxy-writers/") {
		rt.serveProxyWriter(w, r)
		return
	}
	if rt.svc != nil && rt.stack.Load() != nil {
		if !rt.svc.IsLeader() {
			rt.notLeader().ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if err := rt.svc.WriteBarrier(); err != nil {
				rt.notLeader().ServeHTTP(w, r)
				return
			}
		}
	}
	(*rt.handler.Load()).ServeHTTP(w, r)
}

func (rt *managedRuntime) notLeader() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health", "/api/v1/version":
			rt.proto.ServeHTTP(w, r)
			return
		}
		if rt.svc != nil {
			if id, _ := rt.svc.LeaderAddress(); id != "" {
				w.Header().Set("X-DCM-Leader", id)
				if r.Header.Get("X-DCM-Forwarded") == "" && rt.forwardTLS != nil {
					if p, ok := rt.forwardPeers[id]; ok && p.APIAddress != "" && id != rt.svc.Node().ID() {
						rt.forwardTo(w, r, p)
						return
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"not_leader"}`+"\n")
	})
}

// forwardTo sends a request once. An uncertain write is never replayed after
// a transport error; the caller must inspect its outcome on the new leader.
func (rt *managedRuntime) forwardTo(w http.ResponseWriter, r *http.Request, p peerConfig) {
	tr := &http.Transport{TLSClientConfig: rt.forwardTLS.ClientConfig(p.Address),
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: time.Second}
	defer tr.CloseIdleConnections()
	var transport http.RoundTripper = tr
	if rt.forwardTransport != nil {
		transport = rt.forwardTransport(p)
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "https", Host: p.APIAddress})
			pr.Out.Header.Set("X-DCM-Forwarded", "1")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"leader_unavailable"}`+"\n")
		},
	}
	proxy.ServeHTTP(w, r)
}

// leaderAPI builds the full API from the replicated state. A setup token is
// printed only while no administrator exists.
func (rt *managedRuntime) leaderAPI(pk passkey.Config, keys manager.KeyProvider, log *slog.Logger, tokenOut io.Writer) (http.Handler, *manager.AuthStack, error) {
	st, err := manager.LoadAuth(rt.svc.Store(), keys, manager.AuthConfig{Passkey: pk, Logger: log})
	if err != nil {
		return nil, nil, err
	}
	var token *api.SetupToken
	if !st.AdministratorExists() {
		t, plain, err := api.NewSetupToken()
		if err != nil {
			return nil, nil, err
		}
		token = t
		fmt.Fprintf(tokenOut, "First-run setup token (valid until the first administrator is created): %s\n", plain)
	}
	// Alert evaluation and scheduled backups run as the manager's own
	// subject; it needs its system role binding (persisted with the policy).
	if added, err := manager.EnsureSystemBinding(st.Policy); err != nil {
		return nil, nil, fmt.Errorf("manager system binding: %w", err)
	} else if added {
		log.Info("manager: bound the manager system subject to its system role")
	}
	inv := manager.Inventory{Store: rt.svc.Store()}
	svc, err := rt.commandServices(st)
	if err != nil {
		return nil, nil, err
	}
	acfg := api.Config{
		Passkeys: st.Passkeys, Accounts: st.Accounts, Sessions: st.Sessions, RecoverySessions: st.RecoverySessions,
		Codes: st.Codes, URLTokens: st.URLTokens, Limiter: st.Limiter, Authorizer: st.Authorizer, AuditSink: st.Audit,
		SetupToken: token, AdministratorExists: st.AdministratorExists, Inventory: inv, Activity: inv, Logger: log,
		Monitoring: rt.monitoring, Control: svc.control, Backups: svc.backups, Settings: svc.settings, Maintenance: svc.control, EngineUpdates: svc.control,
		Services:    svc.services,
		HostSources: svc.hostSources,
	}
	reg, err := modules.New(moduleStore{rt.svc.Store()}, rt.moduleDefaults, rt.missingConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("module states: %w", err)
	}
	acfg.Modules = reg
	acfg.Kubernetes = manager.KubernetesRegistrations{Enabled: true, Store: rt.svc.Store(), Keys: keys}
	acfg.Provisioning = &manager.Provisioning{Ops: svc.control.Ops, Keys: keys, Commands: provision.SSHRunner{},
		AllowKubernetes: func() bool { return reg.Enabled("kubernetes") }}
	// Registries are always available; builds only with manager.imageBuild.
	// The interfaces stay nil otherwise, so the routes answer 503.
	if svc.images != nil {
		acfg.Registries = svc.images
		if svc.images.Builds != nil {
			acfg.ImageBuilds = svc.images
			if n, err := svc.images.Builds.RecoverInterrupted(context.Background()); err != nil {
				log.Warn("manager: recover interrupted image builds failed", "error", err)
			} else if n > 0 {
				log.Warn("manager: marked interrupted image builds as failed", "count", n)
			}
		}
		rt.images.Store(svc.images)
	}
	if svc.vuln != nil {
		acfg.Vuln = &api.VulnBackend{Svc: svc.vuln}
	}
	// Without manager.patch the interface stays nil and the routes answer 503.
	if svc.patch != nil {
		// The SSH installer needs the enrollment token source of the agent
		// transport; without it its routes answer 503.
		acfg.Patch = &api.PatchBackend{Svc: svc.patch, Installer: svc.installer}
	}
	if svc.bootstrap != nil {
		acfg.Bootstrap = svc.bootstrap
	}
	if svc.builds != nil {
		acfg.ImageComposer = &api.ImageComposerBackend{Builds: svc.builds}
	}
	if svc.imageUpdate != nil {
		iub := &api.ImageUpdateBackend{Svc: svc.imageUpdate}
		if svc.builds != nil {
			iub.Builds = buildLookup{Jobs: manager.ImageJobStore{Store: rt.svc.Store()}}
		}
		acfg.ImageUpdates = iub
	}
	// Without a keyring or the agent transport the respective endpoints
	// answer settings_incomplete.
	acfg.Replicas = &manager.ReplicaService{Svc: rt.svc, Ring: rt.keyring, PKI: rt.pki, Authorizer: st.Authorizer}
	h, err := api.New(acfg)
	if err != nil {
		return nil, nil, err
	}
	rt.modules.Store(reg)
	rt.backups.Store(svc.backups)
	rt.hostSources.Store(svc.hostSources)
	if svc.vuln != nil {
		rt.seedVuln(context.Background(), svc.vuln, log)
		rt.vuln.Store(svc.vuln)
	}
	if svc.patch != nil {
		if n, err := svc.patch.RecoverInterrupted(context.Background()); err != nil {
			log.Warn("manager: interrupted patch runs not recovered", "err", err)
		} else if n > 0 {
			log.Warn("manager: marked patch runs interrupted by a restart or failover as failed", "count", n)
		}
		rt.patch.Store(svc.patch)
		rt.installer.Store(svc.installer)
	}
	if svc.bootstrap != nil {
		// Drafts of a previous process or leader: credentials past their
		// expiry are deleted, a run without a recorded end becomes unknown.
		if err := svc.bootstrap.Restore(context.Background()); err != nil {
			log.Warn("manager: bootstrap drafts not restored", "err", err)
		}
		rt.bootstrap.Store(svc.bootstrap)
	}
	if svc.imageUpdate != nil {
		if n, err := svc.imageUpdate.RecoverInterrupted(context.Background()); err != nil {
			log.Warn("manager: interrupted image updates not recovered", "err", err)
		} else if n > 0 {
			log.Warn("manager: marked image updates interrupted by a restart or failover as failed", "count", n)
		}
		rt.imageUpdate.Store(svc.imageUpdate)
	}
	return h, st, nil
}

// operationRetries bounds, per step, the precondition polls and the retries
// of idempotent agent commands (runner backoff 1 s doubling to 30 s, so
// about 12 minutes). With the former zero value every operation paused at
// its first wait-synced, before the new node could have started (lab E4).
const operationRetries = 30

// commandSet groups the command services of one leadership.
type commandSet struct {
	control     *manager.Control
	backups     *manager.Backups
	hostSources *manager.HostSources
	settings    *manager.SettingsService
	services    *manager.Services
	images      *api.ImageBuildBackend
	vuln        *vuln.Service  // nil without manager.vuln
	patch       *patch.Service // nil without the agent transport
	imageUpdate *imageupdate.Service
	builds      *imagebuild.Service // nil without manager.imageBuild
	installer   *patch.Installer    // nil without the agent transport
	bootstrap   *provision.BootstrapService
}

// newBackups builds the backup service with copy targets, the metastore
// backup settings and the monitoring notification hook.
func (rt *managedRuntime) newBackups(ops *manager.Operations, sw *manager.Swarms) *manager.Backups {
	b := &manager.Backups{Ops: ops, Swarms: sw, Backend: rt.backend, Targets: rt.targets, Keys: rt.keys,
		Notify: func(ctx context.Context, e notify.Event) bool {
			if rt.monitoring == nil {
				return false
			}
			return rt.monitoring.Notify(ctx, e)
		}}
	if c := rt.backupCfg; c != nil {
		b.MetaTarget = c.MetastoreTarget
		b.MetaInterval = time.Duration(c.MetastoreBackupHours) * time.Hour
		b.MetaKeep = c.MetastoreBackupKeep
	}
	return b
}

// commandServices builds the command services on the leadership's
// authorizer.
func (rt *managedRuntime) commandServices(st *manager.AuthStack) (commandSet, error) {
	ops := &manager.Operations{Svc: rt.svc, Authorizer: st.Authorizer, MaxRetries: operationRetries, Notify: func(ctx context.Context, e notify.Event) bool {
		if rt.monitoring == nil {
			return false
		}
		return rt.monitoring.Notify(ctx, e)
	}}
	sw := *rt.swarms
	sw.Svc, sw.Authorizer = rt.svc, st.Authorizer
	ops.Swarms = &sw
	var builder imagebuild.Builder
	maxLog := 0
	if ib := rt.imageCfg; ib != nil {
		builder, maxLog = ib.builder(rt.imageRunner), ib.MaxLogBytes
	}
	builds, regs := manager.ImageServices(rt.svc.Store(), rt.keys, builder, true, maxLog)
	images := &api.ImageBuildBackend{Regs: regs}
	if builder != nil {
		images.Builds = builds
	}
	set := commandSet{
		images:  images,
		control: &manager.Control{Ops: ops, Swarms: &sw},
		backups: rt.newBackups(ops, &sw),
		hostSources: &manager.HostSources{Ops: ops, Keys: rt.keys, Notify: func(ctx context.Context, e notify.Event) bool {
			if rt.monitoring == nil {
				return false
			}
			return rt.monitoring.Notify(ctx, e)
		}},
		settings: &manager.SettingsService{Svc: rt.svc, Authorizer: st.Authorizer, Keys: rt.keys, Monitoring: rt.monitoring},
		services: &manager.Services{Ops: ops},
	}
	if rt.vulnCfg != nil {
		v, err := rt.newVulnService(images)
		if err != nil {
			return commandSet{}, err
		}
		set.vuln = v
	}
	if rt.pki != nil {
		set.patch, set.installer = rt.newPatchService(), rt.newPatchInstaller()
		if rt.bootstrapCfg.releaseErr() == nil {
			set.bootstrap = rt.newBootstrapService(ops.Swarms)
		}
	}
	if builder != nil {
		set.builds = builds
	}
	set.imageUpdate = rt.newImageUpdateService(set.services, regs)
	return set, nil
}

// Check implements authz.Checker for the monitoring service: it delegates
// to the current leadership's authorizer and denies without one.
func (rt *managedRuntime) Check(s authz.Subject, a authz.Action, r authz.Resource) authz.Decision {
	if st := rt.stack.Load(); st != nil {
		return st.Authorizer.Check(s, a, r)
	}
	return authz.Decision{}
}

func (rt *managedRuntime) prune() {
	if st := rt.stack.Load(); st != nil {
		st.Prune()
	}
}

func (rt *managedRuntime) Close() error {
	if rt.renewStop != nil {
		rt.renewStop()
		rt.renewWG.Wait()
	}
	if rt.forwardServer != nil {
		_ = rt.forwardServer.Close()
	}
	if rt.enroll != nil {
		_ = rt.enroll.Close()
	}
	return rt.svc.Close()
}

// startEnrollment serves the agent enrollment endpoint. The installation
// ID is read per request from the replicated settings.
func startEnrollment(addr string, es *enroll.Server, pki *manager.AgentPKI, log *slog.Logger) (*http.Server, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("manager.agentTransport.enrollListen: %w", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inst, err := pki.Installation()
			if err != nil {
				http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			s := *es
			s.Installation = inst
			s.Handler().ServeHTTP(w, r)
		}),
		TLSConfig:         es.TLSConfig(pki.EnrollmentServerCertificate),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	go func() {
		if err := srv.ServeTLS(l, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("manager: enrollment endpoint stopped", "err", err)
		}
	}()
	log.Info("manager: agent enrollment endpoint listening", "addr", l.Addr().String())
	return srv, nil
}
