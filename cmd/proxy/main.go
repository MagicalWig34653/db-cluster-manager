// SPDX-License-Identifier: AGPL-3.0-only

// Command proxy is the layer-4 MariaDB/Galera database proxy of
// internal/proxy. It routes the write endpoint (and an optional read
// endpoint) according to internal/routing, probes the configured nodes and
// serves an unauthenticated GET /status on a loopback-only control address.
//
// Usage:
//
//	proxy -config /etc/dbcm/proxy.json
//	proxy -version
//
// SIGHUP reloads the configuration file (nodes, roles, policy, probe
// settings, limits) and re-reads TLS certificate files; listen addresses and
// enabling/disabling TLS need a restart. An invalid file is rejected and the
// running configuration is kept. SIGINT/SIGTERM shut down gracefully within
// limits.shutdown_grace.
//
// Example configuration:
//
//	{
//	  "write_listen": "0.0.0.0:3306",
//	  "read_listen": "0.0.0.0:3307",
//	  "control_listen": "127.0.0.1:9306",
//	  "nodes": [
//	    {"id": "db1", "address": "10.0.0.11:3306", "role": "writer-eligible"},
//	    {"id": "db2", "address": "10.0.0.12:3306", "role": "writer-eligible"},
//	    {"id": "db3", "address": "10.0.0.13:3306", "role": "reader"}
//	  ],
//	  "policy": {"mode": "prefer-node", "preferred_id": "db1", "read": "exclude-writer",
//	             "stale_after": "15s", "drain_deadline": "30s"},
//	  "probe": {"liveness": "mysql-greeting", "sync": "assume-synced", "interval": "1s", "timeout": "1s"},
//	  "limits": {"max_connections": 1000, "idle_timeout": "8h", "dial_timeout": "5s"},
//	  "tls": {"cert_file": "proxy-chain.pem", "key_file": "proxy-key.pem"}
//	}
//
// "sync": "galera" is accepted but the Galera status probe is not
// implemented, so no node becomes usable (fail closed).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/MagicalWig34653/db-cluster-manager/internal/proxy"
	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

type options struct {
	showVersion bool
	config      string
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(2)
	}
	if o.showVersion {
		_ = printVersion(os.Stdout)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	if err := run(ctx, o, os.Stderr, hup, nil); err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(1)
	}
}

func printVersion(w io.Writer) error { return json.NewEncoder(w).Encode(version.Get("proxy")) }

func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.BoolVar(&o.showVersion, "version", false, "print version information and exit")
	fs.StringVar(&o.config, "config", "", "path of the JSON configuration file (required)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if !o.showVersion && o.config == "" {
		return o, errors.New("-config is required")
	}
	return o, nil
}

// run starts the proxy and blocks until ctx is done. Each value on hup
// reloads the configuration. ready, if not nil, receives the running proxy.
func run(ctx context.Context, o options, logw io.Writer, hup <-chan os.Signal, ready func(*proxy.Proxy)) error {
	log := slog.New(slog.NewTextHandler(logw, nil))
	cfg, err := proxy.LoadConfig(o.config)
	if err != nil {
		return err
	}
	p, err := proxy.New(cfg, proxy.Options{Logger: log})
	if err != nil {
		return err
	}
	if err := p.Start(); err != nil {
		return err
	}
	log.Info("proxy running", "version", version.Version, "config", o.config)
	if ready != nil {
		ready(p)
	}
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), cfg.Limits.ShutdownGrace.D())
			err := p.Shutdown(sctx)
			cancel()
			if err != nil {
				log.Warn("shutdown grace expired", "err", err)
			}
			return nil
		case <-hup:
			next, err := proxy.LoadConfig(o.config)
			if err == nil {
				err = p.Reload(next)
			}
			if err != nil {
				log.Error("reload rejected; keeping the running configuration", "err", err)
				continue
			}
			cfg = next
		}
	}
}
