// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// startApprovedInspection starts an inspection of b1 on h4, reports the
// restore job as succeeded with verified stream digests and approves it.
func startApprovedInspection(t *testing.T, env *backupEnv) (api.InspectionStarted, InspectionRecord) {
	t.Helper()
	rc := env.svc.cfg.Checker.(*RoutingChecker)
	rc.Backup = &dbbackup.Checker{Backend: env.be}
	env.completedBackup(t, "b1", env.clock.Now().Add(-time.Hour))
	ctx := context.Background()
	res, err := env.b.StartInspection(ctx, adminSubject, api.InspectionInput{Cluster: "clu-1", Host: "h4", TTLMinutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	ir, _, err := Get[InspectionRecord](env.svc.Store(), CollInspections, res.Session)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := json.Marshal(dbbackup.JobStatus{Job: ir.Value.Session.JobName, BackupID: "b1", Succeeded: true, StreamVerified: true})
	if err := env.be.Put(ctx, dbbackup.StatusName(ir.Value.Session.JobName), bytes.NewReader(st)); err != nil {
		t.Fatal(err)
	}
	ctl := &Control{Ops: env.ops, Swarms: env.b.Swarms}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, res.ID, res.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	return res, ir.Value
}

// healthAnswer answers collect-health with the given result sequence (the
// last one repeats) and records the probed socket.
type healthAnswer struct {
	mu      sync.Mutex
	results []agent.Result
	probes  []string
}

func (h *healthAnswer) answer(target string, cmd agent.Command) (agent.Result, bool) {
	if cmd.Kind != agent.KindCollectHealth {
		return agent.Result{}, false
	}
	var a agent.CollectHealthArgs
	_ = json.Unmarshal(cmd.Args, &a)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probes = append(h.probes, target+":"+a.Socket)
	r := h.results[0]
	if len(h.results) > 1 {
		h.results = h.results[1:]
	}
	return r, true
}

func healthOK(status map[string]string) agent.Result {
	out, _ := json.Marshal(agent.HealthOutput{Status: status})
	return agent.Result{Status: agent.StatusSucceeded, Output: out}
}

// TestInspectionVerificationGateRecordsReport: lab E4 stopped at the
// restore-verified gate because nothing verified the instance. The gate
// now probes the instance through the agent on its host (socket on the
// inspection volume), polls while it does not answer, records an
// instance-level report and lets the operation succeed.
func TestInspectionVerificationGateRecordsReport(t *testing.T) {
	env := newBackupEnv(t, true)
	env.ops.MaxRetries = 50
	ha := &healthAnswer{results: []agent.Result{
		{Status: agent.StatusFailed, Code: agent.CodeDBUnavailable, Message: "dial unix: no such file"},
		healthOK(map[string]string{"wsrep_ready": "OFF", "wsrep_cluster_status": "Disconnected"}),
	}}
	env.agents.answer = ha.answer
	res, ir := startApprovedInspection(t, env)
	waitOpStatus(t, env.svc, res.ID, runner.StatusSucceeded)

	rep, ok, err := Get[dbbackup.VerifyReport](env.svc.Store(), CollVerifications, res.Session)
	if err != nil || !ok || !rep.Value.Passed || rep.Value.Level != dbbackup.VerifyLevelInstance || rep.Value.ClusterID != "clu-1" || rep.Value.BackupID != "b1" {
		t.Fatalf("report %+v %v %v", rep.Value, ok, err)
	}
	ha.mu.Lock()
	probes := append([]string(nil), ha.probes...)
	ha.mu.Unlock()
	want := "h4:/srv/dbcm/" + ir.Session.Volume + "/run/mysqld.sock"
	if len(probes) < 2 || probes[0] != want {
		t.Fatalf("probes %v, want %s (one unanswered, then answered)", probes, want)
	}
	sums, _ := env.b.Backups(context.Background())
	if len(sums) != 1 || sums[0].LastVerifiedRestoreAt.IsZero() {
		t.Fatalf("summary %+v", sums)
	}
	// The init file leaves the monitoring account unlocked for the probe.
	init := string(env.conn.secrets[ir.InitSecret])
	if !strings.Contains(init, "(User = 'dbcm_monitor' AND Host = 'localhost')") {
		t.Fatalf("init SQL does not keep the monitoring account:\n%s", init)
	}
}

func TestInspectionVerificationFailsForAClusteredInstance(t *testing.T) {
	env := newBackupEnv(t, true)
	env.ops.MaxRetries = 3
	ha := &healthAnswer{results: []agent.Result{healthOK(map[string]string{"wsrep_ready": "ON", "wsrep_cluster_status": "Primary"})}}
	env.agents.answer = ha.answer
	res, _ := startApprovedInspection(t, env)
	rec := waitOpStatus(t, env.svc, res.ID, runner.StatusPaused)
	if !strings.Contains(rec.View.Reason, "verification failed") {
		t.Fatalf("reason %q", rec.View.Reason)
	}
	rep, ok, _ := Get[dbbackup.VerifyReport](env.svc.Store(), CollVerifications, res.Session)
	if !ok || rep.Value.Passed {
		t.Fatalf("report %+v", rep.Value)
	}
	if sums, _ := env.b.Backups(context.Background()); !sums[0].LastVerifiedRestoreAt.IsZero() {
		t.Fatal("failed verification counted as verified restore")
	}
}
