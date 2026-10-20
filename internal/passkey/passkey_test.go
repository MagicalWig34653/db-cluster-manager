// SPDX-License-Identifier: AGPL-3.0-only

package passkey

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

const (
	testRPID   = "db.example.test"
	testOrigin = "https://db.example.test"
	testBind   = "browser-session-1"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)} }

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

type fixture struct {
	t        *testing.T
	clock    *fakeClock
	accounts *recovery.Accounts
	m        *Manager
}

func newFixture(t *testing.T, mut ...func(*Config)) *fixture {
	t.Helper()
	cfg := Config{RPID: testRPID, Origins: []string{testOrigin}}
	for _, f := range mut {
		f(&cfg)
	}
	clock := newClock()
	accts := recovery.NewAccounts()
	m, err := New(cfg, RecoveryAccounts{Accounts: accts}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, clock: clock, accounts: accts, m: m}
}

// register runs a full registration ceremony with tw applied.
func (f *fixture) register(a *softAuthenticator, req RegistrationRequest, tw tweak) (CredentialInfo, error) {
	f.t.Helper()
	if req.Binding == "" {
		req.Binding = testBind
	}
	ch, err := f.m.BeginRegistration(req)
	if err != nil {
		f.t.Fatalf("BeginRegistration: %v", err)
	}
	return f.m.FinishRegistration(ch.CeremonyID, req.Binding, a.create(ch.Options, testOrigin, tw))
}

func (f *fixture) mustRegisterNew(a *softAuthenticator, account string) CredentialInfo {
	f.t.Helper()
	info, err := f.register(a, RegistrationRequest{AccountID: account, NewAccount: true, Label: "laptop"}, tweak{})
	if err != nil {
		f.t.Fatalf("register: %v", err)
	}
	return info
}

func (f *fixture) login(a *softAuthenticator, account string, tw tweak) (LoginResult, error) {
	f.t.Helper()
	ch, err := f.m.BeginLogin(LoginRequest{AccountID: account, Binding: testBind})
	if err != nil {
		f.t.Fatalf("BeginLogin: %v", err)
	}
	return f.m.FinishLogin(ch.CeremonyID, testBind, a.get(ch.Options, testOrigin, tw))
}

func wantErr(t *testing.T, err, target error, fragment string) {
	t.Helper()
	t.Logf("got expected-failure error: %v", err)
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
	if fragment != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(fragment)) {
		t.Fatalf("error %q does not mention %q", err, fragment)
	}
}

func TestConfigValidation(t *testing.T) {
	ok := []struct {
		rpID    string
		origins []string
		want    []string
	}{
		{"example.com", []string{"https://example.com"}, []string{"https://example.com"}},
		{"example.com", []string{"https://DB.Example.com:443", "https://example.com:8443/"}, []string{"https://db.example.com", "https://example.com:8443"}},
		{"localhost", []string{"http://localhost:5173", "http://localhost"}, []string{"http://localhost:5173", "http://localhost"}},
	}
	for _, c := range ok {
		n, err := Config{RPID: c.rpID, Origins: c.origins}.normalized()
		if err != nil {
			t.Fatalf("%v %v: %v", c.rpID, c.origins, err)
		}
		if fmt.Sprint(n.Origins) != fmt.Sprint(c.want) {
			t.Fatalf("origins = %v, want %v", n.Origins, c.want)
		}
		if n.ChallengeTTL != DefaultChallengeTTL || n.RPDisplayName != c.rpID {
			t.Fatalf("defaults not applied: %+v", n)
		}
	}
	bad := []struct {
		name    string
		rpID    string
		origins []string
	}{
		{"http non-localhost", "example.com", []string{"http://example.com"}},
		{"http loopback IP", "localhost", []string{"http://127.0.0.1"}},
		{"other scheme", "example.com", []string{"ftp://example.com"}},
		{"RP ID not suffix", "example.com", []string{"https://evil-example.com"}},
		{"RP ID longer than host", "db.example.com", []string{"https://example.com"}},
		{"RP ID is subdomain of other", "example.com", []string{"https://example.org"}},
		{"path", "example.com", []string{"https://example.com/app"}},
		{"query", "example.com", []string{"https://example.com?x=1"}},
		{"userinfo", "example.com", []string{"https://u@example.com"}},
		{"no origins", "example.com", nil},
		{"empty RP ID", "", []string{"https://example.com"}},
		{"RP ID with scheme", "https://example.com", []string{"https://example.com"}},
		{"RP ID with port", "example.com:443", []string{"https://example.com"}},
		{"RP ID upper case", "Example.com", []string{"https://example.com"}},
		{"RP ID IPv4", "10.0.0.1", []string{"https://10.0.0.1"}},
		{"RP ID IPv6", "::1", []string{"https://[::1]"}},
		{"RP ID trailing dot", "example.com.", []string{"https://example.com"}},
		{"bad port", "example.com", []string{"https://example.com:99999"}},
	}
	for _, c := range bad {
		err := Config{RPID: c.rpID, Origins: c.origins}.Validate()
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("%s: error = %v, want ErrInvalidConfig", c.name, err)
		}
	}
	if _, err := New(Config{RPID: "example.com", Origins: []string{"http://example.com"}}, RecoveryAccounts{Accounts: recovery.NewAccounts()}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New accepted invalid config: %v", err)
	}
}

