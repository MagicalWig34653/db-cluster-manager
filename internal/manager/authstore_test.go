// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

var testAuth = AuthConfig{Passkey: passkey.Config{RPID: "localhost", Origins: []string{"http://localhost:8080"}}}

// TestAuthStatePersistsAcrossRestart creates an administrator, a session
// and recovery codes through the AuthStack, restarts the single-instance
// manager from disk and checks that all of it is back.
func TestAuthStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	start := func() *Service {
		s, err := New(Config{NodeID: "solo", DataDir: dir, BindAddr: addr, Tune: fastRaft, PollInterval: 10 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		s.Start()
		waitFor(t, "leader", s.IsLeader)
		return s
	}
	s := start()
	a, err := LoadAuth(s.Store(), testKey, testAuth)
	if err != nil {
		t.Fatal(err)
	}
	if a.AdministratorExists() {
		t.Fatal("administrator before setup")
	}
	acc := recovery.Account{ID: "admin", Credentials: []recovery.Credential{{ID: "pk1", Kind: recovery.CredentialPasskey}}}
	if err := a.Accounts.Create(acc); err != nil {
		t.Fatal(err)
	}
	admin := authz.Subject{ID: "admin", Kind: authz.SubjectUser}
	if err := a.Policy.Bind(authz.Binding{Subject: admin, Role: authz.RoleAdministrator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	codes, set, err := recovery.GenerateCodes(3, 1000, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a.Codes.Enroll("admin", set)
	tok, _, err := a.Sessions.Issue("admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Audit.Record(authz.AuditRecord{Actor: admin, Action: authz.ActionCreate, Outcome: authz.OutcomeAllowed}); err != nil {
		t.Fatal(err)
	}
	// Secret material is sealed in the replicated store.
	for _, coll := range []string{CollSessions, CollRecoveryCodes} {
		docs, _ := s.Store().RawList(coll)
		if len(docs) != 1 {
			t.Fatalf("%s: %d docs", coll, len(docs))
		}
		if bytes.Contains(docs[0].Value, []byte("admin")) {
			t.Fatalf("%s stored in clear: %s", coll, docs[0].Value)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = start()
	defer s.Close()
	b, err := LoadAuth(s.Store(), testKey, testAuth)
	if err != nil {
		t.Fatal(err)
	}
	if !b.AdministratorExists() {
		t.Fatal("administrator lost on restart")
	}
	if _, err := b.Sessions.Validate(tok); err != nil {
		t.Fatalf("session lost on restart: %v", err)
	}
	if b.Codes.Remaining("admin") != 3 {
		t.Fatalf("codes %d", b.Codes.Remaining("admin"))
	}
	if err := b.Codes.Verify("admin", codes[0]); err != nil {
		t.Fatal(err)
	}
	recs, err := AuditRecords(s.Store())
	if err != nil || len(recs) != 1 {
		t.Fatalf("audit %+v %v", recs, err)
	}
	// A wrong key cannot load the sealed state.
	if _, err := LoadAuth(s.Store(), StaticKey{ID: "k1", Bytes: bytes.Repeat([]byte{1}, 32)}, testAuth); err == nil {
		t.Fatal("loaded sealed state with the wrong key")
	}
}

func TestAuthWritesFailOnFollower(t *testing.T) {
	c := newCluster(t, 3, nil)
	l := c.leader()
	f := c.follower(l)
	a, err := LoadAuth(f.Store(), testKey, testAuth)
	if err != nil {
		t.Fatal(err)
	}
	acc := recovery.Account{ID: "x", Credentials: []recovery.Credential{{ID: "pk", Kind: recovery.CredentialPasskey}}}
	if err := a.Accounts.Create(acc); err == nil {
		t.Fatal("follower accepted an account change")
	}
	if d := a.Authorizer.Authorize(authz.Subject{ID: "x", Kind: authz.SubjectUser}, authz.ActionCreate,
		authz.Resource{Type: authz.ResourceCluster, Scope: authz.SwarmScope("s")}); d.Allowed {
		t.Fatal("authorized without audit")
	}
}

func TestAuditRetentionTrimsOldest(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	at := time.Unix(100, 0)
	sink := AuditStore{Store: s.Store(), MaxRecords: 3}
	for i := range 5 {
		if err := sink.Record(authz.AuditRecord{Time: at.Add(time.Duration(i) * time.Second), Change: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := AuditRecords(s.Store())
	if len(recs) != 3 || recs[0].Change != "c" || recs[2].Change != "e" {
		t.Fatalf("retained %+v", recs)
	}
}
