// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealth(t *testing.T) {
	rec := get(t, "/api/v1/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var h Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil || h.Status != "ok" {
		t.Fatalf("body = %q, err = %v", rec.Body.String(), err)
	}
}

func TestVersion(t *testing.T) {
	rec := get(t, "/api/v1/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var info version.Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Component != "manager" || info.Version == "" || info.GoVersion == "" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	rec := get(t, "/api/v1/nope")
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d, content-type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
