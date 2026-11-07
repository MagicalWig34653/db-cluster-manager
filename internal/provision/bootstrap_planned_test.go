// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// simPlanned is a PlannedEnrollment double.
type simPlanned struct {
	mu         sync.Mutex
	issued     []string
	released   []string
	issueEmpty bool
	issueErr   error
	releaseErr error
}

func (p *simPlanned) Preissue(_ context.Context, h Host) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.issueErr != nil {
		return "", p.issueErr
	}
	if p.issueEmpty {
		return "", nil
	}
	tok := "planned-token-SECRET-" + h.ID + "-" + string(rune('a'+len(p.issued)))
	p.issued = append(p.issued, tok)
	return tok, nil
}

func (p *simPlanned) Release(_ context.Context, h Host) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.releaseErr != nil {
		return p.releaseErr
	}
	p.released = append(p.released, h.ID)
	return nil
}

func (p *simPlanned) releases() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.released)
}

func newPlannedFixture(t *testing.T) (*svcFixture, *simPlanned) {
	t.Helper()
	f := newDurableFixture(t)
	pl := &simPlanned{}
	f.svc.deps.Planned = pl
	return f, pl
}

func (f *svcFixture) nativeTokenOnStdin() string {
	for _, c := range f.sim.calls {
		if c.Step == "agent-native" && c.Kind == "run" {
			return strings.TrimSpace(c.Stdin)
		}
	}
	return ""
}

func TestPlannedTokenReachesTheNativeAgentStepOnStdinOnly(t *testing.T) {
	f, pl := newPlannedFixture(t)
	f.approved()
	v, err := f.svc.Run(context.Background(), "boot1")
	if err != nil || v.Status != StatusMainComplete {
		t.Fatalf("%v %+v", err, v)
	}
	if len(pl.issued) != 1 || f.nativeTokenOnStdin() != pl.issued[0] {
		t.Fatalf("issued %v, stdin %q", pl.issued, f.nativeTokenOnStdin())
	}
	// The token is in no command line, view, plan, draft document or stored secret.
	secret := pl.issued[0]
	for _, c := range f.sim.calls {
		if strings.Contains(c.Cmd, secret) {
			t.Fatalf("token in the command of step %s", c.Step)
		}
	}
	view, _ := json.Marshal(f.svc.Status("boot1"))
	plan, _ := f.svc.Preview("boot1")
	planJSON, _ := json.Marshal(plan)
	stored, _, _ := f.store.LoadSecrets("boot1")
	storedJSON, _ := json.Marshal(stored)
	for name, b := range map[string][]byte{"view": view, "plan": planJSON, "draft": f.store.draftBytes(), "secrets": storedJSON} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("token in the %s", name)
		}
	}
}

func TestPlannedTokenIsIssuedAfterApprovalNotWhilePlanning(t *testing.T) {
	f, pl := newPlannedFixture(t)
	f.approved()
	if len(pl.issued) != 0 {
		t.Fatal("a token was issued before the run")
	}
}

func TestPreissueFailureStopsBeforeTheHostIsTouched(t *testing.T) {
	f, pl := newPlannedFixture(t)
	f.approved()
	pl.issueErr = errors.New("metastore unavailable")
	before := f.sim.mutatingCalls()
	if _, err := f.svc.Run(context.Background(), "boot1"); err == nil {
		t.Fatal("run started without a token")
	}
	if f.sim.mutatingCalls() != before {
		t.Fatal("the host was changed although no token could be issued")
	}
	if st := f.svc.Status("boot1"); st.Status != StatusApproved || !st.CredentialsHeld {
		t.Fatalf("%+v", st)
	}
}

func TestRegisteredHostKeepsTheExistingTokenPath(t *testing.T) {
	f, pl := newPlannedFixture(t)
	pl.issueEmpty = true // the host has an enrolled record: nothing is pre-issued
	f.approved()
	if v, err := f.svc.Run(context.Background(), "boot1"); err != nil || v.Status != StatusMainComplete {
		t.Fatalf("%v %+v", err, v)
	}
	if got := f.nativeTokenOnStdin(); got != "enroll-token-SECRET-fresh1-native" {
		t.Fatalf("stdin %q", got)
	}
}

