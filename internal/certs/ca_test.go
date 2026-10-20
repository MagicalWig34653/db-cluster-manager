// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

func newTestCA(t *testing.T, clk *clock, mod func(*CAOptions)) *CA {
	t.Helper()
	o := CAOptions{Validity: 10 * 365 * 24 * time.Hour, Now: clk.Now}
	if mod != nil {
		mod(&o)
	}
	ca, err := NewCA(o)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestIssueServerVerifiesSANsAndUsage(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	iss, err := ca.Issue(context.Background(), Request{
		Kind: KindServer, CommonName: "db", Validity: 90 * 24 * time.Hour,
		DNSNames: []string{"db.example.com", "*.db.example.com"}, IPs: []net.IP{net.ParseIP("10.0.0.5")},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := iss.Cert
	if err := VerifyLeaf(c, nil, ca.Pool(), x509.ExtKeyUsageServerAuth, clk.t, nil); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLeaf(c, nil, ca.Pool(), x509.ExtKeyUsageClientAuth, clk.t, nil); err == nil {
		t.Fatal("server cert must not verify for client auth")
	}
	for _, h := range []string{"db.example.com", "x.db.example.com", "10.0.0.5"} {
		if err := c.VerifyHostname(h); err != nil {
			t.Errorf("hostname %s: %v", h, err)
		}
	}
	if err := c.VerifyHostname("other.example.com"); err == nil {
		t.Error("unexpected hostname match")
	}
	if c.KeyUsage != x509.KeyUsageDigitalSignature || c.IsCA {
		t.Errorf("bad key usage %v isCA=%v", c.KeyUsage, c.IsCA)
	}
	if !ca.Certificate().IsCA || ca.Certificate().KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA key usage")
	}
	if _, err := iss.TLSCertificate(); err != nil {
		t.Fatal(err)
	}
}

func TestIssueClientUsage(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	iss, err := ca.Issue(context.Background(), Request{Kind: KindClient, CommonName: "agent-1", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyLeaf(iss.Cert, nil, ca.Pool(), x509.ExtKeyUsageClientAuth, clk.t, nil); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLeaf(iss.Cert, nil, ca.Pool(), x509.ExtKeyUsageServerAuth, clk.t, nil); err == nil {
		t.Fatal("client cert must not verify for server auth")
	}
}

func TestIssueValidation(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	for name, r := range map[string]Request{
		"no san":        {Kind: KindServer, Validity: time.Hour},
		"no cn":         {Kind: KindClient, Validity: time.Hour},
		"bad kind":      {Kind: "x", Validity: time.Hour, DNSNames: []string{"a"}},
		"zero validity": {Kind: KindServer, DNSNames: []string{"a"}},
	} {
		if _, err := ca.Issue(context.Background(), r); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := NewCA(CAOptions{}); err == nil {
		t.Error("zero CA validity must fail")
	}
}

func TestExpiryAndValidityCap(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, func(o *CAOptions) { o.Validity = 48 * time.Hour })
	iss, err := ca.Issue(context.Background(), Request{Kind: KindServer, DNSNames: []string{"a.test"}, Validity: 100 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if iss.Cert.NotAfter.After(ca.Certificate().NotAfter) {
		t.Fatal("leaf outlives CA")
	}
	if err := VerifyLeaf(iss.Cert, nil, ca.Pool(), x509.ExtKeyUsageServerAuth, t0.Add(72*time.Hour), nil); err == nil {
		t.Fatal("expected expiry failure")
	}
	if err := VerifyLeaf(iss.Cert, nil, ca.Pool(), x509.ExtKeyUsageServerAuth, t0.Add(-time.Hour), nil); err == nil {
		t.Fatal("expected not-yet-valid failure")
	}
}

func TestNameConstraints(t *testing.T) {
	clk := &clock{t0}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	ca := newTestCA(t, clk, func(o *CAOptions) {
		o.PermittedDNSDomains = []string{"internal.example"}
		o.PermittedIPRanges = []*net.IPNet{cidr}
	})
	ok := Request{Kind: KindServer, DNSNames: []string{"db.internal.example"}, IPs: []net.IP{net.ParseIP("10.1.2.3")}, Validity: time.Hour}
	if _, err := ca.Issue(context.Background(), ok); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Request{
		{Kind: KindServer, DNSNames: []string{"evil.com"}, Validity: time.Hour},
		{Kind: KindServer, DNSNames: []string{"db.internal.example"}, IPs: []net.IP{net.ParseIP("192.168.1.1")}, Validity: time.Hour},
	} {
		if _, err := ca.Issue(context.Background(), r); err == nil {
			t.Errorf("expected name constraint violation for %v", r.Names())
		}
	}
}

func TestSealRoundTripAndNoPlaintext(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	blob, err := ca.Seal("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("PRIVATE KEY")) {
		t.Fatal("sealed blob contains PEM key marker")
	}
	der, _ := x509.MarshalPKCS8PrivateKey(ca.key)
	if bytes.Contains(blob, der) {
		t.Fatal("plain key in sealed blob")
	}
	back, err := LoadCA(blob, "correct horse", clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Certificate().Equal(ca.Certificate()) {
		t.Fatal("certificate changed")
	}
	// The restored CA can issue certificates trusted by the original pool.
	iss, err := back.Issue(context.Background(), Request{Kind: KindServer, DNSNames: []string{"a.test"}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyLeaf(iss.Cert, nil, ca.Pool(), x509.ExtKeyUsageServerAuth, clk.t, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSealWrongPassphraseAndTamper(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	blob, err := ca.sealIter("pw", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(blob, "other", nil); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("got %v", err)
	}
	// Tampering with the authenticated public part (the certificate) fails.
	tampered := bytes.Replace(blob, []byte(`"public":"`), []byte(`"public":"AAAA`), 1)
	if _, err := LoadCA(tampered, "pw", nil); err == nil {
		t.Fatal("tampered blob accepted")
	}
	if _, err := ca.Seal(""); err == nil {
		t.Fatal("empty passphrase accepted")
	}
	if _, err := LoadCA([]byte("junk"), "pw", nil); err == nil {
		t.Fatal("junk accepted")
	}
}

func TestSealDefaultIterationsAndIssuedKey(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	iss, _ := ca.Issue(context.Background(), Request{Kind: KindServer, DNSNames: []string{"a.test"}, Validity: time.Hour})
	blob, err := iss.SealKey("pw")
	if err != nil {
		t.Fatal(err)
	}
	pt, pub, err := open(blob, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x509.ParsePKCS8PrivateKey(pt); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pub, iss.ChainPEM) {
		t.Fatal("public part mismatch")
	}
	if strings.Contains(string(blob), "BEGIN") && strings.Contains(string(blob), "PRIVATE") {
		t.Fatal("plain key leaked")
	}
}

func TestMTLSHandshake(t *testing.T) {
	clk := &clock{time.Now()}
	ca := newTestCA(t, clk, nil)
	srv, _ := ca.Issue(context.Background(), Request{Kind: KindServer, DNSNames: []string{"localhost"}, Validity: time.Hour})
	cli, _ := ca.Issue(context.Background(), Request{Kind: KindClient, CommonName: "agent", Validity: time.Hour})
	deny := NewDenyList()
	sc, _ := srv.TLSCertificate()
	cc, _ := cli.TLSCertificate()
	run := func() error {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		serverConf := &tls.Config{
			Certificates: []tls.Certificate{sc}, ClientCAs: ca.Pool(),
			ClientAuth: tls.RequireAndVerifyClientCert, VerifyPeerCertificate: deny.VerifyPeerCertificate,
		}
		clientConf := &tls.Config{Certificates: []tls.Certificate{cc}, RootCAs: ca.Pool(), ServerName: "localhost"}
		errc := make(chan error, 1)
		go func() {
			s := tls.Server(a, serverConf)
			err := s.Handshake()
			errc <- err
		}()
		c := tls.Client(b, clientConf)
		cerr := c.Handshake()
		b.Close() // unblock the server if it is writing an alert (net.Pipe is synchronous)
		serr := <-errc
		if serr != nil {
			return serr
		}
		return cerr
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	deny.Revoke(cli.Cert.SerialNumber, time.Now(), "test")
	if err := run(); err == nil {
		t.Fatal("revoked client accepted")
	}
}
