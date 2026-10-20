// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"testing"
)

// syncedVars is the status of a healthy node as recorded in E1 step 5.
func syncedVars() map[string]string {
	return map[string]string{
		"wsrep_cluster_size":         "3",
		"wsrep_cluster_status":       "Primary",
		"wsrep_local_state_comment":  "Synced",
		"wsrep_ready":                "ON",
		"wsrep_connected":            "ON",
		"wsrep_cluster_state_uuid":   "9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e",
		"wsrep_last_committed":       "11",
		"wsrep_flow_control_paused":  "0.000000",
		"wsrep_local_recv_queue_avg": "0.000000",
	}
}

func TestHealthFromStatusSynced(t *testing.T) {
	h := HealthFromStatus(syncedVars())
	if h.State != HealthHealthy || !h.Healthy() || !h.InPrimary() || !h.DonorCapable() {
		t.Fatalf("health = %+v", h)
	}
	if h.ClusterSize != 3 || h.StateUUID != "9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e" || h.LastCommitted != 11 {
		t.Errorf("parsed fields = %+v", h)
	}
}

func TestHealthFromStatusCases(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(map[string]string)
		want      HealthState
		inPrimary bool
	}{
		{"nil map", nil, HealthUnknown, false},
		{"missing status", func(v map[string]string) { delete(v, "wsrep_cluster_status") }, HealthUnknown, false},
		{"missing ready", func(v map[string]string) { delete(v, "wsrep_ready") }, HealthUnknown, false},
		{"missing size", func(v map[string]string) { delete(v, "wsrep_cluster_size") }, HealthUnknown, false},
		{"garbage size", func(v map[string]string) { v["wsrep_cluster_size"] = "three" }, HealthUnknown, false},
		{"garbage ready", func(v map[string]string) { v["wsrep_ready"] = "maybe" }, HealthUnknown, false},
		{"empty state", func(v map[string]string) { v["wsrep_local_state_comment"] = "" }, HealthUnknown, false},
		{"non-primary (E2 minority)", func(v map[string]string) {
			v["wsrep_cluster_status"] = "non-Primary"
			v["wsrep_cluster_size"] = "1"
			v["wsrep_local_state_comment"] = "Initialized"
			v["wsrep_ready"] = "OFF"
		}, HealthUnhealthy, false},
		{"disconnected", func(v map[string]string) { v["wsrep_connected"] = "OFF" }, HealthUnhealthy, false},
		{"not ready", func(v map[string]string) { v["wsrep_ready"] = "OFF" }, HealthUnhealthy, false},
		{"joining", func(v map[string]string) { v["wsrep_local_state_comment"] = "Joining: receiving State Transfer" }, HealthUnhealthy, false},
		{"joined", func(v map[string]string) { v["wsrep_local_state_comment"] = "Joined" }, HealthDegraded, true},
		{"donor", func(v map[string]string) { v["wsrep_local_state_comment"] = "Donor/Desynced" }, HealthDegraded, true},
		{"flow control", func(v map[string]string) { v["wsrep_flow_control_paused"] = "0.35" }, HealthDegraded, true},
		{"flow control garbage", func(v map[string]string) { v["wsrep_flow_control_paused"] = "x" }, HealthDegraded, true},
		{"size zero", func(v map[string]string) { v["wsrep_cluster_size"] = "0" }, HealthUnhealthy, false},
		{"lowercase on", func(v map[string]string) { v["wsrep_ready"] = "on"; v["wsrep_cluster_status"] = "primary" }, HealthUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v map[string]string
			if tc.mutate != nil {
				v = syncedVars()
				tc.mutate(v)
			}
			h := HealthFromStatus(v)
			if h.State != tc.want {
				t.Errorf("state = %s, want %s (reasons %v)", h.State, tc.want, h.Reasons)
			}
			if h.InPrimary() != tc.inPrimary {
				t.Errorf("InPrimary = %v, want %v", h.InPrimary(), tc.inPrimary)
			}
			if tc.want != HealthHealthy && (h.Healthy() || h.DonorCapable()) {
				t.Error("non-healthy node reported healthy or donor-capable")
			}
			if tc.want != HealthHealthy && len(h.Reasons) == 0 {
				t.Error("no reason given")
			}
		})
	}
}

func TestZeroHealthIsUnknown(t *testing.T) {
	var h NodeHealth
	if h.State != HealthUnknown || h.Healthy() || h.InPrimary() || h.DonorCapable() {
		t.Fatalf("zero NodeHealth = %+v", h)
	}
}

func TestParseStatusOutput(t *testing.T) {
	// `mariadb -N -B` output of SHOW GLOBAL STATUS (tab separated).
	out := "wsrep_cluster_size\t3\nwsrep_cluster_status\tPrimary\n\nwsrep_local_state_comment\tSynced\nwsrep_ready\tON\nwsrep_connected\tON\nwsrep_incoming_addresses\t10.0.1.3:0,10.0.1.5:0\n"
	v := ParseStatusOutput(out)
	if v["wsrep_cluster_size"] != "3" || v["wsrep_local_state_comment"] != "Synced" || v["wsrep_incoming_addresses"] != "10.0.1.3:0,10.0.1.5:0" {
		t.Fatalf("vars = %v", v)
	}
	if HealthFromStatus(v).State != HealthHealthy {
		t.Error("parsed output not healthy")
	}
}