func TestNoNativeAgentMeansNoPreissue(t *testing.T) {
	f, pl := newPlannedFixture(t)
	p := fullProfile()
	p.NativeAgent, p.Patch, p.Restic, p.ResticPaths = false, false, false, nil
	f.draft(p)
	if _, err := f.svc.ConfirmFingerprint(context.Background(), "boot1", f.sim.keyFP); err != nil {
		t.Fatal(err)
	}
	plan, _ := f.svc.Preview("boot1")
	if _, err := f.svc.Approve("boot1", plan.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Run(context.Background(), "boot1"); err != nil {
		t.Fatal(err)
	}
	if len(pl.issued) != 0 {
		t.Fatalf("issued %v", pl.issued)
	}
}

func TestFailoverIssuesAFreshTokenAndPersistsNone(t *testing.T) {
	f, pl := newPlannedFixture(t)
	f.approved()
	n := f.restart(2)
	n.deps.Planned = pl
	if v, err := n.Run(context.Background(), "boot1"); err != nil || v.Status != StatusMainComplete {
		t.Fatalf("%v %+v", err, v)
	}
	if len(pl.issued) != 1 || f.nativeTokenOnStdin() != pl.issued[0] {
		t.Fatalf("issued %v, stdin %q", pl.issued, f.nativeTokenOnStdin())
	}
	stored, _, _ := f.store.LoadSecrets("boot1")
	if b, _ := json.Marshal(stored); bytes.Contains(b, []byte("planned-token")) {
		t.Fatal("token persisted")
	}
}

func TestPlannedEnrollmentIsReleasedOnCancelMismatchExpiryAndCompletion(t *testing.T) {
	ctx := context.Background()
	t.Run("cancel", func(t *testing.T) {
		f, pl := newPlannedFixture(t)
		f.approved()
		if _, err := f.svc.Cancel("boot1"); err != nil || pl.releases() != 1 {
			t.Fatalf("%v releases %d", err, pl.releases())
		}
	})
	t.Run("fingerprint mismatch", func(t *testing.T) {
		f, pl := newPlannedFixture(t)
		f.draft(fullProfile())
		_, _ = f.svc.ConfirmFingerprint(ctx, "boot1", "SHA256:OTHER")
		if pl.releases() != 1 {
			t.Fatalf("releases %d", pl.releases())
		}
	})
	t.Run("expiry", func(t *testing.T) {
		f, pl := newPlannedFixture(t)
		f.approved()
		f.now = f.now.Add(2 * CredentialTTL)
		f.svc.Sweep()
		if pl.releases() != 1 {
			t.Fatalf("releases %d", pl.releases())
		}
	})
	t.Run("completion", func(t *testing.T) {
		f, pl := newPlannedFixture(t)
		plan := f.approved()
		if _, err := f.svc.Run(ctx, "boot1"); err != nil {
			t.Fatal(err)
		}
		if pl.releases() != 0 {
			t.Fatal("released while the lockdown phase still needs the draft")
		}
		if _, err := f.svc.ApproveLockdown("boot1", plan.LockdownDigest); err != nil {
			t.Fatal(err)
		}
		if v, err := f.svc.Run(ctx, "boot1"); err != nil || v.Status != StatusCompleted {
			t.Fatalf("%v %+v", err, v)
		}
		if pl.releases() != 1 {
			t.Fatalf("releases %d", pl.releases())
		}
	})
	t.Run("failed release is retried", func(t *testing.T) {
		f, pl := newPlannedFixture(t)
		f.approved()
		pl.releaseErr = errors.New("metastore unavailable")
		if _, err := f.svc.Cancel("boot1"); err != nil {
			t.Fatal(err)
		}
		f.svc.Sweep()
		if pl.releases() != 0 {
			t.Fatal("release reported done")
		}
		pl.releaseErr = nil
		f.svc.Sweep()
		if pl.releases() != 1 {
			t.Fatalf("releases %d", pl.releases())
		}
		f.svc.Sweep()
		if pl.releases() != 1 {
			t.Fatal("release repeated after success")
		}
	})
}
