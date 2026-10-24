// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"net/url"
	"sort"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// VulnScheduler polls the advisory feeds while this replica leads (X13,
// ADR 0015). Poll returns nil when the current leadership has no
// vulnerability service; a failing poll is reported through Logf and tried
// again at the next interval (the service backs failing feeds off itself).
type VulnScheduler struct {
	Poll func(ctx context.Context) error
	// Interval defaults to one hour.
	Interval time.Duration
	// InitialDelay is waited before the first poll (default 30 s) so a
	// starting leader finishes its own setup first.
	InitialDelay time.Duration
	Logf         func(msg string, args ...any)
}

// Name implements Reconciler.
func (s VulnScheduler) Name() string { return "vuln-scheduler" }

// Run implements Reconciler.
func (s VulnScheduler) Run(ctx context.Context, _ *Leadership) error {
	iv := s.Interval
	if iv <= 0 {
		iv = time.Hour
	}
	delay := s.InitialDelay
	if delay <= 0 {
		delay = 30 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if s.Poll != nil {
			if err := s.Poll(ctx); err != nil && ctx.Err() == nil && s.Logf != nil {
				s.Logf("manager: vulnerability poll failed", "err", err)
			}
		}
		timer.Reset(iv)
	}
}

// VulnNotifier delivers vulnerability notifications through the manager's
// notification dispatcher (UI inbox, e-mail, webhooks; routing and
// suppression rules apply). Send returns false when no dispatcher runs.
type VulnNotifier struct {
	Send func(ctx context.Context, e notify.Event) bool
	Now  func() time.Time
}

var _ vuln.Notifier = VulnNotifier{}

// Notify implements vuln.Notifier. The event carries the finding and
// proposal IDs but nothing from advisories or AI answers.
func (n VulnNotifier) Notify(ctx context.Context, v vuln.Notification) error {
	if n.Send == nil {
		return nil
	}
	now := time.Now
	if n.Now != nil {
		now = n.Now
	}
	res := "vuln"
	d := map[string]string{"kind": v.Kind}
	if v.FindingID != "" {
		res += "/finding/" + v.FindingID
		d["finding"] = v.FindingID
	}
	if v.ProposalID != "" {
		d["proposal"] = v.ProposalID
	}
	if v.Priority != "" {
		d["priority"] = string(v.Priority)
	}
	n.Send(ctx, notify.Event{Time: now(), Severity: notifySeverity(v), Kind: "vuln." + v.Kind, Resource: res, Message: v.Title, Details: d})
	return nil
}

func notifySeverity(v vuln.Notification) notify.Severity {
	switch v.Priority {
	case vuln.P1:
		return notify.SeverityCritical
	case vuln.P2:
		return notify.SeverityError
	case vuln.P3:
		return notify.SeverityWarning
	case vuln.P4:
		return notify.SeverityInfo
	}
	return notify.SeverityWarning
}

// VulnRedactNames returns the environment identifiers that must not reach
// an AI provider: Swarm, host and cluster names and registry hosts. Service
// names are not included because they often equal package names (a service
// "nginx") and redacting them would corrupt the component data.
func VulnRedactNames(st *Store, regs RegistryLister) func() []string {
	return func() []string {
		set := map[string]bool{}
		if rs, err := List[SwarmRecord](st, CollSwarms); err == nil {
			for _, r := range rs {
				set[r.Value.ID], set[r.Value.Name] = true, true
			}
		}
		if hs, err := List[HostRecord](st, CollHosts); err == nil {
			for _, h := range hs {
				set[h.Value.Hostname] = true
			}
		}
		if cs, err := List[ClusterRecord](st, CollClusters); err == nil {
			for _, c := range cs {
				set[c.Value.Name] = true
			}
		}
		if regs != nil {
			if cfgs, err := regs.List(context.Background()); err == nil {
				for _, c := range cfgs {
					if c.Kind == registry.KindManaged {
						continue
					}
					if u, err := url.Parse(c.Endpoint); err == nil {
						set[u.Host], set[u.Hostname()] = true, true
					}
				}
			}
		}
		out := make([]string, 0, len(set))
		for n := range set {
			if len(n) >= 3 {
				out = append(out, n)
			}
		}
		sort.Strings(out)
		return out
	}
}
