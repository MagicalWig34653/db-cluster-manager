// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

const (
	testRPID   = "manager.example.test"
	testOrigin = "https://manager.example.test"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
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

// fakeInventory is an in-memory Inventory and Activity.
type fakeInventory struct {
	swarms     []Swarm
	clusters   []Cluster
	operations []Operation
	events     []Event
	err        error
}

func (f *fakeInventory) Swarms(context.Context) ([]Swarm, error) {
	return append([]Swarm(nil), f.swarms...), f.err
}

func (f *fakeInventory) Clusters(context.Context) ([]Cluster, error) {
	return append([]Cluster(nil), f.clusters...), f.err
}

func (f *fakeInventory) Cluster(_ context.Context, id string) (Cluster, error) {
	if f.err != nil {
		return Cluster{}, f.err
	}
	for _, c := range f.clusters {
		if c.ID == id {
			return c, nil
		}
	}
	return Cluster{}, ErrNotFound
}

func (f *fakeInventory) Operations(context.Context) ([]Operation, error) {
	return append([]Operation(nil), f.operations...), f.err
}

func (f *fakeInventory) Events(context.Context) ([]Event, error) {
	return append([]Event(nil), f.events...), f.err
}

type fixture struct {
	t          *testing.T
	clock      *fakeClock
	accounts   *recovery.Accounts
	passkeys   *passkey.Manager
	sessions   *recovery.Sessions
	recSess    *recovery.Sessions
	urlTokens  *recovery.URLTokens
	policy     *authz.Policy
	sink       *authz.MemorySink
	inventory  *fakeInventory
	setupToken string
	srv        *server
	handler    http.Handler
}

func newFixture(t *testing.T, origins ...string) *fixture {
	t.Helper()
	if len(origins) == 0 {
		origins = []string{testOrigin}
	}
	rpID := testRPID
	if isLocalhostHTTP(origins[0]) {
		rpID = "localhost"
	}
	f := &fixture{t: t, clock: &fakeClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}}
	now := f.clock.Now
	f.accounts = recovery.NewAccounts()
	pk, err := passkey.New(passkey.Config{RPID: rpID, Origins: origins}, passkey.RecoveryAccounts{Accounts: f.accounts}, now)
	if err != nil {
		t.Fatal(err)
	}
	f.passkeys = pk
	f.sessions = recovery.NewSessions(recovery.DefaultSessionPolicy, f.accounts, now)
	f.recSess = recovery.NewSessions(recovery.SessionPolicy{IdleTimeout: 10 * time.Minute, AbsoluteTimeout: 15 * time.Minute}, f.accounts, now)
	f.urlTokens = recovery.NewURLTokens(now)
	f.policy = authz.NewPolicy()
	f.sink = &authz.MemorySink{}
	tok, plain, err := NewSetupToken()
	if err != nil {
		t.Fatal(err)
	}
	f.setupToken = plain
	f.inventory = &fakeInventory{}
	cfg := Config{
		Passkeys:               pk,
		Accounts:               f.accounts,
		Sessions:               f.sessions,
		RecoverySessions:       f.recSess,
		Codes:                  recovery.NewCodeVerifier(recovery.NewLimiter(recovery.DefaultLockoutPolicy, now), now),
		URLTokens:              f.urlTokens,
		Limiter:                recovery.NewLimiter(recovery.DefaultLockoutPolicy, now),
		Authorizer:             authz.NewAuthorizer(f.policy, f.sink, now),
		AuditSink:              f.sink,
		SetupToken:             tok,
		Inventory:              f.inventory,
		Activity:               f.inventory,
		RecoveryCodeIterations: 1000,
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:                    now,
	}
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = s
	f.handler = s.mux()
	return f
}

// client is a minimal browser stand-in: it keeps cookies and sends the
// configured Origin on every request.
type client struct {
	f       *fixture
	origin  string
	remote  string
	cookies map[string]string
	headers map[string]string
}

