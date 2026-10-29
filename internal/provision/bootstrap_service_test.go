// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type svcFixture struct {
	t   *testing.T
	sim *simHost
	svc *BootstrapService
	mgr *simManager
	now time.Time
}

func newSvcFixture(t *testing.T) *svcFixture {
	t.Helper()
	f := &svcFixture{t: t, sim: newSimHost("debian", "12.5"), now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	deps, _, mgr := newDeps()
	deps.Runner = f.sim
	f.mgr = mgr
	f.svc = NewBootstrapService(deps, bootEnv(), f.sim, simAgents{})
	f.svc.Spawn = func(fn func()) { fn() }
	f.svc.Term = func() uint64 { return 1 }
	f.svc.Now = func() time.Time { return f.now }
	return f
}

func (f *svcFixture) draft(p BootstrapProfile) BootstrapView {
	f.t.Helper()
	v, err := f.svc.Draft(context.Background(), DraftInput{
		Request:    BootstrapRequest{ID: "boot1", Host: bootHost, Profile: p},
		Credential: Credential{PrivateKey: f.sim.rootKey},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func TestWrongFingerprintIsRefusedBeforeAnyCommand(t *testing.T) {
	f := newSvcFixture(t)
	v := f.draft(fullProfile())
	if v.Status != StatusAwaitingFingerprint || v.HostKey == nil || v.HostKey.Fingerprint != f.sim.keyFP || v.Plan != nil {
		t.Fatalf("%+v", v)
	}
	if len(f.sim.calls) != 0 {
		t.Fatal("the host was contacted before the operator confirmed the fingerprint")
	}
	_, err := f.svc.ConfirmFingerprint(context.Background(), "boot1", "SHA256:SOMETHINGELSE")
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("err = %v", err)
	}
	if len(f.sim.calls) != 0 || f.sim.hostKeyRejects != 0 {
		t.Fatal("credentials were used after a wrong fingerprint")
	}
	// The draft and its credentials are gone; nothing can continue.
	if _, err := f.svc.ConfirmFingerprint(context.Background(), "boot1", f.sim.keyFP); !errors.Is(err, ErrBootstrapNotFound) && !errors.Is(err, ErrBootstrapState) {
		t.Fatalf("draft survived: %v", err)
	}
}

func TestHostKeyChangeAfterConfirmationStopsTheRun(t *testing.T) {
	f := newSvcFixture(t)
	f.draft(fullProfile())
	if _, err := f.svc.ConfirmFingerprint(context.Background(), "boot1", f.sim.keyFP); err != nil {
		t.Fatal(err)
	}
	plan, _ := f.svc.Preview("boot1")
	if _, err := f.svc.Approve("boot1", plan.Digest); err != nil {
		t.Fatal(err)
	}
	f.sim.keyFP = "SHA256:ROTATEDKEYAFTERCONFIRMATION000000000000000" // man in the middle
	before := f.sim.mutatingCalls()
	v, err := f.svc.Run(context.Background(), "boot1")
	if err == nil && v.Status == StatusCompleted {
		t.Fatal("run must fail")
	}
	if f.sim.mutatingCalls() != before || f.sim.hostKeyRejects == 0 {
		t.Fatal("a command reached a host with a changed key")
	}
	if f.svc.Status("boot1").Status != StatusPaused {
		t.Fatalf("%+v", f.svc.Status("boot1"))
	}
}

func TestServiceFlowWithSeparateLockdownApproval(t *testing.T) {
	f := newSvcFixture(t)
	ctx := context.Background()
	f.draft(fullProfile())
	v, err := f.svc.ConfirmFingerprint(ctx, "boot1", f.sim.keyFP)
	if err != nil || v.Status != StatusPlanned || v.Plan == nil {
		t.Fatalf("%v %+v", err, v)
	}
	plan, err := f.svc.Preview("boot1")
	if err != nil || plan.Step("runtime-docker").Script == "" {
		t.Fatal("preview must carry the scripts")
	}
	// Not runnable before approval.
	if _, err := f.svc.Run(ctx, "boot1"); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("%v", err)
	}
	if _, err := f.svc.Approve("boot1", strings.Repeat("0", 64)); !errors.Is(err, ErrDigest) {
		t.Fatalf("%v", err)
	}
	// The lockdown digest cannot approve the main plan and vice versa.
	if _, err := f.svc.Approve("boot1", plan.LockdownDigest); !errors.Is(err, ErrDigest) {
		t.Fatalf("%v", err)
	}
	if _, err := f.svc.Approve("boot1", plan.Digest); err != nil {
		t.Fatal(err)
	}
	v, err = f.svc.Run(ctx, "boot1")
	if err != nil || v.Status != StatusMainComplete {
		t.Fatalf("%v %+v", err, v)
	}
	if f.sim.rootDisabled || f.sim.dedPub != "" || !f.sim.rootLogin {
		t.Fatal("lockdown ran without its own approval")
	}
	// Running again without lockdown approval does not lock down.
	if _, err := f.svc.Run(ctx, "boot1"); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("%v", err)
	}
	if _, err := f.svc.ApproveLockdown("boot1", plan.Digest); !errors.Is(err, ErrDigest) {
		t.Fatalf("%v", err)
	}
	if _, err := f.svc.ApproveLockdown("boot1", plan.LockdownDigest); err != nil {
		t.Fatal(err)
	}
	v, err = f.svc.Run(ctx, "boot1")
	if err != nil || v.Status != StatusCompleted {
		t.Fatalf("%v %+v", err, v)
	}
	if !f.sim.rootDisabled || len(f.mgr.stored) != 1 {
		t.Fatal("lockdown did not complete")
	}
	// Everything the view returns is free of secrets, and the credentials are wiped.
	b, _ := json.Marshal(v)
	for _, secret := range []string{f.sim.rootKey, "PRIVATE KEY", "enroll-token"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("view contains %q", secret)
		}
	}
	if f.svc.hasSecrets("boot1") {
		t.Fatal("credentials were not wiped after the run")
	}
	if len(v.Progress) != len(plan.Steps) {
		t.Fatalf("progress %d steps %d", len(v.Progress), len(plan.Steps))
	}
}

