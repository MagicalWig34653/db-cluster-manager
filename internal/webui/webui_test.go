// SPDX-License-Identifier: AGPL-3.0-only

package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func do(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestServesAssetsAndFallsBackToIndex(t *testing.T) {
	h := newHandler(fstest.MapFS{
		"index.html":  {Data: []byte("<html>index</html>")},
		"app/main.js": {Data: []byte("js")},
	})
	if rec := do(h, "/app/main.js"); rec.Code != 200 || rec.Body.String() != "js" {
		t.Fatalf("asset: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(h, "/clusters/42"); rec.Code != 200 || rec.Body.String() != "<html>index</html>" {
		t.Fatalf("fallback: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(h, "/missing.js"); rec.Code != 404 {
		t.Fatalf("missing asset: %d", rec.Code)
	}
}

func TestReportsMissingBuild(t *testing.T) {
	if rec := do(newHandler(fstest.MapFS{}), "/"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
}
