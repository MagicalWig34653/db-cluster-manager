// SPDX-License-Identifier: AGPL-3.0-only

// Command manager runs the manager API and serves the embedded web UI.
//
// Usage:
//
//	manager [-listen addr] [-config file] [-version] [-healthcheck]
//	manager init -rp-id host -manager-addr addr [flags]   (config skeleton, see init.go)
//
// Flags:
//
//	-listen addr   address to listen on (default 127.0.0.1:8080)
//	-config file   JSON configuration file; defaults to $DCM_CONFIG
//	-version       print the version and exit
//	-healthcheck   probe the local manager at -listen using the TLS config, exit 0 if healthy
//
// Without a configuration, or with a configuration that does not enable
// auth, the manager runs in prototype mode: only GET /api/v1/health and
// GET /api/v1/version plus the UI are served.
//
// A configuration enabling auth looks like this:
//
//	{
//	  "auth": {
//	    "enabled": true,
//	    "rpId": "db.example.com",
//	    "rpDisplayName": "DB Cluster Manager",
//	    "origins": ["https://db.example.com"]
//	  }
//	}
//
// rpId and origins follow internal/passkey.Config (https origins whose host
// is rpId or a subdomain; http://localhost[:port] only for development). The
// full API (setup, login, recovery, resources) is then mounted. On start the
// manager prints a one-time setup token to stderr; it is needed to create the
// first administrator through POST /api/v1/setup/begin and /finish.
//
// Without a "manager" section (prototype mode): all accounts, sessions,
// roles and audit records are held in memory and lost on restart (a new
// setup token is printed each start); audit records are written to the log;
// resource endpoints answer 503 because no inventory store is wired.
//
// With a "manager" section the state is kept in the replicated store of
// internal/manager (see managed.go):
//
//	"manager": {
//	  "nodeId": "manager-1",
//	  "dataDir": "/var/lib/dbcm-manager",
//	  "bind": "127.0.0.1:7445",  // not 7946 (Swarm gossip) with host networking
//	  "advertise": "",
//	  "peers": [],          // empty: single-instance mode
//	  "bootstrap": true,
//	  "keyFile": "",        // default <dataDir>/data.key, created if missing (without keyring)
//	  "keyring": false,     // replicated data keyring; identity in <dataDir>/replica.key
//	  "peerTLS": {          // mTLS between metastore replicas (-issue-peer-certs)
//	    "installation": "inst-1", "caFile": "", "certFile": "", "keyFile": ""
//	  },
//	  "labAgents": false,   // unauthenticated lab agent transport
//	  "autoResume": false,
//	  "agentTransport": {   // mTLS agent transport with enrollment (ADR 0010)
//	    "enrollListen": "0.0.0.0:7444",
//	    "managerUrl": "https://10.0.0.4:7444",
//	    "serverDns": [], "serverIps": [],
//	    "agentPort": 7443,
//	    "allowAnySource": false,
//	    "monitorUser": "", "monitorSecret": ""
//	  },
//	  "monitoring": {       // default: enabled, metric store in <dataDir>/monitoring
//	    "disabled": false, "dataDir": ""
//	  },
//	  "backup": {           // manager side of backups (ADR 0009, ADR 0011)
//	    "backendConfigFile": "/etc/dbcm/backend.json",  // dbbackup.BackendConfig JSON
//	    "scheduleIntervalSeconds": 60
//	  },
//	  "vuln": {             // vulnerability response (X13, ADR 0015); needs auth.enabled
//	    "pollIntervalSeconds": 3600,   // minimum 300
//	    "feeds": [{"kind": "osv", "url": "https://api.osv.dev"}],  // explicit URLs only; no defaults
//	    "defaultMode": "notify_only",  // seeds the policy once; or approval_required
//	    "providers": []                // see vuln.go; stay unusable until consent in the UI
//	  }
//	}
//
// The optional manager.tls section configures native API/UI HTTPS: either
// acme (directoryUrl, email, domains, challenge, httpListen), or certFile and
// keyFile for an imported PEM keypair. ACME defaults to Let's Encrypt staging
// and tls-alpn-01; http-01 binds a separate challenge-only HTTP listener. Only
// the leader issues/renews; every replica reads sealed replicated certificates
// and challenges. See docs/guides/en/17-manager-tls.md. Without TLS, non-loopback
// HTTP binds require the explicit manager.tls.allowPlainHttp opt-in.
//
// With auth enabled, the leader also serves the command endpoints (hosts,
// Swarms, clusters, operations, backups, notification settings; ADR 0011),
// runs the monitoring service and the backup scheduler, and binds the
// manager's system subject to the manager_system role.
//
// Accounts, passkeys, sessions, recovery codes, roles and audit records then
// survive a restart; the setup token is printed only while no administrator
// exists. Only the metastore leader serves the API; other replicas answer
// 503 with the X-DCM-Leader header (no forwarding). Without manager.peerTLS
// the metastore transport has no TLS, and without manager.keyring the data
// key must be copied to every replica out of band (ADR 0013). A command-line hook to issue a recovery
// URL (-print-recovery-url) does not exist: URL tokens live in the leader's
// memory and no local admin channel to the running process exists yet.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
	"github.com/MagicalWig34653/db-cluster-manager/internal/webui"
)

