// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// TestAbortNeverStartedOperation: an operation whose start was refused
// (runner busy, not leader) is stored as stopped without steps and shown as
// unknown. The runner never registered it, so the abort used to answer 404;
// it now ends the record as aborted (lab E13 observation).
func TestAbortNeverStartedOperation(t *testing.T) {
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Checker = okChecker{} })
	l := c.leader()
	ops := seed(t, l, 3)
	v := runner.View{ID: "op-never", Kind: "galera/rolling-restart", Status: runner.StatusStopped, Term: 1, Reason: "not started"}
	rec := OperationRecord{ID: "op-never", Kind: v.Kind, Swarm: "sw1", Cluster: "clu-1", View: &v}
	if _, err := Put(l.Store(), CollOperations, "op-never", rec, IfVersion(0)); err != nil {
		t.Fatal(err)
	}
	if err := ops.Abort(adminSubject, "op-never"); err != nil {
		t.Fatalf("abort of a never-started operation: %v", err)
	}
	got := waitOpStatus(t, l, "op-never", runner.StatusAborted)
	if got.EndedAt.IsZero() {
		t.Fatal("aborted operation without end time")
	}
	if err := ops.Abort(adminSubject, "op-never"); !errors.Is(err, ErrOperationEnded) {
		t.Fatalf("second abort: %v", err)
	}
	if err := ops.Abort(adminSubject, "op-missing"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("abort of a missing operation: %v", err)
	}
}
