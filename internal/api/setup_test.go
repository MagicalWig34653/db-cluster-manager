// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

func TestSetupToken(t *testing.T) {
	tok, plain, err := NewSetupToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) < 43 {
		t.Fatalf("token too short: %d", len(plain))
	}
	if !tok.Active() || !tok.Verify(plain) {
		t.Fatal("fresh token must verify")
	}
	for _, bad := range []string{"", "x", plain[:len(plain)-1], plain + "x"} {
		if tok.Verify(bad) {
			t.Fatalf("Verify(%q) = true", bad)
		}
	}
	tok.Invalidate()
	if tok.Active() || tok.Verify(plain) {
		t.Fatal("invalidated token must not verify")
	}
	var nilTok *SetupToken
	if nilTok.Active() || nilTok.Verify(plain) {
		t.Fatal("nil token must be inactive")
	}
}

func setupStatus(t *testing.T, c *client) bool {
	t.Helper()
	r := c.do("GET", "/api/v1/setup/status", nil)
	wantStatus(t, r, http.StatusOK)
	var st SetupStatus
	r.decode(t, &st)
	return st.SetupRequired
}

func TestSetupCreatesFirstAdministrator(t *testing.T) {
	f := newFixture(t)
	if !setupStatus(t, f.client()) {
		t.Fatal("setup must be required on a fresh manager")
	}
	c, _, codes := f.setupAdmin("admin")
	if len(codes) != 10 {
		t.Fatalf("recovery codes = %d", len(codes))
	}
	if setupStatus(t, f.client()) {
		t.Fatal("setup must be disabled once an administrator exists")
	}
	// The session works and carries the administrator role.
	r := c.do("GET", "/api/v1/auth/session", nil)
	wantStatus(t, r, http.StatusOK)
	var si SessionInfo
	r.decode(t, &si)
	if si.AccountID != "admin" || si.Restricted {
		t.Fatalf("session = %+v", si)
	}
	if !f.policy.Check(userSubject("admin"), authz.ActionDelete, authz.Resource{Type: authz.ResourceCluster, Scope: authz.ClusterScope("s", "c")}).Allowed {
		t.Fatal("first account must be a global administrator")
	}
	acc, err := f.accounts.Get("admin")
	if err != nil || len(acc.Credentials) != 2 {
		t.Fatalf("account = %+v, err %v", acc, err)
	}
	var created bool
	for _, rec := range f.sink.Records() {
		if rec.Reason == reasonFirstRunSetup && rec.Outcome == authz.OutcomeSucceeded && rec.Actor.ID == "admin" {
			created = true
		}
	}
	if !created {
		t.Fatalf("no audit record for setup: %+v", f.sink.Records())
	}
}

func TestSetupSessionCookieAttributes(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	a := newSoftAuthenticator(t)
	r := c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": f.setupToken, "accountId": "admin"})
	wantStatus(t, r, http.StatusOK)
	cer := r.cookie("__Host-dcm_ceremony")
	if cer == nil || !cer.HttpOnly || !cer.Secure || cer.SameSite != http.SameSiteStrictMode || cer.Path != "/" {
		t.Fatalf("ceremony cookie = %+v", cer)
	}
	var ch challenge
	r.decode(t, &ch)
	r = c.do("POST", "/api/v1/setup/finish", map[string]any{
		"setupToken": f.setupToken, "ceremonyId": ch.CeremonyID, "credential": json.RawMessage(a.create(ch.Options, testOrigin)),
	})
	wantStatus(t, r, http.StatusCreated)
	ck := r.cookie("__Host-dcm_session")
	if ck == nil || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" || ck.Domain != "" {
		t.Fatalf("session cookie = %+v", ck)
	}
}

func TestLocalhostDevCookiesAreNotSecure(t *testing.T) {
	f := newFixture(t, "http://localhost:5173")
	c := f.client()
	c.origin = "http://localhost:5173"
	r := c.do("POST", "/api/v1/auth/login/begin", nil)
	wantStatus(t, r, http.StatusOK)
	ck := r.cookie("dcm_ceremony")
	if ck == nil || ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("dev cookie = %+v (all: %v)", ck, r.setCookies)
	}
}