func TestRegistrationOptionsPolicy(t *testing.T) {
	f := newFixture(t)
	ch, err := f.m.BeginRegistration(RegistrationRequest{AccountID: "alice", NewAccount: true, Binding: testBind})
	if err != nil {
		t.Fatal(err)
	}
	o := parseOptions(t, ch.Options)
	if o.RP.ID != testRPID || o.Attestation != "none" ||
		o.AuthenticatorSelection.UserVerification != "required" || o.AuthenticatorSelection.ResidentKey != "required" {
		t.Fatalf("options = %s", ch.Options)
	}
	if h := mustB64(t, o.User.ID); len(h) != 32 || string(h) == "alice" {
		t.Fatalf("user handle must be random 32 bytes, got %q", h)
	}
	if !ch.ExpiresAt.Equal(f.clock.Now().Add(DefaultChallengeTTL)) {
		t.Fatalf("expiry = %v", ch.ExpiresAt)
	}
	lo, err := f.m.BeginLogin(LoginRequest{Binding: testBind})
	if err != nil {
		t.Fatal(err)
	}
	if p := parseOptions(t, lo.Options); p.UserVerification != "required" || p.RPID != testRPID || len(p.AllowCreds) != 0 {
		t.Fatalf("login options = %s", lo.Options)
	}
}

