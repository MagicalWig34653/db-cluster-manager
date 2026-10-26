// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"fmt"
	"strconv"
	"strings"
)

// HealthState classifies a node. The zero value is HealthUnknown, and
// Unknown is never treated as healthy.
type HealthState string

// Health states.
const (
	HealthUnknown   HealthState = ""          // not observed, incomplete or unparsable
	HealthHealthy   HealthState = "healthy"   // Primary, connected, ready, Synced
	HealthDegraded  HealthState = "degraded"  // Primary member, but Joined, Donor/Desynced or throttled
	HealthUnhealthy HealthState = "unhealthy" // not a usable member of a Primary component
)

func (s HealthState) String() string {
	if s == HealthUnknown {
		return "unknown"
	}
	return string(s)
}

// FlowControlDegradedThreshold is the wsrep_flow_control_paused fraction
// above which a Synced node is reported degraded. It is a starting value,
// not a measured one.
const FlowControlDegradedThreshold = 0.1

// ReasonZeroUUIDPrimary identifies a Primary component with no valid state
// UUID. It must not be used as a donor or as quorum evidence for operations.
const ReasonZeroUUIDPrimary = "zero_cluster_uuid_primary"

// NodeHealth is derived from wsrep status variables.
type NodeHealth struct {
	State             HealthState `json:"state"`
	ClusterSize       int         `json:"cluster_size,omitempty"`
	ClusterStatus     string      `json:"cluster_status,omitempty"`
	LocalState        string      `json:"local_state,omitempty"`
	Ready             bool        `json:"ready,omitempty"`
	Connected         bool        `json:"connected,omitempty"`
	FlowControlPaused float64     `json:"flow_control_paused,omitempty"`
	StateUUID         string      `json:"state_uuid,omitempty"`
	LastCommitted     int64       `json:"last_committed,omitempty"`
	Reasons           []string    `json:"reasons,omitempty"`
	ReasonCode        string      `json:"reason_code,omitempty"`
}

// Healthy reports a fully healthy node.
func (h NodeHealth) Healthy() bool { return h.State == HealthHealthy }

// InPrimary reports a connected, ready member of a Primary component. Only
// such nodes count toward quorum. Unknown health never does.
func (h NodeHealth) InPrimary() bool {
	return (h.State == HealthHealthy || h.State == HealthDegraded) &&
		h.ClusterStatus == "Primary" && h.Connected && h.Ready && h.ClusterSize > 0
}

// ReportsPrimary records that a component exists even when its members are
// unsafe for operations. Full-outage recovery must be refused in this case.
func (h NodeHealth) ReportsPrimary() bool {
	return h.ClusterStatus == "Primary" && h.ClusterSize > 0
}

// DonorCapable reports a node that may serve as a verified state-transfer
// donor: healthy and Synced.
func (h NodeHealth) DonorCapable() bool { return h.Healthy() && h.LocalState == "Synced" }

// ParseStatusOutput parses the tab-separated `mariadb -N -B` output of
// SHOW GLOBAL STATUS into a map. Blank or malformed lines are skipped.
func ParseStatusOutput(out string) map[string]string {
	vars := map[string]string{}
	for line := range strings.Lines(out) {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "\t")
		if ok && k != "" {
			vars[k] = v
		}
	}
	return vars
}

func parseOnOff(v string) (bool, bool) {
	switch v {
	case "ON":
		return true, true
	case "OFF":
		return false, true
	}
	return false, false
}

// HealthFromStatus derives NodeHealth from SHOW GLOBAL STATUS variables.
// Required: wsrep_cluster_size, wsrep_cluster_status,
// wsrep_local_state_comment, wsrep_ready, wsrep_connected. A missing or
// unparsable required variable yields HealthUnknown. Values are compared
// case-sensitively as MariaDB reports them; anything unexpected is Unknown.
func HealthFromStatus(vars map[string]string) NodeHealth {
	unknown := func(reason string) NodeHealth {
		return NodeHealth{State: HealthUnknown, Reasons: []string{reason}}
	}
	if vars == nil {
		return unknown("no status observed")
	}
	for _, k := range []string{"wsrep_cluster_size", "wsrep_cluster_status", "wsrep_local_state_comment", "wsrep_ready", "wsrep_connected"} {
		if v, ok := vars[k]; !ok || v == "" {
			return unknown("missing " + k)
		}
	}
	size, err := strconv.Atoi(vars["wsrep_cluster_size"])
	if err != nil || size < 0 {
		return unknown("unparsable wsrep_cluster_size")
	}
	ready, ok1 := parseOnOff(vars["wsrep_ready"])
	connected, ok2 := parseOnOff(vars["wsrep_connected"])
	if !ok1 || !ok2 {
		return unknown("unparsable wsrep_ready or wsrep_connected")
	}
	status := vars["wsrep_cluster_status"]
	switch status {
	case "Primary", "non-Primary", "Disconnected":
	default:
		return unknown("unexpected wsrep_cluster_status " + strconv.Quote(status))
	}
	h := NodeHealth{
		ClusterSize:   size,
		ClusterStatus: status,
		LocalState:    vars["wsrep_local_state_comment"],
		Ready:         ready,
		Connected:     connected,
		StateUUID:     vars["wsrep_cluster_state_uuid"],
	}
	if lc, err := strconv.ParseInt(vars["wsrep_last_committed"], 10, 64); err == nil {
		h.LastCommitted = lc
	}
	var unhealthy, degraded []string
	if status == "Primary" && h.StateUUID == ZeroUUID {
		h.ReasonCode = ReasonZeroUUIDPrimary
		unhealthy = append(unhealthy, "Primary component reports the zero cluster UUID; preserve all node data and logs, inspect Galera state and state-transfer failures, and use an operator-led recovery plan only after the Primary component is gone; do not bootstrap or replace automatically")
	}
	if status != "Primary" {
		unhealthy = append(unhealthy, "cluster status "+status)
	}
	if !connected {
		unhealthy = append(unhealthy, "not connected")
	}
	if !ready {
		unhealthy = append(unhealthy, "not ready")
	}
	if size < 1 {
		unhealthy = append(unhealthy, "cluster size 0")
	}
	switch {
	case h.LocalState == "Synced":
	case h.LocalState == "Joined", strings.HasPrefix(h.LocalState, "Donor"):
		degraded = append(degraded, "local state "+h.LocalState)
	default: // Joining, Initialized, Waiting, ...
		unhealthy = append(unhealthy, "local state "+h.LocalState)
	}
	if fc, ok := vars["wsrep_flow_control_paused"]; ok {
		f, err := strconv.ParseFloat(fc, 64)
		switch {
		case err != nil:
			degraded = append(degraded, "unparsable wsrep_flow_control_paused")
		case f > FlowControlDegradedThreshold:
			h.FlowControlPaused = f
			degraded = append(degraded, fmt.Sprintf("flow control paused %.2f", f))
		default:
			h.FlowControlPaused = f
		}
	}
	switch {
	case len(unhealthy) > 0:
		h.State, h.Reasons = HealthUnhealthy, append(unhealthy, degraded...)
	case len(degraded) > 0:
		h.State, h.Reasons = HealthDegraded, degraded
	default:
		h.State = HealthHealthy
	}
	return h
}