func TestSetupRequiresToken(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	for _, tok := range []string{"", "wrong"} {
		r := c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": tok, "accountId": "admin"})
		wantError(t, r, http.StatusUnauthorized, errAuthFailed)
	}
	r := c.do("POST", "/api/v1/setup/finish", map[string]any{"setupToken": "wrong", "ceremonyId": "x", "credential": json.RawMessage(`{}`)})
	wantError(t, r, http.StatusUnauthorized, errAuthFailed)
	if !setupStatus(t, c) {
		t.Fatal("failed attempts must not disable setup")
	}
}

func TestSetupTokenGuessingIsLimited(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	for range 5 {
		wantError(t, c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": "guess", "accountId": "a"}), http.StatusUnauthorized, errAuthFailed)
	}
	r := c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": f.setupToken, "accountId": "a"})
	wantError(t, r, http.StatusTooManyRequests, errRateLimited)
	if r.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestSetupDisabledAfterAdministrator(t *testing.T) {
	f := newFixture(t)
	f.setupAdmin("admin")
	c := f.client()
	r := c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": f.setupToken, "accountId": "second"})
	wantError(t, r, http.StatusConflict, errSetupUnavailable)
	r = c.do("POST", "/api/v1/setup/finish", map[string]any{"setupToken": f.setupToken, "ceremonyId": "x", "credential": json.RawMessage(`{}`)})
	wantError(t, r, http.StatusConflict, errSetupUnavailable)
}

func TestSetupDisabledByExistingAdministrator(t *testing.T) {
	f := newFixture(t)
	f.srv.cfg.AdministratorExists = func() bool { return true }
	if setupStatus(t, f.client()) {
		t.Fatal("persisted administrator must disable setup")
	}
}

func TestSetupFinishNeedsSameBrowser(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	a := newSoftAuthenticator(t)
	r := c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": f.setupToken, "accountId": "admin"})
	var ch challenge
	r.decode(t, &ch)
	other := f.client()
	body := map[string]any{"setupToken": f.setupToken, "ceremonyId": ch.CeremonyID, "credential": json.RawMessage(a.create(ch.Options, testOrigin))}
	wantError(t, other.do("POST", "/api/v1/setup/finish", body), http.StatusUnauthorized, errAuthFailed)
	other.cookies["__Host-dcm_ceremony"] = "forged"
	wantError(t, other.do("POST", "/api/v1/setup/finish", body), http.StatusUnauthorized, errAuthFailed)
	if !setupStatus(t, c) {
		t.Fatal("setup must still be possible")
	}
}

func TestSetupAuditFailureDenies(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	a := newSoftAuthenticator(t)
	r := c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": f.setupToken, "accountId": "admin"})
	var ch challenge
	r.decode(t, &ch)
	f.sink.SetErr(errors.New("disk full"))
	r = c.do("POST", "/api/v1/setup/finish", map[string]any{
		"setupToken": f.setupToken, "ceremonyId": ch.CeremonyID, "credential": json.RawMessage(a.create(ch.Options, testOrigin)),
	})
	wantError(t, r, http.StatusServiceUnavailable, errUnavailable)
	if r.cookie("__Host-dcm_session") != nil {
		t.Fatal("no session may be issued")
	}
	if _, err := f.accounts.Get("admin"); err == nil {
		t.Fatal("account must not be created without audit")
	}
	f.sink.SetErr(nil)
	if !setupStatus(t, c) {
		t.Fatal("setup must remain possible after a denied attempt")
	}
}

func TestConcurrentSetupCreatesOneAdministrator(t *testing.T) {
	f := newFixture(t)
	type pending struct {
		c    *client
		body map[string]any
	}
	var ps []pending
	for _, acc := range []string{"alice", "bob", "carol"} {
		c := f.client()
		a := newSoftAuthenticator(t)
		r := c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": f.setupToken, "accountId": acc})
		wantStatus(t, r, http.StatusOK)
		var ch challenge
		r.decode(t, &ch)
		ps = append(ps, pending{c, map[string]any{
			"setupToken": f.setupToken, "ceremonyId": ch.CeremonyID, "credential": json.RawMessage(a.create(ch.Options, testOrigin)),
		}})
	}
	codes := make(chan int, len(ps))
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Go(func() { codes <- p.c.do("POST", "/api/v1/setup/finish", p.body).Code })
	}
	wg.Wait()
	close(codes)
	created := 0
	for code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d administrators", created)
	}
}
