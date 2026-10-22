// SPDX-License-Identifier: AGPL-3.0-only

// Package fakemysql is a minimal MySQL/MariaDB server on loopback for tests:
// greeting v10, mysql_native_password, COM_QUERY with text resultsets for
// SHOW GLOBAL STATUS and optional TLS. It is just enough for the real
// go-sql-driver/mysql to connect and query. It is test support, not a
// MariaDB emulation.
package fakemysql

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Capability flags used by the fake.
const (
	capLongPassword     = 1
	capProtocol41       = 0x200
	capSSL              = 0x800
	capTransactions     = 0x2000
	capSecureConnection = 0x8000
	capPluginAuth       = 0x80000
)

// Server is a minimal MySQL server on loopback: greeting v10,
// mysql_native_password, COM_QUERY with text resultsets, optional TLS. It is
// just enough for the real go-sql-driver/mysql to connect and query.
type Server struct {
	t         *testing.T
	l         net.Listener
	wg        sync.WaitGroup
	user      string
	password  string
	TLSConfig *tls.Config // non-nil: SSL is offered

	// behaviour
	Mode string // "" normal, "silent" (no greeting), "garbage" (junk after auth), "badrows"
	// FailOn makes a query containing it fail with an error that echoes the
	// query, as MariaDB's syntax errors quote the statement.
	FailOn   string
	vars     map[string]string // answered for SHOW GLOBAL STATUS
	Accepted atomic.Int32
	Queries  atomic.Int32
	QueryLog chan string
}

func New(t *testing.T, user, password string, vars map[string]string, opts ...func(*Server)) *Server {
	t.Helper()
	return listen(t, "tcp", "127.0.0.1:0", user, password, vars, opts...)
}

// WithFailOn sets Server.FailOn before the server starts serving.
func WithFailOn(substr string) func(*Server) { return func(s *Server) { s.FailOn = substr } }

// NewUnix is New on a unix socket at path.
func NewUnix(t *testing.T, path, user, password string, vars map[string]string, opts ...func(*Server)) *Server {
	t.Helper()
	return listen(t, "unix", path, user, password, vars, opts...)
}

func listen(t *testing.T, network, addr, user, password string, vars map[string]string, opts ...func(*Server)) *Server {
	t.Helper()
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	f := &Server{t: t, l: l, user: user, password: password, vars: vars, QueryLog: make(chan string, 64)}
	for _, o := range opts {
		o(f)
	}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(func() { _ = l.Close(); f.wg.Wait() })
	return f
}

func (f *Server) Addr() string { return f.l.Addr().String() }

// StopListening closes the listener so new connections are refused.
func (f *Server) StopListening() { _ = f.l.Close() }

func (f *Server) serve() {
	defer f.wg.Done()
	for {
		c, err := f.l.Accept()
		if err != nil {
			return
		}
		f.Accepted.Add(1)
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer c.Close()
			f.handle(c)
		}()
	}
}

func writePkt(w io.Writer, seq byte, payload []byte) error {
	h := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	_, err := w.Write(append(h, payload...))
	return err
}

func readPkt(r io.Reader) (byte, []byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	p := make([]byte, n)
	_, err := io.ReadFull(r, p)
	return h[3], p, err
}

func lenenc(s string) []byte {
	return append([]byte{byte(len(s))}, s...) // all strings here are < 251 bytes
}

