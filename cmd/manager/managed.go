// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
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
	// <dataDir>/data.key, created if missing. Every replica needs the same
	// key: provisioning it is not solved (copy it out of band).
	KeyFile string `json:"keyFile"`
	// LabAgents routes agent commands to each enrolled host's lab HTTP
	// endpoint (unauthenticated; lab only). Without it agent commands are
	// refused.
	LabAgents bool `json:"labAgents"`
	// AutoResume resumes taken-over operations on a new leader.
	AutoResume bool `json:"autoResume"`
	// AgentTransport enables the authenticated mTLS agent transport with
	// enrollment (ADR 0010). It excludes LabAgents.
	AgentTransport *agentTransportConfig `json:"agentTransport,omitempty"`
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
	return nil
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
	keyFile := mc.KeyFile
	if keyFile == "" {
		keyFile = filepath.Join(mc.DataDir, "data.key")
	}
	keys, err := manager.FileKeyProvider(keyFile)
	if err != nil {
		return nil, err
	}
	nodeID := mc.NodeID
	if nodeID == "" {
		nodeID = "manager-1"
	}
	scfg := manager.Config{
		NodeID: nodeID, DataDir: filepath.Join(mc.DataDir, "metastore"), BindAddr: mc.Bind, AdvertiseAddr: mc.Advertise,
		Bootstrap: mc.Bootstrap, AutoResume: mc.AutoResume, Logger: log,
	}
	for _, p := range mc.Peers {
		scfg.Peers = append(scfg.Peers, metastore.Peer{ID: p.ID, Address: p.Address})
	}
	swarms := &manager.Swarms{Matrix: swarm.DefaultMatrix(), Dial: func(h string) (swarm.Connector, error) { return swarm.Dial(h) }}
	scfg.Reconcilers = []manager.Reconciler{manager.SyncReconciler{Swarms: swarms}}
	var probe manager.AgentHealthProbe
	if mc.LabAgents || mc.AgentTransport != nil {
		// Live preconditions and periodic health go through the agents.
		scfg.Checker = &galeraprobe.Checker{Prober: &probe, Topology: &probe}
		scfg.Reconcilers = append(scfg.Reconcilers, manager.HealthCollector{})
	}
	var serverIPs []net.IP
	if at := mc.AgentTransport; at != nil {
		if serverIPs, err = at.validate(); err != nil {
			return nil, err
		}
	}
	rt := &managedRuntime{proto: api.NewHandler()}
	rt.setHandler(rt.notLeader())
	svc, err := manager.New(scfg)
	if err != nil {
		return nil, err
	}
	probe.Svc = svc
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
			MonitorUser: at.MonitorUser, MonitorSecret: at.MonitorSecret}
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
			rt.setHandler(rt.notLeader())
			return
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
	inv := manager.Inventory{Store: rt.svc.Store()}
	h, err := api.New(api.Config{
		Passkeys: st.Passkeys, Accounts: st.Accounts, Sessions: st.Sessions, RecoverySessions: st.RecoverySessions,
		Codes: st.Codes, URLTokens: st.URLTokens, Limiter: st.Limiter, Authorizer: st.Authorizer, AuditSink: st.Audit,
		SetupToken: token, AdministratorExists: st.AdministratorExists, Inventory: inv, Activity: inv, Logger: log,
	})
	if err != nil {
		return nil, nil, err
	}
	return h, st, nil
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
