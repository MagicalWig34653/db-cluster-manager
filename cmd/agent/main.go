// SPDX-License-Identifier: AGPL-3.0-only

// Command agent is the per-host agent. It runs the fenced command executor
// of internal/agent against the Docker Engine API (internal/dockerclient).
//
// Production transport (default, ADR 0010): HTTP/JSON over mutually
// authenticated TLS 1.3 on a host-reachable address. On first start the
// agent enrolls: it generates its key locally, sends a CSR with the
// one-time enrollment token to the manager, and trusts the manager only if
// its CA has the pinned fingerprint. Credentials are kept in <state>/tls
// and renewed before they expire.
//
//	agent -listen 0.0.0.0:7443 -manager-url https://10.0.0.4:7444 \
//	      -ca-fingerprint SHA256HEX -enroll-token-file /run/secrets/TOKEN \
//	      -host-id NODE_ID (default $DBCM_HOST_ID) \
//	      -docker unix:///var/run/docker.sock -state /var/lib/dbcm-agent \
//	      -installation INSTALLATION-ID -volume-base /srv/dbcm \
//	      [-monitor-user dbcm_monitor -monitor-password-file /run/secrets/PW] \
//	      [-db-secrets-dir /run/secrets]
//
// Lab transport, only with -insecure-lab: UNAUTHENTICATED HTTP/JSON on a
// loopback IP literal.
//
//	agent -insecure-lab -listen 127.0.0.1:7070 -docker ... -state ... \
//	      -installation ... -volume-base ...
//	agent -version
//
// The state directory holds the executor state file (agent-state.json), the
// fencing term file (fencing-term) and the credentials (tls/). It is
// created with mode 0700 if it does not exist. -volume-base must be an
// existing clean absolute directory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dockerclient"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe"
	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

// File names inside the -state directory.
const (
	stateFileName   = "agent-state.json"
	fencingFileName = "fencing-term"
	tlsDirName      = "tls"
	hostIDEnv       = "DBCM_HOST_ID"
)

