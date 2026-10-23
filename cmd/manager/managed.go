// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
)

// managerConfig is the optional "manager" section. With it the manager
// keeps its state in the replicated store of internal/manager instead of
// memory.
type managerConfig struct {
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
	// Settings are installation settings the leader merges into the
	// replicated settings on every leadership start (no API sets them yet).
	Settings *settingsConfig `json:"settings,omitempty"`
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
			InspectionNetwork: b.InspectionNetwork}
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
}

func (b *backupConfig) validate() error {
	if b.ScheduleIntervalSeconds != 0 && b.ScheduleIntervalSeconds < 10 {
		return errors.New("manager.backup.scheduleIntervalSeconds must be at least 10")
	}
	if b.BackendConfigFile != "" && !filepath.IsAbs(b.BackendConfigFile) {
		return errors.New("manager.backup.backendConfigFile must be an absolute path")
	}
	return nil
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
	ID      string `json:"id"`
	Address string `json:"address"`
}

func (m *managerConfig) validate() error {
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
	svc     *manager.Service
	handler atomic.Pointer[http.Handler]
	stack   atomic.Pointer[manager.AuthStack]
	proto   http.Handler

	// Command services of the current leadership's API.
	keys       manager.KeyProvider
	keyring    *manager.Keyring // nil without manager.keyring
	swarms     *manager.Swarms
	monitoring *manager.Monitoring
	backend    backup.Backend
	backups    atomic.Pointer[manager.Backups]

	enroll *http.Server

	mu      sync.Mutex
	initErr error
}

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
	if mc.PeerTLS != nil {
		if scfg.PeerTLS, err = mc.PeerTLS.load(); err != nil {
			return nil, err
		}
	}
	for _, p := range mc.Peers {
		scfg.Peers = append(scfg.Peers, metastore.Peer{ID: p.ID, Address: p.Address})
	}
	backend, err := mc.Backup.openBackend()
	if err != nil {
		return nil, err
	}
	rt := &managedRuntime{proto: api.NewHandler(), keys: keys, backend: backend}
	rt.setHandler(rt.notLeader())
	swarms := &manager.Swarms{Matrix: swarm.DefaultMatrix(), Dial: func(h string) (swarm.Connector, error) { return swarm.Dial(h) }}
	rt.swarms = swarms
	scfg.Reconcilers = []manager.Reconciler{
		manager.SyncReconciler{Swarms: swarms},
		manager.BackupScheduler{Backups: rt.backups.Load, Interval: mc.scheduleInterval()},
	}
	if keyring != nil {
		rt.keyring = keyring
		scfg.Reconcilers = append(scfg.Reconcilers, manager.KeyringReconciler{Keyring: keyring})
	}
	// Every condition goes through the routing checker: approvals from the
	// replicated store, backup conditions through the backend, cluster
	// conditions through live agent probes. Anything unavailable fails
	// closed.
	checker := &manager.RoutingChecker{}
	if backend != nil {
		checker.Backup = &dbbackup.Checker{Backend: backend}
	}
	scfg.Checker = checker
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
	probe.Svc = svc
	checker.Store, checker.Ops = svc.Store(), &manager.Operations{Svc: svc}
	monCfg := manager.MonitoringConfig{DataDir: mc.monitoringDir(), Checker: rt, Keys: keys, Logger: log}
	if agents {
		monCfg.Status = manager.AgentStatusSource{Svc: svc}
	}
	if rt.monitoring, err = manager.NewMonitoring(svc, monCfg); err != nil {
		_ = svc.Close()
		return nil, err
	}
	switch {
	case mc.LabAgents:
		// Agents are only configurable before Start; the directory reads
		// host records from the service's own store.
		svc.SetAgents(manager.LabAgentDirectory{Store: svc.Store()})
	case mc.AgentTransport != nil:
		at := mc.AgentTransport
		pki := &manager.AgentPKI{Store: svc.Store(), Keys: keys, ServerDNS: at.ServerDNS, ServerIPs: serverIPs,
			RequireSourceAddr: !at.AllowAnySource}
		svc.SetAgents(&manager.AgentDirectory{Store: svc.Store(), PKI: pki, Port: at.AgentPort})
		swarms.Transport = &manager.AgentTransport{PKI: pki, ManagerURL: at.ManagerURL, Port: at.AgentPort,
			MonitorUser: at.MonitorUser, MonitorSecret: at.MonitorSecret, DBSecrets: at.DBSecrets}
		es := &enroll.Server{Authority: pki, Registry: pki, Log: log, Installation: ""}
		if rt.enroll, err = startEnrollment(at.EnrollListen, es, pki, log); err != nil {
			_ = svc.Close()
			return nil, err
		}
	}
	rt.svc = svc
	svc.OnLeadership(func(l *manager.Leadership) {
		if l == nil {
			rt.stack.Store(nil)
			rt.backups.Store(nil)
			rt.setHandler(rt.notLeader())
			return
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
	svc.Start()
	return rt, nil
}

func (rt *managedRuntime) setHandler(h http.Handler) { rt.handler.Store(&h) }

// ServeHTTP implements http.Handler.
func (rt *managedRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"not_leader"}`+"\n")
	})
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
	svc := rt.commandServices(st)
	acfg := api.Config{
		Passkeys: st.Passkeys, Accounts: st.Accounts, Sessions: st.Sessions, RecoverySessions: st.RecoverySessions,
		Codes: st.Codes, URLTokens: st.URLTokens, Limiter: st.Limiter, Authorizer: st.Authorizer, AuditSink: st.Audit,
		SetupToken: token, AdministratorExists: st.AdministratorExists, Inventory: inv, Activity: inv, Logger: log,
		Monitoring: rt.monitoring, Control: svc.control, Backups: svc.backups, Settings: svc.settings,
	}
	if rt.keyring != nil {
		acfg.Replicas = &manager.KeyringService{Svc: rt.svc, Ring: rt.keyring, Authorizer: st.Authorizer}
	}
	h, err := api.New(acfg)
	if err != nil {
		return nil, nil, err
	}
	rt.backups.Store(svc.backups)
	return h, st, nil
}

// operationRetries bounds, per step, the precondition polls and the retries
// of idempotent agent commands (runner backoff 1 s doubling to 30 s, so
// about 12 minutes). With the former zero value every operation paused at
// its first wait-synced, before the new node could have started (lab E4).
const operationRetries = 30

// commandSet groups the command services of one leadership.
type commandSet struct {
	control  *manager.Control
	backups  *manager.Backups
	settings *manager.SettingsService
}

// commandServices builds the command services on the leadership's
// authorizer.
func (rt *managedRuntime) commandServices(st *manager.AuthStack) commandSet {
	ops := &manager.Operations{Svc: rt.svc, Authorizer: st.Authorizer, MaxRetries: operationRetries}
	sw := *rt.swarms
	sw.Svc, sw.Authorizer = rt.svc, st.Authorizer
	ops.Swarms = &sw
	return commandSet{
		control:  &manager.Control{Ops: ops, Swarms: &sw},
		backups:  &manager.Backups{Ops: ops, Swarms: &sw, Backend: rt.backend},
		settings: &manager.SettingsService{Svc: rt.svc, Authorizer: st.Authorizer, Keys: rt.keys, Monitoring: rt.monitoring},
	}
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
