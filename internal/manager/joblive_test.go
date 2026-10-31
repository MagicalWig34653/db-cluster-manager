// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// A backup or restore job that died without a result must fail the result
// gate with a clear cause instead of being polled until the retry budget is
// spent (lab E14/E16: a dead job was invisible to its operation).
func TestResultGateFailsWhenJobIsDead(t *testing.T) {
	l, _, _, conn := updateFixture(t)
	args, _ := json.Marshal(agent.RunBackupArgs{JobName: "dbcm-job-b1", Object: "obj-b1"})
	rec := OperationRecord{ID: "op-b1", Kind: dbbackup.OpBackup, Swarm: "sw1", Cluster: "clu-1",
		Definition: runner.Operation{ID: "op-b1", Steps: []runner.Step{{ID: "01-run-backup", Command: agent.KindRunBackup, Args: args}}}}
	if _, err := Put(l.Store(), CollOperations, rec.ID, rec, nil); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	rc := &RoutingChecker{Store: l.Store(), Backup: &dbbackup.Checker{Backend: backup.NewMemBackend()}, Now: clock.Now, JobIdleGrace: time.Minute,
		Ops: &Operations{Svc: l, Swarms: &Swarms{Dial: func(string) (swarm.Connector, error) { return conn, nil }}}}
	check := func() (bool, string, error) {
		return rc.Check(context.Background(), rec.ID, runner.Step{}, runner.Condition{Kind: dbbackup.CondBackupComplete, Target: "obj-b1"})
	}
	job := func(running uint64) agent.Object {
		return agent.Object{Kind: agent.KindService, ID: "sw-job", Name: "dbcm-job-b1", Labels: map[string]string{agent.LabelJob: agent.JobBackup}, Replicas: 1, RunningTasks: &running}
	}

	// Running job: just unmet.
	conn.set(job(1))
	if ok, why, err := check(); ok || err != nil || !strings.Contains(why, "manifest") {
		t.Fatalf("running: %v %q %v", ok, why, err)
	}
	// No task yet (image pull): unmet within the grace period.
	conn.set(job(0))
	if ok, _, err := check(); ok || err != nil {
		t.Fatalf("starting: %v %v", ok, err)
	}
	clock.Set(clock.Now().Add(30 * time.Second))
	if ok, _, err := check(); ok || err != nil {
		t.Fatalf("within grace: %v %v", ok, err)
	}
	// Idle longer than the grace without a result: fatal, cause named.
	clock.Set(clock.Now().Add(2 * time.Minute))
	ok, why, err := check()
	if ok || !errors.Is(err, runner.ErrConditionFatal) || !strings.Contains(why, "no running task") || !strings.Contains(why, "dbcm-job-b1") {
		t.Fatalf("idle: %v %q %v", ok, why, err)
	}
	// Service gone: fatal at once.
	conn.set()
	ok, why, err = check()
	if ok || !errors.Is(err, runner.ErrConditionFatal) || !strings.Contains(why, "no longer exists") {
		t.Fatalf("absent: %v %q %v", ok, why, err)
	}
}
