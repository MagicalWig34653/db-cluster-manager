// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeHealth(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	// A wildcard listen address must still probe loopback.
	if err := probeHealth(context.Background(), "0.0.0.0:"+port); err != nil {
		t.Fatalf("healthy server: %v", err)
	}
	status = http.StatusInternalServerError
	if err := probeHealth(context.Background(), ":"+port); err == nil {
		t.Fatal("expected error for 500")
	}
	if err := probeHealth(context.Background(), "bad"); err == nil {
		t.Fatal("expected error for malformed address")
	}
}
