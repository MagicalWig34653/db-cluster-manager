// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"errors"
	"net/http"
	"testing"
)

const recCookie = "__Host-dcm_recovery"

func TestRecoveryCodeAllowsOnlyReenrolment(t *testing.T) {
	f := newFixture(t)
	adminClient, lost, codes := f.setupAdmin("admin")
	c := f.client()
	r := c.do("POST", "/api/v1/auth/recovery/code", map[string]string{"accountId": "admin", "code": codes[0]})
	wantStatus(t, r, http.StatusOK)
	ck := r.cookie(recCookie)
	if ck == nil || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("recovery cookie = %+v", ck)
	}

	// Recovery ends existing sessions and the lost passkey.
	wantError(t, adminClient.do("GET", "/api/v1/auth/session", nil), http.StatusUnauthorized, errUnauthenticated)
	wantError(t, f.login(f.client(), lost), http.StatusUnauthorized, errAuthFailed)

	// The recovery session is restricted.
	r = c.do("GET", "/api/v1/auth/session", nil)
	wantStatus(t, r, http.StatusOK)
	var si SessionInfo
	r.decode(t, &si)
	if !si.Restricted || !si.PendingPasskeyEnrollment {
		t.Fatalf("session = %+v", si)
	}
	for _, p := range []string{"/api/v1/swarms", "/api/v1/clusters", "/api/v1/clusters/c1", "/api/v1/operations", "/api/v1/events"} {
		wantError(t, c.do("GET", p, nil), http.StatusForbidden, errEnrollmentNeeded)
	}

	// Re-enrolment yields a full session and a working passkey.
	fresh := newSoftAuthenticator(t)
	wantStatus(t, registerPasskey(t, c, fresh), http.StatusCreated)
	if _, ok := c.cookies[recCookie]; ok {
		t.Fatal("recovery cookie must be cleared")
	}
	r = c.do("GET", "/api/v1/auth/session", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &si)
	if si.Restricted || si.PendingPasskeyEnrollment {
		t.Fatalf("session after re-enrolment = %+v", si)
	}
	wantStatus(t, c.do("GET", "/api/v1/swarms", nil), http.StatusOK)
	wantStatus(t, f.login(f.client(), fresh), http.StatusOK)

	// A used code cannot be redeemed again.
	wantError(t, f.client().do("POST", "/api/v1/auth/recovery/code", map[string]string{"accountId": "admin", "code": codes[0]}),
		http.StatusUnauthorized, errAuthFailed)
}

func TestRecoveryURLToken(t *testing.T) {
	f := newFixture(t)
	f.setupAdmin("admin")
	tok, _, err := f.urlTokens.Issue("admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	c := f.client()
	wantStatus(t, c.do("POST", "/api/v1/auth/recovery/token", map[string]string{"accountId": "admin", "token": tok}), http.StatusOK)
	wantStatus(t, c.do("GET", "/api/v1/auth/session", nil), http.StatusOK)
	wantError(t, f.client().do("POST", "/api/v1/auth/recovery/token", map[string]string{"accountId": "admin", "token": tok}),
		http.StatusUnauthorized, errAuthFailed)
}

func TestRecoveryFailuresAreGeneric(t *testing.T) {
	f := newFixture(t)
	f.setupAdmin("admin")
	tok, _, _ := f.urlTokens.Issue("admin", 0)
	for _, tc := range []struct {
		path string
		body map[string]string
	}{
		{"/api/v1/auth/recovery/code", map[string]string{"accountId": "admin", "code": "AAAA-BBBB-CCCC-DDDD"}},
		{"/api/v1/auth/recovery/code", map[string]string{"accountId": "nobody", "code": "AAAA-BBBB-CCCC-DDDD"}},
		{"/api/v1/auth/recovery/code", map[string]string{"accountId": "bad id!", "code": "x"}},
		{"/api/v1/auth/recovery/token", map[string]string{"accountId": "nobody", "token": tok}},
		{"/api/v1/auth/recovery/token", map[string]string{"accountId": "admin", "token": "wrong"}},
	} {
		r := f.client().do("POST", tc.path, tc.body)
		wantError(t, r, http.StatusUnauthorized, errAuthFailed)
		if r.cookie(recCookie) != nil {
			t.Fatal("no recovery session on failure")
		}
	}
}

func TestRecoveryRateLimitByIPAndAccount(t *testing.T) {
	f := newFixture(t)
	_, _, codes := f.setupAdmin("admin")
	c := f.client()
	for range 5 {
		wantError(t, c.do("POST", "/api/v1/auth/recovery/code", map[string]string{"accountId": "admin", "code": "WRONG"}),
			http.StatusUnauthorized, errAuthFailed)
	}
	r := c.do("POST", "/api/v1/auth/recovery/code", map[string]string{"accountId": "admin", "code": codes[1]})
	wantError(t, r, http.StatusTooManyRequests, errRateLimited)
	if r.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	// Another account from the same IP is not locked by this key.
	wantError(t, c.do("POST", "/api/v1/auth/recovery/code", map[string]string{"accountId": "other", "code": "WRONG"}),
		http.StatusUnauthorized, errAuthFailed)
	// The same account from another IP is not locked by this key either
	// (the per-account verifier limit still applies, but answers generically).
	other := f.client()
	other.remote = "198.51.100.7:1234"
	wantError(t, other.do("POST", "/api/v1/auth/recovery/code", map[string]string{"accountId": "admin", "code": "WRONG"}),
		http.StatusUnauthorized, errAuthFailed)
}

func TestRecoveryAuditFailureDenies(t *testing.T) {
	f := newFixture(t)
	_, a, codes := f.setupAdmin("admin")
	f.sink.SetErr(errors.New("audit down"))
	r := f.client().do("POST", "/api/v1/auth/recovery/code", map[string]string{"accountId": "admin", "code": codes[0]})
	wantError(t, r, http.StatusServiceUnavailable, errUnavailable)
	if r.cookie(recCookie) != nil {
		t.Fatal("no recovery session without audit")
	}
	f.sink.SetErr(nil)
	// The passkeys were not removed.
	wantStatus(t, f.login(f.client(), a), http.StatusOK)
}
