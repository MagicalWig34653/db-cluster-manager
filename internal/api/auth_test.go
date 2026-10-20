// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestLoginAndLogout(t *testing.T) {
	f := newFixture(t)
	_, a, _ := f.setupAdmin("admin")
	c := f.client()
	wantError(t, c.do("GET", "/api/v1/auth/session", nil), http.StatusUnauthorized, errUnauthenticated)
	r := f.login(c, a)
	wantStatus(t, r, http.StatusOK)
	if r.cookie("__Host-dcm_session") == nil {
		t.Fatal("no session cookie")
	}
	if r.cookie("__Host-dcm_ceremony") == nil || r.cookie("__Host-dcm_ceremony").MaxAge >= 0 {
		t.Fatal("ceremony cookie must be cleared")
	}
	wantStatus(t, c.do("GET", "/api/v1/auth/session", nil), http.StatusOK)
	token := c.cookies["__Host-dcm_session"]

	wantStatus(t, c.do("POST", "/api/v1/auth/logout", nil), http.StatusNoContent)
	if _, ok := c.cookies["__Host-dcm_session"]; ok {
		t.Fatal("logout must clear the cookie")
	}
	c.cookies["__Host-dcm_session"] = token
	wantError(t, c.do("GET", "/api/v1/auth/session", nil), http.StatusUnauthorized, errUnauthenticated)
}

func TestSessionExpiresOnIdle(t *testing.T) {
	f := newFixture(t)
	c, _, _ := f.setupAdmin("admin")
	f.clock.Advance(31 * time.Minute)
	wantError(t, c.do("GET", "/api/v1/auth/session", nil), http.StatusUnauthorized, errUnauthenticated)
}

func TestLoginFailuresAreGeneric(t *testing.T) {
	f := newFixture(t)
	f.setupAdmin("admin")
	c := f.client()

	// Unregistered authenticator.
	r := f.login(c, newSoftAuthenticator(t))
	wantError(t, r, http.StatusUnauthorized, errAuthFailed)
	if r.cookie("__Host-dcm_session") != nil {
		t.Fatal("no session on failure")
	}
	// Unknown ceremony and garbage credential.
	wantStatus(t, c.do("POST", "/api/v1/auth/login/begin", nil), http.StatusOK)
	wantError(t, c.do("POST", "/api/v1/auth/login/finish", map[string]any{"ceremonyId": "nope", "credential": json.RawMessage(`{"id":"x"}`)}),
		http.StatusUnauthorized, errAuthFailed)
	// No ceremony cookie.
	wantError(t, f.client().do("POST", "/api/v1/auth/login/finish", map[string]any{"ceremonyId": "nope", "credential": json.RawMessage(`{}`)}),
		http.StatusUnauthorized, errAuthFailed)
}

func TestLoginAuditFailureDenies(t *testing.T) {
	f := newFixture(t)
	_, a, _ := f.setupAdmin("admin")
	f.sink.SetErr(errors.New("audit down"))
	c := f.client()
	r := f.login(c, a)
	wantError(t, r, http.StatusServiceUnavailable, errUnavailable)
	if r.cookie("__Host-dcm_session") != nil {
		t.Fatal("no session without audit")
	}
}

func TestOriginValidation(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name, origin, sfs string
		want              int
	}{
		{"missing origin", "", "", http.StatusForbidden},
		{"null origin", "null", "", http.StatusForbidden},
		{"foreign origin", "https://evil.example", "", http.StatusForbidden},
		{"sibling port", "https://manager.example.test:8443", "", http.StatusForbidden},
		{"http downgrade", "http://manager.example.test", "", http.StatusForbidden},
		{"cross-site fetch", testOrigin, "cross-site", http.StatusForbidden},
		{"same-site fetch", testOrigin, "same-site", http.StatusForbidden},
		{"same origin", testOrigin, "same-origin", http.StatusOK},
		{"origin without fetch metadata", testOrigin, "", http.StatusOK},
		{"origin case", "HTTPS://Manager.Example.Test", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := f.client()
			c.origin = tc.origin
			if tc.sfs != "" {
				c.headers["Sec-Fetch-Site"] = tc.sfs
			}
			r := c.do("POST", "/api/v1/auth/login/begin", nil)
			if tc.want == http.StatusForbidden {
				wantError(t, r, tc.want, errOriginRejected)
			} else {
				wantStatus(t, r, tc.want)
			}
		})
	}
	// Safe methods need no Origin.
	c := f.client()
	c.origin = ""
	wantStatus(t, c.do("GET", "/api/v1/setup/status", nil), http.StatusOK)
}

