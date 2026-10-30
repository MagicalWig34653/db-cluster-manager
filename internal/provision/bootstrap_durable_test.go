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
	"time"
)

// memDraftStore is a DraftStore that keeps only serialized bytes, like the
// replicated store: a service built on it sees nothing but what was written.
type memDraftStore struct {
	mu       sync.Mutex
	drafts   map[string][]byte
	secrets  map[string][]byte
	failWipe bool
}

func newMemDraftStore() *memDraftStore {
	return &memDraftStore{drafts: map[string][]byte{}, secrets: map[string][]byte{}}
}

func (m *memDraftStore) SaveDraft(r DraftRecord) error {
	b, err := json.Marshal(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drafts[r.Request.ID] = b
	return err
}

func (m *memDraftStore) LoadDrafts() ([]DraftRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DraftRecord
	for _, b := range m.drafts {
		var r DraftRecord
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (m *memDraftStore) SaveSecrets(id string, s SecretsRecord) error {
	b, err := json.Marshal(s)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets[id] = b
	return err
}

func (m *memDraftStore) LoadSecrets(id string) (SecretsRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.secrets[id]
	if !ok {
		return SecretsRecord{}, false, nil
	}
	var s SecretsRecord
	return s, true, json.Unmarshal(b, &s)
}

func (m *memDraftStore) DeleteSecrets(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWipe {
		return errors.New("store unavailable")
	}
	delete(m.secrets, id)
	return nil
}

func (m *memDraftStore) draftBytes() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []byte
	for _, b := range m.drafts {
		all = append(all, b...)
	}
	return all
}

func (m *memDraftStore) secretCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.secrets)
}

// restart builds a second service on the same store, simulating a manager
// restart or failover to another replica.
func (f *svcFixture) restart(term uint64) *BootstrapService {
	deps, _, _ := newDeps()
	deps.Runner = f.sim
	svc := NewBootstrapService(deps, bootEnv(), f.sim, simAgents{})
	svc.Spawn = func(fn func()) { fn() }
	svc.Term = func() uint64 { return term }
	svc.Now = func() time.Time { return f.now }
	svc.Store = f.store
	if err := svc.Restore(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	return svc
}

func newDurableFixture(t *testing.T) *svcFixture {
	f := newSvcFixture(t)
	f.store = newMemDraftStore()
	f.svc.Store = f.store
	return f
}

func (f *svcFixture) approved() BootstrapPlan {
	f.t.Helper()
	ctx := context.Background()
	f.draft(fullProfile())
	if _, err := f.svc.ConfirmFingerprint(ctx, "boot1", f.sim.keyFP); err != nil {
		f.t.Fatal(err)
	}
	plan, _ := f.svc.Preview("boot1")
	if _, err := f.svc.Approve("boot1", plan.Digest); err != nil {
		f.t.Fatal(err)
	}
	return plan
}

func TestDurableDraftSurvivesRestartAndKeepsPlaintextOutOfTheDraftDocument(t *testing.T) {
	f := newDurableFixture(t)
	ctx := context.Background()
	plan := f.approved()
	if bytes.Contains(f.store.draftBytes(), []byte(f.sim.rootKey)) {
		t.Fatal("root credential is in the draft document")
	}
	n := f.restart(2)
	v := n.Status("boot1")
	if v.Status != StatusApproved || !v.MainApproved || v.Plan == nil || v.Plan.Digest != plan.Digest || !v.CredentialsHeld {
		t.Fatalf("restored view %+v", v)
	}
	if v.HostKey == nil || v.HostKey.Fingerprint != f.sim.keyFP {
		t.Fatalf("host key not restored: %+v", v.HostKey)
	}
	out, err := n.Run(ctx, "boot1")
	if err != nil || out.Status != StatusMainComplete {
		t.Fatalf("%v %+v", err, out)
	}
	// The lockdown approval survives a second restart too.
	n = f.restart(3)
	if _, err := n.ApproveLockdown("boot1", plan.LockdownDigest); err != nil {
		t.Fatal(err)
	}
	n = f.restart(4)
	out, err = n.Run(ctx, "boot1")
	if err != nil || out.Status != StatusCompleted {
		t.Fatalf("%v %+v", err, out)
	}
	if f.store.secretCount() != 0 || n.hasSecrets("boot1") {
		t.Fatal("credentials outlived the run")
	}
	if got := f.restart(5).List(); len(got) != 1 || got[0].Status != StatusCompleted || got[0].CredentialsHeld {
		t.Fatalf("%+v", got)
	}
}

func TestDurableExpiryDeletesStoredCredentials(t *testing.T) {
	f := newDurableFixture(t)
	f.draft(fullProfile())
	if f.store.secretCount() != 1 {
		t.Fatal("credentials were not stored")
	}
	f.now = f.now.Add(2 * time.Hour)
	// A restarted replica deletes expired credentials when it restores.
	n := f.restart(2)
	if f.store.secretCount() != 0 {
		t.Fatal("expired credentials stayed in the store")
	}
	if v := n.Status("boot1"); v.CredentialsHeld {
		t.Fatalf("%+v", v)
	}
	// Sweep wipes drafts that expire while the service runs.
	f2 := newDurableFixture(t)
	f2.draft(fullProfile())
	f2.now = f2.now.Add(2 * time.Hour)
	f2.svc.Sweep()
	if f2.store.secretCount() != 0 {
		t.Fatal("sweep did not delete the expired credentials")
	}
}

func TestDurableWipeRetriesWhenStoreFails(t *testing.T) {
	f := newDurableFixture(t)
	f.draft(fullProfile())
	f.store.failWipe = true
	if _, err := f.svc.Cancel("boot1"); err != nil {
		t.Fatal(err)
	}
	if f.store.secretCount() != 1 {
		t.Fatal("test setup: delete should have failed")
	}
	f.store.failWipe = false
	f.svc.Sweep()
	if f.store.secretCount() != 0 {
		t.Fatal("failed wipe was not retried")
	}
}

func TestFingerprintMismatchDeletesStoredCredentials(t *testing.T) {
	f := newDurableFixture(t)
	f.draft(fullProfile())
	if _, err := f.svc.ConfirmFingerprint(context.Background(), "boot1", "SHA256:WRONG"); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatal(err)
	}
	if f.store.secretCount() != 0 {
		t.Fatal("credentials stayed after the fingerprint mismatch")
	}
}

