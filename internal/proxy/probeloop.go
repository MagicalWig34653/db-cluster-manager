// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"context"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

// probeLoop runs probe rounds every probe interval, or earlier when kicked
// (backend dial failure, reload), but never more often than a quarter of the
// interval.
func (p *Proxy) probeLoop() {
	defer p.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	var last time.Time
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-timer.C:
		case <-p.kick:
			if time.Since(last) < p.interval()/4 {
				continue
			}
		}
		last = time.Now()
		p.probeRound(p.ctx)
		timer.Reset(max(p.interval()-time.Since(last), p.interval()/4))
	}
}

func (p *Proxy) interval() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.Probe.Interval.D()
}

// probeRound probes every node that has no probe in flight, waits for the
// results up to the probe timeout (plus a small grace) and evaluates the
// routing decision. A node whose prober does not return is simply not
// updated and becomes stale; at most one probe per node is ever in flight,
// so goroutines stay bounded by the node count.
func (p *Proxy) probeRound(ctx context.Context) {
	p.syncWriterChoice(ctx)
	p.mu.Lock()
	prober := p.prober
	timeout := p.cfg.Probe.Timeout.D()
	type job struct {
		ns   *nodeState
		addr string
	}
	var jobs []job
	for _, ns := range p.nodes {
		if ns.inflight {
			continue
		}
		ns.inflight = true
		jobs = append(jobs, job{ns, ns.cfg.Address})
	}
	p.lastRound = time.Now()
	p.mu.Unlock()

	results := make(chan struct{}, len(jobs))
	for _, j := range jobs {
		p.probeWG.Add(1)
		go func() {
			defer p.probeWG.Done()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			r := prober.Probe(pctx, j.addr)
			cancel()
			p.record(j.ns, r, time.Now())
			results <- struct{}{}
		}()
	}
	wait := time.NewTimer(timeout + timeout/4 + 10*time.Millisecond)
	defer wait.Stop()
collect:
	for range jobs {
		select {
		case <-results:
		case <-wait.C:
			break collect
		case <-ctx.Done():
			return
		}
	}
	p.mu.Lock()
	p.evaluateLocked(time.Now())
	p.mu.Unlock()
}

// syncWriterChoice applies a leader decision before probing. During a freeze
// it closes every write connection before acknowledging the epoch. If the
// manager cannot be reached, the last choice remains, subject to local
// health; a fresh proxy has no writer until it reaches the manager.
func (p *Proxy) syncWriterChoice(ctx context.Context) {
	p.mu.Lock()
	if p.cfg.Coordination == nil {
		p.mu.Unlock()
		return
	}
	cfg := *p.cfg.Coordination
	source := p.writerSource
	p.mu.Unlock()
	requestCtx, cancel := context.WithTimeout(ctx, cfg.Timeout.D())
	h, err := source.Fetch(requestCtx, cfg)
	cancel()
	if err != nil {
		p.log.Warn("writer choice unavailable; keeping last safe choice", "err", err)
		return
	}
	p.mu.Lock()
	if p.cfg.Coordination == nil || p.cfg.Coordination.ClusterID != cfg.ClusterID || p.cfg.Coordination.ProxyID != cfg.ProxyID {
		p.mu.Unlock()
		return
	}
	// Never regress to an older epoch or from an active choice back into
	// the freeze of that same epoch.
	if h.Epoch < p.hint.Epoch || (h.Epoch == p.hint.Epoch && p.hint.Pending == "" && p.hint.Writer != "" && h.Pending != "") {
		p.mu.Unlock()
		return
	}
	p.hint = h
	if h.Pending != "" {
		p.evaluateLocked(time.Now())
		for c := range p.conns {
			if c.ep == EndpointWrite {
				c.close()
			}
		}
	}
	p.mu.Unlock()
	if h.Pending != "" {
		ackCtx, stop := context.WithTimeout(ctx, cfg.Timeout.D())
		if err := source.Ack(ackCtx, cfg, h.Epoch); err != nil {
			p.log.Warn("writer freeze acknowledgement failed", "err", err)
		}
		stop()
	}
}

func (p *Proxy) record(ns *nodeState, r ProbeResult, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ns.inflight = false
	if ns.removed {
		return
	}
	ns.reported = true
	ns.lastSeen = now
	ns.healthy = r.Healthy
	ns.sync = r.Sync
	ns.detail = r.Detail
	ns.lastErr = ""
	if r.Err != nil {
		ns.lastErr = r.Err.Error()
	}
	if r.Healthy && r.Latency > 0 {
		ns.samples = append(ns.samples, r.Latency)
		if n := p.cfg.Probe.Samples; len(ns.samples) > n {
			ns.samples = slices.Clone(ns.samples[len(ns.samples)-n:])
		}
	}
}

func (p *Proxy) routingNodesLocked() []routing.Node {
	out := make([]routing.Node, 0, len(p.nodes))
	for _, ns := range p.nodes {
		n := routing.Node{ID: ns.cfg.ID, Role: ns.role, Sync: routing.SyncUnknown}
		if ns.reported {
			n.Healthy = ns.healthy
			n.LastSeen = ns.lastSeen
			n.Sync = ns.sync
			n.LatencySamples = slices.Clone(ns.samples)
		}
		if p.cfg.Coordination != nil && n.Role == routing.RoleWriterEligible && n.ID != p.hint.Writer {
			n.Role = routing.RoleReader
		}
		out = append(out, n)
	}
	return out
}

// evaluateLocked runs the router and applies the decision: new connections
// go to the new writer at once; existing connections to the previous writer
// (and to nodes that left the reader set) are closed at the drain deadline.
func (p *Proxy) evaluateLocked(now time.Time) {
	d := p.router.Decide(now, p.routingNodesLocked())
	p.decision, p.decidedAt = d, now
	deadline := now.Add(p.router.Policy().DrainDeadline)
	if d.Writer != p.writer {
		if p.writer != "" {
			dl := deadline
			if d.Drain != nil && d.Drain.NodeID == p.writer {
				dl = d.Drain.Deadline
			}
			p.drainLocked(EndpointWrite, p.writer, dl, now)
		}
		p.log.Warn("writer changed", "from", p.writer, "to", d.Writer, "has_writer", d.HasWriter, "reasons", d.Reasons)
		p.changes++
		p.writer = d.Writer
		if d.Writer != "" {
			p.undrainLocked(EndpointWrite, d.Writer)
		}
	}
	for _, old := range p.readers {
		if !slices.Contains(d.Readers, old) {
			p.drainLocked(EndpointRead, old, deadline, now)
		}
	}
	for _, r := range d.Readers {
		if !slices.Contains(p.readers, r) {
			p.undrainLocked(EndpointRead, r)
		}
	}
	p.readers = slices.Clone(d.Readers)
}

func (p *Proxy) drainLocked(ep Endpoint, node string, deadline, now time.Time) {
	n := 0
	for c := range p.conns {
		if c.ep == ep && c.node == node {
			c.scheduleClose(deadline, now)
			n++
		}
	}
	if n > 0 {
		p.log.Info("draining connections", "endpoint", ep, "node", node, "count", n, "deadline", deadline)
	}
}

func (p *Proxy) undrainLocked(ep Endpoint, node string) {
	for c := range p.conns {
		if c.ep == ep && c.node == node {
			c.cancelDrain()
		}
	}
}
