// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a minimal SMTP server for tests.
type fakeSMTP struct {
	ln          net.Listener
	cert        tls.Certificate
	pool        *x509.CertPool
	starttls    bool
	rejectRcpt  bool
	mu          sync.Mutex
	sawMail     bool
	mailTLS     bool
	auth        string
	from        string
	rcpts       []string
	data        string
	wg          sync.WaitGroup
	plaintextOK bool
}

func newFakeSMTP(t *testing.T, starttls bool) *fakeSMTP {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool: pool, starttls: starttls}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(c) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	var conn net.Conn = c
	r := bufio.NewReader(conn)
	say := func(l string) { conn.Write([]byte(l + "\r\n")) }
	say("220 fake ESMTP")
	isTLS := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			say("250-fake")
			if s.starttls && !isTLS {
				say("250-STARTTLS")
			}
			if isTLS || s.plaintextOK {
				say("250-AUTH PLAIN")
			}
			say("250 8BITMIME")
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, isTLS = tc, true
			r = bufio.NewReader(conn)
		case strings.HasPrefix(up, "AUTH PLAIN"):
			s.mu.Lock()
			s.auth = line
			s.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			s.mu.Lock()
			s.sawMail, s.mailTLS, s.from = true, isTLS, line
			s.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			if s.rejectRcpt {
				say("550 no such user")
				continue
			}
			s.mu.Lock()
			s.rcpts = append(s.rcpts, line)
			s.mu.Unlock()
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (s *fakeSMTP) addr() string { return s.ln.Addr().String() }

func (s *fakeSMTP) snapshot() (sawMail, mailTLS bool, data, auth string, rcpts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawMail, s.mailTLS, s.data, s.auth, append([]string(nil), s.rcpts...)
}

func emailCfg(s *fakeSMTP) EmailConfig {
	return EmailConfig{
		Addr: s.addr(), From: "manager@example.com", To: []string{"ops@example.com", "dba@example.com"},
		TLSConfig: &tls.Config{RootCAs: s.pool, ServerName: "localhost"},
		Timeout:   3 * time.Second, Now: func() time.Time { return t0 },
	}
}

var testEvent = Event{ID: "ev-9", Time: t0, Severity: SeverityCritical, Kind: "cluster.quorum_lost", Resource: "cluster/a", Message: "quorum lost", Details: map[string]string{"nodes": "2"}}

func TestEmailSTARTTLS(t *testing.T) {
	s := newFakeSMTP(t, true)
	e, err := NewEmail(emailCfg(s))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Send(context.Background(), testEvent); err != nil {
		t.Fatal(err)
	}
	saw, tlsOK, data, _, rcpts := s.snapshot()
	if !saw || !tlsOK {
		t.Fatalf("mail must be sent after STARTTLS: saw=%v tls=%v", saw, tlsOK)
	}
	if len(rcpts) != 2 {
		t.Fatalf("rcpts %v", rcpts)
	}
	for _, want := range []string{"Subject: ", "critical", "cluster.quorum_lost", "quorum lost", "nodes: 2", "To: ops@example.com, dba@example.com"} {
		if !strings.Contains(data, want) {
			t.Errorf("message missing %q:\n%s", want, data)
		}
	}
}

func TestEmailRefusesWithoutSTARTTLS(t *testing.T) {
	s := newFakeSMTP(t, false)
	e, _ := NewEmail(emailCfg(s))
	if err := e.Send(context.Background(), testEvent); err == nil {
		t.Fatal("must refuse a server without STARTTLS")
	}
	if saw, _, _, _, _ := s.snapshot(); saw {
		t.Fatal("no mail may be sent in plaintext")
	}
}

func TestEmailAllowInsecure(t *testing.T) {
	s := newFakeSMTP(t, false)
	cfg := emailCfg(s)
	cfg.AllowInsecure = true
	e, _ := NewEmail(cfg)
	if err := e.Send(context.Background(), testEvent); err != nil {
		t.Fatal(err)
	}
	if saw, tlsOK, _, _, _ := s.snapshot(); !saw || tlsOK {
		t.Fatal("explicit opt-in sends in plaintext")
	}
}

func TestEmailRejectsUntrustedCertificate(t *testing.T) {
	s := newFakeSMTP(t, true)
	cfg := emailCfg(s)
	cfg.TLSConfig = nil // system roots do not trust the test cert
	e, _ := NewEmail(cfg)
	if err := e.Send(context.Background(), testEvent); err == nil {
		t.Fatal("untrusted certificate must fail")
	}
	if saw, _, _, _, _ := s.snapshot(); saw {
		t.Fatal("no mail after failed TLS")
	}
}

func TestEmailAuthOnlyAfterTLS(t *testing.T) {
	s := newFakeSMTP(t, true)
	cfg := emailCfg(s)
	cfg.Username, cfg.Password = "user", "pw"
	e, _ := NewEmail(cfg)
	if err := e.Send(context.Background(), testEvent); err != nil {
		t.Fatal(err)
	}
	if _, _, _, auth, _ := s.snapshot(); !strings.HasPrefix(auth, "AUTH PLAIN") {
		t.Fatalf("auth not sent: %q", auth)
	}
}

func TestEmailHeaderInjectionNeutralised(t *testing.T) {
	s := newFakeSMTP(t, true)
	e, _ := NewEmail(emailCfg(s))
	ev := testEvent
	ev.Kind = "x\r\nBcc: evil@example.com"
	ev.Resource = "r\nX-Evil: 1"
	if err := e.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	_, _, data, _, _ := s.snapshot()
	head, _, _ := strings.Cut(data, "\r\n\r\n")
	for _, l := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(l, "Bcc:") || strings.HasPrefix(l, "X-Evil:") {
			t.Fatalf("header injected: %q", l)
		}
	}
}

func TestEmailRecipientRejected(t *testing.T) {
	s := newFakeSMTP(t, true)
	s.rejectRcpt = true
	e, _ := NewEmail(emailCfg(s))
	if err := e.Send(context.Background(), testEvent); err == nil {
		t.Fatal("expected error")
	}
}

func TestEmailUnreachable(t *testing.T) {
	s := newFakeSMTP(t, true)
	cfg := emailCfg(s)
	s.ln.Close()
	e, _ := NewEmail(cfg)
	if err := e.Send(context.Background(), testEvent); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestEmailConfigValidation(t *testing.T) {
	for name, c := range map[string]EmailConfig{
		"no addr": {From: "a@b.c", To: []string{"x@y.z"}},
		"no from": {Addr: "h:25", To: []string{"x@y.z"}},
		"no to":   {Addr: "h:25", From: "a@b.c"},
		"bad to":  {Addr: "h:25", From: "a@b.c", To: []string{"x@y.z\r\nRCPT"}},
	} {
		if _, err := NewEmail(c); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