// configEnv names the environment variable used when -config is not given.
const configEnv = "DCM_CONFIG"

type authConfig struct {
	Enabled       bool     `json:"enabled"`
	RPID          string   `json:"rpId"`
	RPDisplayName string   `json:"rpDisplayName"`
	Origins       []string `json:"origins"`
}

type fileConfig struct {
	Auth authConfig `json:"auth"`
	// Manager enables the replicated control-plane state (managed.go).
	// Without it the prototype behaviour with in-memory stores stays.
	Manager *managerConfig `json:"manager,omitempty"`
}

func loadConfig(path string) (fileConfig, error) {
	var cfg fileConfig
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// logAuditSink writes audit records to the log. It is the only sink until a
// persistent, replicated audit store exists.
type logAuditSink struct{ log *slog.Logger }

func (s logAuditSink) Record(r authz.AuditRecord) error {
	s.log.Info("audit",
		"time", r.Time, "actor", r.Actor.ID, "actorKind", r.Actor.Kind, "action", r.Action,
		"resource", r.Resource.String(), "outcome", r.Outcome, "reason", r.Reason,
		"role", r.Role, "change", r.Change, "error", r.Error)
	return nil
}

// stores groups the in-memory state that needs periodic pruning.
type stores struct {
	passkeys  *passkey.Manager
	sessions  *recovery.Sessions
	recSess   *recovery.Sessions
	urlTokens *recovery.URLTokens
	limiter   *recovery.Limiter
}

func (s *stores) prune() {
	s.passkeys.PruneCeremonies()
	s.sessions.Prune()
	s.recSess.Prune()
	s.urlTokens.Prune()
	s.limiter.Prune()
}

// newAPIHandler returns the API handler for cfg. With auth enabled it also
// returns the stores to prune and writes the setup token to tokenOut.
func newAPIHandler(cfg fileConfig, log *slog.Logger, tokenOut io.Writer) (http.Handler, *stores, error) {
	if !cfg.Auth.Enabled {
		return api.NewHandler(), nil, nil
	}
	accounts := recovery.NewAccounts()
	pk, err := passkey.New(passkey.Config{
		RPID: cfg.Auth.RPID, RPDisplayName: cfg.Auth.RPDisplayName, Origins: cfg.Auth.Origins,
	}, passkey.RecoveryAccounts{Accounts: accounts}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("auth config: %w", err)
	}
	st := &stores{
		passkeys:  pk,
		sessions:  recovery.NewSessions(recovery.DefaultSessionPolicy, accounts, nil),
		recSess:   recovery.NewSessions(recovery.SessionPolicy{IdleTimeout: 10 * time.Minute, AbsoluteTimeout: 15 * time.Minute}, accounts, nil),
		urlTokens: recovery.NewURLTokens(nil),
		limiter:   recovery.NewLimiter(recovery.DefaultLockoutPolicy, nil),
	}
	sink := logAuditSink{log: log}
	token, plain, err := api.NewSetupToken()
	if err != nil {
		return nil, nil, err
	}
	h, err := api.New(api.Config{
		Passkeys:         pk,
		Accounts:         accounts,
		Sessions:         st.sessions,
		RecoverySessions: st.recSess,
		Codes:            recovery.NewCodeVerifier(recovery.NewLimiter(recovery.DefaultLockoutPolicy, nil), nil),
		URLTokens:        st.urlTokens,
		Limiter:          st.limiter,
		Authorizer:       authz.NewAuthorizer(authz.NewPolicy(), sink, nil),
		AuditSink:        sink,
		SetupToken:       token,
		Inventory:        api.Unavailable{},
		Activity:         api.Unavailable{},
		Logger:           log,
	})
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(tokenOut, "First-run setup token (valid until the first administrator is created): %s\n", plain)
	return h, st, nil
}

func newMux(apiHandler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/", webui.Handler())
	return mux
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stderr); err != nil {
		slog.Error("manager failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "init" {
		return runInit(args[1:], os.Stdout, stderr)
	}
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("listen", "127.0.0.1:8080", "address to listen on")
	configPath := fs.String("config", getenv(configEnv), "JSON configuration file (default $"+configEnv+")")
	showVersion := fs.Bool("version", false, "print version and exit")
	health := fs.Bool("healthcheck", false, "probe the local manager at -listen and exit 0 if healthy (for container HEALTHCHECK)")
	printKey := fs.Bool("print-replica-key", false, "print this replica's public data-key identity (manager.dataDir of -config) and exit")
	peerCertsDir := fs.String("issue-peer-certs", "", "create a metastore peer CA and replica certificates in this directory and exit (CA passphrase from $DBCM_PEER_CA_PASSPHRASE)")
	installation := fs.String("installation", "", "installation ID for -issue-peer-certs")
	peerIDs := fs.String("peer-ids", "", "comma-separated replica IDs for -issue-peer-certs")
	peerValidity := fs.Duration("peer-validity", 365*24*time.Hour, "certificate validity for -issue-peer-certs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *peerCertsDir != "" {
		return issuePeerCerts(*peerCertsDir, *installation, *peerIDs, getenv("DBCM_PEER_CA_PASSPHRASE"), *peerValidity, time.Now)
	}
	if *health {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			return err
		}
		if err := probeConfiguredHealth(context.Background(), *addr, cfg); err != nil {
			return fmt.Errorf("unhealthy: %w", err)
		}
		return nil
	}
	if *showVersion {
		fmt.Println(version.Get("manager").Version)
		return nil
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *printKey {
		return printReplicaKey(cfg, os.Stdout)
	}
	if err := validateListener(*addr, listenerTLS(cfg)); err != nil {
		return err
	}
	log := slog.Default()
	var (
		apiHandler http.Handler
		pruner     interface{ prune() }
		managed    *managedRuntime
	)
	if cfg.Manager != nil {
		rt, err := newManagedRuntime(cfg, log, stderr)
		if err != nil {
			return err
		}
		defer func() {
			if err := rt.Close(); err != nil {
				log.Error("manager: close", "err", err)
			}
		}()
		apiHandler, pruner = rt, rt
		managed = rt
	} else {
		h, st, err := newAPIHandler(cfg, log, stderr)
		if err != nil {
			return err
		}
		apiHandler = h
		if st != nil {
			pruner = st
		}
	}

	srv := &http.Server{Addr: *addr, Handler: newMux(apiHandler), ReadHeaderTimeout: 10 * time.Second}
	if tc := listenerTLS(cfg); tc != nil {
		if managed != nil && managed.acme != nil {
			srv.TLSConfig = managed.acme.TLSConfig()
		} else if tc.CertFile != "" {
			srv.TLSConfig, err = staticTLS(tc)
			if err != nil {
				return fmt.Errorf("manager TLS: %w", err)
			}
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var challengeServer *http.Server
	if tc := listenerTLS(cfg); tc != nil && tc.ACME != nil && tc.ACME.Defaults().Challenge == "http-01" {
		challengeServer = &http.Server{Addr: tc.ACME.Defaults().HTTPListen, Handler: managed.acme.HTTPHandler(), ReadHeaderTimeout: 10 * time.Second}
		listener, err := net.Listen("tcp", challengeServer.Addr)
		if err != nil {
			return fmt.Errorf("ACME HTTP-01 listener: %w", err)
		}
		defer challengeServer.Close()
		go func() {
			if err := challengeServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("ACME HTTP-01 server failed", "err", err)
				stop()
			}
		}()
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if pruner != nil {
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					pruner.prune()
				}
			}
		}()
	}

	slog.Info("manager listening", "addr", *addr, "version", version.Version, "auth", cfg.Auth.Enabled)
	serve := srv.ListenAndServe
	if srv.TLSConfig != nil {
		serve = func() error { return srv.ListenAndServeTLS("", "") }
	}
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

// probeHealth requests the health endpoint on the loopback interface at the
// port of the given listen address.
func probeHealth(ctx context.Context, listen string) error {
	return probeConfiguredHealth(ctx, listen, fileConfig{})
}

func probeConfiguredHealth(ctx context.Context, listen string, cfg fileConfig) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	scheme := "http"
	client := http.DefaultClient
	if tc := listenerTLS(cfg); tc != nil && (tc.ACME != nil || tc.CertFile != "") {
		scheme = "https"
		name := cfg.Auth.RPID
		if tc.ACME != nil {
			if err := tc.ACME.Validate(); err != nil {
				return err
			}
			name = tc.ACME.Domains[0]
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name}
		defer transport.CloseIdleConnections()
		client = &http.Client{Transport: transport}
	}
	loopback := "127.0.0.1"
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		loopback = "::1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+net.JoinHostPort(loopback, port)+"/api/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
