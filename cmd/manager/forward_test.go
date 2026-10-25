// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/hashicorp/raft"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Three manager services use in-memory Raft and an in-process forwarding
// transport. The leader serves the real authenticated approval route. No TCP
// listener, Docker daemon, agent or database is contacted.
func TestFollowerForwardsRecoveryApprovalAcrossLeaderChange(t *testing.T) {
	ids := []string{"m1", "m2", "m3"}
	trs := map[string]*raft.InmemTransport{}
	peers := []metastore.Peer{}
	for _, id := range ids {
		_, tr := raft.NewInmemTransport(raft.ServerAddress(id))
		trs[id] = tr
		peers = append(peers, metastore.Peer{ID: id, Address: id})
	}
	for _, a := range ids {
		for _, b := range ids {
			if a != b {
				trs[a].Connect(raft.ServerAddress(b), trs[b])
			}
		}
	}
	runtimes := map[string]*managedRuntime{}
	key := manager.StaticKey{ID: "test", Bytes: bytes.Repeat([]byte{7}, 32)}
	pk := passkey.Config{RPID: "db.example.test", RPDisplayName: "cluster-manager", Origins: []string{"https://db.example.test"}}
	for _, id := range ids {
		s, err := manager.New(manager.Config{NodeID: id, Transport: trs[id], Peers: peers, Bootstrap: true,
			PollInterval: 10 * time.Millisecond, Logger: quietLog(), Tune: func(c *metastore.Config) {
				c.HeartbeatTimeout = 50 * time.Millisecond
				c.ElectionTimeout = 50 * time.Millisecond
				c.LeaderLeaseTimeout = 50 * time.Millisecond
				c.CommitTimeout = 5 * time.Millisecond
			}})
		if err != nil {
			t.Fatal(err)
		}
		rt := &managedRuntime{svc: s, proto: api.NewHandler(), forwardTLS: &metastore.PeerTLS{}, forwardPeers: map[string]peerConfig{},
			keys: key, swarms: &manager.Swarms{}}
		for _, p := range peers {
			rt.forwardPeers[p.ID] = peerConfig{ID: p.ID, Address: p.Address, APIAddress: p.ID + ":7447"}
		}
		rt.setHandler(rt.notLeader())
		runtimes[id] = rt
		s.OnLeadership(func(l *manager.Leadership) {
			if l == nil {
				rt.stack.Store(nil)
				rt.setHandler(rt.notLeader())
			}
		})
	}
	for _, id := range ids {
		rt := runtimes[id]
		rt.forwardTransport = func(p peerConfig) http.RoundTripper {
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				runtimes[p.ID].ServeHTTP(rec, r)
				return rec.Result(), nil
			})
		}
		rt.svc.Start()
	}
	t.Cleanup(func() {
		for _, rt := range runtimes {
			_ = rt.svc.Close()
		}
	})
	leader := func(exclude string) string {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			for _, id := range ids {
				if id != exclude && runtimes[id].svc.IsLeader() {
					return id
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("no leader")
		return ""
	}
	first := leader("")
	st, err := manager.LoadAuth(runtimes[first].svc.Store(), key, manager.AuthConfig{Passkey: pk, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Accounts.Create(recovery.Account{ID: "admin", Credentials: []recovery.Credential{{ID: "pk", Kind: recovery.CredentialPasskey}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Policy.Bind(authz.Binding{Subject: authz.Subject{ID: "admin", Kind: authz.SubjectUser}, Role: authz.RoleAdministrator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	token, _, err := st.Sessions.Issue("admin")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := manager.DefinitionDigest(runner.Operation{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"op-1", "op-2"} {
		rec := manager.OperationRecord{ID: id, Kind: api.OperationKindOutageRecovery, Swarm: "sw1", Cluster: "c1",
			RequestedBy: "admin", RequestedAt: time.Now(), ApprovalID: "approval-" + id, ApprovalDigest: digest}
		if _, err := manager.Put(runtimes[first].svc.Store(), manager.CollOperations, id, rec, nil); err != nil {
			t.Fatal(err)
		}
	}
	serveLeader := func(id string) {
		rt := runtimes[id]
		h, stack, err := rt.leaderAPI(pk, key, quietLog(), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		rt.stack.Store(stack)
		rt.setHandler(h)
	}
	serveLeader(first)
	var caller string
	for _, id := range ids {
		if id != first {
			caller = id
			break
		}
	}
	approve := func(id, op string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/operations/"+op+"/approve", bytes.NewBufferString(`{"digest":"`+digest+`"}`))
		req.Header.Set("Origin", "https://db.example.test")
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "__Host-dcm_session", Value: token})
		rec := httptest.NewRecorder()
		runtimes[id].ServeHTTP(rec, req)
		return rec
	}
	if rec := approve(caller, "op-1"); rec.Code != http.StatusOK {
		t.Fatalf("first follower approval: %d %s", rec.Code, rec.Body.String())
	}
	if err := runtimes[first].svc.Close(); err != nil {
		t.Fatal(err)
	}
	second := leader(first)
	serveLeader(second)
	for _, id := range ids {
		if id != first && id != second {
			caller = id
		}
	}
	if rec := approve(caller, "op-2"); rec.Code != http.StatusOK {
		t.Fatalf("approval after leader change: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok, err := manager.Get[manager.ApprovalRecord](runtimes[second].svc.Store(), manager.CollApprovals, "approval-op-2", manager.Consistent()); err != nil || !ok {
		t.Fatalf("new leader did not commit approval: %v %v", ok, err)
	}

	// A transport failure has an unknown write outcome: never replay it.
	var attempts atomic.Int32
	runtimes[caller].forwardTransport = func(peerConfig) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts.Add(1)
			return nil, errors.New("connection lost")
		})
	}
	if rec := approve(caller, "op-2"); rec.Code != http.StatusServiceUnavailable || attempts.Load() != 1 {
		t.Fatalf("uncertain write replayed or not rejected: status=%d attempts=%d", rec.Code, attempts.Load())
	}
}