func TestRestoreMarksInterruptedRunUnknownThenResumes(t *testing.T) {
	f := newDurableFixture(t)
	ctx := context.Background()
	f.approved()
	// The leader dies while the run is in progress: the stored status is
	// running and nothing ever wrote the end of the run.
	f.svc.Spawn = func(func()) {}
	if v, err := f.svc.Run(ctx, "boot1"); err != nil || v.Status != StatusRunning {
		t.Fatalf("%v %+v", err, v)
	}
	n := f.restart(7)
	v := n.Status("boot1")
	if v.Status != StatusUnknown || !strings.Contains(v.Error, "unknown") || !v.CredentialsHeld {
		t.Fatalf("%+v", v)
	}
	out, err := n.Run(ctx, "boot1")
	if err != nil || out.Status != StatusMainComplete {
		t.Fatalf("%v %+v", err, out)
	}
	if f.sim.mutatingCalls() == 0 {
		t.Fatal("resume did not run")
	}
}

func TestUnknownRunCanBeCancelled(t *testing.T) {
	f := newDurableFixture(t)
	f.approved()
	f.svc.Spawn = func(func()) {}
	_, _ = f.svc.Run(context.Background(), "boot1")
	n := f.restart(7)
	if _, err := n.Cancel("boot1"); err != nil {
		t.Fatalf("%v", err)
	}
	if f.store.secretCount() != 0 || f.restart(8).Status("boot1").Status != StatusCancelled {
		t.Fatal("cancel did not persist")
	}
}

func TestOnlyTheLeaderRunsAndAStaleRunDoesNotOverwriteState(t *testing.T) {
	f := newDurableFixture(t)
	ctx := context.Background()
	f.approved()
	f.svc.Term = func() uint64 { return 0 }
	if _, err := f.svc.Run(ctx, "boot1"); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("%v", err)
	}
	if f.sim.mutatingCalls() != 0 {
		t.Fatal("a non-leader reached the host")
	}
	// Leadership changes while the executor runs: the finishing run must not
	// write the final state (the new leader owns it).
	f.svc.Term = func() uint64 { return 3 }
	var deferred func()
	f.svc.Spawn = func(fn func()) { deferred = fn }
	if _, err := f.svc.Run(ctx, "boot1"); err != nil {
		t.Fatal(err)
	}
	f.svc.Term = func() uint64 { return 4 }
	deferred()
	recs, _ := f.store.LoadDrafts()
	if recs[0].Status != StatusRunning {
		t.Fatalf("stale run wrote %q", recs[0].Status)
	}
}

func TestJoinTokenIsStoredOnlyInSecrets(t *testing.T) {
	f := newDurableFixture(t)
	_, err := f.svc.Draft(context.Background(), DraftInput{
		Request:    BootstrapRequest{ID: "boot1", Host: bootHost, Profile: fullProfile()},
		Credential: Credential{PrivateKey: f.sim.rootKey}, JoinToken: "SWMTKN-1-secretjoin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f.store.draftBytes(), []byte("SWMTKN")) {
		t.Fatal("join token in draft document")
	}
	if s, ok, _ := f.store.LoadSecrets("boot1"); !ok || s.JoinToken != "SWMTKN-1-secretjoin" {
		t.Fatalf("%+v %v", s, ok)
	}
}

func TestEnvFuncSuppliesTheEnvironmentAtPlanTimeAndFailureBlocks(t *testing.T) {
	f := newDurableFixture(t)
	f.svc.EnvFunc = func() (BootstrapEnv, error) { return BootstrapEnv{}, errors.New("installation settings missing") }
	f.draft(fullProfile())
	v, err := f.svc.ConfirmFingerprint(context.Background(), "boot1", f.sim.keyFP)
	if err == nil || v.Status != StatusBlocked || !strings.Contains(v.Error, "installation settings") {
		t.Fatalf("%v %+v", err, v)
	}
	f2 := newDurableFixture(t)
	f2.svc.EnvFunc = func() (BootstrapEnv, error) { return bootEnv(), nil }
	f2.draft(fullProfile())
	if v, err := f2.svc.ConfirmFingerprint(context.Background(), "boot1", f2.sim.keyFP); err != nil || v.Status != StatusPlanned {
		t.Fatalf("%v %+v", err, v)
	}
}
