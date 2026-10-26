// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/hashicorp/raft"
)

func TestProxyWriterHTTPProtocolWithoutListener(t *testing.T) {
	_, tr := raft.NewInmemTransport(raft.ServerAddress("m1"))
	svc, err := manager.New(manager.Config{NodeID: "m1", Transport: tr, Tune: func(c *metastore.Config) {
		c.HeartbeatTimeout = 50 * time.Millisecond
		c.ElectionTimeout = 50 * time.Millisecond
		c.LeaderLeaseTimeout = 50 * time.Millisecond
		c.CommitTimeout = 5 * time.Millisecond
	}})
	if err != nil {
		t.Fatal(err)
	}
	svc.Start()
	t.Cleanup(func() { _ = svc.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for !svc.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !svc.IsLeader() {
		t.Fatal("no in-memory leader")
	}
	dir := t.TempDir()
	writeToken := func(name, value string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := proxyWriterConfig{ClusterID: "c1", InitialWriter: "n1", AdminTokenFile: writeToken("admin", "admin-secret"), Members: []proxyWriterMember{
		{ID: "p1", TokenFile: writeToken("p1", "token-one")}, {ID: "p2", TokenFile: writeToken("p2", "token-two")},
	}}
	cfg.Candidates = []string{"n1", "n2"}
	rt := &managedRuntime{svc: svc, proxyWriters: map[string]proxyWriterConfig{"c1": cfg}}
	call := func(method, suffix, id, token string, body any) (int, manager.WriterChoice) {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		r := httptest.NewRequest(method, "/api/v1/proxy-writers/c1"+suffix, bytes.NewReader(b))
		r.Header.Set("X-Proxy-ID", id)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		rt.serveProxyWriter(w, r)
		var choice manager.WriterChoice
		if w.Code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &choice); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code, choice
	}
	if status, _ := call("GET", "", "p1", "token-two", nil); status != 401 {
		t.Fatalf("cross-proxy token: %d", status)
	}
	status, choice := call("GET", "", "p1", "token-one", nil)
	if status != 200 || choice.Writer != "n1" {
		t.Fatalf("initial: %d %+v", status, choice)
	}
	if status, _ := call("POST", "/switch", "", "token-one", map[string]any{"writer": "n2"}); status != 401 {
		t.Fatalf("proxy switched writer: %d", status)
	}
	if status, _ := call("POST", "/switch", "", "admin-secret", map[string]any{"writer": "unknown"}); status != 400 {
		t.Fatalf("unknown candidate: %d", status)
	}
	status, choice = call("POST", "/switch", "", "admin-secret", map[string]any{"writer": "n2"})
	if status != 200 || choice.Pending != "n2" || choice.Writer != "" {
		t.Fatalf("freeze: %d %+v", status, choice)
	}
	audit, err := manager.AuditRecords(svc.Store())
	if err != nil || len(audit) != 1 || audit[0].Change != "request writer switch to n2" {
		t.Fatalf("switch audit: %+v %v", audit, err)
	}
	status, choice = call("POST", "/ack", "p1", "token-one", map[string]any{"proxy_id": "p1", "epoch": choice.Epoch})
	if status != 200 || choice.Writer != "" {
		t.Fatalf("first ack: %d %+v", status, choice)
	}
	status, choice = call("POST", "/ack", "p2", "token-two", map[string]any{"proxy_id": "p2", "epoch": choice.Epoch})
	if status != 200 || choice.Writer != "n2" {
		t.Fatalf("second ack: %d %+v", status, choice)
	}
}
