// SPDX-License-Identifier: AGPL-3.0-only
package manager

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/hashicorp/raft"
)

func TestMembershipProvisionAddAndRemove(t *testing.T) {
	ctx := context.Background()
	c, rings := keyringCluster(t, 3)
	l := c.leader()
	ops := opsFor(t, l)
	replicas := &ReplicaService{Svc: l, Ring: rings[l.cfg.NodeID], Authorizer: ops.Authorizer}
	for _, s := range c.live(l.cfg.NodeID) {
		pub, _ := rings[s.cfg.NodeID].PublicKey()
		if _, err := replicas.Register(adminSubject, s.cfg.NodeID, pub); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Put(l.Store(), CollSettings, SettingsKey, Settings{InstallationID: "membership-test"}, nil); err != nil {
		t.Fatal(err)
	}
	ca, err := certs.NewCA(certs.CAOptions{CommonName: "fake peer CA", Validity: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	identity := newIdentity(t)
	pub, _ := identity.StreamPublicKey()
	_, csr, err := certs.NewKeyAndCSR("ignored untrusted subject")
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:7445"
	_, tr := raft.NewInmemTransport(raft.ServerAddress(addr))
	for _, s := range c.live() {
		other := s.cfg.Transport.(*raft.InmemTransport)
		tr.Connect(other.LocalAddr(), other)
		other.Connect(tr.LocalAddr(), tr)
	}
	members, _ := l.node.Members()
	ring := &Keyring{ReplicaID: "m4", Identity: identity}
	cfg := Config{NodeID: "m4", Transport: tr, Peers: members, Bootstrap: false, Keyring: ring, Tune: fastRaft, RunnerTune: fastRunner, PollInterval: 10 * time.Millisecond}
	candidate, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Start()
	t.Cleanup(func() { _ = candidate.Close() })
	ready := func(ctx context.Context, p metastore.Peer, index uint64) bool {
		if p.ID != "m4" {
			return true
		}
		until := time.Now().Add(2 * time.Second)
		for time.Now().Before(until) {
			if candidate.Node().AppliedIndex() >= index {
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(5 * time.Millisecond):
			}
		}
		return false
	}
	svc := &MembershipService{Svc: l, Ring: rings[l.cfg.NodeID], PeerCA: ca, Authorizer: ops.Authorizer, Ready: ready}
	in := api.PeerCertificateInput{Address: addr, CSR: string(csr), PublicKey: pub.String()}
	if err := ops.Authorizer.Policy().Bind(authz.Binding{Subject: operatorSubject, Role: authz.RoleOperator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.IssuePeerCertificate(ctx, operatorSubject, "m4", in); !errors.Is(err, authz.ErrDenied) {
		t.Fatal(err)
	}
	issued, err := svc.IssuePeerCertificate(ctx, adminSubject, "m4", in)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(issued.Certificate))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := metastore.PeerIDOf(leaf, "membership-test"); !ok || id != "m4" {
		t.Fatal("wrong peer identity")
	}
	if _, err := svc.IssuePeerCertificate(ctx, adminSubject, "m4", in); !errors.Is(err, api.ErrConflict) {
		t.Fatal("enrollment replaced")
	}
	preview, err := svc.ChangeMembership(ctx, adminSubject, "m4", api.MembershipInput{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ChangeMembership(ctx, adminSubject, "m4", api.MembershipInput{Digest: "wrong"}); !errors.Is(err, api.ErrApprovalMismatch) {
		t.Fatal(err)
	}
	result, err := svc.ChangeMembership(ctx, adminSubject, "m4", api.MembershipInput{Digest: preview.Digest})
	if err != nil || !result.Complete {
		t.Fatalf("add: %+v %v", result, err)
	}
	waitFor(t, "candidate data key", func() bool { _, _, err := ring.Current(); return err == nil })
	voters, _ := l.node.Members()
	if len(voters) != 4 {
		t.Fatalf("voters: %v", voters)
	}
	// Simulate a crash after Raft removal commits, before certificate/key cleanup.
	// The durable approved intent must survive and be finished by the successor.
	preview, err = svc.ChangeMembership(ctx, adminSubject, "m4", api.MembershipInput{Remove: true})
	if err != nil {
		t.Fatal(err)
	}
	prev, _, _ := Get[MembershipChange](l.Store(), CollMembershipChanges, "m4")
	intent := MembershipChange{Peer: metastore.Peer{ID: "m4", Address: addr}, Remove: true, Digest: preview.Digest, By: adminSubject.ID}
	op, err := PutOp(CollMembershipChanges, "m4", intent, IfVersion(prev.Version))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := PutOp(CollMembershipChanges, "active", "m4", IfVersion(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Store().Txn(op, lock); err != nil {
		t.Fatal(err)
	}
	v, err := l.node.Membership()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.node.ChangeMembership(ctx, v, intent.Peer, true, ready); err != nil {
		t.Fatal(err)
	}
	if PeerAllowed(l.Store(), "m4") {
		t.Fatal("committed removal not tombstoned")
	}
	c.close(l)
	next := c.leader()
	svc = &MembershipService{Svc: next, Ring: rings[next.cfg.NodeID], PeerCA: ca, Authorizer: opsFor(t, next).Authorizer, Ready: ready}
	result, err = svc.ChangeMembership(ctx, adminSubject, "m4", api.MembershipInput{Remove: true, Digest: preview.Digest})
	if err != nil || !result.Complete {
		t.Fatalf("resume removal: %+v %v", result, err)
	}
	if !PeerRevoked(next.Store(), leaf) || PeerAllowed(next.Store(), "m4") {
		t.Fatal("peer not revoked")
	}
	rec, _, _ := Get[KeyringRecord](next.Store(), CollKeys, KeyringKey)
	if _, ok := rec.Value.Replicas["m4"]; ok {
		t.Fatal("removed key recipient retained")
	}
	for _, key := range rec.Value.Keys {
		if _, ok := key.Envelopes["m4"]; ok {
			t.Fatal("removed envelope retained")
		}
	}
	result, err = svc.ChangeMembership(ctx, adminSubject, "m4", api.MembershipInput{Remove: true, Digest: preview.Digest})
	if err != nil || !result.Complete {
		t.Fatalf("retry: %v", err)
	}
}