type options struct {
	showVersion  bool
	insecureLab  bool
	listen       string
	docker       string
	stateDir     string
	installation string
	volumeBase   string

	managerURL    string
	caFingerprint string
	tokenFile     string
	hostID        string

	monitorUser      string
	monitorPWFile    string
	secretsDir       string
	resticAllowPaths []string

	// info is the runtime report sent at enrollment and renewal (ADR 0024).
	info agentrt.Info
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "patch" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err := runPatch(ctx, os.Args[2:], os.Stdout, os.Stderr, nil)
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent patch:", err)
			os.Exit(1)
		}
		return
	}
	o, err := parseFlags(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(2)
	}
	if o.showVersion {
		_ = json.NewEncoder(os.Stdout).Encode(version.Get("agent"))
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.BoolVar(&o.showVersion, "version", false, "print version information and exit")
	fs.BoolVar(&o.insecureLab, "insecure-lab", false, "serve the UNAUTHENTICATED lab transport on a loopback address instead of mTLS (labs only)")
	fs.StringVar(&o.listen, "listen", "", "listen address: host:port for mTLS, a loopback IP literal with port for -insecure-lab (required)")
	fs.StringVar(&o.docker, "docker", dockerclient.DefaultHost, "Docker Engine API endpoint: unix:///path or tcp://host:port (plain, no TLS)")
	fs.StringVar(&o.stateDir, "state", "", "state directory for the step record, fencing term and credentials (required)")
	fs.StringVar(&o.installation, "installation", "", "installation ID; objects must carry it in "+agent.LabelInstallation+" (required)")
	fs.StringVar(&o.volumeBase, "volume-base", "", "existing absolute directory below which all database volume roots lie (required)")
	fs.StringVar(&o.managerURL, "manager-url", "", "manager enrollment endpoint, https://host:port (mTLS mode)")
	fs.StringVar(&o.caFingerprint, "ca-fingerprint", "", "SHA-256 fingerprint of the manager's internal CA (mTLS mode)")
	fs.StringVar(&o.tokenFile, "enroll-token-file", "", "file holding the one-time enrollment token, a Swarm secret (mTLS mode, first start)")
	fs.StringVar(&o.hostID, "host-id", os.Getenv(hostIDEnv), "Swarm node ID of this host (default $"+hostIDEnv+")")
	fs.StringVar(&o.monitorUser, "monitor-user", "", "database monitoring account for collect-health")
	fs.StringVar(&o.monitorPWFile, "monitor-password-file", "", "file holding the monitoring password (a Swarm secret)")
	fs.StringVar(&o.secretsDir, "db-secrets-dir", agent.SecretsDir, "directory of the agent's mounted Swarm secrets; exec-sql reads passwords from it by secret name")
	fs.Func("restic-allow-path", "absolute host filesystem root allowed for restic backups and new-path restores; repeatable", func(p string) error {
		o.resticAllowPaths = append(o.resticAllowPaths, p)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if o.showVersion {
		return o, nil
	}
	for _, f := range []struct{ name, v string }{
		{"-listen", o.listen}, {"-state", o.stateDir}, {"-installation", o.installation}, {"-volume-base", o.volumeBase},
	} {
		if f.v == "" {
			return o, fmt.Errorf("%s is required", f.name)
		}
	}
	host, _, err := net.SplitHostPort(o.listen)
	if err != nil {
		return o, fmt.Errorf("-listen: %w", err)
	}
	if o.insecureLab {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return o, fmt.Errorf("-listen %q must be a loopback IP literal with -insecure-lab (the lab transport is unauthenticated)", o.listen)
		}
	} else {
		for _, f := range []struct{ name, v string }{
			{"-manager-url", o.managerURL}, {"-ca-fingerprint", o.caFingerprint}, {"-host-id", o.hostID},
		} {
			if f.v == "" {
				return o, fmt.Errorf("%s is required for the mTLS transport (or use -insecure-lab in a lab)", f.name)
			}
		}
		if u, err := url.Parse(o.managerURL); err != nil || u.Scheme != "https" || u.Host == "" {
			return o, fmt.Errorf("-manager-url %q must be https://host:port", o.managerURL)
		}
		if _, err := certs.NormalizeFingerprint(o.caFingerprint); err != nil {
			return o, fmt.Errorf("-ca-fingerprint: %w", err)
		}
	}
	// The host ID also pins backup/restore jobs (node.id constraint), so
	// it is checked in both transports.
	if strings.ContainsAny(o.hostID, "/ \t\r\n") {
		return o, fmt.Errorf("-host-id %q is not a node ID", o.hostID)
	}
	if (o.monitorUser == "") != (o.monitorPWFile == "") {
		return o, errors.New("-monitor-user and -monitor-password-file go together")
	}
	if !filepath.IsAbs(o.stateDir) {
		return o, fmt.Errorf("-state %q must be an absolute path", o.stateDir)
	}
	if !filepath.IsAbs(o.secretsDir) || filepath.Clean(o.secretsDir) != o.secretsDir {
		return o, fmt.Errorf("-db-secrets-dir %q must be a clean absolute path", o.secretsDir)
	}
	if !filepath.IsAbs(o.volumeBase) || filepath.Clean(o.volumeBase) != o.volumeBase {
		return o, fmt.Errorf("-volume-base %q must be a clean absolute path", o.volumeBase)
	}
	return o, nil
}

// statusAdapter lets the executor query the local database through
// galeraprobe.
type statusAdapter struct{ p *galeraprobe.Prober }

func (s statusAdapter) Status(ctx context.Context, network, addr string) (map[string]string, error) {
	return s.p.StatusNet(ctx, network, addr)
}

// run starts the agent and blocks until ctx is done or the server fails.
// ready, if not nil, receives the bound listen address once serving.
func run(ctx context.Context, o options, logw io.Writer, ready func(addr string)) error {
	log := slog.New(slog.NewTextHandler(logw, nil))
	if err := os.MkdirAll(o.stateDir, 0o700); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	if fi, err := os.Stat(o.volumeBase); err != nil {
		return fmt.Errorf("volume base: %w", err)
	} else if !fi.IsDir() {
		return fmt.Errorf("volume base %s is not a directory", o.volumeBase)
	}

	dc, err := dockerclient.New(dockerclient.Options{Host: o.docker})
	if err != nil {
		return err
	}
	defer dc.Close()
	nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	apiVersion, err := dc.NegotiateAPIVersion(nctx)
	cancel()
	if err != nil {
		return fmt.Errorf("docker %s: %w", o.docker, err)
	}

	guard, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(o.stateDir, fencingFileName)))
	if err != nil {
		return err
	}
	cfg := agent.Config{
		InstallationID: o.installation,
		StatePath:      filepath.Join(o.stateDir, stateFileName),
		VolumeBase:     o.volumeBase,
		Guard:          guard,
		Docker:         dc,
		// Backup and restore jobs are pinned to this host's Swarm node;
		// without a host ID run-backup and run-restore are refused.
		NodeID: o.hostID,
		// exec-sql (bootstrap account templates, ADR 0011) connects through
		// the node's socket with passwords from the agent's own secrets.
		SQL:                galeraprobe.Admin{},
		SecretsDir:         o.secretsDir,
		ResticAllowedPaths: o.resticAllowPaths,
	}
	if o.monitorUser != "" {
		p, err := galeraprobe.New(galeraprobe.Config{Credentials: galeraprobe.FileCredentials{User: o.monitorUser, Path: o.monitorPWFile}})
		if err != nil {
			return err
		}
		cfg.Status = statusAdapter{p}
	}
	ex, err := agent.NewExecutor(cfg)
	if err != nil {
		return err
	}
	o.info = ex.RuntimeInfo()

	var (
		srv   *http.Server
		l     net.Listener
		serve func() error
	)
	if o.insecureLab {
		if l, err = agent.ListenLoopback(o.listen); err != nil {
			return err
		}
		srv = agent.NewLabServer(ex)
		serve = func() error { return srv.Serve(l) }
		log.Warn("agent started with the UNAUTHENTICATED lab transport (loopback only, not for production)",
			"listen", l.Addr().String())
	} else {
		holder, err := credentials(ctx, o, log)
		if err != nil {
			return err
		}
		pool, err := holder.Credentials().CAPool()
		if err != nil {
			return err
		}
		srv, err = agent.NewMTLSServer(ex, agent.MTLSServerConfig{Certificate: holder.Certificate, ClientCAs: pool,
			ManagerIdentity: certs.ManagerURI(o.installation), Revoked: ex.Revoked})
		if err != nil {
			return err
		}
		if l, err = net.Listen("tcp", o.listen); err != nil {
			return err
		}
		serve = func() error { return srv.ServeTLS(l, "", "") }
		renewer := &enroll.Renewer{
			Config: enroll.RenewConfig{ManagerURL: o.managerURL, Installation: o.installation, HostID: o.hostID, Info: o.info},
			Holder: holder, Store: enroll.FileStore{Dir: filepath.Join(o.stateDir, tlsDirName)},
			Policy: certs.Policy{Fraction: 0.3, Floor: 24 * time.Hour}, Log: log,
		}
		rctx, stopRenew := context.WithCancel(ctx)
		renewDone := make(chan struct{})
		go func() { defer close(renewDone); renewer.Run(rctx) }()
		defer func() { stopRenew(); <-renewDone }()
		log.Info("agent started with the mTLS transport", "listen", l.Addr().String(), "host_id", o.hostID)
	}
	errc := make(chan error, 1)
	go func() { errc <- serve() }()
	log.Info("agent ready", "docker", o.docker, "api_version", apiVersion, "installation", o.installation,
		"state", o.stateDir, "volume_base", o.volumeBase, "fencing_term", guard.Highest(), "health", cfg.Status != nil)
	if ready != nil {
		ready(l.Addr().String())
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdownCtx)
	log.Info("agent stopped")
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// credentials loads the stored credentials or enrolls, retrying with
// backoff until ctx ends (the manager may not be reachable yet).
func credentials(ctx context.Context, o options, log *slog.Logger) (*enroll.Holder, error) {
	store := enroll.FileStore{Dir: filepath.Join(o.stateDir, tlsDirName)}
	cr, err := store.Load()
	if err == nil {
		return enroll.NewHolder(cr)
	}
	if !errors.Is(err, enroll.ErrNoCredentials) {
		return nil, err
	}
	if o.tokenFile == "" {
		return nil, errors.New("not enrolled and no -enroll-token-file given")
	}
	tok, err := os.ReadFile(o.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("enrollment token: %w", err)
	}
	cfg := enroll.AgentConfig{ManagerURL: o.managerURL, Installation: o.installation, HostID: o.hostID,
		CAFingerprint: o.caFingerprint, Token: strings.TrimSpace(string(tok)), Info: o.info}
	wait := time.Second
	for {
		cr, err = enroll.Enroll(ctx, cfg)
		if err == nil {
			break
		}
		// A refusal (wrong token, replay, unpinned manager) will not heal by
		// retrying with the same input.
		if errors.Is(err, enroll.ErrRejected) || errors.Is(err, enroll.ErrManagerIdentity) {
			return nil, fmt.Errorf("enrollment: %w", err)
		}
		log.Warn("agent: enrollment failed; retrying", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
	if err := store.Save(cr); err != nil {
		return nil, err
	}
	log.Info("agent: enrolled", "host_id", o.hostID)
	return enroll.NewHolder(cr)
}
