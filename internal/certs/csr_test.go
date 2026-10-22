// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func csrPEM(t *testing.T, tmpl *x509.CertificateRequest, key any) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestSignCSRUsesRequestIdentitiesOnly(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The CSR asks for a foreign identity; it must not appear.
	evil, _ := url.Parse("dbcm://inst/manager")
	p := csrPEM(t, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "manager"},
		DNSNames: []string{"evil.example"}, URIs: []*url.URL{evil}}, key)
	csr, err := ParseCSR(p)
	if err != nil {
		t.Fatal(err)
	}
	id := AgentURI("inst", "node1")
	s, err := ca.SignCSR(context.Background(), csr, Request{Kind: KindPeer, CommonName: "agent node1",
		IPs: []net.IP{net.ParseIP("10.0.0.7")}, URIs: []*url.URL{id}, Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c := s.Cert
	if !key.PublicKey.Equal(c.PublicKey) {
		t.Fatal("certificate is not for the CSR key")
	}
	if len(c.DNSNames) != 0 || HasURI(c, evil) || !HasURI(c, id) || c.Subject.CommonName != "agent node1" {
		t.Fatalf("identities taken from the CSR: dns=%v uris=%v cn=%q", c.DNSNames, c.URIs, c.Subject.CommonName)
	}
	for _, u := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if err := VerifyLeaf(c, nil, ca.Pool(), u, clk.t, nil); err != nil {
			t.Fatalf("usage %v: %v", u, err)
		}
	}
	if !strings.Contains(string(s.ChainPEM), string(ca.CertificatePEM())) {
		t.Fatal("chain lacks the CA")
	}
}

func TestParseCSRRejects(t *testing.T) {
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good := csrPEM(t, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "a"}}, ecKey)
	block, _ := pem.Decode(good)
	tampered := append([]byte(nil), block.Bytes...)
	tampered[len(tampered)-5] ^= 0xff
	weakKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	p224, _ := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	cases := map[string][]byte{
		"empty":     nil,
		"cert type": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}),
		"garbage":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: []byte("xx")}),
		"tampered":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: tampered}),
		"trailing":  append(append([]byte(nil), good...), good...),
		"rsa1024":   csrPEM(t, &x509.CertificateRequest{}, weakKey),
		"p224":      csrPEM(t, &x509.CertificateRequest{}, p224),
		"too large": append(good, []byte(strings.Repeat(" ", maxCSRPEM))...),
	}
	for name, in := range cases {
		if _, err := ParseCSR(in); !errors.Is(err, ErrBadCSR) {
			t.Errorf("%s: err = %v, want ErrBadCSR", name, err)
		}
	}
	if _, err := ParseCSR(good); err != nil {
		t.Fatalf("good CSR: %v", err)
	}
}

func TestSignCSRValidatesRequest(t *testing.T) {
	ca := newTestCA(t, &clock{t0}, nil)
	_, p, err := NewKeyAndCSR("x")
	if err != nil {
		t.Fatal(err)
	}
	csr, err := ParseCSR(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.SignCSR(context.Background(), csr, Request{Kind: KindPeer, CommonName: "x", Validity: time.Hour}); err == nil {
		t.Fatal("peer certificate without SAN accepted")
	}
	if _, err := ca.SignCSR(context.Background(), nil, Request{Kind: KindClient, CommonName: "x", Validity: time.Hour}); !errors.Is(err, ErrBadCSR) {
		t.Fatalf("nil CSR: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ca.SignCSR(ctx, csr, Request{Kind: KindClient, CommonName: "x", Validity: time.Hour}); err == nil {
		t.Fatal("cancelled context accepted")
	}
}

func TestNewKeyAndCSRAndKeyPEM(t *testing.T) {
	key, p, err := NewKeyAndCSR("agent")
	if err != nil {
		t.Fatal(err)
	}
	csr, err := ParseCSR(p)
	if err != nil || csr.Subject.CommonName != "agent" {
		t.Fatalf("csr %v %v", csr, err)
	}
	kp, err := MarshalKeyPEM(key)
	if err != nil || !strings.Contains(string(kp), "PRIVATE KEY") {
		t.Fatalf("key pem: %v", err)
	}
}

func TestFingerprintAndIdentities(t *testing.T) {
	ca := newTestCA(t, &clock{t0}, nil)
	fp := Fingerprint(ca.Certificate())
	if len(fp) != 64 {
		t.Fatalf("fingerprint %q", fp)
	}
	var colon []string
	for i := 0; i < len(fp); i += 2 {
		colon = append(colon, strings.ToUpper(fp[i:i+2]))
	}
	for _, in := range []string{fp, "sha256:" + fp, strings.Join(colon, ":"), " " + strings.ToUpper(fp) + "\n"} {
		got, err := NormalizeFingerprint(in)
		if err != nil || got != fp {
			t.Errorf("normalize %q = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "abc", fp[:62], fp + "00", "zz" + fp[2:]} {
		if _, err := NormalizeFingerprint(in); err == nil {
			t.Errorf("normalize %q accepted", in)
		}
	}
	if got := AgentURI("inst-1", "abc").String(); got != "dbcm://inst-1/agent/abc" {
		t.Errorf("agent URI %s", got)
	}
	if got := ManagerURI("inst-1").String(); got != "dbcm://inst-1/manager" {
		t.Errorf("manager URI %s", got)
	}
}
