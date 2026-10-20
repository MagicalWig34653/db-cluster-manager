// SPDX-License-Identifier: AGPL-3.0-only

package recovery

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testIterations keeps PBKDF2 cheap under -race; production uses
// DefaultCodeIterations.
const testIterations = 1_000

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func passkeyAccount(id string) Account {
	return Account{ID: id, Credentials: []Credential{{ID: id + "-pk1", Kind: CredentialPasskey}}}
}

func TestAccountModel(t *testing.T) {
	if err := passkeyAccount("alice").Validate(); err != nil {
		t.Fatalf("passkey-only account must be valid: %v", err)
	}
	bad := []Account{
		{ID: ""},
		{ID: "pw-only", Credentials: []Credential{{ID: "p", Kind: CredentialPassword}}},
		{ID: "codes-only", Credentials: []Credential{{ID: "c", Kind: CredentialRecoveryCodes}}},
		{ID: "two-pw", Credentials: []Credential{{ID: "k", Kind: CredentialPasskey}, {ID: "p1", Kind: CredentialPassword}, {ID: "p2", Kind: CredentialPassword}}},
		{ID: "dup", Credentials: []Credential{{ID: "k", Kind: CredentialPasskey}, {ID: "k", Kind: CredentialPasskey}}},
		{ID: "kind", Credentials: []Credential{{ID: "k", Kind: CredentialPasskey}, {ID: "x", Kind: "totp"}}},
	}
	for _, a := range bad {
		if err := a.Validate(); !errors.Is(err, ErrInvalidAccount) {
			t.Errorf("%q: want ErrInvalidAccount, got %v", a.ID, err)
		}
	}
	full := Account{ID: "full", Credentials: []Credential{
		{ID: "k", Kind: CredentialPasskey}, {ID: "p", Kind: CredentialPassword}, {ID: "c", Kind: CredentialRecoveryCodes},
	}}
	if err := full.Validate(); err != nil {
		t.Fatal(err)
	}

	s := NewAccounts()
	if err := s.Create(passkeyAccount("alice")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(passkeyAccount("alice")); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := s.RemoveCredential("alice", "alice-pk1"); !errors.Is(err, ErrLastPasskey) {
		t.Fatalf("remove last passkey: %v", err)
	}
	if err := s.AddCredential("alice", Credential{ID: "pw", Kind: CredentialPassword}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCredential("alice", Credential{ID: "pw2", Kind: CredentialPassword}); !errors.Is(err, ErrInvalidAccount) {
		t.Fatalf("second password: %v", err)
	}
	if err := s.BeginPasskeyReenrollment("alice"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get("alice")
	if a.HasKind(CredentialPasskey) || !a.PendingPasskeyEnrollment || a.CredentialEpoch != 2 {
		t.Fatalf("after re-enrollment start: %+v", a)
	}
	if err := s.AddCredential("alice", Credential{ID: "pk2", Kind: CredentialPasskey}); err != nil {
		t.Fatal(err)
	}
	a, _ = s.Get("alice")
	if a.PendingPasskeyEnrollment || a.CredentialEpoch != 3 {
		t.Fatalf("after enrollment: %+v", a)
	}
	// Failed mutations do not advance the epoch; returned copies do not alias.
	if err := s.RemoveCredential("alice", "missing"); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("remove missing: %v", err)
	}
	for i := range a.Credentials {
		a.Credentials[i].Kind = CredentialPassword
	}
	b, _ := s.Get("alice")
	if b.CredentialEpoch != 3 {
		t.Fatalf("failed mutation advanced epoch: %+v", b)
	}
	if !b.HasKind(CredentialPasskey) {
		t.Fatal("returned copy aliased store state")
	}
}

func TestRecoveryCodeGeneration(t *testing.T) {
	clk := newClock()
	plain, set, err := GenerateCodes(10, testIterations, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 10 || len(set.Codes) != 10 || set.Remaining() != 10 {
		t.Fatalf("counts: %d %d", len(plain), len(set.Codes))
	}
	format := regexp.MustCompile(`^[A-Z2-7]{4}-[A-Z2-7]{4}-[A-Z2-7]{4}-[A-Z2-7]{4}$`)
	seen := map[string]bool{}
	for i, c := range plain {
		if !format.MatchString(c) {
			t.Errorf("code %q has wrong format", c)
		}
		if seen[c] {
			t.Errorf("duplicate code %q", c)
		}
		seen[c] = true
		h := set.Codes[i]
		if len(h.Salt) != saltBytes || len(h.Hash) != hashBytes || h.Iterations != testIterations {
			t.Errorf("stored code %d: %+v", i, h)
		}
		if strings.Contains(string(h.Hash), NormalizeCode(c)) {
			t.Error("plaintext in stored hash")
		}
	}
	if string(set.Codes[0].Salt) == string(set.Codes[1].Salt) {
		t.Error("salts must differ per code")
	}
}

func TestRecoveryCodeSingleUseAndReplay(t *testing.T) {
	clk := newClock()
	lim := NewLimiter(DefaultLockoutPolicy, clk.Now)
	v := NewCodeVerifier(lim, clk.Now)
	plain, set, err := GenerateCodes(3, testIterations, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify("alice", plain[0]); !errors.Is(err, ErrNoCodes) {
		t.Fatalf("before enrollment: %v", err)
	}
	lim.Reset(limiterKey("alice"))
	v.Enroll("alice", set)

	// Normalization: lower case and spaces instead of dashes.
	in := strings.ToLower(strings.ReplaceAll(plain[1], "-", " "))
	if err := v.Verify("alice", in); err != nil {
		t.Fatalf("valid code: %v", err)
	}
	if err := v.Verify("alice", plain[1]); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("replay must fail: %v", err)
	}
	if v.Remaining("alice") != 2 {
		t.Fatalf("remaining %d", v.Remaining("alice"))
	}
	// A code of alice is useless for bob.
	_, bobSet, _ := GenerateCodes(1, testIterations, clk.Now())
	v.Enroll("bob", bobSet)
	if err := v.Verify("bob", plain[0]); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("cross-user code: %v", err)
	}
	if err := v.Verify("alice", "not-a-code"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("malformed: %v", err)
	}
	snap, _ := v.Snapshot("alice")
	if snap.Codes[1].UsedAt.IsZero() || !snap.Codes[0].UsedAt.IsZero() {
		t.Fatalf("snapshot usage: %+v", snap.Codes)
	}

	// Regenerating replaces every previous code.
	newPlain, newSet, _ := GenerateCodes(2, testIterations, clk.Now())
	v.Enroll("alice", newSet)
	if err := v.Verify("alice", plain[0]); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("old code after regeneration: %v", err)
	}
	if err := v.Verify("alice", newPlain[0]); err != nil {
		t.Fatalf("new code: %v", err)
	}
	v.Revoke("alice")
	if err := v.Verify("alice", newPlain[1]); !errors.Is(err, ErrNoCodes) {
		t.Fatalf("after revoke: %v", err)
	}
}

func TestRecoveryCodeConcurrentConsumption(t *testing.T) {
	clk := newClock()
	v := NewCodeVerifier(NewLimiter(LockoutPolicy{MaxFailures: 100, Window: time.Hour, BaseLockout: time.Minute}, clk.Now), clk.Now)
	plain, set, _ := GenerateCodes(2, testIterations, clk.Now())
	v.Enroll("alice", set)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if v.Verify("alice", plain[0]) == nil {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("code consumed %d times", ok.Load())
	}
}

func TestLockoutStateMachine(t *testing.T) {
	clk := newClock()
	pol := LockoutPolicy{MaxFailures: 3, Window: 10 * time.Minute, BaseLockout: 5 * time.Minute, MaxLockout: 15 * time.Minute}
	lim := NewLimiter(pol, clk.Now)
	v := NewCodeVerifier(lim, clk.Now)
	plain, set, _ := GenerateCodes(2, testIterations, clk.Now())
	v.Enroll("alice", set)
	key := limiterKey("alice")

	fail := func() error { return v.Verify("alice", "AAAA-AAAA-AAAA-AAAA") }
	for i := range 3 {
		if err := fail(); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	st := lim.Status(key)
	if st.State != StateLocked || st.Lockouts != 1 || !st.LockedUntil.Equal(clk.Now().Add(5*time.Minute)) {
		t.Fatalf("after 3 failures: %+v", st)
	}
	// While locked, even a correct code is rejected without being consumed.
	err := v.Verify("alice", plain[0])
	var le *LockedError
	if !errors.As(err, &le) || !errors.Is(err, ErrLocked) {
		t.Fatalf("locked: %v", err)
	}
	if v.Remaining("alice") != 2 {
		t.Fatal("code consumed while locked")
	}
	// Other users are unaffected.
	if lim.Status(limiterKey("bob")).State != StateOpen {
		t.Fatal("bob locked")
	}

	// Lockout expires; the next round doubles the lockout (5m -> 10m).
	clk.Advance(5 * time.Minute)
	if lim.Status(key).State != StateOpen {
		t.Fatal("lockout should have expired")
	}
	for range 3 {
		_ = fail()
	}
	if st := lim.Status(key); st.Lockouts != 2 || !st.LockedUntil.Equal(clk.Now().Add(10*time.Minute)) {
		t.Fatalf("second lockout: %+v", st)
	}
	// Third lockout is capped at MaxLockout.
	clk.Advance(10 * time.Minute)
	for range 3 {
		_ = fail()
	}
	clk.Advance(15 * time.Minute)
	for range 3 {
		_ = fail()
	}
	if st := lim.Status(key); st.Lockouts != 4 || !st.LockedUntil.Equal(clk.Now().Add(15*time.Minute)) {
		t.Fatalf("capped lockout: %+v", st)
	}

	// Administrative reset clears everything; success resets failures.
	lim.Reset(key)
	if st := lim.Status(key); st.State != StateOpen || st.Lockouts != 0 || st.Failures != 0 {
		t.Fatalf("after reset: %+v", st)
	}
	_ = fail()
	_ = fail()
	if err := v.Verify("alice", plain[0]); err != nil {
		t.Fatalf("valid code after 2 failures: %v", err)
	}
	if st := lim.Status(key); st.Failures != 0 || st.Lockouts != 0 {
		t.Fatalf("success must reset: %+v", st)
	}

	// Failures outside the window do not accumulate.
	_ = fail()
	_ = fail()
	clk.Advance(11 * time.Minute)
	_ = fail()
	if st := lim.Status(key); st.State != StateOpen || st.Failures != 1 {
		t.Fatalf("window reset: %+v", st)
	}

	// Prune drops idle unlocked entries only.
	clk.Advance(time.Hour)
	if n := lim.Prune(); n != 1 {
		t.Fatalf("prune removed %d", n)
	}
}

func TestLockoutConcurrentGuessesBounded(t *testing.T) {
	clk := newClock()
	lim := NewLimiter(LockoutPolicy{MaxFailures: 5, Window: time.Hour, BaseLockout: time.Hour}, clk.Now)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if lim.Begin("k") == nil {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if allowed.Load() != 5 {
		t.Fatalf("allowed %d concurrent attempts, want 5", allowed.Load())
	}
}

func TestURLTokens(t *testing.T) {
	clk := newClock()
	ts := NewURLTokens(clk.Now)

	tok, exp, err := ts.Issue("alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(clk.Now().Add(DefaultURLTokenTTL)) {
		t.Fatalf("expiry %v", exp)
	}
	if len(tok) < 43 || strings.ContainsAny(tok, "+/=") {
		t.Fatalf("token not URL-safe or too short: %q", tok)
	}
	if _, ok := ts.tokens[digest(tok)]; !ok || len(ts.tokens) != 1 {
		t.Fatal("digest not stored")
	}

	// Success, then replay.
	if err := ts.Redeem("alice", tok); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if err := ts.Redeem("alice", tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("replay: %v", err)
	}

	// Expiry: valid just before, expired at the deadline.
	tok, _, _ = ts.Issue("alice", 10*time.Minute)
	clk.Advance(10*time.Minute - time.Nanosecond)
	tok2, _, _ := ts.Issue("bob", 10*time.Minute)
	clk.Advance(time.Nanosecond)
	if err := ts.Redeem("alice", tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired: %v", err)
	}
	if err := ts.Redeem("alice", tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expired token must be removed: %v", err)
	}

	// Wrong user burns the token.
	if err := ts.Redeem("alice", tok2); !errors.Is(err, ErrTokenWrongUser) {
		t.Fatalf("wrong user: %v", err)
	}
	if err := ts.Redeem("bob", tok2); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("burned token: %v", err)
	}

	// A new token supersedes the previous one of the same user.
	old, _, _ := ts.Issue("alice", 0)
	cur, _, _ := ts.Issue("alice", 0)
	if err := ts.Redeem("alice", old); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("superseded: %v", err)
	}
	ts.RevokeUser("alice")
	if err := ts.Redeem("alice", cur); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("revoked: %v", err)
	}

	// Lifetime bounds and garbage input.
	if _, _, err := ts.Issue("alice", 2*time.Hour); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("ttl cap: %v", err)
	}
	if _, _, err := ts.Issue("alice", -time.Second); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("negative ttl: %v", err)
	}
	if _, _, err := ts.Issue("", 0); err == nil {
		t.Fatal("empty user")
	}
	if err := ts.Redeem("alice", ""); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("empty token: %v", err)
	}

	_, _, _ = ts.Issue("carol", time.Minute)
	_, _, _ = ts.Issue("dave", time.Hour)
	clk.Advance(2 * time.Minute)
	if n := ts.Prune(); n != 1 {
		t.Fatalf("prune removed %d", n)
	}
}

