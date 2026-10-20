// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

func TestParsePeers(t *testing.T) {
	got, err := parsePeers("a=127.0.0.1:7001, b=127.0.0.1:7002,c=10.0.0.3:7003")
	if err != nil {
		t.Fatal(err)
	}
	want := []metastore.Peer{{ID: "a", Address: "127.0.0.1:7001"}, {ID: "b", Address: "127.0.0.1:7002"}, {ID: "c", Address: "10.0.0.3:7003"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, bad := range []string{"a", "=x:1", "a=", "a=1.2.3.4:1,a=1.2.3.4:2", "a=1.2.3.4:1,b=1.2.3.4:1", "a=nohostport"} {
		if _, err := parsePeers(bad); err == nil {
			t.Errorf("parsePeers(%q): got nil error", bad)
		}
	}
}

func TestParseFlags(t *testing.T) {
	base := []string{"-id", "a", "-bind", "127.0.0.1:7001", "-data", "/tmp/x", "-http", "127.0.0.1:8001"}
	o, err := parseFlags(append(base, "-bootstrap", "-peers", "a=127.0.0.1:7001,b=127.0.0.1:7002"))
	if err != nil {
		t.Fatal(err)
	}
	if o.id != "a" || !o.bootstrap || len(o.peers) != 2 || o.http != "127.0.0.1:8001" {
		t.Fatalf("parsed %+v", o)
	}
	bad := map[string][]string{
		"missing id":            {"-bind", "127.0.0.1:7001", "-data", "/tmp/x", "-http", "127.0.0.1:8001"},
		"missing data":          {"-id", "a", "-bind", "127.0.0.1:7001", "-http", "127.0.0.1:8001"},
		"missing http":          {"-id", "a", "-bind", "127.0.0.1:7001", "-data", "/tmp/x"},
		"bootstrap no peers":    append(base, "-bootstrap"),
		"bootstrap self absent": append(base, "-bootstrap", "-peers", "b=127.0.0.1:7002"),
		"non-loopback http":     {"-id", "a", "-bind", "127.0.0.1:7001", "-data", "/tmp/x", "-http", "0.0.0.0:8001"},
		"hostname http":         {"-id", "a", "-bind", "127.0.0.1:7001", "-data", "/tmp/x", "-http", "example.org:8001"},
		"stray argument":        append(base, "extra"),
	}
	for name, args := range bad {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%s: got nil error", name)
		}
	}
	// The explicit lab override permits a non-loopback HTTP listener.
	if _, err := parseFlags([]string{"-id", "a", "-bind", "127.0.0.1:7001", "-data", "/tmp/x", "-http", "0.0.0.0:8001", "-unsafe-http-any"}); err != nil {
		t.Fatalf("with -unsafe-http-any: %v", err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// singleNode starts a one-node metastore that elects itself.
func singleNode(t *testing.T) *metastore.Node {
	t.Helper()
	addr := freeAddr(t)
	n, err := metastore.NewNode(metastore.Config{
		ID: "solo", BindAddr: addr, DataDir: t.TempDir(), Bootstrap: true,
		Peers:            []metastore.Peer{{ID: "solo", Address: addr}},
		HeartbeatTimeout: 50 * time.Millisecond, ElectionTimeout: 50 * time.Millisecond,
		LeaderLeaseTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for !n.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("single node did not become leader")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return n
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: response %q is not a JSON object: %v", method, path, rec.Body.String(), err)
	}
	return rec.Code, out
}

func TestHandlerStateAndApply(t *testing.T) {
	n := singleNode(t)
	h := newHandler(n)

	code, st := do(t, h, http.MethodGet, "/state?barrier=1", "")
	if code != http.StatusOK || st["leader"] != true || st["id"] != "solo" || st["barrier"] != true {
		t.Fatalf("GET /state: %d %v", code, st)
	}
	term := uint64(st["term"].(float64))
	if term == 0 {
		t.Fatal("term 0")
	}

	begin, _ := json.Marshal(metastore.Command{Kind: "begin", OpID: "op1", OpKind: "k", Steps: []string{"a"}, Term: term})
	if code, out := do(t, h, http.MethodPost, "/apply", string(begin)); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("POST /apply begin: %d %v", code, out)
	}
	if code, out := do(t, h, http.MethodPost, "/apply", string(begin)); code != http.StatusConflict || out["code"] != "exists" {
		t.Fatalf("duplicate begin: %d %v", code, out)
	}
	stale, _ := json.Marshal(metastore.Command{Kind: "start", OpID: "op1", StepID: "a", Term: term + 1})
	if code, out := do(t, h, http.MethodPost, "/apply", string(stale)); code != http.StatusConflict || out["code"] != "not_owner" {
		t.Fatalf("foreign term: %d %v", code, out)
	}
	missing, _ := json.Marshal(metastore.Command{Kind: "start", OpID: "nope", StepID: "a", Term: term})
	if code, out := do(t, h, http.MethodPost, "/apply", string(missing)); code != http.StatusConflict || out["code"] != "not_found" {
		t.Fatalf("missing op: %d %v", code, out)
	}
	if code, out := do(t, h, http.MethodPost, "/apply", `{"Kind":"bogus","Term":1}`); code != http.StatusConflict || out["code"] != "rejected" {
		t.Fatalf("unknown kind: %d %v", code, out)
	}
	if code, _ := do(t, h, http.MethodPost, "/apply", `{"Kind":`); code != http.StatusBadRequest {
		t.Fatalf("bad JSON: %d", code)
	}
	if code, _ := do(t, h, http.MethodPost, "/apply", `{"Kind":"begin","Extra":1}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}
	if code, _ := do(t, h, http.MethodGet, "/apply", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /apply: %d", code)
	}
	if code, _ := do(t, h, http.MethodPost, "/state", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /state: %d", code)
	}

	_, st = do(t, h, http.MethodGet, "/state", "")
	ops, ok := st["unfinished"].([]any)
	if !ok || len(ops) != 1 || ops[0].(map[string]any)["ID"] != "op1" {
		t.Fatalf("unfinished: %v", st["unfinished"])
	}
}

func TestHandlerRejectsLargeBody(t *testing.T) {
	h := newHandler(singleNode(t))
	big := `{"Kind":"begin","OpID":"` + strings.Repeat("x", maxBody) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/apply", bytes.NewReader([]byte(big)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		b, _ := io.ReadAll(rec.Body)
		t.Fatalf("large body: %d %s", rec.Code, b)
	}
}
