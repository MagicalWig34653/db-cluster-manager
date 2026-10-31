// SPDX-License-Identifier: AGPL-3.0-only

package main

// Native patch mode (X15, ADR 0023):
//
//	agent patch -once                                  # print one report as JSON, change nothing
//	agent patch -listen 0.0.0.0:7445 -state /var/lib/dbcm-patch \
//	      -installation ID -host-id HOST -manager-url https://m:7444 \
//	      -ca-fingerprint SHA256HEX -enroll-token-file /etc/dbcm-patch/enroll-token \
//	      [-allow-apply]
//	agent patch -insecure-lab -listen 127.0.0.1:7445 -state DIR   # unauthenticated, loopback only
//
// The mode serves patch.CommandPath over the same enrollment and mTLS
// transport as the Docker agent. It reports and simulates by default; apply
// and reboot need -allow-apply (the install plan sets it only for the apply
// mode, which runs as root). Mutating commands are fenced with the manager's
// term. It runs no Docker code and needs no Docker socket.

import (
	"cmp"
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
	"path/filepath"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

// Seams for tests: the production defaults execute real commands.
var (
	patchRunner patch.Runner = patch.ExecRunner{}
	patchFS     patch.FS     = patch.OSFS{}
	// resticRunner is nil in production (the real restic binary).
	resticRunner restic.Runner
)

// patchStateFileName holds replay suppression and the deny list of the patch
// mode (distinct from the main agent's stateFileName).
// resticProbe is the repository-open bound; tests with a fake runner set it
// negative to disable the probe.
var resticProbe time.Duration

const patchStateFileName = "patch-state.json"

// resticStateFileName is the step record of the restic capability, and
// defaultNativeSecretsDir the secret files directory of the native agent.
const (
	resticStateFileName     = "restic-state.json"
	defaultNativeSecretsDir = "/etc/dbcm-patch/secrets"
)

type patchOptions struct {
	options
	once       bool
	allowApply bool
}

func parsePatchFlags(args []string) (patchOptions, error) {
	var o patchOptions
	fs := flag.NewFlagSet("agent patch", flag.ContinueOnError)
	fs.BoolVar(&o.once, "once", false, "print one report as JSON and exit; read-only, no network")
	fs.BoolVar(&o.allowApply, "allow-apply", false, "enable apply and reboot commands (default: report and simulate only)")
	fs.BoolVar(&o.insecureLab, "insecure-lab", false, "serve UNAUTHENTICATED HTTP on a loopback address (labs only)")
	fs.StringVar(&o.listen, "listen", "", "listen address host:port (mTLS) or loopback IP literal with -insecure-lab")
	fs.StringVar(&o.stateDir, "state", "", "state directory (fencing term, credentials)")
	fs.StringVar(&o.installation, "installation", "", "installation ID")
	fs.StringVar(&o.managerURL, "manager-url", "", "manager enrollment endpoint, https://host:port")
	fs.StringVar(&o.caFingerprint, "ca-fingerprint", "", "SHA-256 fingerprint of the manager's internal CA")
	fs.StringVar(&o.tokenFile, "enroll-token-file", "", "file with the one-time enrollment token (first start; removed after enrollment)")
	fs.StringVar(&o.hostID, "host-id", os.Getenv(hostIDEnv), "host ID")
	fs.StringVar(&o.secretsDir, "restic-secrets-dir", defaultNativeSecretsDir, "directory of restic password/credential secret files (regular files, mode 0600, owned by the agent user)")
	fs.Func("restic-allow-path", "absolute host filesystem root allowed for restic backups and new-path restores; repeatable; enables the restic capability", func(p string) error {
		o.resticAllowPaths = append(o.resticAllowPaths, p)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if o.once {
		return o, nil
	}
	if o.listen == "" || o.stateDir == "" || !filepath.IsAbs(o.stateDir) {
		return o, errors.New("-listen and an absolute -state are required")
	}
	if strings.ContainsAny(o.hostID, "/ \t\r\n") {
		return o, fmt.Errorf("-host-id %q is not a host ID", o.hostID)
	}
	host, _, err := net.SplitHostPort(o.listen)
	if err != nil {
		return o, fmt.Errorf("-listen: %w", err)
	}
	if o.insecureLab {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return o, fmt.Errorf("-listen %q must be a loopback IP literal with -insecure-lab", o.listen)
		}
		return o, nil
	}
	for _, f := range []struct{ name, v string }{{"-manager-url", o.managerURL}, {"-ca-fingerprint", o.caFingerprint}, {"-host-id", o.hostID}, {"-installation", o.installation}} {
		if f.v == "" {
			return o, fmt.Errorf("%s is required for the mTLS transport", f.name)
		}
	}
	if u, err := url.Parse(o.managerURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return o, fmt.Errorf("-manager-url %q must be https://host:port", o.managerURL)
	}
	if _, err := certs.NormalizeFingerprint(o.caFingerprint); err != nil {
		return o, fmt.Errorf("-ca-fingerprint: %w", err)
	}
	return o, nil
}

// ensureResticCacheDir points restic's cache below the agent state directory
// unless the operator set RESTIC_CACHE_DIR. Without it restic uses
// $HOME/.cache, and the apply-mode unit (root, ProtectHome=true) has no
// writable home: every command failed with "unable to open cache: read-only
// file system" (lab E17).
func ensureResticCacheDir(stateDir string) error {
	if os.Getenv("RESTIC_CACHE_DIR") != "" {
		return nil
	}
	dir := filepath.Join(stateDir, "restic-cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("restic cache directory: %w", err)
	}
	return os.Setenv("RESTIC_CACHE_DIR", dir)
}

func runPatch(ctx context.Context, args []string, stdout, logw io.Writer, ready func(addr string)) error {
	o, err := parsePatchFlags(args)
	if err != nil {
		return err
	}
	col := patch.Collector{HostID: o.hostID, Runner: patchRunner, FS: patchFS}
	if o.once {
		col.Refresh = false
		rep, err := col.Collect(ctx)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	log := slog.New(slog.NewTextHandler(logw, nil))
	if err := os.MkdirAll(o.stateDir, 0o700); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	guard, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(o.stateDir, fencingFileName)))
	if err != nil {
		return err
	}
	// Replay suppression and the certificate deny list live on disk, so a
	// restart (for example after a patch reboot) forgets neither.
	state, err := patch.OpenAgentState(filepath.Join(o.stateDir, patchStateFileName))
	if err != nil {
		return err
	}
	// The native agent reports its runtime and capabilities (ADR 0024). The
	// restic capability is on only with at least one allowed root (ADR 0022
	// rules: allow-list, secret files, no secrets in argv).
	o.info = agentrt.NativeInfo(len(o.resticAllowPaths) > 0)
	svc := &patch.AgentService{Collector: col, Guard: guard, AllowApply: o.allowApply, State: state, Info: o.info}
	var resticHandler agent.Handler
	if o.info.Has(agentrt.Restic) {
		if err := ensureResticCacheDir(o.stateDir); err != nil {
			return err
		}
		ex, err := agent.NewExecutor(agent.Config{
			InstallationID: cmp.Or(o.installation, "native-lab"), StatePath: filepath.Join(o.stateDir, resticStateFileName), VolumeBase: o.stateDir,
			Guard: guard, Info: o.info, ResticRunner: resticRunner, ResticProbeTimeout: resticProbe, SecretsDir: o.secretsDir, ResticAllowedPaths: o.resticAllowPaths, NodeID: o.hostID,
		})
		if err != nil {
			return err
		}
		resticHandler = ex
	}
	withRestic := func(peer func(*http.Request) string) http.Handler {
		h := svc.Handler(peer)
		if resticHandler == nil {
			return h
		}
		mux := http.NewServeMux()
		mux.Handle(patch.CommandPath, h)
		mux.Handle(agent.LabCommandPath, agent.NewCommandHandler(resticHandler, peer))
		return mux
	}

	var (
		srv   *http.Server
		l     net.Listener
		serve func() error
	)
	if o.insecureLab {
		if l, err = agent.ListenLoopback(o.listen); err != nil {
			return err
		}
		srv = &http.Server{Handler: withRestic(func(r *http.Request) string {
			if ip, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || net.ParseIP(ip) == nil || !net.ParseIP(ip).IsLoopback() {
				return "peer is not loopback"
			}
			return ""
		}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 10 * time.Minute}
		serve = func() error { return srv.Serve(l) }
		log.Warn("patch agent started with the UNAUTHENTICATED lab transport (loopback only)", "listen", l.Addr().String(), "allowApply", o.allowApply)
	} else {
		holder, err := credentials(ctx, o.options, log)
		if err != nil {
			return err
		}
		if o.tokenFile != "" {
			_ = os.Remove(o.tokenFile) // the token is single-use; do not keep it
		}
		pool, err := holder.Credentials().CAPool()
		if err != nil {
			return err
		}
		tc, err := agent.ServerTLSConfig(agent.MTLSServerConfig{Certificate: holder.Certificate, ClientCAs: pool, ManagerIdentity: certs.ManagerURI(o.installation), Revoked: svc.Revoked})
		if err != nil {
			return err
		}
		srv = &http.Server{Handler: withRestic(func(r *http.Request) string {
			if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
				return "no verified client certificate"
			}
			return ""
		}), TLSConfig: tc, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 10 * time.Minute, IdleTimeout: time.Minute}
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
		done := make(chan struct{})
		go func() { defer close(done); renewer.Run(rctx) }()
		defer func() { stopRenew(); <-done }()
		log.Info("patch agent started with the mTLS transport", "listen", l.Addr().String(), "host_id", o.hostID, "allowApply", o.allowApply)
	}
	errc := make(chan error, 1)
	go func() { errc <- serve() }()
	if ready != nil {
		ready(l.Addr().String())
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
