// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
)

type pki struct {
	ca  *certs.CA
	now time.Time
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	// The CA predates the test so certificates can be issued in the past.
	p := &pki{now: time.Now().Add(-12 * time.Hour)}
	ca, err := certs.NewCA(certs.CAOptions{Validity: 48 * time.Hour, Now: func() time.Time { return p.now }})
	if err != nil {
		t.Fatal(err)
	}
	p.ca = ca
	p.now = time.Now()
	return p
}

func (p *pki) issue(t *testing.T, kind certs.Kind, cn string, validity time.Duration, uris ...*url.URL) *tls.Certificate {
	t.Helper()
	req := certs.Request{Kind: kind, CommonName: cn, Validity: validity, URIs: uris}
	if kind != certs.KindClient {
		req.IPs = []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	iss, err := p.ca.Issue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	c, err := iss.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

func static(c *tls.Certificate) func() (*tls.Certificate, error) {
	return func() (*tls.Certificate, error) { return c, nil }
}

type mtlsEnv struct {
	ca      *pki
	addr    string
	revoked map[string]bool
}

func (e *mtlsEnv) isRevoked(sn *big.Int) bool { return e.revoked[sn.Text(16)] }

// startMTLS serves the fixture executor with an agent certificate for host
// "node1" of installation "inst".
func startMTLS(t *testing.T, h Handler) *mtlsEnv {
	t.Helper()
	env := &mtlsEnv{ca: newPKI(t), revoked: map[string]bool{}}
	agentCert := env.ca.issue(t, certs.KindPeer, "agent node1", time.Hour, certs.AgentURI("inst", "node1"))
	srv, err := NewMTLSServer(h, MTLSServerConfig{
		Certificate: static(agentCert), ClientCAs: env.ca.ca.Pool(), ManagerIdentity: certs.ManagerURI("inst"),
		Revoked: env.isRevoked,
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.ErrorLog = log.New(io.Discard, "", 0)
	go func() { _ = srv.ServeTLS(l, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	env.addr = l.Addr().String()
	return env
}

func (e *mtlsEnv) client(t *testing.T, cert *tls.Certificate, roots *pki, identity *url.URL) *Client {
	t.Helper()
	c, err := NewMTLSClient("https://"+e.addr, MTLSClientConfig{
		Certificate: static(cert), RootCAs: roots.ca.Pool(), AgentIdentity: identity, Revoked: e.isRevoked, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func inspectCmd(t *testing.T, step string) Command {
	return Command{Term: 3, OpID: "op", StepID: step, Kind: KindInspect, Args: mustArgs(t, InspectArgs{ObjectKind: KindService, ID: "svc-1"})}
}

func TestMTLSRoundTrip(t *testing.T) {
	f := newFixture(t)
	env := startMTLS(t, f.exec)
	mgr := env.ca.issue(t, certs.KindClient, "manager", time.Hour, certs.ManagerURI("inst"))
	c := env.client(t, mgr, env.ca, certs.AgentURI("inst", "node1"))
	r, err := c.Handle(context.Background(), inspectCmd(t, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, r, StatusSucceeded, "")
}

func TestMTLSServerRejectsClients(t *testing.T) {
	f := newFixture(t)
	env := startMTLS(t, f.exec)
	other := newPKI(t)
	expiredPKI := env.ca
	cases := map[string]func() *tls.Certificate{
		"no client certificate": func() *tls.Certificate { return &tls.Certificate{} },
		"wrong CA": func() *tls.Certificate {
			return other.issue(t, certs.KindClient, "manager", time.Hour, certs.ManagerURI("inst"))
		},
		"expired": func() *tls.Certificate {
			saved := expiredPKI.now
			expiredPKI.now = time.Now().Add(-3 * time.Hour)
			defer func() { expiredPKI.now = saved }()
			return expiredPKI.issue(t, certs.KindClient, "manager", time.Hour, certs.ManagerURI("inst"))
		},
		// Another agent's certificate has clientAuth too, but not the
		// manager identity: agents must not command each other.
		"agent certificate": func() *tls.Certificate {
			return env.ca.issue(t, certs.KindPeer, "agent node2", time.Hour, certs.AgentURI("inst", "node2"))
		},
		"other installation": func() *tls.Certificate {
			return env.ca.issue(t, certs.KindClient, "manager", time.Hour, certs.ManagerURI("inst-2"))
		},
		"server usage only": func() *tls.Certificate {
			return env.ca.issue(t, certs.KindServer, "manager", time.Hour, certs.ManagerURI("inst"))
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			c := env.client(t, mk(), env.ca, certs.AgentURI("inst", "node1"))
			if _, err := c.Handle(context.Background(), inspectCmd(t, "x-"+name)); err == nil {
				t.Fatal("command accepted")
			}
		})
	}
	if calls := f.docker.Calls(); len(calls) != 0 {
		t.Fatalf("rejected clients reached Docker: %v", calls)
	}
}

func TestMTLSServerRejectsRevokedManager(t *testing.T) {
	f := newFixture(t)
	env := startMTLS(t, f.exec)
	mgr := env.ca.issue(t, certs.KindClient, "manager", time.Hour, certs.ManagerURI("inst"))
	env.revoked[mgr.Leaf.SerialNumber.Text(16)] = true
	c := env.client(t, mgr, env.ca, certs.AgentURI("inst", "node1"))
	if _, err := c.Handle(context.Background(), inspectCmd(t, "s1")); err == nil {
		t.Fatal("revoked manager certificate accepted")
	}
}

func TestMTLSClientRejectsAgents(t *testing.T) {
	f := newFixture(t)
	env := startMTLS(t, f.exec)
	mgr := env.ca.issue(t, certs.KindClient, "manager", time.Hour, certs.ManagerURI("inst"))

	// Expecting node2 but node1 answers (address reuse, wrong record).
	c := env.client(t, mgr, env.ca, certs.AgentURI("inst", "node2"))
	if _, err := c.Handle(context.Background(), inspectCmd(t, "s1")); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("wrong agent identity: %v", err)
	}
	// The manager trusts another CA.
	c = env.client(t, mgr, newPKI(t), certs.AgentURI("inst", "node1"))
	if _, err := c.Handle(context.Background(), inspectCmd(t, "s2")); err == nil {
		t.Fatal("agent from a foreign CA accepted")
	}
	if calls := f.docker.Calls(); len(calls) != 0 {
		t.Fatalf("commands reached Docker: %v", calls)
	}
}

func TestMTLSClientRejectsRevokedAgent(t *testing.T) {
	f := newFixture(t)
	env := startMTLS(t, f.exec)
	mgr := env.ca.issue(t, certs.KindClient, "manager", time.Hour, certs.ManagerURI("inst"))
	c := env.client(t, mgr, env.ca, certs.AgentURI("inst", "node1"))
	if _, err := c.Handle(context.Background(), inspectCmd(t, "s1")); err != nil {
		t.Fatal(err)
	}
	// Revoke every serial except the manager's: the agent certificate is
	// the only other one presented.
	c.HTTP.CloseIdleConnections()
	env.revoked = map[string]bool{}
	cs, err := tls.Dial("tcp", env.addr, &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{*mgr}, MinVersion: tls.VersionTLS13}) //nolint:gosec // test inspects the presented cert
	if err != nil {
		t.Fatal(err)
	}
	agentSerial := cs.ConnectionState().PeerCertificates[0].SerialNumber.Text(16)
	_ = cs.Close()
	env.revoked[agentSerial] = true
	if _, err := c.Handle(context.Background(), inspectCmd(t, "s2")); err == nil || !errors.Is(err, certs.ErrRevoked) && !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked agent accepted: %v", err)
	}
}

func TestMTLSConfigValidation(t *testing.T) {
	if _, err := ServerTLSConfig(MTLSServerConfig{}); err == nil {
		t.Fatal("empty server config accepted")
	}
	if _, err := ClientTLSConfig(MTLSClientConfig{}); err == nil {
		t.Fatal("empty client config accepted")
	}
	p := newPKI(t)
	cert := p.issue(t, certs.KindClient, "m", time.Hour, certs.ManagerURI("i"))
	for _, u := range []string{"http://127.0.0.1:1", "127.0.0.1:1", "https://"} {
		if _, err := NewMTLSClient(u, MTLSClientConfig{Certificate: static(cert), RootCAs: p.ca.Pool(), AgentIdentity: certs.AgentURI("i", "n")}); err == nil {
			t.Errorf("base URL %q accepted", u)
		}
	}
	tc, err := ServerTLSConfig(MTLSServerConfig{Certificate: static(cert), ClientCAs: p.ca.Pool(), ManagerIdentity: certs.ManagerURI("i")})
	if err != nil || tc.MinVersion != tls.VersionTLS13 || tc.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("server TLS config %+v %v", tc, err)
	}
}
