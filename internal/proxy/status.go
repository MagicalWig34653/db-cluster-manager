// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

// Unknown is the literal shown for values the proxy does not know.
const Unknown = "unknown"

// Status is the JSON document served at /status.
type Status struct {
	Now time.Time `json:"now"`
	// Writer is the current writer ID or "none".
	Writer        string   `json:"writer"`
	HasWriter     bool     `json:"has_writer"`
	WriterAddress string   `json:"writer_address,omitempty"`
	Readers       []string `json:"readers"`
	// DecidedAt is null before the first decision.
	DecidedAt     *time.Time   `json:"decided_at"`
	Reasons       []string     `json:"reasons"`
	WriterChanges int          `json:"writer_changes"`
	Policy        PolicyStatus `json:"policy"`
	Nodes         []NodeStatus `json:"nodes"`
	Connections   ConnStatus   `json:"connections"`
}

// PolicyStatus summarises the effective policy.
type PolicyStatus struct {
	Mode          string `json:"mode"`
	PreferredID   string `json:"preferred_id,omitempty"`
	StaleAfter    string `json:"stale_after"`
	DrainDeadline string `json:"drain_deadline"`
	ProbeInterval string `json:"probe_interval"`
	Sync          string `json:"sync_source"`
	TLS           bool   `json:"tls"`
}

// NodeStatus is one node. Health is "healthy", "unhealthy" or "unknown"
// (never reported or stale); latency and age are "unknown" when not known.
type NodeStatus struct {
	ID          string     `json:"id"`
	Address     string     `json:"address"`
	Role        string     `json:"role"`
	Health      string     `json:"health"`
	Sync        string     `json:"sync"`
	LatencyEWMA string     `json:"latency_ewma"`
	Samples     int        `json:"samples"`
	LastSeen    *time.Time `json:"last_seen"`
	Age         string     `json:"age"`
	Detail      string     `json:"detail,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	Connections int        `json:"connections"`
	Draining    int        `json:"draining"`
}

// ConnStatus counts open client connections per endpoint.
type ConnStatus struct {
	Write int `json:"write"`
	Read  int `json:"read"`
}

// Status returns a snapshot of the proxy state.
func (p *Proxy) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	pol := p.router.Policy()
	s := Status{
		Now:           now,
		Writer:        "none",
		HasWriter:     p.writer != "",
		Readers:       slices.Clone(p.readers),
		WriterChanges: p.changes,
		Policy: PolicyStatus{
			Mode:          pol.Mode.String(),
			PreferredID:   pol.PreferredID,
			StaleAfter:    pol.StaleAfter.String(),
			DrainDeadline: pol.DrainDeadline.String(),
			ProbeInterval: p.cfg.Probe.Interval.D().String(),
			Sync:          p.cfg.Probe.Sync,
			TLS:           p.tlsCfg != nil,
		},
		Connections: ConnStatus{Write: p.epCount[EndpointWrite], Read: p.epCount[EndpointRead]},
	}
	if s.Readers == nil {
		s.Readers = []string{}
	}
	if p.decidedAt.IsZero() {
		s.Writer = Unknown
		s.Reasons = []string{"no routing decision yet (first probe round pending)"}
	} else {
		t := p.decidedAt
		s.DecidedAt = &t
		s.Reasons = slices.Clone(p.decision.Reasons)
	}
	if p.writer != "" {
		s.Writer = p.writer
	}
	for _, ns := range p.nodes {
		n := NodeStatus{
			ID: ns.cfg.ID, Address: ns.cfg.Address, Role: ns.role.String(),
			Health: Unknown, Sync: routing.SyncUnknown.String(), LatencyEWMA: Unknown, Age: Unknown,
			Samples: len(ns.samples), Detail: ns.detail, LastError: ns.lastErr,
		}
		if ns.reported {
			t := ns.lastSeen
			n.LastSeen = &t
			age := now.Sub(ns.lastSeen)
			n.Age = age.Round(time.Millisecond).String()
			if age <= pol.StaleAfter {
				n.Health = "unhealthy"
				if ns.healthy {
					n.Health = "healthy"
				}
				n.Sync = ns.sync.String()
			} else {
				n.Detail = "stale report (older than stale_after); " + n.Detail
			}
		}
		if v, ok := routing.EWMA(ns.samples, pol.EWMAAlpha); ok {
			n.LatencyEWMA = v.String()
		}
		if ns.cfg.ID == p.writer {
			s.WriterAddress = ns.cfg.Address
		}
		for c := range p.conns {
			if c.node == ns.cfg.ID {
				n.Connections++
				if c.draining() {
					n.Draining++
				}
			}
		}
		s.Nodes = append(s.Nodes, n)
	}
	return s
}

// StatusHandler serves GET /status as JSON. It has no authentication; the
// proxy only binds it to loopback addresses.
func (p *Proxy) StatusHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(p.Status())
	})
	return mux
}
