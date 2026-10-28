// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// ProxyWriterRoute is a configured, optional automatic writer failover group.
type ProxyWriterRoute struct {
	Cluster       string
	InitialWriter string
	Candidates    []string
	Members       []string
}

// ProxyWriterReconciler observes the manager's replicated Galera health and
// requests a freeze when the chosen writer is no longer freshly healthy. It
// never bypasses the all-proxy acknowledgement barrier and never bootstraps
// Galera. Unknown/stale candidates cannot be selected.
type ProxyWriterReconciler struct {
	Routes   []ProxyWriterRoute
	Interval time.Duration // default 5s
	MaxAge   time.Duration // default 30s
}

func (ProxyWriterReconciler) Name() string { return "proxy-writer-failover" }

func (r ProxyWriterReconciler) Run(ctx context.Context, l *Leadership) error {
	iv := r.Interval
	if iv <= 0 {
		iv = 5 * time.Second
	}
	ticker := time.NewTicker(iv)
	defer ticker.Stop()
	for {
		if err := r.ReconcileOnce(l); err != nil && ctx.Err() == nil {
			l.svc.log.Warn("proxy writer failover check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// ReconcileOnce is deterministic given the current replicated health records.
// A manager without a quorum cannot read consistently or initiate a switch.
func (r ProxyWriterReconciler) ReconcileOnce(l *Leadership) error {
	if l == nil || !l.Valid() {
		return ErrNotLeader
	}
	maxAge := r.MaxAge
	if maxAge <= 0 {
		maxAge = 30 * time.Second
	}
	now := l.svc.cfg.Now()
	for _, route := range r.Routes {
		coord := ProxyWriterStore{Store: l.svc.Store(), Cluster: route.Cluster, InitialWriter: route.InitialWriter, Candidates: route.Candidates, Members: route.Members}
		choice, err := coord.Current()
		if err != nil {
			return err
		}
		if choice.Pending != "" {
			continue
		}
		rec, ok, err := Get[ClusterRecord](l.svc.Store(), CollClusters, route.Cluster, Consistent())
		if err != nil {
			return err
		}
		if !ok {
			// Cluster record IDs are generated, but the route is configured
			// before the cluster exists: also accept the cluster name.
			all, err := List[ClusterRecord](l.svc.Store(), CollClusters, Consistent())
			if err != nil {
				return err
			}
			for _, c := range all {
				if c.Value.Name == route.Cluster {
					rec, ok = c, true
					break
				}
			}
		}
		if !ok || len(rec.Value.Nodes) == 0 {
			continue
		}
		healthy := func(id string) bool {
			if !slices.ContainsFunc(rec.Value.Nodes, func(n galera.Node) bool { return n.Slot == id }) {
				return false
			}
			obs, ok := rec.Value.Health[id]
			if !ok || obs.ObservedAt.IsZero() || obs.ObservedAt.After(now) || now.Sub(obs.ObservedAt) > maxAge {
				return false
			}
			h := obs.Health
			return h.Healthy() && h.InPrimary() && h.LocalState == "Synced" && h.ClusterSize > len(rec.Value.Nodes)/2
		}
		if healthy(choice.Writer) {
			continue
		}
		candidates := slices.Clone(choice.Candidates)
		slices.Sort(candidates)
		for _, id := range candidates {
			if id == choice.Writer || !healthy(id) {
				continue
			}
			freeze, err := coord.Switch(id)
			if errors.Is(err, ErrWriterSwitchPending) {
				break
			}
			if err != nil {
				return err
			}
			l.svc.log.Warn("proxy writer failover frozen", "cluster", route.Cluster, "from", choice.Writer, "to", id, "epoch", freeze.Epoch)
			break
		}
	}
	return nil
}
