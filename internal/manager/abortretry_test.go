// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// abortRetryEnv is a one-manager cluster whose exec-sql commands never
// succeed and whose runner backs off for an hour after the first retries,
// as in production after a few attempts (30 s). Without an abort that
// interrupts the backoff, the tests below time out.
func abortRetryEnv(t *testing.T, answer func(target string, cmd agent.Command) (agent.Result, bool),
	block func(ctx context.Context, cmd agent.Command) error) (*Service, *Operations) {
	t.Helper()
	agents := newFakeAgents()
	agents.answer, agents.block = answer, block
	c := newCluster(t, 1, func(_ string, cfg *Config) {
		cfg.Agents, cfg.Checker = agents, okChecker{}
		cfg.RunnerTune = func(rc *runner.Config) {
			rc.BackoffBase, rc.BackoffMax, rc.StepTimeout = time.Hour, time.Hour, time.Hour
		}
	})
	l := c.leader()
	ops := seed(t, l, 3)
	ops.MaxRetries = 30
	if _, _, err := Update(l.Store(), CollSettings, SettingsKey, func(s Settings, _ bool) (Settings, error) {
		s.Accounts = galera.Accounts{MonitorSecret: "dbcm-mon"}
		return s, nil
	}); err != nil {
		t.Fatal(err)
	}
	return l, ops
}

func execSQLStep(t *testing.T, l *Service, opID string) (runner.StepView, opjournal.Step) {
	t.Helper()
	rec := opRecord(t, l, opID)
	for _, s := range rec.View.Steps {
		if s.Command == agent.KindExecSQL {
			return s, opjournal.Step{ID: s.ID, State: s.State, Attempts: s.Attempts}
		}
	}
	t.Fatalf("no exec-sql step in %s", opID)
	return runner.StepView{}, opjournal.Step{}
}

func progressOf(t *testing.T, l *Service, opID string) *api.OperationProgress {
	t.Helper()
	return apiOperation(t, l, opID).Progress
}

// apiOperation returns the operation as the API lists it: lab E4 (A3)
// asserted the API state, which differed from the runner status that the
// fakes tests checked.
func apiOperation(t *testing.T, l *Service, opID string) api.Operation {
	t.Helper()
	inv := Inventory{Store: l.Store()}
	list, err := inv.Operations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range list {
		if o.ID == opID {
			return o
		}
	}
	t.Fatalf("operation %s not listed", opID)
	return api.Operation{}
}

func assertAPIAborted(t *testing.T, l *Service, opID string) api.Operation {
	t.Helper()
	o := apiOperation(t, l, opID)
	if o.State != api.OperationAborted || o.EndedAt.IsZero() || !strings.HasPrefix(o.Reason, "aborted at a safe boundary") {
		t.Fatalf("API view of the aborted operation: state %s, ended %v, reason %q", o.State, o.EndedAt, o.Reason)
	}
	return o
}

// TestAbortCreateRetryingExecSQLThenDelete reproduces lab E4 failcreate:
// abort ends the create promptly although its exec-sql step is in its
// retry backoff, the progress shows the agent's error, and the cluster's
// deletion is then accepted instead of busy.
func TestAbortCreateRetryingExecSQLThenDelete(t *testing.T) {
	notRunning := func(_ string, cmd agent.Command) (agent.Result, bool) {
		if cmd.Kind != agent.KindExecSQL {
			return agent.Result{}, false
		}
		return agent.Result{Status: agent.StatusFailed, Code: agent.CodeDBNotRunning,
			Message: "database socket /srv/dbcm/v/run/mysqld.sock does not exist: the database of this node is not running"}, true
	}
	l, ops := abortRetryEnv(t, notRunning, nil)
	create, err := ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exec-sql retry wait in the progress", func() bool {
		p := progressOf(t, l, create.ID)
		return p != nil && p.Command == string(agent.KindExecSQL) && strings.Contains(p.Waiting, "retry")
	})
	p := progressOf(t, l, create.ID)
	if !strings.Contains(p.LastError, "db-not-running") || !strings.Contains(p.LastError, "mysqld.sock does not exist") ||
		!strings.Contains(p.Waiting, "dependency not running") {
		t.Fatalf("progress %+v", p)
	}
	if _, err := ops.DeleteCluster(adminSubject, create.Cluster); err == nil || !strings.Contains(err.Error(), "abort it first") {
		t.Fatalf("delete while the create runs: %v", err)
	}
	start := time.Now()
	if err := ops.Abort(adminSubject, create.ID); err != nil {
		t.Fatal(err)
	}
	rec := waitOpStatus(t, l, create.ID, runner.StatusAborted)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("abort took %s", d)
	}
	if rec.EndedAt.IsZero() {
		t.Error("aborted create not marked ended")
	}
	assertAPIAborted(t, l, create.ID)
	if sv, _ := execSQLStep(t, l, create.ID); sv.State != opjournal.Failed {
		t.Errorf("exec-sql step %s; the observed failure must stay recorded", sv.State)
	}
	d, err := ops.DeleteCluster(adminSubject, create.Cluster)
	if err != nil {
		t.Fatalf("delete after the abort: %v", err)
	}
	// The deletion gets the cluster lock and runs (okChecker accepts its
	// approval condition); afterwards the name is free again.
	waitOpStatus(t, l, d.Operation.ID, runner.StatusSucceeded)
	waitFor(t, "cluster record deleted", func() bool {
		_, ok, _ := Get[ClusterRecord](l.Store(), CollClusters, create.Cluster)
		return !ok
	})
}

// TestAbortCreateWithExecSQLInFlight: the abort cancels an exec-sql command
// in flight; the step is recorded Unknown, never Succeeded.
func TestAbortCreateWithExecSQLInFlight(t *testing.T) {
	var entered atomic.Bool
	block := func(ctx context.Context, cmd agent.Command) error {
		if cmd.Kind != agent.KindExecSQL {
			return nil
		}
		entered.Store(true)
		<-ctx.Done()
		return ctx.Err()
	}
	l, ops := abortRetryEnv(t, nil, block)
	create, err := ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exec-sql in flight", entered.Load)
	if err := ops.Abort(adminSubject, create.ID); err != nil {
		t.Fatal(err)
	}
	rec := waitOpStatus(t, l, create.ID, runner.StatusAborted)
	if sv, _ := execSQLStep(t, l, create.ID); sv.State != opjournal.Unknown {
		t.Errorf("in-flight exec-sql step %s", sv.State)
	}
	if !strings.Contains(rec.View.Reason, "outcome unknown") {
		t.Errorf("reason %q", rec.View.Reason)
	}
	if o := assertAPIAborted(t, l, create.ID); !strings.Contains(o.Reason, "outcome unknown") {
		t.Errorf("API reason %q", o.Reason)
	}
	if _, err := ops.DeleteCluster(adminSubject, create.Cluster); err != nil {
		t.Fatalf("delete after the abort: %v", err)
	}
}
