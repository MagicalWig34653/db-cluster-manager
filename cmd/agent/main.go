// SPDX-License-Identifier: AGPL-3.0-only

// Command agent is the per-host agent. It runs the fenced command executor
// of internal/agent against the Docker Engine API (internal/dockerclient)
// and serves it over the UNAUTHENTICATED LAB TRANSPORT of internal/agent
// (HTTP/JSON on a loopback IP literal only). It is for lab use; the
// authenticated manager-agent transport is not implemented.
//
// Usage:
//
//	agent -listen 127.0.0.1:7070 -docker unix:///var/run/docker.sock \
//	      -state /var/lib/dbcm-agent -installation INSTALLATION-ID \
//	      -volume-base /srv/dbcm
//	agent -version
//
// The state directory holds the executor state file (agent-state.json) and
// the fencing term file (fencing-term). It is created with mode 0700 if it
// does not exist. -volume-base must be an existing clean absolute directory.
package main

import (
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dockerclient"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

// File names inside the -state directory.
const (
	stateFileName   = "agent-state.json"
	fencingFileName = "fencing-term"
)

type options struct {
	showVersion  bool
	listen       string
	docker       string
	stateDir     string
	installation string
	volumeBase   string
}

func main() {
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
	fs.StringVar(&o.listen, "listen", "", "lab transport address, a loopback IP literal with port, for example 127.0.0.1:7070 (required; unauthenticated)")
	fs.StringVar(&o.docker, "docker", dockerclient.DefaultHost, "Docker Engine API endpoint: unix:///path or tcp://host:port (plain, no TLS)")
	fs.StringVar(&o.stateDir, "state", "", "state directory for the step record and fencing term (required)")
	fs.StringVar(&o.installation, "installation", "", "installation ID; objects must carry it in "+agent.LabelInstallation+" (required)")
	fs.StringVar(&o.volumeBase, "volume-base", "", "existing absolute directory below which all database volume roots lie (required)")
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
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return o, fmt.Errorf("-listen %q must be a loopback IP literal (lab transport is unauthenticated)", o.listen)
	}
	if !filepath.IsAbs(o.stateDir) {
		return o, fmt.Errorf("-state %q must be an absolute path", o.stateDir)
	}
	if !filepath.IsAbs(o.volumeBase) || filepath.Clean(o.volumeBase) != o.volumeBase {
		return o, fmt.Errorf("-volume-base %q must be a clean absolute path", o.volumeBase)
	}
	return o, nil
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
	ex, err := agent.NewExecutor(agent.Config{
		InstallationID: o.installation,
		StatePath:      filepath.Join(o.stateDir, stateFileName),
		VolumeBase:     o.volumeBase,
		Guard:          guard,
		Docker:         dc,
	})
	if err != nil {
		return err
	}

	l, err := agent.ListenLoopback(o.listen)
	if err != nil {
		return err
	}
	srv := agent.NewLabServer(ex)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	log.Warn("agent started with the UNAUTHENTICATED lab transport (loopback only, not for production)",
		"listen", l.Addr().String(), "docker", o.docker, "api_version", apiVersion,
		"installation", o.installation, "state", o.stateDir, "volume_base", o.volumeBase,
		"fencing_term", guard.Highest())
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