func TestPasskeyOnlyAccountRegistrationAndLogin(t *testing.T) {
	for _, format := range []string{"none", "packed"} {
		t.Run(format, func(t *testing.T) {
			f := newFixture(t)
			a := newSoftAuthenticator(t)
			info, err := f.register(a, RegistrationRequest{AccountID: "alice", NewAccount: true, Label: "laptop"}, tweak{format: format})
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			if info.AccountID != "alice" || info.RPID != testRPID || info.AttestationFormat != format || info.Label != "laptop" {
				t.Fatalf("info = %+v", info)
			}
			acct, err := f.accounts.Get("alice")
			if err != nil {
				t.Fatal(err)
			}
			if len(acct.Credentials) != 1 || acct.Credentials[0].Kind != recovery.CredentialPasskey || acct.Credentials[0].ID != info.ID {
				t.Fatalf("recovery account = %+v", acct)
			}

			// Discoverable login: no account name given.
			res, err := f.login(a, "", tweak{})
			if err != nil {
				t.Fatalf("discoverable login: %v", err)
			}
			if res.AccountID != "alice" || res.CredentialID != info.ID || res.SignCount != 1 {
				t.Fatalf("result = %+v", res)
			}
			// User-name login with allowCredentials.
			res, err = f.login(a, "alice", tweak{})
			if err != nil || res.AccountID != "alice" || res.SignCount != 2 {
				t.Fatalf("named login: %+v %v", res, err)
			}
			list, err := f.m.ListCredentials("alice")
			if err != nil || len(list) != 1 || list[0].SignCount != 2 || !list[0].LastUsedAt.Equal(f.clock.Now()) {
				t.Fatalf("list = %+v %v", list, err)
			}
			// The account model's sessions work with the result.
			sess := recovery.NewSessions(recovery.DefaultSessionPolicy, f.accounts, f.clock.Now)
			if _, _, err := sess.Issue(res.AccountID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSecondPasskeyOnExistingAccount(t *testing.T) {
	f := newFixture(t)
	a1, a2 := newSoftAuthenticator(t), newSoftAuthenticator(t)
	first := f.mustRegisterNew(a1, "alice")
	ch, err := f.m.BeginRegistration(RegistrationRequest{AccountID: "alice", Label: "phone", Binding: testBind})
	if err != nil {
		t.Fatal(err)
	}
	o := parseOptions(t, ch.Options)
	if len(o.ExcludeCreds) != 1 || o.ExcludeCreds[0].ID != first.ID {
		t.Fatalf("excludeCredentials = %+v", o.ExcludeCreds)
	}
	if _, err := f.m.FinishRegistration(ch.CeremonyID, testBind, a2.create(ch.Options, testOrigin, tweak{})); err != nil {
		t.Fatal(err)
	}
	if !equalHandles(a1.userHandle, a2.userHandle) {
		t.Fatal("both passkeys must carry the same user handle")
	}
	for _, a := range []*softAuthenticator{a1, a2} {
		if res, err := f.login(a, "", tweak{}); err != nil || res.AccountID != "alice" {
			t.Fatalf("login: %+v %v", res, err)
		}
	}
	// Re-registering the same credential (an authenticator ignoring
	// excludeCredentials) is refused.
	_, err = f.register(a1, RegistrationRequest{AccountID: "alice"}, tweak{})
	wantErr(t, err, ErrCredentialExists, "")
}

func equalHandles(a, b []byte) bool { return string(a) == string(b) }

func TestRegistrationRequestValidation(t *testing.T) {
	f := newFixture(t)
	f.mustRegisterNew(newSoftAuthenticator(t), "alice")
	if _, err := f.m.BeginRegistration(RegistrationRequest{AccountID: "alice", NewAccount: true, Binding: testBind}); !errors.Is(err, recovery.ErrAccountExists) {
		t.Fatalf("new account over existing: %v", err)
	}
	if _, err := f.m.BeginRegistration(RegistrationRequest{AccountID: "bob", Binding: testBind}); !errors.Is(err, recovery.ErrUnknownAccount) {
		t.Fatalf("add to unknown account: %v", err)
	}
	if _, err := f.m.BeginRegistration(RegistrationRequest{AccountID: "bob", NewAccount: true}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing binding: %v", err)
	}
	if _, err := f.m.BeginLogin(LoginRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing login binding: %v", err)
	}
	if _, err := f.m.BeginLogin(LoginRequest{AccountID: "bob", Binding: testBind}); !errors.Is(err, recovery.ErrUnknownAccount) {
		t.Fatalf("login unknown account: %v", err)
	}
}

func TestRegistrationNegative(t *testing.T) {
	cases := []struct {
		name     string
		tw       tweak
		target   error
		fragment string
	}{
		{"wrong origin", tweak{origin: "https://evil.example.test"}, ErrVerificationFailed, "origin"},
		{"wrong RP ID hash", tweak{rpID: "evil.test"}, ErrVerificationFailed, "rp hash mismatch"},
		{"UV flag missing", tweak{noUV: true}, ErrUserVerificationRequired, ""},
		{"wrong challenge", tweak{challenge: b64([]byte("0123456789abcdef0123456789abcdef"))}, ErrVerificationFailed, "challenge"},
		{"get instead of create", tweak{clientType: "webauthn.get"}, ErrVerificationFailed, "ceremony type"},
		{"bad packed signature", tweak{format: "packed", badSig: true}, ErrVerificationFailed, "verify signature"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.register(newSoftAuthenticator(t), RegistrationRequest{AccountID: "alice", NewAccount: true}, c.tw)
			wantErr(t, err, c.target, c.fragment)
			if _, gerr := f.accounts.Get("alice"); !errors.Is(gerr, recovery.ErrUnknownAccount) {
				t.Fatalf("failed registration created the account: %v", gerr)
			}
		})
	}
}

func TestRegistrationChallengeLifecycle(t *testing.T) {
	f := newFixture(t)
	a := newSoftAuthenticator(t)
	begin := func() Challenge {
		ch, err := f.m.BeginRegistration(RegistrationRequest{AccountID: "alice", NewAccount: true, Binding: testBind})
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}

	// Expired.
	ch := begin()
	resp := a.create(ch.Options, testOrigin, tweak{})
	f.clock.Advance(DefaultChallengeTTL)
	_, err := f.m.FinishRegistration(ch.CeremonyID, testBind, resp)
	wantErr(t, err, ErrCeremonyExpired, "")

	// Other session.
	ch = begin()
	_, err = f.m.FinishRegistration(ch.CeremonyID, "other-session", a.create(ch.Options, testOrigin, tweak{}))
	wantErr(t, err, ErrBindingMismatch, "")
	// The mismatched attempt consumed the ceremony.
	_, err = f.m.FinishRegistration(ch.CeremonyID, testBind, a.create(ch.Options, testOrigin, tweak{}))
	wantErr(t, err, ErrUnknownCeremony, "")

	// Success, then replay of the same ceremony.
	ch = begin()
	resp = a.create(ch.Options, testOrigin, tweak{})
	if _, err := f.m.FinishRegistration(ch.CeremonyID, testBind, resp); err != nil {
		t.Fatal(err)
	}
	_, err = f.m.FinishRegistration(ch.CeremonyID, testBind, resp)
	wantErr(t, err, ErrUnknownCeremony, "")

	// A login ceremony ID is not a registration ceremony.
	lo, err := f.m.BeginLogin(LoginRequest{Binding: testBind})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.m.FinishRegistration(lo.CeremonyID, testBind, resp)
	wantErr(t, err, ErrUnknownCeremony, "")
	_, err = f.m.FinishRegistration("never-issued", testBind, resp)
	wantErr(t, err, ErrUnknownCeremony, "")
}

func TestLoginNegative(t *testing.T) {
	cases := []struct {
		name     string
		account  string
		tw       tweak
		target   error
		fragment string
	}{
		{"wrong origin", "", tweak{origin: "https://evil.example.test"}, ErrVerificationFailed, "origin"},
		{"http origin", "alice", tweak{origin: "http://db.example.test"}, ErrVerificationFailed, "origin"},
		{"wrong RP ID hash", "", tweak{rpID: "example.test"}, ErrVerificationFailed, "rp hash mismatch"},
		{"bad signature", "", tweak{badSig: true}, ErrVerificationFailed, "signature"},
		{"bad signature named", "alice", tweak{badSig: true}, ErrVerificationFailed, "signature"},
		{"UV flag missing", "", tweak{noUV: true}, ErrUserVerificationRequired, ""},
		{"wrong challenge", "alice", tweak{challenge: b64([]byte("0123456789abcdef0123456789abcdef"))}, ErrVerificationFailed, "challenge"},
		{"create instead of get", "", tweak{clientType: "webauthn.create"}, ErrVerificationFailed, "ceremony type"},
		{"unknown user handle", "", tweak{userHandle: []byte("not-a-registered-handle")}, ErrVerificationFailed, "unknown credential"},
		{"discoverable without handle", "", tweak{noHandle: true}, ErrVerificationFailed, "user handle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			a := newSoftAuthenticator(t)
			f.mustRegisterNew(a, "alice")
			_, err := f.login(a, c.account, c.tw)
			wantErr(t, err, c.target, c.fragment)
			list, _ := f.m.ListCredentials("alice")
			if list[0].SignCount != 0 || !list[0].LastUsedAt.IsZero() {
				t.Fatalf("failed login updated the credential: %+v", list[0])
			}
		})
	}
}

func TestLoginChallengeLifecycle(t *testing.T) {
	f := newFixture(t)
	a := newSoftAuthenticator(t)
	f.mustRegisterNew(a, "alice")

	// Replay: the same ceremony twice.
	ch, err := f.m.BeginLogin(LoginRequest{Binding: testBind})
	if err != nil {
		t.Fatal(err)
	}
	resp := a.get(ch.Options, testOrigin, tweak{})
	if _, err := f.m.FinishLogin(ch.CeremonyID, testBind, resp); err != nil {
		t.Fatal(err)
	}
	_, err = f.m.FinishLogin(ch.CeremonyID, testBind, resp)
	wantErr(t, err, ErrUnknownCeremony, "")

	// Replay: a captured assertion against a fresh ceremony.
	ch2, err := f.m.BeginLogin(LoginRequest{Binding: testBind})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.m.FinishLogin(ch2.CeremonyID, testBind, resp)
	wantErr(t, err, ErrVerificationFailed, "challenge")

	// Expired.
	ch3, err := f.m.BeginLogin(LoginRequest{AccountID: "alice", Binding: testBind})
	if err != nil {
		t.Fatal(err)
	}
	resp = a.get(ch3.Options, testOrigin, tweak{})
	f.clock.Advance(DefaultChallengeTTL + time.Second)
	_, err = f.m.FinishLogin(ch3.CeremonyID, testBind, resp)
	wantErr(t, err, ErrCeremonyExpired, "")

	// Just before expiry works.
	ch4, err := f.m.BeginLogin(LoginRequest{Binding: testBind})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(DefaultChallengeTTL - time.Second)
	if _, err := f.m.FinishLogin(ch4.CeremonyID, testBind, a.get(ch4.Options, testOrigin, tweak{})); err != nil {
		t.Fatal(err)
	}

	// Bound to the session.
	ch5, err := f.m.BeginLogin(LoginRequest{Binding: testBind})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.m.FinishLogin(ch5.CeremonyID, "other-session", a.get(ch5.Options, testOrigin, tweak{}))
	wantErr(t, err, ErrBindingMismatch, "")
}

func TestLoginBoundToAccount(t *testing.T) {
	f := newFixture(t)
	alice, bob := newSoftAuthenticator(t), newSoftAuthenticator(t)
	f.mustRegisterNew(alice, "alice")
	f.mustRegisterNew(bob, "bob")
	// A ceremony started for alice cannot be completed with bob's passkey.
	_, err := f.login(bob, "alice", tweak{})
	wantErr(t, err, ErrVerificationFailed, "not owned by the user")
	// Bob's credential presented with alice's user handle is rejected.
	_, err = f.login(bob, "", tweak{userHandle: alice.userHandle})
	wantErr(t, err, ErrVerificationFailed, "unable to find the credential")
}

func TestSignCountPolicy(t *testing.T) {
	f := newFixture(t)
	a := newSoftAuthenticator(t)
	info := f.mustRegisterNew(a, "alice")
	for i := 0; i < 2; i++ {
		if _, err := f.login(a, "", tweak{}); err != nil {
			t.Fatal(err)
		}
	}
	// A clone holding the same key but an older counter: its next counter
	// (2) equals the stored one.
	clone := a.clone()
	clone.counter = 1
	_, err := f.login(clone, "", tweak{})
	wantErr(t, err, ErrSignCountRegression, "stored 2, received 2")
	list, _ := f.m.ListCredentials("alice")
	if list[0].SignCount != 2 || list[0].CloneWarnings != 1 || !list[0].LastCloneWarningAt.Equal(f.clock.Now()) {
		t.Fatalf("after regression: %+v", list[0])
	}
	// A lower counter is rejected too.
	clone.counter = 0
	_, err = f.login(clone, "alice", tweak{})
	wantErr(t, err, ErrSignCountRegression, "")
	// The genuine authenticator continues (counter 3).
	res, err := f.login(a, "", tweak{})
	if err != nil || res.SignCount != 3 || res.CredentialID != info.ID {
		t.Fatalf("genuine login after warning: %+v %v", res, err)
	}
	list, _ = f.m.ListCredentials("alice")
	if list[0].CloneWarnings != 2 {
		t.Fatalf("clone warnings = %d", list[0].CloneWarnings)
	}
}

func TestZeroCounterSyncedPasskey(t *testing.T) {
	f := newFixture(t)
	a := newSoftAuthenticator(t)
	a.countless, a.synced = true, true
	info := f.mustRegisterNew(a, "alice")
	if !info.BackupEligible || !info.BackupState {
		t.Fatalf("backup flags not recorded: %+v", info)
	}
	for i := 0; i < 3; i++ {
		if res, err := f.login(a, "", tweak{}); err != nil || res.SignCount != 0 {
			t.Fatalf("login %d: %+v %v", i, res, err)
		}
	}
}

func TestCredentialRemoval(t *testing.T) {
	f := newFixture(t)
	a1, a2 := newSoftAuthenticator(t), newSoftAuthenticator(t)
	first := f.mustRegisterNew(a1, "alice")
	if err := f.m.RemoveCredential("alice", first.ID); !errors.Is(err, ErrLastPasskey) {
		t.Fatalf("removing the only passkey: %v", err)
	}
	f.clock.Advance(time.Minute)
	second, err := f.register(a2, RegistrationRequest{AccountID: "alice", Label: "phone"}, tweak{})
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.m.ListCredentials("alice")
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
	if err := f.m.RemoveCredential("bob", first.ID); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("removing via another account: %v", err)
	}
	if err := f.m.RemoveCredential("alice", "nope"); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("removing unknown: %v", err)
	}
	if err := f.m.RemoveCredential("alice", first.ID); err != nil {
		t.Fatal(err)
	}
	acct, _ := f.accounts.Get("alice")
	if len(acct.Credentials) != 1 || acct.Credentials[0].ID != second.ID {
		t.Fatalf("recovery account = %+v", acct)
	}
	_, err = f.login(a1, "", tweak{})
	wantErr(t, err, ErrVerificationFailed, "unable to find the credential")
	if err := f.m.RemoveCredential("alice", second.ID); !errors.Is(err, ErrLastPasskey) {
		t.Fatalf("removing the remaining passkey: %v", err)
	}
	if _, err := f.login(a2, "", tweak{}); err != nil {
		t.Fatal(err)
	}
}

// After recovery removes all passkeys, the old ones no longer log in, can be
// cleaned up, and a newly registered passkey ends pending enrollment.
func TestRecoveryReenrollment(t *testing.T) {
	f := newFixture(t)
	old := newSoftAuthenticator(t)
	oldInfo := f.mustRegisterNew(old, "alice")
	if err := f.accounts.BeginPasskeyReenrollment("alice"); err != nil {
		t.Fatal(err)
	}
	_, err := f.login(old, "", tweak{})
	wantErr(t, err, ErrVerificationFailed, "unable to find the credential")
	if _, err := f.m.BeginLogin(LoginRequest{AccountID: "alice", Binding: testBind}); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("named login with no usable passkey: %v", err)
	}
	if list, _ := f.m.ListCredentials("alice"); len(list) != 0 {
		t.Fatalf("removed passkey still listed: %+v", list)
	}
	if err := f.m.RemoveCredential("alice", oldInfo.ID); err != nil {
		t.Fatalf("cleaning up stale key material: %v", err)
	}
	fresh := newSoftAuthenticator(t)
	if _, err := f.register(fresh, RegistrationRequest{AccountID: "alice"}, tweak{}); err != nil {
		t.Fatal(err)
	}
	if acct, _ := f.accounts.Get("alice"); acct.PendingPasskeyEnrollment {
		t.Fatal("still pending enrollment")
	}
	if res, err := f.login(fresh, "", tweak{}); err != nil || res.AccountID != "alice" {
		t.Fatalf("login with new passkey: %+v %v", res, err)
	}
	if !equalHandles(old.userHandle, fresh.userHandle) {
		t.Fatal("user handle must survive re-enrollment")
	}
}