func TestURLTokenConcurrentRedeem(t *testing.T) {
	ts := NewURLTokens(newClock().Now)
	tok, _, _ := ts.Issue("alice", 0)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if ts.Redeem("alice", tok) == nil {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("token redeemed %d times", ok.Load())
	}
}

func newSessionFixture(t *testing.T) (*fakeClock, *Accounts, *Sessions) {
	t.Helper()
	clk := newClock()
	acc := NewAccounts()
	for _, id := range []string{"alice", "bob"} {
		if err := acc.Create(passkeyAccount(id)); err != nil {
			t.Fatal(err)
		}
	}
	pol := SessionPolicy{IdleTimeout: 30 * time.Minute, AbsoluteTimeout: 2 * time.Hour}
	return clk, acc, NewSessions(pol, acc, clk.Now)
}

func TestSessionIdleAndAbsoluteExpiry(t *testing.T) {
	clk, _, ss := newSessionFixture(t)
	tok, sess, err := ss.Issue("alice")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sess.ID, tok) || len(tok) < 43 {
		t.Fatalf("session ID/token: %q %q", sess.ID, tok)
	}
	if _, ok := ss.byHash[digest(tok)]; !ok {
		t.Fatal("only the digest must be the key")
	}
	if _, _, err := ss.Issue("nobody"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("unknown account: %v", err)
	}

	// Activity keeps the session alive until the absolute limit.
	for range 4 {
		clk.Advance(29 * time.Minute)
		if _, err := ss.Validate(tok); err != nil {
			t.Fatalf("active session: %v", err)
		}
	}
	clk.Advance(5 * time.Minute) // 121 minutes after creation
	if _, err := ss.Validate(tok); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("absolute expiry: %v", err)
	}
	if _, err := ss.Validate(tok); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expired session must be removed: %v", err)
	}

	tok, _, _ = ss.Issue("alice")
	clk.Advance(30 * time.Minute)
	if _, err := ss.Validate(tok); !errors.Is(err, ErrSessionIdle) {
		t.Fatalf("idle expiry: %v", err)
	}
	if _, err := ss.Validate("garbage"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestSessionRotation(t *testing.T) {
	clk, _, ss := newSessionFixture(t)
	tok, first, _ := ss.Issue("alice")
	clk.Advance(20 * time.Minute)
	next, rotated, err := ss.Rotate(tok)
	if err != nil {
		t.Fatal(err)
	}
	if next == tok || rotated.ID == first.ID || !rotated.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("rotation: %+v vs %+v", rotated, first)
	}
	if _, err := ss.Validate(tok); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("old token after rotation: %v", err)
	}
	if _, _, err := ss.Rotate(tok); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("rotate old token: %v", err)
	}
	if _, err := ss.Validate(next); err != nil {
		t.Fatalf("new token: %v", err)
	}
	// Rotation does not extend the absolute lifetime.
	for range 3 {
		clk.Advance(25 * time.Minute)
		next, _, err = ss.Rotate(next)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}
	clk.Advance(26 * time.Minute) // 121 minutes after creation, 26 idle
	if _, _, err := ss.Rotate(next); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("rotation past absolute expiry: %v", err)
	}
}

