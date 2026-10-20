// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

// These tests use a fake MariaDB server and client written from the MariaDB
// protocol documentation. They do not prove interoperability with real
// MariaDB servers or connectors.

func fakeGreeting(version string, caps uint32) []byte {
	var b bytes.Buffer
	b.WriteByte(10)
	b.WriteString(version)
	b.WriteByte(0)
	_ = binary.Write(&b, binary.LittleEndian, uint32(77)) // connection id
	b.WriteString("abcdefgh")                             // auth data part 1
	b.WriteByte(0)
	_ = binary.Write(&b, binary.LittleEndian, uint16(caps))
	b.WriteByte(45)                                      // charset
	_ = binary.Write(&b, binary.LittleEndian, uint16(2)) // status
	_ = binary.Write(&b, binary.LittleEndian, uint16(caps>>16))
	b.WriteByte(21)
	b.Write(make([]byte, 10))
	b.WriteString("ijklmnopqrst\x00")
	b.WriteString("mysql_native_password\x00")
	return b.Bytes()
}

func TestParseGreeting(t *testing.T) {
	g, err := parseGreeting(fakeGreeting("11.4.2-MariaDB", capClientProtocol41|capClientSSL))
	if err != nil {
		t.Fatal(err)
	}
	if g.ServerVersion != "11.4.2-MariaDB" || g.ConnectionID != 77 || g.Capabilities&capClientSSL == 0 {
		t.Fatalf("greeting = %+v", g)
	}
	for name, p := range map[string][]byte{
		"empty":        nil,
		"version 9":    {9, 'x', 0},
		"unterminated": {10, 'x', 'y'},
		"truncated":    {10, 'x', 0, 1, 2, 3},
	} {
		if _, err := parseGreeting(p); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	_, err = parseGreeting(errPayload(1040, "08004", "Too many connections", false))
	if err == nil || !strings.Contains(err.Error(), "1040") || !strings.Contains(err.Error(), "Too many connections") {
		t.Fatalf("ERR greeting: %v", err)
	}
}

func TestPacketRoundTripAndLimit(t *testing.T) {
	var b bytes.Buffer
	if err := writePacket(&b, 7, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	seq, p, err := readPacket(bytes.NewReader(b.Bytes()), 16)
	if err != nil || seq != 7 || string(p) != "hello" {
		t.Fatalf("got %d %q %v", seq, p, err)
	}
	if _, _, err := readPacket(bytes.NewReader(b.Bytes()), 4); err == nil {
		t.Fatal("oversized packet accepted")
	}
	code, msg := parseErr(errPayload(1043, "08S01", "bad", true))
	if code != 1043 || msg != "bad" {
		t.Fatalf("parseErr = %d %q", code, msg)
	}
}

// mysqlBackend is a fake MariaDB server: greeting without CLIENT_SSL,
// expects the handshake response as sequence 1 without CLIENT_SSL, then
// optionally an auth switch, then OK and echo.
type mysqlBackend struct {
	l          net.Listener
	authSwitch bool
	mu         sync.Mutex
	got        []string // observations
	wg         sync.WaitGroup
}

func newMySQLBackend(t *testing.T, authSwitch bool) *mysqlBackend {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &mysqlBackend{l: l, authSwitch: authSwitch}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			m.wg.Add(1)
			go func() { defer m.wg.Done(); m.serve(c) }()
		}
	}()
	t.Cleanup(func() { _ = l.Close(); m.wg.Wait() })
	return m
}

func (m *mysqlBackend) note(s string) {
	m.mu.Lock()
	m.got = append(m.got, s)
	m.mu.Unlock()
}

func (m *mysqlBackend) notes() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.got, ",")
}

func (m *mysqlBackend) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if writePacket(c, 0, fakeGreeting("10.11.8-MariaDB-fake", capClientProtocol41)) != nil {
		return
	}
	seq, p, err := readPacket(c, 1<<16)
	if err != nil {
		return // probe or refused client
	}
	caps := binary.LittleEndian.Uint32(p)
	user := string(p[32 : bytes.IndexByte(p[32:], 0)+32])
	m.note("seq=" + string('0'+seq))
	if caps&capClientSSL != 0 {
		m.note("ssl-flag")
	}
	m.note("user=" + user)
	next := seq + 1
	if m.authSwitch {
		_ = writePacket(c, next, append([]byte{0xfe}, "client_ed25519\x00salt"...))
		s, p, err := readPacket(c, 1<<16)
		if err != nil {
			return
		}
		m.note("switch-seq=" + string('0'+s) + ":" + string(p))
		next = s + 1
	}
	_ = writePacket(c, next, []byte{0x00, 0, 0, 2, 0, 0, 0})
	_ = c.SetDeadline(time.Time{})
	_, _ = io.Copy(c, c)
}

