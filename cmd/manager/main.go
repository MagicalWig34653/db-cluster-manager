// SPDX-License-Identifier: AGPL-3.0-only

// Command manager runs the manager API and serves the embedded web UI.
//
// Usage:
//
//	manager [-listen addr] [-config file] [-version] [-healthcheck]
//
// Flags:
//
//	-listen addr   address to listen on (default 127.0.0.1:8080)
//	-config file   JSON configuration file; defaults to $DCM_CONFIG
//	-version       print the version and exit
//	-healthcheck   probe the local manager at -listen and exit 0 if healthy
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
//	  "bind": "127.0.0.1:7946",
//	  "advertise": "",
//	  "peers": [],          // empty: single-instance mode
//	  "bootstrap": true,
//	  "keyFile": "",        // default <dataDir>/data.key, created if missing
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
//	  }
//	}
//
// With auth enabled, the leader also serves the command endpoints (hosts,
// Swarms, clusters, operations, backups, notification settings; ADR 0011),
// runs the monitoring service and the backup scheduler, and binds the
// manager's system subject to the manager_system role.
//
// Accounts, passkeys, sessions, recovery codes, roles and audit records then
// survive a restart; the setup token is printed only while no administrator
// exists. Only the metastore leader serves the API; other replicas answer
// 503 with the X-DCM-Leader header (no forwarding). The metastore transport
// has no TLS, and the data key must be copied to every replica out of band
// (key provisioning is not solved). A command-line hook to issue a recovery
// URL (-print-recovery-url) does not exist: URL tokens live in the leader's
// memory and no local admin channel to the running process exists yet.
package main

import (
	"bytes"
	"context"
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
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("listen", "127.0.0.1:8080", "address to listen on")
	configPath := fs.String("config", getenv(configEnv), "JSON configuration file (default $"+configEnv+")")
	showVersion := fs.Bool("version", false, "print version and exit")
	health := fs.Bool("healthcheck", false, "probe the local manager at -listen and exit 0 if healthy (for container HEALTHCHECK)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *health {
		if err := probeHealth(context.Background(), *addr); err != nil {
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
	log := slog.Default()
	var (
		apiHandler http.Handler
		pruner     interface{ prune() }
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

// probeHealth requests the health endpoint on the loopback interface at the
// port of the given listen address.
func probeHealth(ctx context.Context, listen string) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/api/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