func (f *fixture) client() *client {
	return &client{f: f, origin: testOrigin, remote: "192.0.2.10:40000", cookies: map[string]string{}, headers: map[string]string{}}
}

type response struct {
	*httptest.ResponseRecorder
	setCookies []*http.Cookie
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", r.Body.String(), err)
	}
}

func (r response) cookie(name string) *http.Cookie {
	for _, c := range r.setCookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (c *client) do(method, path string, body any) response {
	c.f.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.f.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = c.remote
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.origin != "" {
		req.Header.Set("Origin", c.origin)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	rec := httptest.NewRecorder()
	c.f.handler.ServeHTTP(rec, req)
	resp := response{ResponseRecorder: rec, setCookies: rec.Result().Cookies()}
	for _, ck := range resp.setCookies {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck.Value
		}
	}
	return resp
}

func wantStatus(t *testing.T, r response, status int) {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status = %d, want %d, body %s", r.Code, status, r.Body.String())
	}
}

// wantError checks a generic error body exactly.
func wantError(t *testing.T, r response, status int, code string) {
	t.Helper()
	wantStatus(t, r, status)
	want := `{"error":"` + code + `"}` + "\n"
	if r.Body.String() != want {
		t.Fatalf("body = %q, want %q", r.Body.String(), want)
	}
}

type challenge struct {
	CeremonyID string          `json:"ceremonyId"`
	Options    json.RawMessage `json:"options"`
}

// setupAdmin runs first-run setup for account and returns the logged-in
// client, its authenticator and the recovery codes.
func (f *fixture) setupAdmin(account string) (*client, *softAuthenticator, []string) {
	f.t.Helper()
	c := f.client()
	a := newSoftAuthenticator(f.t)
	r := c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": f.setupToken, "accountId": account})
	wantStatus(f.t, r, http.StatusOK)
	var ch challenge
	r.decode(f.t, &ch)
	r = c.do("POST", "/api/v1/setup/finish", map[string]any{
		"setupToken": f.setupToken, "ceremonyId": ch.CeremonyID, "credential": json.RawMessage(a.create(ch.Options, testOrigin)),
	})
	wantStatus(f.t, r, http.StatusCreated)
	var res SetupResult
	r.decode(f.t, &res)
	return c, a, res.RecoveryCodes
}

// login performs a discoverable passkey login with a.
func (f *fixture) login(c *client, a *softAuthenticator) response {
	f.t.Helper()
	r := c.do("POST", "/api/v1/auth/login/begin", nil)
	wantStatus(f.t, r, http.StatusOK)
	var ch challenge
	r.decode(f.t, &ch)
	return c.do("POST", "/api/v1/auth/login/finish", map[string]any{
		"ceremonyId": ch.CeremonyID, "credential": json.RawMessage(a.get(ch.Options, testOrigin)),
	})
}

// addUser creates a passkey-only account directly through the passkey layer
// and binds role at scope.
func (f *fixture) addUser(account, role string, scope authz.Scope) *softAuthenticator {
	f.t.Helper()
	a := newSoftAuthenticator(f.t)
	ch, err := f.passkeys.BeginRegistration(passkey.RegistrationRequest{AccountID: account, NewAccount: true, Binding: "test"})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.passkeys.FinishRegistration(ch.CeremonyID, "test", a.create(ch.Options, testOrigin)); err != nil {
		f.t.Fatal(err)
	}
	if role != "" {
		if err := f.policy.Bind(authz.Binding{Subject: userSubject(account), Role: role, Scope: scope}); err != nil {
			f.t.Fatal(err)
		}
	}
	return a
}

func (f *fixture) loggedIn(account, role string, scope authz.Scope) *client {
	f.t.Helper()
	a := f.addUser(account, role, scope)
	c := f.client()
	wantStatus(f.t, f.login(c, a), http.StatusOK)
	return c
}
