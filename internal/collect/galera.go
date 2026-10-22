// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
)

// StatusSource returns the parsed SHOW GLOBAL STATUS of a node and the time
// it was observed (see galera.ParseStatusOutput). It is implemented over the
// agent's status command. An error or a zero observation time means the node
// could not be observed.
type StatusSource interface {
	Status(ctx context.Context, t Target) (vars map[string]string, observedAt time.Time, err error)
}

// GaleraConfig configures a GaleraCollector.
type GaleraConfig struct {
	Source StatusSource
	// Targets lists the nodes to collect, re-evaluated on every run.
	Targets func() []Target
	// StaleAfter: an observation older than this is treated as missing
	// (default 1 minute).
	StaleAfter time.Duration
	// MaxTargets bounds the number of targets and so the series count
	// (default 256). Extra targets are not collected.
	MaxTargets int
	// MaxSamples caps samples per run (default 20000).
	MaxSamples int
}

// GaleraCollector maps MariaDB/Galera status variables to the catalog.
type GaleraCollector struct{ cfg GaleraConfig }

// NewGaleraCollector validates cfg.
func NewGaleraCollector(cfg GaleraConfig) (*GaleraCollector, error) {
	if cfg.Source == nil || cfg.Targets == nil {
		return nil, errors.New("collect: galera collector needs Source and Targets")
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = time.Minute
	}
	if cfg.MaxTargets <= 0 {
		cfg.MaxTargets = 256
	}
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = 20000
	}
	return &GaleraCollector{cfg: cfg}, nil
}

// Name implements Collector.
func (g *GaleraCollector) Name() string { return "galera" }

// Collect implements Collector. Targets are queried sequentially so that a
// slow node cannot multiply load; the scheduler's timeout bounds the run.
func (g *GaleraCollector) Collect(ctx context.Context, now time.Time) ([]metrics.Entry, error) {
	b := newBuilder(now, g.cfg.MaxSamples)
	targets := g.cfg.Targets()
	var errs []error
	if len(targets) > g.cfg.MaxTargets {
		errs = append(errs, fmt.Errorf("collect: %d targets exceed the limit of %d; %d not collected", len(targets), g.cfg.MaxTargets, len(targets)-g.cfg.MaxTargets))
		targets = targets[:g.cfg.MaxTargets]
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		lbl := []string{LabelCluster, t.Cluster, LabelNode, t.Node, LabelSwarm, t.Swarm}
		up := func(v float64) { b.add(MTargetUp, v, append([]string{LabelCollector, "galera"}, lbl...)...) }
		vars, at, err := g.cfg.Source.Status(ctx, t)
		switch {
		case err != nil:
			up(0)
			errs = append(errs, fmt.Errorf("node %s/%s/%s: %w", t.Swarm, t.Cluster, t.Node, err))
			continue
		case len(vars) == 0 || at.IsZero() || now.Sub(at) > g.cfg.StaleAfter || at.Sub(now) > g.cfg.StaleAfter:
			up(0) // stale or empty: no value samples, queries show a gap
			continue
		}
		up(1)
		g.node(b, t, vars, lbl)
	}
	if b.dropped > 0 {
		errs = append(errs, fmt.Errorf("collect: %d samples dropped by the per-run cap", b.dropped))
	}
	return b.entries, errors.Join(errs...)
}

func (g *GaleraCollector) node(b *builder, t Target, v map[string]string, l []string) {
	gauge := func(metric, key string) {
		if f, ok := parseF(v, key); ok {
			b.add(metric, f, l...)
		}
	}
	if f, ok := parseF(v, "wsrep_cluster_size"); ok {
		b.add(MGaleraClusterSize, f, l...)
		if t.ExpectedSize > 0 {
			b.add(MGaleraSizeDeficit, float64(t.ExpectedSize)-f, l...)
		}
	}
	switch v["wsrep_cluster_status"] {
	case "Primary":
		b.add(MGaleraPrimary, 1, l...)
	case "non-Primary", "Disconnected":
		b.add(MGaleraPrimary, 0, l...)
	}
	gauge(MGaleraLocalState, "wsrep_local_state")
	if f, ok := onOff(v, "wsrep_ready"); ok {
		b.add(MGaleraReady, f, l...)
	}
	if f, ok := onOff(v, "wsrep_connected"); ok {
		b.add(MGaleraConnected, f, l...)
	}
	gauge(MGaleraFlowControlPaused, "wsrep_flow_control_paused")
	gauge(MGaleraFlowControlSent, "wsrep_flow_control_sent")
	gauge(MGaleraFlowControlRecv, "wsrep_flow_control_recv")
	gauge(MGaleraRecvQueueAvg, "wsrep_local_recv_queue_avg")
	gauge(MGaleraSendQueueAvg, "wsrep_local_send_queue_avg")
	gauge(MGaleraLastCommitted, "wsrep_last_committed")
	gauge(MGaleraReplicatedBytes, "wsrep_replicated_bytes")
	gauge(MGaleraReceivedBytes, "wsrep_received_bytes")
	gauge(MDBThreadsConnected, "Threads_connected")
	gauge(MDBThreadsRunning, "Threads_running")
	gauge(MDBMaxUsedConnections, "Max_used_connections")
	gauge(MDBConnections, "Connections")
	gauge(MDBAbortedConnects, "Aborted_connects")
	gauge(MDBQueries, "Queries")
	gauge(MDBQuestions, "Questions")
	gauge(MDBSlowQueries, "Slow_queries")
	gauge(MDBBytesReceived, "Bytes_received")
	gauge(MDBBytesSent, "Bytes_sent")
	gauge(MDBUptime, "Uptime")
	gauge(MDBReplicationLagSeconds, "Seconds_Behind_Master")

	reads, ok1 := parseF(v, "Innodb_buffer_pool_reads")
	reqs, ok2 := parseF(v, "Innodb_buffer_pool_read_requests")
	if ok1 && ok2 && reqs > 0 {
		r := 1 - reads/reqs
		b.add(MDBBufferPoolHitRatio, clamp01(r), l...)
	}
	total, ok3 := parseF(v, "Innodb_buffer_pool_pages_total")
	if ok3 && total > 0 {
		if free, ok := parseF(v, "Innodb_buffer_pool_pages_free"); ok {
			b.add(MDBBufferPoolUsedRatio, clamp01(1-free/total), l...)
		}
		if dirty, ok := parseF(v, "Innodb_buffer_pool_pages_dirty"); ok {
			b.add(MDBBufferPoolDirtyRatio, clamp01(dirty/total), l...)
		}
	}
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	}
	return f
}