func TestPendingCeremonyLimit(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MaxPendingCeremonies = 2; c.ChallengeTTL = time.Minute })
	for i := 0; i < 2; i++ {
		if _, err := f.m.BeginLogin(LoginRequest{Binding: testBind}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.m.BeginLogin(LoginRequest{Binding: testBind}); !errors.Is(err, ErrTooManyCeremonies) {
		t.Fatalf("limit not enforced: %v", err)
	}
	f.clock.Advance(time.Minute)
	if _, err := f.m.BeginLogin(LoginRequest{Binding: testBind}); err != nil {
		t.Fatalf("expired ceremonies not pruned: %v", err)
	}
	f.clock.Advance(time.Minute)
	if n := f.m.PruneCeremonies(); n != 1 || f.m.challenges.size() != 0 {
		t.Fatalf("pruned %d, left %d", n, f.m.challenges.size())
	}
}

func TestConcurrentCeremonies(t *testing.T) {
	f := newFixture(t)
	const users = 8
	auths := make([]*softAuthenticator, users)
	for i := range auths {
		auths[i] = newSoftAuthenticator(t)
		f.mustRegisterNew(auths[i], fmt.Sprintf("user-%d", i))
	}
	var wg sync.WaitGroup
	errs := make(chan error, users*5)
	for i := range auths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				ch, err := f.m.BeginLogin(LoginRequest{Binding: fmt.Sprint(i)})
				if err != nil {
					errs <- err
					return
				}
				res, err := f.m.FinishLogin(ch.CeremonyID, fmt.Sprint(i), auths[i].get(ch.Options, testOrigin, tweak{}))
				if err != nil {
					errs <- err
					return
				}
				if res.AccountID != fmt.Sprintf("user-%d", i) {
					errs <- fmt.Errorf("got %s", res.AccountID)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
