// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAuthEndpointLockouts(t *testing.T) {
	t.Run("login begin", func(t *testing.T) {
		f := newFixture(t)
		c := f.client()
		for i := 0; i < 5; i++ {
			wantStatus(t, c.do("POST", "/api/v1/auth/login/begin", nil), http.StatusOK)
		}
		r := c.do("POST", "/api/v1/auth/login/begin", nil)
		wantError(t, r, http.StatusTooManyRequests, errRateLimited)
		if r.Header().Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
		f.clock.Advance(16 * time.Minute)
		wantStatus(t, c.do("POST", "/api/v1/auth/login/begin", nil), http.StatusOK)
	})
	t.Run("login finish", func(t *testing.T) {
		f := newFixture(t)
		c := f.client()
		for i := 0; i < 5; i++ {
			wantError(t, c.do("POST", "/api/v1/auth/login/finish", map[string]any{"ceremonyId": "wrong", "credential": json.RawMessage(`{}`)}), http.StatusUnauthorized, errAuthFailed)
		}
		wantError(t, c.do("POST", "/api/v1/auth/login/finish", map[string]any{"ceremonyId": "wrong", "credential": json.RawMessage(`{}`)}), http.StatusTooManyRequests, errRateLimited)
	})
	t.Run("setup token", func(t *testing.T) {
		f := newFixture(t)
		c := f.client()
		for i := 0; i < 5; i++ {
			wantError(t, c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": "wrong", "accountId": "admin"}), http.StatusUnauthorized, errAuthFailed)
		}
		wantError(t, c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": f.setupToken, "accountId": "admin"}), http.StatusTooManyRequests, errRateLimited)
	})
}

func TestAuthSecretIsAbsentFromLogsAndAudit(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	f.srv.log = slog.New(slog.NewTextHandler(&logs, nil))
	secret := "synthetic-auth-secret-9fd52"
	c := f.client()
	wantError(t, c.do("POST", "/api/v1/setup/begin", map[string]string{"setupToken": secret, "accountId": "admin"}), http.StatusUnauthorized, errAuthFailed)
	wantError(t, c.do("POST", "/api/v1/auth/recovery/token", map[string]string{"accountId": "admin", "token": secret}), http.StatusUnauthorized, errAuthFailed)
	wantError(t, c.do("POST", "/api/v1/auth/login/finish", map[string]any{"ceremonyId": "bad", "credential": json.RawMessage(`{"id":"synthetic-auth-secret-9fd52"}`)}), http.StatusUnauthorized, errAuthFailed)
	if strings.Contains(logs.String(), secret) {
		t.Fatal("secret entered authentication log")
	}
	for _, record := range f.sink.Records() {
		if strings.Contains(record.Change, secret) || strings.Contains(record.Error, secret) {
			t.Fatal("secret entered audit record")
		}
	}
}