func (f *Server) handle(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if f.Mode == "silent" {
		// Never greet; hold the connection until the client gives up.
		_, _ = io.Copy(io.Discard, c)
		return
	}
	scramble := make([]byte, 20)
	_, _ = rand.Read(scramble)
	for i := range scramble { // avoid NUL and keep it printable
		scramble[i] = scramble[i]%90 + 33
	}
	caps := uint32(capLongPassword | capProtocol41 | capTransactions | capSecureConnection | capPluginAuth)
	if f.TLSConfig != nil {
		caps |= capSSL
	}
	var g bytes.Buffer
	g.WriteByte(10)
	g.WriteString("10.11.6-MariaDB-fake\x00")
	g.Write([]byte{1, 0, 0, 0})
	g.Write(scramble[:8])
	g.WriteByte(0)
	g.Write([]byte{byte(caps), byte(caps >> 8)})
	g.WriteByte(33)
	g.Write([]byte{2, 0})
	g.Write([]byte{byte(caps >> 16), byte(caps >> 24)})
	g.WriteByte(21) // auth data length
	g.Write(make([]byte, 10))
	g.Write(scramble[8:])
	g.WriteByte(0)
	g.WriteString("mysql_native_password\x00")
	if writePkt(c, 0, g.Bytes()) != nil {
		return
	}
	seq, p, err := readPkt(c)
	if err != nil || len(p) < 32 {
		return
	}
	var rw io.ReadWriter = c
	if binary.LittleEndian.Uint32(p)&capSSL != 0 && len(p) == 32 {
		tc := tls.Server(c, f.TLSConfig)
		if tc.Handshake() != nil {
			return
		}
		rw = tc
		seq, p, err = readPkt(tc)
		if err != nil {
			return
		}
	}
	// user NUL, lenenc auth, plugin name...
	rest := p[32:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return
	}
	user := string(rest[:i])
	rest = rest[i+1:]
	if len(rest) == 0 || int(rest[0])+1 > len(rest) {
		return
	}
	auth := rest[1 : 1+int(rest[0])]
	if user != f.user || !bytes.Equal(auth, nativeScramble(f.password, scramble)) {
		_ = writePkt(rw, seq+1, errPacket(1045, "28000", "Access denied for user '"+user+"'@'127.0.0.1' (using password: YES)"))
		return
	}
	if writePkt(rw, seq+1, []byte{0, 0, 0, 2, 0, 0, 0}) != nil {
		return
	}
	if f.Mode == "garbage" {
		_, _ = rw.Write([]byte{0xff, 0xff, 0xff, 1, 0xde, 0xad})
		return
	}
	for {
		_, p, err := readPkt(rw)
		if err != nil || len(p) == 0 {
			return
		}
		switch p[0] {
		case 0x01: // COM_QUIT
			return
		case 0x0e: // COM_PING
			_ = writePkt(rw, 1, []byte{0, 0, 0, 2, 0, 0, 0})
		case 0x03: // COM_QUERY
			q := string(p[1:])
			f.Queries.Add(1)
			select {
			case f.QueryLog <- q:
			default:
			}
			if f.FailOn != "" && strings.Contains(q, f.FailOn) {
				_ = writePkt(rw, 1, errPacket(1064, "42000", "You have an error in your SQL syntax near '"+q+"'"))
			} else if strings.HasPrefix(strings.ToUpper(q), "SHOW GLOBAL STATUS") {
				f.sendStatus(rw)
			} else {
				_ = writePkt(rw, 1, []byte{0, 0, 0, 2, 0, 0, 0})
			}
		default:
			_ = writePkt(rw, 1, errPacket(1047, "08S01", "Unknown command"))
		}
	}
}

func (f *Server) sendStatus(w io.Writer) {
	colDef := func(name string) []byte {
		var b bytes.Buffer
		for _, s := range []string{"def", "", "", "", name, name} {
			b.Write(lenenc(s))
		}
		b.WriteByte(0x0c)
		b.Write([]byte{33, 0})
		b.Write([]byte{0xff, 0, 0, 0})
		b.WriteByte(0xfd)
		b.Write([]byte{0, 0, 0, 0, 0})
		return b.Bytes()
	}
	eof := []byte{0xfe, 0, 0, 2, 0}
	seq := byte(1)
	send := func(p []byte) { _ = writePkt(w, seq, p); seq++ }
	send([]byte{2})
	send(colDef("Variable_name"))
	send(colDef("Value"))
	send(eof)
	if f.Mode == "badrows" {
		send([]byte{0xfb, 0xfb}) // two NULLs
	}
	for k, v := range f.vars {
		send(append(lenenc(k), lenenc(v)...))
	}
	send(eof)
}

func errPacket(code uint16, state, msg string) []byte {
	b := []byte{0xff, byte(code), byte(code >> 8), '#'}
	b = append(b, state...)
	return append(b, msg...)
}

// nativeScramble = SHA1(pw) XOR SHA1(scramble + SHA1(SHA1(pw))).
func nativeScramble(pw string, scramble []byte) []byte {
	if pw == "" {
		return nil
	}
	s1 := sha1.Sum([]byte(pw))
	s2 := sha1.Sum(s1[:])
	h := sha1.New()
	h.Write(scramble)
	h.Write(s2[:])
	s3 := h.Sum(nil)
	out := make([]byte, 20)
	for i := range out {
		out[i] = s1[i] ^ s3[i]
	}
	return out
}

// selfSigned returns a server TLS config and a CA pool trusting its cert.
func SelfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake-mariadb"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames: []string{"fake-mariadb"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	_ = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}, pool
}

func SyncedVars() map[string]string {
	return map[string]string{
		"wsrep_cluster_size": "3", "wsrep_cluster_status": "Primary", "wsrep_local_state": "4",
		"wsrep_local_state_comment": "Synced", "wsrep_ready": "ON", "wsrep_connected": "ON",
		"wsrep_cluster_state_uuid": "uuid-1", "wsrep_last_committed": "42", "wsrep_flow_control_paused": "0.000000",
	}
}

func WithVars(base map[string]string, kv ...string) map[string]string {
	m := map[string]string{}
	for k, v := range base {
		m[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}