func TestSessionRevocation(t *testing.T) {
	_, acc, ss := newSessionFixture(t)
	a1, _, _ := ss.Issue("alice")
	a2, _, _ := ss.Issue("alice")
	b1, _, _ := ss.Issue("bob")

	ss.Revoke(a1)
	if _, err := ss.Validate(a1); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("revoked: %v", err)
	}
	if _, err := ss.Validate(a2); err != nil {
		t.Fatalf("other session: %v", err)
	}

	// A credential change ends all of alice's sessions, not bob's.
	if err := acc.AddCredential("alice", Credential{ID: "pw", Kind: CredentialPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Validate(a2); !errors.Is(err, ErrSessionCredentialsChanged) {
		t.Fatalf("after credential change: %v", err)
	}
	if _, err := ss.Validate(b1); err != nil {
		t.Fatalf("bob after alice change: %v", err)
	}
	// Sessions issued after the change are valid.
	a3, _, _ := ss.Issue("alice")
	if _, err := ss.Validate(a3); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func() error{
		func() error { return acc.RecordCredentialChange("alice") },
		func() error { return acc.RemoveCredential("alice", "pw") },
		func() error { return acc.BeginPasskeyReenrollment("alice") },
	} {
		tok, _, _ := ss.Issue("alice")
		if err := change(); err != nil {
			t.Fatal(err)
		}
		if _, err := ss.Validate(tok); !errors.Is(err, ErrSessionCredentialsChanged) {
			t.Fatalf("change did not revoke: %v", err)
		}
	}

	_, _, _ = ss.Issue("bob")
	if n := ss.RevokeUser("bob"); n != 2 {
		t.Fatalf("revoke user ended %d", n)
	}
	if _, err := ss.Validate(b1); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("bob after revoke user: %v", err)
	}
	if len(ss.List("bob")) != 0 {
		t.Fatal("bob still listed")
	}
}

