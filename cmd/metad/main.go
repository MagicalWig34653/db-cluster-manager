// SPDX-License-Identifier: AGPL-3.0-only

// Command metad runs one metastore node (package internal/metastore) as a
// standalone process. It is a TEST HARNESS for loopback and lab VM
// experiments, not product API and not part of the manager distribution.
//
// It exposes a minimal HTTP endpoint without authentication, authorization
// or TLS:
//
//	GET  /state[?barrier=1]  JSON: id, leader, leader_id, leader_address, term,
//	                         barrier, unfinished operations; barrier=1 runs a
//	                         Raft barrier first (leader only, else 503).
//	POST /apply              JSON metastore.Command; 200 {"ok":true} or an
//	                         error object with a code.
//
// The HTTP listener must be a loopback address unless -unsafe-http-any is
// given. The Raft transport is raft's plain TCP transport: no TLS, no peer
// authentication. Use it only on loopback or an isolated lab network.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

type options struct {
	id, bind, advertise, data, http string
	peers                           []metastore.Peer
	bootstrap, anyHTTP              bool
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "metad:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "metad:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (options, error) {
	var o options
	var peers string
	fs := flag.NewFlagSet("metad", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.id, "id", "", "node ID (required)")
	fs.StringVar(&o.bind, "bind", "", "Raft TCP bind address host:port (required)")
	fs.StringVar(&o.advertise, "advertise", "", "Raft address peers dial; defaults to -bind")
	fs.StringVar(&o.data, "data", "", "data directory (required)")
	fs.StringVar(&peers, "peers", "", "initial members id=host:port,... including this node")
	fs.BoolVar(&o.bootstrap, "bootstrap", false, "bootstrap the cluster from -peers if no state exists")
	fs.StringVar(&o.http, "http", "", "harness HTTP listen address, loopback only (required)")
	fs.BoolVar(&o.anyHTTP, "unsafe-http-any", false, "allow a non-loopback -http address (unauthenticated; lab only)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	for name, v := range map[string]string{"-id": o.id, "-bind": o.bind, "-data": o.data, "-http": o.http} {
		if v == "" {
			return o, fmt.Errorf("%s is required", name)
		}
	}
	if peers != "" {
		p, err := parsePeers(peers)
		if err != nil {
			return o, err
		}
		o.peers = p
	}
	if o.bootstrap {
		found := false
		for _, p := range o.peers {
			found = found || p.ID == o.id
		}
		if !found {
			return o, fmt.Errorf("-bootstrap needs -peers including -id %q", o.id)
		}
	}
	if !o.anyHTTP {
		host, _, err := net.SplitHostPort(o.http)
		if err != nil {
			return o, fmt.Errorf("-http: %w", err)
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return o, fmt.Errorf("-http %q is not a loopback address (see -unsafe-http-any)", o.http)
		}
	}
	return o, nil
}

// parsePeers parses "id=host:port,..." and rejects empty, duplicate or
// malformed entries.
func parsePeers(s string) ([]metastore.Peer, error) {
	var out []metastore.Peer
	ids, addrs := map[string]bool{}, map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		id, addr, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || id == "" || addr == "" {
			return nil, fmt.Errorf("peer %q: want id=host:port", item)
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("peer %q: %w", item, err)
		}
		if ids[id] || addrs[addr] {
			return nil, fmt.Errorf("peer %q: duplicate ID or address", item)
		}
		ids[id], addrs[addr] = true, true
		out = append(out, metastore.Peer{ID: id, Address: addr})
	}
	return out, nil
}

func run(ctx context.Context, o options, logw io.Writer) error {
	log := slog.New(slog.NewTextHandler(logw, nil))
	n, err := metastore.NewNode(metastore.Config{
		ID:            o.id,
		BindAddr:      o.bind,
		AdvertiseAddr: o.advertise,
		DataDir:       o.data,
		Peers:         o.peers,
		Bootstrap:     o.bootstrap,
		LogOutput:     logw,
	})
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", o.http)
	if err != nil {
		_ = n.Close()
		return fmt.Errorf("http listen: %w", err)
	}
	srv := &http.Server{Handler: newHandler(n), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	log.Info("metad started (test harness, no authentication)", "id", o.id, "raft", o.bind, "http", l.Addr().String(), "data", o.data)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	closeErr := n.Close()
	log.Info("metad stopped", "id", o.id)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return closeErr
}
