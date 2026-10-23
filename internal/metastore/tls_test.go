// SPDX-License-Identifier: AGPL-3.0-only

package metastore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/hashicorp/raft"
)

const tlsInstallation = "inst-tls"

func peerCert(t *testing.T, ca *certs.CA, installation, id string) *tls.Certificate {
	t.Helper()
	iss, err := ca.Issue(context.Background(), certs.Request{Kind: certs.KindPeer, CommonName: "metastore " + id,
		URIs: []*url.URL{PeerURI(installation, id)}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c, err := iss.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

func newPeerCA(t *testing.T, name string) *certs.CA {
	t.Helper()
	ca, err := certs.NewCA(certs.CAOptions{CommonName: name, Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

type tlsNodeSpec struct {
	id   string
	cert *tls.Certificate
}

func startTLSNodes(t *testing.T, roots *x509.CertPool, specs []tlsNodeSpec, revoked func(*x509.Certificate) bool) []*Node {
	t.Helper()
	addrs := map[string]string{}
	var peers []Peer
	for _, s := range specs {
		addrs[s.id] = freeLoopbackAddr(t)
		peers = append(peers, Peer{ID: s.id, Address: addrs[s.id]})
	}
	var nodes []*Node
	for _, s := range specs {
		cert := s.cert
		n, err := NewNode(Config{ID: s.id, BindAddr: addrs[s.id], Peers: peers, Bootstrap: true,
			TLS: &PeerTLS{Installation: tlsInstallation, Roots: roots, Revoked: revoked,
				Certificate: func() (*tls.Certificate, error) { return cert, nil }},
			HeartbeatTimeout: tcpTestTimeout, ElectionTimeout: tcpTestTimeout, LeaderLeaseTimeout: tcpTestTimeout / 2,
			CommitTimeout: 5 * time.Millisecond, ApplyTimeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("NewNode(%s): %v", s.id, err)
		}
		nodes = append(nodes, n)
		t.Cleanup(func() { _ = n.Close() })
	}
	return nodes
}

func leaderOf(nodes []*Node) *Node {
	for _, n := range nodes {
		if n.IsLeader() {
			return n
		}
	}
	return nil
}

func applyDocOnLeader(t *testing.T, nodes []*Node, key string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if l := leaderOf(nodes); l != nil {
			if _, err := l.ApplyDocs(DocOp{Collection: "c", Key: key, Value: []byte(`"v"`)}); err == nil {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no write committed")
}

// TestTLSPeersReplicate: three replicas with certificates of the
// installation's peer CA elect a leader and replicate over mutual TLS.
func TestTLSPeersReplicate(t *testing.T) {
	ca := newPeerCA(t, "peer ca")
	var specs []tlsNodeSpec
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("m%d", i)
		specs = append(specs, tlsNodeSpec{id: id, cert: peerCert(t, ca, tlsInstallation, id)})
	}
	nodes := startTLSNodes(t, ca.Pool(), specs, nil)
	applyDocOnLeader(t, nodes, "k1")
	waitFor(t, waitTimeout, "replicated to every node", func() bool {
		for _, n := range nodes {
			if _, ok := n.Doc("c", "k1"); !ok {
				return false
			}
		}
		return true
	})
}

// TestTLSRejectsForeignAndRevokedPeers: a replica whose certificate comes
// from another CA, carries another installation's identity, or is revoked
// never receives the log; the two valid replicas still form a majority.
func TestTLSRejectsForeignAndRevokedPeers(t *testing.T) {
	ca := newPeerCA(t, "peer ca")
	cases := map[string]*tls.Certificate{
		"foreign-ca":   peerCert(t, newPeerCA(t, "other ca"), tlsInstallation, "m3"),
		"installation": peerCert(t, ca, "inst-other", "m3"),
		"revoked":      peerCert(t, ca, tlsInstallation, "m3"),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			var revoked func(*x509.Certificate) bool
			if name == "revoked" {
				serial := bad.Leaf.SerialNumber
				revoked = func(c *x509.Certificate) bool { return c.SerialNumber.Cmp(serial) == 0 }
			}
			nodes := startTLSNodes(t, ca.Pool(), []tlsNodeSpec{
				{id: "m1", cert: peerCert(t, ca, tlsInstallation, "m1")},
				{id: "m2", cert: peerCert(t, ca, tlsInstallation, "m2")},
				{id: "m3", cert: bad},
			}, revoked)
			applyDocOnLeader(t, nodes[:2], "k1")
			waitFor(t, waitTimeout, "valid replicas replicate", func() bool {
				_, a := nodes[0].Doc("c", "k1")
				_, b := nodes[1].Doc("c", "k1")
				return a && b
			})
			time.Sleep(3 * tcpTestTimeout)
			if _, ok := nodes[2].Doc("c", "k1"); ok {
				t.Fatal("rejected replica received the log")
			}
			if nodes[2].IsLeader() {
				t.Fatal("rejected replica became leader")
			}
		})
	}
}

// TestTLSOutboundChecksExpectedReplica: a listener at m2's address that
// presents m3's (valid) identity is refused, and a plaintext client gets
// no Raft answer from a TLS listener.
func TestTLSOutboundChecksExpectedReplica(t *testing.T) {
	ca := newPeerCA(t, "peer ca")
	m1, m3 := peerCert(t, ca, tlsInstallation, "m1"), peerCert(t, ca, tlsInstallation, "m3")
	addr := freeLoopbackAddr(t)
	srv := &PeerTLS{Installation: tlsInstallation, Roots: ca.Pool(), Certificate: func() (*tls.Certificate, error) { return m3, nil }}
	layer, err := newTLSStreamLayer(addr, "", srv)
	if err != nil {
		t.Fatal(err)
	}
	defer layer.Close()
	go func() {
		for {
			c, err := layer.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 1)
				_, _ = c.Read(buf) // drives the server handshake
				_ = c.Close()
			}()
		}
	}()
	cli := &PeerTLS{Installation: tlsInstallation, Roots: ca.Pool(), PeerIDs: map[string]string{addr: "m2"},
		Certificate: func() (*tls.Certificate, error) { return m1, nil }}
	dl := &tlsStreamLayer{cfg: cli}
	if _, err := dl.Dial(raft.ServerAddress(addr), time.Second); !errors.Is(err, ErrPeerIdentity) {
		t.Fatalf("dial to wrong replica: %v", err)
	}
	cli.PeerIDs = map[string]string{addr: "m3"}
	c, err := dl.Dial(raft.ServerAddress(addr), time.Second)
	if err != nil {
		t.Fatalf("dial to expected replica: %v", err)
	}
	_ = c.Close()
	// Plaintext: the server never completes a handshake for it.
	pc, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_, _ = pc.Write([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _ := pc.Read(buf)
	if n > 0 && buf[0] != 0x15 { // only a TLS alert record may come back
		t.Fatalf("plaintext client got %x", buf[:n])
	}
}

func TestPeerIDOf(t *testing.T) {
	ca := newPeerCA(t, "peer ca")
	c := peerCert(t, ca, tlsInstallation, "m1")
	if id, ok := PeerIDOf(c.Leaf, tlsInstallation); !ok || id != "m1" {
		t.Fatalf("%q %v", id, ok)
	}
	if _, ok := PeerIDOf(c.Leaf, "other"); ok {
		t.Fatal("other installation accepted")
	}
	agent, err := ca.Issue(context.Background(), certs.Request{Kind: certs.KindPeer, CommonName: "agent",
		URIs: []*url.URL{certs.AgentURI(tlsInstallation, "h1")}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := PeerIDOf(agent.Cert, tlsInstallation); ok {
		t.Fatal("agent identity accepted as metastore peer")
	}
}