func TestCredentialsExpireAndMustBeSuppliedAgain(t *testing.T) {
	f := newSvcFixture(t)
	ctx := context.Background()
	p := fullProfile()
	p.DedicatedKey, p.DisableRootLogin, p.NativeAgent, p.ContainerAgent, p.Patch, p.Restic, p.ResticPaths = false, false, false, false, false, false, nil
	f.draft(p)
	if _, err := f.svc.ConfirmFingerprint(ctx, "boot1", f.sim.keyFP); err != nil {
		t.Fatal(err)
	}
	plan, _ := f.svc.Preview("boot1")
	if _, err := f.svc.Approve("boot1", plan.Digest); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Hour)
	if _, err := f.svc.Run(ctx, "boot1"); !errors.Is(err, ErrCredentialsExpired) {
		t.Fatalf("%v", err)
	}
	if f.sim.mutatingCalls() != 0 {
		t.Fatal("expired credentials were used")
	}
	if _, err := f.svc.SupplyCredentials("boot1", Credential{PrivateKey: f.sim.rootKey}); err != nil {
		t.Fatal(err)
	}
	v, err := f.svc.Run(ctx, "boot1")
	if err != nil || v.Status != StatusCompleted {
		t.Fatalf("%v %+v", err, v)
	}
	if f.svc.hasSecrets("boot1") {
		t.Fatal("credentials not wiped")
	}
}

func TestCancelWipesAndDuplicatesAreRefused(t *testing.T) {
	f := newSvcFixture(t)
	f.draft(fullProfile())
	if _, err := f.svc.Draft(context.Background(), DraftInput{Request: BootstrapRequest{ID: "boot1", Host: bootHost, Profile: fullProfile()}, Credential: Credential{PrivateKey: "x"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
	if _, err := f.svc.Cancel("boot1"); err != nil {
		t.Fatal(err)
	}
	if f.svc.hasSecrets("boot1") || f.svc.Status("boot1").Status != StatusCancelled {
		t.Fatal("cancel must wipe the credentials")
	}
	if _, err := f.svc.Draft(context.Background(), DraftInput{Request: BootstrapRequest{ID: "bad id", Host: bootHost, Profile: fullProfile()}, Credential: Credential{PrivateKey: "x"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
	if _, err := f.svc.Draft(context.Background(), DraftInput{Request: BootstrapRequest{ID: "nocred", Host: bootHost, Profile: fullProfile()}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a draft needs a credential: %v", err)
	}
}

func TestBlockedPlanCannotBeApproved(t *testing.T) {
	f := newSvcFixture(t)
	f.sim.busy = []string{"2377"}
	f.draft(fullProfile())
	v, err := f.svc.ConfirmFingerprint(context.Background(), "boot1", f.sim.keyFP)
	if err != nil || v.Status != StatusBlocked || v.Plan == nil || !v.Plan.Blocked {
		t.Fatalf("%v %+v", err, v)
	}
	if _, err := f.svc.Approve("boot1", v.Plan.Digest); !errors.Is(err, ErrBootstrapBlocked) {
		t.Fatalf("%v", err)
	}
}