func TestLogoutRequiresOrigin(t *testing.T) {
	f := newFixture(t)
	c, _, _ := f.setupAdmin("admin")
	c.origin = "https://evil.example"
	wantError(t, c.do("POST", "/api/v1/auth/logout", nil), http.StatusForbidden, errOriginRejected)
	c.origin = testOrigin
	wantStatus(t, c.do("GET", "/api/v1/auth/session", nil), http.StatusOK)
}

func TestRequestBodyValidation(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	wantError(t, c.do("POST", "/api/v1/setup/begin", map[string]any{"unknown": 1}), http.StatusBadRequest, errBadRequest)
	big := make([]byte, maxBodyBytes+10)
	for i := range big {
		big[i] = 'a'
	}
	wantError(t, c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": string(big)}), http.StatusRequestEntityTooLarge, errRequestTooLarge)
}

// registerPasskey adds a passkey for the logged-in client.
func registerPasskey(t *testing.T, c *client, a *softAuthenticator) response {
	t.Helper()
	r := c.do("POST", "/api/v1/auth/passkeys/begin", map[string]string{"label": "second"})
	wantStatus(t, r, http.StatusOK)
	var ch challenge
	r.decode(t, &ch)
	return c.do("POST", "/api/v1/auth/passkeys/finish", map[string]any{
		"ceremonyId": ch.CeremonyID, "credential": json.RawMessage(a.create(ch.Options, testOrigin)),
	})
}

func TestRegisterAdditionalPasskey(t *testing.T) {
	f := newFixture(t)
	c, first, _ := f.setupAdmin("admin")
	old := c.cookies["__Host-dcm_session"]
	second := newSoftAuthenticator(t)
	r := registerPasskey(t, c, second)
	wantStatus(t, r, http.StatusCreated)
	if c.cookies["__Host-dcm_session"] == old || old == "" {
		t.Fatal("session must be replaced after a credential change")
	}
	wantStatus(t, c.do("GET", "/api/v1/auth/session", nil), http.StatusOK)
	stale := f.client()
	stale.cookies["__Host-dcm_session"] = old
	wantError(t, stale.do("GET", "/api/v1/auth/session", nil), http.StatusUnauthorized, errUnauthenticated)

	for _, a := range []*softAuthenticator{first, second} {
		wantStatus(t, f.login(f.client(), a), http.StatusOK)
	}
}

func TestRegisterPasskeyRequiresSession(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	wantError(t, c.do("POST", "/api/v1/auth/passkeys/begin", nil), http.StatusUnauthorized, errUnauthenticated)
	wantError(t, c.do("POST", "/api/v1/auth/passkeys/finish", map[string]any{"ceremonyId": "x"}), http.StatusUnauthorized, errUnauthenticated)
}

func TestRegisterPasskeyAuditFailureDenies(t *testing.T) {
	f := newFixture(t)
	c, _, _ := f.setupAdmin("admin")
	r := c.do("POST", "/api/v1/auth/passkeys/begin", nil)
	var ch challenge
	r.decode(t, &ch)
	f.sink.SetErr(errors.New("audit down"))
	r = c.do("POST", "/api/v1/auth/passkeys/finish", map[string]any{
		"ceremonyId": ch.CeremonyID, "credential": json.RawMessage(newSoftAuthenticator(t).create(ch.Options, testOrigin)),
	})
	wantError(t, r, http.StatusServiceUnavailable, errUnavailable)
	acc, _ := f.accounts.Get("admin")
	n := 0
	for _, cr := range acc.Credentials {
		if cr.Kind == "passkey" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("passkeys = %d, want 1", n)
	}
}