func TestSessionPrune(t *testing.T) {
	clk, acc, ss := newSessionFixture(t)
	_, _, _ = ss.Issue("alice")
	keep, _, _ := ss.Issue("bob")
	_ = acc.RecordCredentialChange("alice")
	clk.Advance(10 * time.Minute)
	if _, err := ss.Validate(keep); err != nil {
		t.Fatal(err)
	}
	if n := ss.Prune(); n != 1 {
		t.Fatalf("prune removed %d", n)
	}
	clk.Advance(30 * time.Minute)
	if n := ss.Prune(); n != 1 {
		t.Fatalf("idle prune removed %d", n)
	}
}

func TestRecoveryFlowEndToEnd(t *testing.T) {
	// Lost passkey: operator issues a URL token via CLI, the user redeems it,
	// passkeys are reset, and the attacker-held old session dies.
	clk, acc, ss := newSessionFixture(t)
	ts := NewURLTokens(clk.Now)
	old, _, _ := ss.Issue("alice")

	tok, _, err := ts.Issue("alice", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Redeem("alice", tok); err != nil {
		t.Fatal(err)
	}
	if err := acc.BeginPasskeyReenrollment("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Validate(old); !errors.Is(err, ErrSessionCredentialsChanged) {
		t.Fatalf("old session survived recovery: %v", err)
	}
	enroll, _, _ := ss.Issue("alice")
	if err := acc.AddCredential("alice", Credential{ID: "pk-new", Kind: CredentialPasskey}); err != nil {
		t.Fatal(err)
	}
	// The enrollment session also ends with the credential change, forcing a
	// fresh sign-in with the new passkey.
	if _, err := ss.Validate(enroll); !errors.Is(err, ErrSessionCredentialsChanged) {
		t.Fatalf("enrollment session: %v", err)
	}
	a, _ := acc.Get("alice")
	if a.PendingPasskeyEnrollment || !a.HasKind(CredentialPasskey) {
		t.Fatalf("account after recovery: %+v", a)
	}
}
