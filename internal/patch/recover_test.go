// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"strings"
	"testing"
)

// A plan left in "running" by a previous leader (failover or restart during
// the run) is marked failed with an unknown outcome instead of staying
// running forever; nothing is re-sent to the host.
func TestRecoverInterruptedPlans(t *testing.T) {
	f := newFixture(t)
	f.setup(t, approval)
	ctx := context.Background()
	p, err := f.svc.Propose(ctx, "alice", "h1", ProposeInput{Packages: []string{"openssl"}})
	if err != nil {
		t.Fatal(err)
	}
	if p, err = f.svc.Approve(ctx, "alice", p.ID, p.Digest); err != nil {
		t.Fatal(err)
	}
	p.State, p.OpID = PlanRunning, "op-gone"
	if err := f.svc.putAny(p); err != nil {
		t.Fatal(err)
	}
	cmds := len(f.host.cmds)
	f.events = nil
	n, err := f.svc.RecoverInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	got, _ := f.svc.Plan(p.ID)
	if got.State != PlanFailed || !strings.Contains(got.Error, "unknown") || got.FinishedAt.IsZero() {
		t.Fatalf("%+v", got)
	}
	if len(f.host.cmds) != cmds {
		t.Fatal("recovery talked to the host")
	}
	if k := f.kinds(); len(k) != 1 || k[0] != "patch.run_failed" {
		t.Fatalf("events %v", k)
	}
	h, _ := f.svc.History("h1")
	if len(h) == 0 || !strings.HasPrefix(h[0].Result, "failed") {
		t.Fatalf("history %+v", h)
	}
	if n, err := f.svc.RecoverInterrupted(ctx); err != nil || n != 0 {
		t.Fatal("second run changed something:", n, err)
	}
	// Other states stay untouched.
	if _, err := f.svc.Apply(ctx, p.ID); err == nil {
		t.Fatal("a recovered (failed) plan must not be applied again")
	}
}
