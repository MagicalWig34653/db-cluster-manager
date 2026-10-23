// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

// ProbeResult is one health report for a node.
type ProbeResult struct {
	// Healthy reports liveness. A failed or timed-out probe is a report of
	// "unhealthy"; a probe that never returns produces no report, so the
	// node goes stale (unknown, therefore unusable).
	Healthy bool
	// Sync is the replication state. SyncUnknown makes the node unusable.
	Sync routing.SyncState
	// Latency is the measured TCP connect latency; 0 means not measured.
	Latency time.Duration
	// Detail is a human-readable note (server version, sync source).
	Detail string
	// Err is the failure, if any (also set when only the sync state failed).
	Err error
}

// Prober produces a health report for the node at addr. It must honour ctx
// (the proxy cancels it at the probe timeout) and be safe for concurrent use.
type Prober interface {
	Probe(ctx context.Context, addr string) ProbeResult
}

// ProberFunc adapts a function to Prober.
type ProberFunc func(ctx context.Context, addr string) ProbeResult

// Probe calls f.
func (f ProberFunc) Probe(ctx context.Context, addr string) ProbeResult { return f(ctx, addr) }

// TCPProber reports a node live if a TCP connection can be opened. It knows
// nothing about the sync state (SyncUnknown) and measures connect latency.
//
// Caveat: each probe is an aborted connect on the server. MariaDB blocks a
// client host after max_connect_errors such connects only when it keeps a
// host cache (name resolution on); the Galera service spec sets
// skip_name_resolve (lab E5: no block after hundreds of probes with it).
type TCPProber struct{}

// Probe dials addr and closes the connection.
func (TCPProber) Probe(ctx context.Context, addr string) ProbeResult {
	c, lat, err := dialMeasured(ctx, addr)
	if err != nil {
		return ProbeResult{Err: err}
	}
	_ = c.Close()
	return ProbeResult{Healthy: true, Latency: lat, Detail: "tcp connect"}
}

func dialMeasured(ctx context.Context, addr string) (net.Conn, time.Duration, error) {
	var d net.Dialer
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", addr)
	lat := time.Since(start)
	if err != nil {
		return nil, 0, err
	}
	if lat <= 0 {
		lat = time.Nanosecond // a successful measurement must count as a sample
	}
	return c, lat, nil
}

// GreetingProber connects and reads the MySQL/MariaDB server greeting
// (Initial Handshake v10). Receiving a valid greeting proves the server
// process accepts connections; an ERR packet instead of a greeting (for
// example "Too many connections") is unhealthy. It does not authenticate and
// therefore cannot read the sync state (SyncUnknown). The max_connect_errors
// caveat of TCPProber applies as well. Latency is the TCP connect time.
type GreetingProber struct{}

// Probe connects, reads the greeting and closes the connection.
func (GreetingProber) Probe(ctx context.Context, addr string) ProbeResult {
	c, lat, err := dialMeasured(ctx, addr)
	if err != nil {
		return ProbeResult{Err: err}
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	seq, p, err := readPacket(c, maxHandshakePacket)
	if err != nil {
		return ProbeResult{Err: fmt.Errorf("reading greeting: %w", err)}
	}
	g, err := parseGreeting(p)
	if err != nil {
		return ProbeResult{Err: err}
	}
	if seq != 0 {
		return ProbeResult{Err: fmt.Errorf("%w: greeting sequence %d", errProtocol, seq)}
	}
	return ProbeResult{Healthy: true, Latency: lat, Detail: "server " + g.ServerVersion}
}

// GaleraStatus is the subset of wsrep status variables routing needs.
type GaleraStatus struct {
	// LocalState is wsrep_local_state (1 joining, 2 donor/desynced,
	// 3 joined, 4 synced).
	LocalState int
	// LocalStateComment is wsrep_local_state_comment ("Synced", "Donor/Desynced", ...).
	LocalStateComment string
	// ClusterStatus is wsrep_cluster_status ("Primary", "non-Primary", "Disconnected").
	ClusterStatus string
	// Ready is wsrep_ready = ON.
	Ready bool
}

// GaleraStatusProber reads the wsrep status of the node at addr.
type GaleraStatusProber interface {
	GaleraStatus(ctx context.Context, addr string) (GaleraStatus, error)
}

// SyncFromGalera maps wsrep status to a routing sync state. Only a ready
// member of the primary component in local state 4 (Synced) is SyncSynced.
func SyncFromGalera(s GaleraStatus) (routing.SyncState, string) {
	if s.ClusterStatus != "Primary" {
		return routing.SyncDesynced, fmt.Sprintf("wsrep_cluster_status %q", s.ClusterStatus)
	}
	if !s.Ready {
		return routing.SyncDesynced, "wsrep_ready OFF"
	}
	switch s.LocalState {
	case 4:
		return routing.SyncSynced, "wsrep synced"
	case 2:
		if s.LocalStateComment == "Desynced" {
			return routing.SyncDesynced, "wsrep desynced"
		}
		return routing.SyncDonor, "wsrep donor"
	case 1, 3:
		return routing.SyncJoining, fmt.Sprintf("wsrep %s", s.LocalStateComment)
	}
	return routing.SyncUnknown, fmt.Sprintf("wsrep_local_state %d", s.LocalState)
}

// NodeProber combines a liveness prober with a sync-state source.
type NodeProber struct {
	Liveness Prober
	// Status supplies the sync state. Ignored when AssumeSynced.
	Status GaleraStatusProber
	// AssumeSynced treats every live node as synced. Only for setups where
	// something else guarantees sync (labs, tests); it is an explicit
	// configuration choice ("sync": "assume-synced").
	AssumeSynced bool
}

// Probe runs the liveness probe and, if live, determines the sync state.
func (p NodeProber) Probe(ctx context.Context, addr string) ProbeResult {
	r := p.Liveness.Probe(ctx, addr)
	r.Sync = routing.SyncUnknown
	if !r.Healthy {
		return r
	}
	switch {
	case p.AssumeSynced:
		r.Sync = routing.SyncSynced
		r.Detail += "; sync assumed (no Galera status probe)"
	case p.Status == nil:
		r.Detail += "; no sync source"
	default:
		st, err := p.Status.GaleraStatus(ctx, addr)
		if err != nil {
			r.Err = err
			r.Detail += "; sync unknown"
			return r
		}
		var why string
		r.Sync, why = SyncFromGalera(st)
		r.Detail += "; " + why
	}
	return r
}

// proberFromConfig builds the configured prober. status is the Galera status
// source for "sync": "galera" (see Options.Status); when it is nil, nodes
// stay sync-unknown and are never routed to (fail closed).
func proberFromConfig(pc ProbeConfig, status GaleraStatusProber) Prober {
	var live Prober = TCPProber{}
	if pc.Liveness == LivenessMySQLGreeting {
		live = GreetingProber{}
	}
	np := NodeProber{Liveness: live}
	if pc.Sync == SyncAssumeSynced {
		np.AssumeSynced = true
	} else {
		np.Status = status
	}
	return np
}
