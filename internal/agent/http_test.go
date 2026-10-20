// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
)

func startLabServer(t *testing.T, h Handler) *LabClient {
	t.Helper()
	l, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewLabServer(h)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &LabClient{BaseURL: "http://" + l.Addr().String() + "/"}
}

func TestLabHTTPRoundTrip(t *testing.T) {
	f := newFixture(t)
	c := startLabServer(t, f.exec)
	ctx := context.Background()
	cmd := Command{Term: 3, OpID: "op", StepID: "s1", Kind: KindInspect, Args: mustArgs(t, InspectArgs{ObjectKind: KindService, ID: "svc-1"})}

	r, err := c.Handle(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, r, StatusSucceeded, "")
	if r.Replayed || !strings.Contains(string(r.Output), `"svc-1"`) {
		t.Fatalf("unexpected result %+v", r)
	}
	r, err = c.Handle(ctx, cmd)
	if err != nil || !r.Replayed {
		t.Fatalf("replay over HTTP: %+v %v", r, err)
	}

	refused, err := c.Handle(ctx, Command{Term: 3, OpID: "op", StepID: "s2", Kind: KindRemoveService, Args: mustArgs(t, ServiceArgs{ServiceID: "svc-unlisted"})})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, refused, StatusRefused, CodeNotOwned)

	stale := cmd
	stale.Term, stale.StepID = 2, "s3"
	if _, err := c.Handle(ctx, stale); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("want ErrStaleTerm over HTTP, got %v", err)
	}
	bad := cmd
	bad.Kind = "format-disk"
	if _, err := c.Handle(ctx, bad); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("want ErrInvalidCommand over HTTP, got %v", err)
	}
	conflict := cmd
	conflict.Args = mustArgs(t, InspectArgs{ObjectKind: KindService, ID: "svc-foreign"})
	if _, err := c.Handle(ctx, conflict); !errors.Is(err, ErrStepConflict) {
		t.Fatalf("want ErrStepConflict over HTTP, got %v", err)
	}
	if m := f.docker.Mutations(); len(m) != 0 {
		t.Fatalf("unexpected mutations %v", m)
	}
}

func TestLabHTTPHandlerRejections(t *testing.T) {
	f := newFixture(t)
	h := NewLabHTTPHandler(f.exec)
	body := `{"term":1,"op_id":"o","step_id":"s","kind":"inspect","args":{"object_kind":"service","id":"svc-1"}}`
	cases := []struct {
		name       string
		method     string
		remote     string
		host       string
		ctype      string
		body       string
		wantStatus int
	}{
		{"ok", http.MethodPost, "127.0.0.1:5000", "127.0.0.1:7070", "application/json", body, http.StatusOK},
		{"ipv6 loopback", http.MethodPost, "[::1]:5000", "[::1]:7070", "application/json; charset=utf-8", body, http.StatusOK},
		{"remote peer", http.MethodPost, "192.168.1.10:5000", "127.0.0.1:7070", "application/json", body, http.StatusForbidden},
		{"rebinding host", http.MethodPost, "127.0.0.1:5000", "evil.example:7070", "application/json", body, http.StatusForbidden},
		{"localhost name", http.MethodPost, "127.0.0.1:5000", "localhost:7070", "application/json", body, http.StatusForbidden},
		{"GET", http.MethodGet, "127.0.0.1:5000", "127.0.0.1", "application/json", "", http.StatusMethodNotAllowed},
		{"text/plain", http.MethodPost, "127.0.0.1:5000", "127.0.0.1", "text/plain", body, http.StatusUnsupportedMediaType},
		{"unknown field", http.MethodPost, "127.0.0.1:5000", "127.0.0.1", "application/json", `{"term":1,"sudo":true}`, http.StatusBadRequest},
		{"trailing data", http.MethodPost, "127.0.0.1:5000", "127.0.0.1", "application/json", body + body, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, LabCommandPath, strings.NewReader(tc.body))
			req.RemoteAddr = tc.remote
			req.Host = tc.host
			req.Header.Set("Content-Type", tc.ctype)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
	// Rejected requests never reached the executor's term guard.
	if got := f.exec.guard.Highest(); got != 1 {
		t.Fatalf("highest term %d, want 1 (only the accepted requests)", got)
	}
}

func TestListenLoopbackRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "localhost:0", "192.168.1.1:0", "[::]:0", "127.0.0.1"} {
		l, err := ListenLoopback(addr)
		if err == nil {
			_ = l.Close()
			t.Fatalf("%q: want error", addr)
		}
	}
	l, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
}
