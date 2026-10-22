// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

// DefaultRules is the proposed starting rule set for Galera clusters. The
// thresholds are starting values, not measured ones; they are configurable by
// replacing or disabling rules in the manager settings.
func DefaultRules() []Rule {
	return []Rule{
		{ID: "galera-cluster-size-low", Summary: "Galera cluster has fewer nodes than expected",
			Selector: collect.MGaleraSizeDeficit, Cmp: Above, Threshold: 0, For: time.Minute, Severity: notify.SeverityError, Cooldown: time.Hour},
		{ID: "galera-not-primary", Summary: "Galera node is not part of the Primary component",
			Selector: collect.MGaleraPrimary, Cmp: Below, Threshold: 1, For: 30 * time.Second, Severity: notify.SeverityCritical, Cooldown: 30 * time.Minute},
		{ID: "galera-flow-control-high", Summary: "Galera flow control is pausing replication",
			Selector: collect.MGaleraFlowControlPaused, Func: "avg", Window: 2 * time.Minute, Cmp: Above, Threshold: 0.1, For: 2 * time.Minute, Severity: notify.SeverityWarning, Cooldown: time.Hour},
		{ID: "disk-almost-full", Summary: "Database volume is almost full",
			Selector: collect.MHostDiskRatio, Cmp: Above, Threshold: 0.9, For: 5 * time.Minute, Severity: notify.SeverityError, Cooldown: 2 * time.Hour},
		{ID: "backup-too-old", Summary: "Newest backup is older than the policy allows",
			Selector: collect.MBackupAgeRatio, Cmp: Above, Threshold: 1, Severity: notify.SeverityError, Cooldown: 6 * time.Hour},
		{ID: "certificate-expiry-soon", Summary: "Certificate expires within 14 days",
			Selector: collect.MCertExpirySeconds, Cmp: Below, Threshold: 14 * 24 * 3600, Severity: notify.SeverityWarning, Cooldown: 24 * time.Hour},
		{ID: "certificate-expiry-imminent", Summary: "Certificate expires within 3 days",
			Selector: collect.MCertExpirySeconds, Cmp: Below, Threshold: 3 * 24 * 3600, Severity: notify.SeverityCritical, Cooldown: 6 * time.Hour},
		{ID: "target-down", Summary: "A monitored target cannot be observed",
			Selector: collect.MTargetUp, Cmp: Equal, Threshold: 0, For: time.Minute, Severity: notify.SeverityWarning, Cooldown: time.Hour},
		{ID: "proxy-no-writer", Summary: "Routing proxy has no writer",
			Selector: collect.MProxyHasWriter, Cmp: Equal, Threshold: 0, For: 30 * time.Second, Severity: notify.SeverityCritical, Cooldown: 30 * time.Minute},
	}
}

// Without returns rules minus the given IDs (settings: disabled rules).
func Without(rules []Rule, disabled ...string) []Rule {
	skip := map[string]bool{}
	for _, d := range disabled {
		skip[d] = true
	}
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if !skip[r.ID] {
			out = append(out, r)
		}
	}
	return out
}