func handshakeResponse(caps uint32, user string) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, caps)
	_ = binary.Write(&b, binary.LittleEndian, uint32(1<<24))
	b.WriteByte(45)
	b.Write(make([]byte, 23))
	b.WriteString(user + "\x00")
	b.WriteByte(0) // empty auth response
	return b.Bytes()
}

// mysqlTLSClient performs the client side of the TLS upgrade and returns
// the TLS connection after authentication.
func mysqlTLSClient(t *testing.T, addr string, roots *x509.CertPool, authSwitch bool) (*tls.Conn, *x509.Certificate) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	seq, p, err := readPacket(c, 1<<16)
	if err != nil || seq != 0 {
		t.Fatalf("greeting: seq %d err %v", seq, err)
	}
	g, err := parseGreeting(p)
	if err != nil {
		t.Fatal(err)
	}
	if g.Capabilities&capClientSSL == 0 {
		t.Fatal("proxy greeting does not advertise CLIENT_SSL")
	}
	caps := uint32(capClientProtocol41 | capClientSSL)
	if err := writePacket(c, 1, handshakeResponse(caps, "")[:sslRequestLen]); err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(c, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("tls handshake: %v", err)
	}
	if err := writePacket(tc, 2, handshakeResponse(caps, "app")); err != nil {
		t.Fatal(err)
	}
	seq, p, err = readPacket(tc, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	if authSwitch {
		if seq != 3 || p[0] != 0xfe {
			t.Fatalf("auth switch: seq %d first byte %x", seq, p[0])
		}
		if err := writePacket(tc, 4, []byte("signature")); err != nil {
			t.Fatal(err)
		}
		if seq, p, err = readPacket(tc, 1<<16); err != nil || seq != 5 {
			t.Fatalf("OK after switch: seq %d err %v", seq, err)
		}
	} else if seq != 3 {
		t.Fatalf("OK sequence %d, want 3", seq)
	}
	if p[0] != 0x00 {
		t.Fatalf("expected OK, got %x", p)
	}
	_ = c.SetDeadline(time.Time{})
	return tc, tc.ConnectionState().PeerCertificates[0]
}

func tlsEcho(t *testing.T, c net.Conn, s string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	defer c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, s); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(s))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != s {
		t.Fatalf("echo: %q %v", buf, err)
	}
}

