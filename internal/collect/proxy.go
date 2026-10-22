// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
	"github.com/MagicalWig34653/db-cluster-manager/internal/proxy"
)

// ProxyTarget is one routing proxy whose /status document is scraped.
type ProxyTarget struct {
	Swarm, Cluster, Name string
	URL                  string // full URL of the /status endpoint
}

// HTTPDoer is the part of http.Client the proxy collector needs.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ProxyConfig configures a ProxyCollector.
type ProxyConfig struct {
	Targets    func() []ProxyTarget
	Client     HTTPDoer
	StaleAfter time.Duration // age of Status.Now (default 1 minute)
	MaxTargets int           // default 64
	MaxNodes   int           // per proxy, default 64
	MaxBody    int64         // default 1 MiB
}

// ProxyCollector scrapes the proxy's /status JSON.
type ProxyCollector struct{ cfg ProxyConfig }

// NewProxyCollector validates cfg.
func NewProxyCollector(cfg ProxyConfig) (*ProxyCollector, error) {
	if cfg.Targets == nil || cfg.Client == nil {
		return nil, errors.New("collect: proxy collector needs Targets and Client")
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = time.Minute
	}
	if cfg.MaxTargets <= 0 {
		cfg.MaxTargets = 64
	}
	if cfg.MaxNodes <= 0 {
		cfg.MaxNodes = 64
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 1 << 20
	}
	return &ProxyCollector{cfg: cfg}, nil
}

// Name implements Collector.
func (p *ProxyCollector) Name() string { return "proxy" }

// Collect implements Collector.
func (p *ProxyCollector) Collect(ctx context.Context, now time.Time) ([]metrics.Entry, error) {
	b := newBuilder(now, 20000)
	ts := p.cfg.Targets()
	var errs []error
	if len(ts) > p.cfg.MaxTargets {
		errs = append(errs, fmt.Errorf("collect: %d proxies exceed the limit of %d", len(ts), p.cfg.MaxTargets))
		ts = ts[:p.cfg.MaxTargets]
	}
	for _, t := range ts {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		l := []string{LabelSwarm, t.Swarm, LabelCluster, t.Cluster, LabelProxy, t.Name}
		up := func(v float64) { b.add(MTargetUp, v, append([]string{LabelCollector, "proxy"}, l...)...) }
		st, err := p.fetch(ctx, t)
		if err != nil {
			up(0)
			errs = append(errs, fmt.Errorf("proxy %s: %w", t.Name, err))
			continue
		}
		if st.Now.IsZero() || now.Sub(st.Now) > p.cfg.StaleAfter || st.Now.Sub(now) > p.cfg.StaleAfter {
			up(0)
			continue
		}
		up(1)
		b.add(MProxyHasWriter, boolV(st.HasWriter), l...)
		b.add(MProxyWriterChanges, float64(st.WriterChanges), l...)
		b.add(MProxyConnections, float64(st.Connections.Write), append([]string{LabelRole, "write"}, l...)...)
		b.add(MProxyConnections, float64(st.Connections.Read), append([]string{LabelRole, "read"}, l...)...)
		nodes := st.Nodes
		if len(nodes) > p.cfg.MaxNodes {
			nodes = nodes[:p.cfg.MaxNodes]
			errs = append(errs, fmt.Errorf("proxy %s: node list truncated to %d", t.Name, p.cfg.MaxNodes))
		}
		for _, n := range nodes {
			nl := append([]string{LabelNode, n.ID}, l...)
			switch n.Health { // "unknown" (never reported or stale) emits nothing
			case "healthy":
				b.add(MProxyNodeHealthy, 1, nl...)
			case "unhealthy":
				b.add(MProxyNodeHealthy, 0, nl...)
			}
			b.add(MProxyNodeConnections, float64(n.Connections), nl...)
			b.add(MProxyNodeDraining, float64(n.Draining), nl...)
		}
	}
	if b.dropped > 0 {
		errs = append(errs, fmt.Errorf("collect: %d samples dropped", b.dropped))
	}
	return b.entries, errors.Join(errs...)
}

func (p *ProxyCollector) fetch(ctx context.Context, t ProxyTarget) (proxy.Status, error) {
	var st proxy.Status
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return st, err
	}
	resp, err := p.cfg.Client.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, p.cfg.MaxBody+1))
	if err != nil {
		return st, err
	}
	if int64(len(body)) > p.cfg.MaxBody {
		return st, errors.New("response too large")
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("decode: %w", err)
	}
	return st, nil
}
