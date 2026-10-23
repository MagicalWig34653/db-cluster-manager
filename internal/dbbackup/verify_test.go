// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"testing"
	"time"
)

func TestEvaluateInstance(t *testing.T) {
	m := Manifest{BackupID: "b1", ClusterID: "c1"}
	now := time.Date(2026, 10, 23, 1, 0, 0, 0, time.FixedZone("x", 3600))
	ok := InstanceProbe{StreamVerified: true, Answered: true, Status: map[string]string{"wsrep_ready": "OFF", "wsrep_cluster_status": "Disconnected"}}
	r := EvaluateInstance(m, "s1", ok, now)
	if !r.Passed || r.Level != VerifyLevelInstance || r.ClusterID != "c1" || r.SessionID != "s1" || r.At.Location() != time.UTC || len(r.Checks) != 3 {
		t.Fatalf("%+v", r)
	}
	for name, p := range map[string]InstanceProbe{
		"stream not verified": {Answered: true, Status: ok.Status},
		"no answer":           {StreamVerified: true, Detail: "connection refused"},
		"joined a component":  {StreamVerified: true, Answered: true, Status: map[string]string{"wsrep_cluster_status": "Primary"}},
		"wsrep ready":         {StreamVerified: true, Answered: true, Status: map[string]string{"wsrep_ready": "ON"}},
	} {
		if r := EvaluateInstance(m, "s1", p, now); r.Passed {
			t.Errorf("%s: passed %+v", name, r)
		}
	}
	if r := EvaluateVerification(m, "s1", Observation{}, now); r.Level != VerifyLevelFull {
		t.Fatalf("full level %q", r.Level)
	}
}