func issue(t *testing.T, ca *certs.CA) tls.Certificate {
	t.Helper()
	is, err := ca.Issue(context.Background(), certs.Request{Kind: certs.KindServer, CommonName: "proxy", IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c, err := is.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tlsProxy(t *testing.T, backend string, allowPlain bool, src CertSource) *Proxy {
	t.Helper()
	cfg := testConfig(node("a", backend))
	cfg.TLS = &TLSConfig{CertFile: "unused", KeyFile: "unused", AllowPlaintext: allowPlain}
	pr := ProberFunc(func(context.Context, string) ProbeResult {
		return ProbeResult{Healthy: true, Sync: routing.SyncSynced, Latency: time.Millisecond}
	})
	p := startProxy(t, cfg, Options{Prober: pr, Certs: src})
	waitWriter(t, p, "a", 2*time.Second)
	return p
}

func TestTLSTermination(t *testing.T) {
	ca, err := certs.NewCA(certs.CAOptions{CommonName: "test CA", Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, authSwitch := range []bool{false, true} {
		name := "native"
		if authSwitch {
			name = "auth switch"
		}
		t.Run(name, func(t *testing.T) {
			be := newMySQLBackend(t, authSwitch)
			src := &AtomicCertSource{}
			src.Set(issue(t, ca))
			p := tlsProxy(t, be.l.Addr().String(), false, src)
			tc, _ := mysqlTLSClient(t, p.Addr(EndpointWrite), ca.Pool(), authSwitch)
			tlsEcho(t, tc, "SELECT 1 over TLS")
			want := "seq=1,user=app"
			if authSwitch {
				want += ",switch-seq=3:signature"
			}
			if got := be.notes(); got != want {
				t.Fatalf("backend observed %q, want %q", got, want)
			}
		})
	}
}

func TestTLSCertificateHotReload(t *testing.T) {
	ca, err := certs.NewCA(certs.CAOptions{CommonName: "test CA", Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	be := newMySQLBackend(t, false)
	src := &AtomicCertSource{}
	src.Set(issue(t, ca))
	p := tlsProxy(t, be.l.Addr().String(), false, src)

	c1, cert1 := mysqlTLSClient(t, p.Addr(EndpointWrite), ca.Pool(), false)
	src.Set(issue(t, ca)) // renewal
	c2, cert2 := mysqlTLSClient(t, p.Addr(EndpointWrite), ca.Pool(), false)
	if cert1.SerialNumber.Cmp(cert2.SerialNumber) == 0 {
		t.Fatal("new connection still got the old certificate")
	}
	tlsEcho(t, c1, "old connection survives")
	tlsEcho(t, c2, "new connection works")
}

func TestTLSPlaintextClient(t *testing.T) {
	ca, err := certs.NewCA(certs.CAOptions{CommonName: "test CA", Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	plainClient := func(t *testing.T, addr string) (byte, []byte, net.Conn) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := readPacket(c, 1<<16); err != nil {
			t.Fatal(err)
		}
		if err := writePacket(c, 1, handshakeResponse(capClientProtocol41, "app")); err != nil {
			t.Fatal(err)
		}
		seq, p, err := readPacket(c, 1<<16)
		if err != nil {
			t.Fatal(err)
		}
		return seq, p, c
	}
	src := &AtomicCertSource{}
	src.Set(issue(t, ca))
	t.Run("required", func(t *testing.T) {
		be := newMySQLBackend(t, false)
		p := tlsProxy(t, be.l.Addr().String(), false, src)
		seq, pl, _ := plainClient(t, p.Addr(EndpointWrite))
		if code, msg := parseErr(pl); seq != 2 || code != erHandshakeError || !strings.Contains(msg, "TLS is required") {
			t.Fatalf("got seq %d %d %q", seq, code, msg)
		}
	})
	t.Run("allowed", func(t *testing.T) {
		be := newMySQLBackend(t, false)
		p := tlsProxy(t, be.l.Addr().String(), true, src)
		seq, pl, c := plainClient(t, p.Addr(EndpointWrite))
		if seq != 2 || pl[0] != 0x00 {
			t.Fatalf("got seq %d %x", seq, pl)
		}
		tlsEcho(t, c, "plain")
		if got := be.notes(); got != "seq=1,user=app" {
			t.Fatalf("backend observed %q", got)
		}
	})
}

func TestTLSHandshakeTimeout(t *testing.T) {
	ca, err := certs.NewCA(certs.CAOptions{CommonName: "test CA", Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	be := newMySQLBackend(t, false)
	src := &AtomicCertSource{}
	src.Set(issue(t, ca))
	cfg := testConfig(node("a", be.l.Addr().String()))
	cfg.TLS = &TLSConfig{CertFile: "x", KeyFile: "y"}
	cfg.Limits.HandshakeTimeout = Duration(200 * time.Millisecond)
	p := startProxy(t, cfg, Options{Certs: src})
	waitWriter(t, p, "a", 2*time.Second)
	c, err := net.Dial("tcp", p.Addr(EndpointWrite))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := readPacket(c, 1<<16); err != nil {
		t.Fatal(err)
	}
	// Send nothing: the proxy must give up after the handshake timeout.
	start := time.Now()
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected close")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("handshake timeout took %s", el)
	}
	waitFor(t, time.Second, "connection released", func() bool { return p.Status().Connections.Write == 0 })
}

func TestFileCertSourceReload(t *testing.T) {
	ca, err := certs.NewCA(certs.CAOptions{CommonName: "test CA", Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func() *x509.Certificate {
		is, err := ca.Issue(context.Background(), certs.Request{Kind: certs.KindServer, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, Validity: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		k, _ := is.KeyPEM()
		if err := os.WriteFile(filepath.Join(dir, "c.pem"), is.ChainPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "k.pem"), k, 0o600); err != nil {
			t.Fatal(err)
		}
		return is.Cert
	}
	first := write()
	src, err := NewFileCertSource(filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem"))
	if err != nil {
		t.Fatal(err)
	}
	leaf := func() *x509.Certificate {
		c, err := src.GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		x, _ := x509.ParseCertificate(c.Certificate[0])
		return x
	}
	if leaf().SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatal("wrong initial certificate")
	}
	second := write()
	if err := src.Reload(); err != nil {
		t.Fatal(err)
	}
	if leaf().SerialNumber.Cmp(second.SerialNumber) != 0 {
		t.Fatal("reload did not pick up the new certificate")
	}
	_ = os.WriteFile(filepath.Join(dir, "k.pem"), []byte("garbage"), 0o600)
	if err := src.Reload(); err == nil {
		t.Fatal("reload of broken key succeeded")
	}
	if leaf().SerialNumber.Cmp(second.SerialNumber) != 0 {
		t.Fatal("failed reload replaced the certificate")
	}
	var empty AtomicCertSource
	if _, err := empty.GetCertificate(nil); err == nil {
		t.Fatal("empty source returned a certificate")
	}
}
