// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type sftpTestServer struct {
	addr     string
	root     string
	hostSig  ssh.Signer
	fp       string
	password string
	userKey  ssh.PublicKey
	ln       net.Listener
	wg       sync.WaitGroup
	mu       sync.Mutex
	conns    []net.Conn
}

func newSigner(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemb := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	s, err := ssh.ParsePrivateKey(pemb)
	if err != nil {
		t.Fatal(err)
	}
	return s, pemb
}

func startSFTP(t *testing.T) *sftpTestServer {
	t.Helper()
	hs, _ := newSigner(t)
	s := &sftpTestServer{root: t.TempDir(), hostSig: hs, fp: ssh.FingerprintSHA256(hs.PublicKey()), password: "pw"}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == "backup" && string(p) == s.password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if s.userKey != nil && c.User() == "backup" && string(k.Marshal()) == string(s.userKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(hs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln, s.addr = ln, ln.Addr().String()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, nc)
			s.mu.Unlock()
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(nc, cfg) }()
		}
	}()
	t.Cleanup(func() { s.stop() })
	return s
}

func (s *sftpTestServer) stop() {
	_ = s.ln.Close()
	s.dropConns()
	s.wg.Wait()
}

func (s *sftpTestServer) dropConns() {
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
	s.mu.Unlock()
}

func (s *sftpTestServer) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for r := range creqs {
				ok := r.Type == "subsystem" && len(r.Payload) >= 8 && string(r.Payload[4:]) == "sftp"
				_ = r.Reply(ok, nil)
				if ok {
					go func() {
						srv, err := sftp.NewServer(ch)
						if err == nil {
							_ = srv.Serve()
						}
						_ = ch.Close()
					}()
				}
			}
		}()
	}
}

func (s *sftpTestServer) cfg() SFTPConfig {
	return SFTPConfig{Addr: s.addr, User: "backup", Password: s.password, Dir: filepath.Join(s.root, "bk"),
		HostKeyFingerprint: s.fp, DialTimeout: 5 * time.Second}
}

func TestSFTPSharedSuite(t *testing.T) {
	s := startSFTP(t)
	b, err := NewSFTPBackend(s.cfg())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	testBackend(t, b)
	des, _ := os.ReadDir(filepath.Join(s.root, "bk"))
	for _, d := range des {
		if strings.HasPrefix(d.Name(), ".tmp-") {
			t.Fatalf("leftover temp %s", d.Name())
		}
	}
}

func TestSFTPHostKeyMismatchRejected(t *testing.T) {
	s := startSFTP(t)
	other, _ := newSigner(t)
	c := s.cfg()
	c.HostKeyFingerprint = ssh.FingerprintSHA256(other.PublicKey())
	b, _ := NewSFTPBackend(c)
	err := b.Put(context.Background(), "x", strings.NewReader("v"))
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(s.root, "bk")); serr == nil {
		t.Fatal("remote directory created despite host key mismatch")
	}
}

func TestSFTPKnownHostsFile(t *testing.T) {
	s := startSFTP(t)
	dir := t.TempDir()
	write := func(line string) string {
		p := filepath.Join(dir, "kh")
		if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c := s.cfg()
	c.HostKeyFingerprint = ""
	c.KnownHostsFile = write(knownHostsLine(s.addr, s.hostSig.PublicKey()))
	b, err := NewSFTPBackend(c)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Put(context.Background(), "x", strings.NewReader("v")); err != nil {
		t.Fatal(err)
	}
	other, _ := newSigner(t)
	c.KnownHostsFile = write(knownHostsLine(s.addr, other.PublicKey()))
	b2, _ := NewSFTPBackend(c)
	if err := b2.Put(context.Background(), "x", strings.NewReader("v")); err == nil {
		t.Fatal("known_hosts mismatch accepted")
	}
	c.KnownHostsFile = write("# no entry for this host")
	b3, _ := NewSFTPBackend(c)
	if err := b3.Put(context.Background(), "x", strings.NewReader("v")); err == nil {
		t.Fatal("unknown host accepted")
	}
}

func knownHostsLine(addr string, k ssh.PublicKey) string {
	host, port, _ := net.SplitHostPort(addr)
	return "[" + host + "]:" + port + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func TestSFTPAuth(t *testing.T) {
	s := startSFTP(t)
	c := s.cfg()
	c.Password = "wrong"
	b, _ := NewSFTPBackend(c)
	if err := b.Put(context.Background(), "x", strings.NewReader("v")); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("wrong password: %v", err)
	}
	// Key auth.
	us, pemb := newSigner(t)
	s.userKey = us.PublicKey()
	c = s.cfg()
	c.Password, c.PrivateKeyPEM = "", pemb
	kb, err := NewSFTPBackend(c)
	if err != nil {
		t.Fatal(err)
	}
	defer kb.Close()
	testBackend(t, kb)
	// Unauthorized key.
	_, other := newSigner(t)
	c.PrivateKeyPEM = other
	ob, _ := NewSFTPBackend(c)
	if err := ob.Put(context.Background(), "x", strings.NewReader("v")); err == nil {
		t.Fatal("unauthorized key accepted")
	}
}

func TestSFTPConfigValidation(t *testing.T) {
	good := SFTPConfig{Addr: "h:22", User: "u", Dir: "/d", Password: "p", HostKeyFingerprint: "SHA256:abc"}
	if _, err := NewSFTPBackend(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*SFTPConfig){
		"no host key":   func(c *SFTPConfig) { c.HostKeyFingerprint = "" },
		"both":          func(c *SFTPConfig) { c.KnownHostsFile = "/x" },
		"bad fp":        func(c *SFTPConfig) { c.HostKeyFingerprint = "MD5:aa" },
		"no auth":       func(c *SFTPConfig) { c.Password = "" },
		"bad key":       func(c *SFTPConfig) { c.PrivateKeyPEM = []byte("junk") },
		"no dir":        func(c *SFTPConfig) { c.Dir = "" },
		"missing khost": func(c *SFTPConfig) { c.HostKeyFingerprint = ""; c.KnownHostsFile = "/nonexistent/kh" },
	} {
		c := good
		mut(&c)
		if _, err := NewSFTPBackend(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSFTPAtomicPut(t *testing.T) {
	s := startSFTP(t)
	b, _ := NewSFTPBackend(s.cfg())
	defer b.Close()
	ctx := context.Background()
	if err := b.Put(ctx, "x", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	// Failing upload keeps the old object and leaves no temp file.
	r := io.MultiReader(strings.NewReader("partial"), failReader{})
	if err := b.Put(ctx, "x", r); err == nil {
		t.Fatal("expected error")
	}
	got, _ := os.ReadFile(filepath.Join(s.root, "bk", "x"))
	if string(got) != "old" {
		t.Fatalf("old content lost: %q", got)
	}
	// While an upload is in flight, the target is unchanged and the temp
	// file is hidden from List.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- b.Put(ctx, "x", pr) }()
	_, _ = pw.Write([]byte("new-part-1"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		des, _ := os.ReadDir(filepath.Join(s.root, "bk"))
		if len(des) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got, _ := os.ReadFile(filepath.Join(s.root, "bk", "x")); string(got) != "old" {
		t.Fatalf("target changed before rename: %q", got)
	}
	if l, err := b.List(ctx, ""); err != nil || len(l) != 1 || l[0].Name != "x" {
		t.Fatalf("temp visible: %v %+v", err, l)
	}
	_, _ = pw.Write([]byte("-part-2"))
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(s.root, "bk", "x"))
	if string(got) != "new-part-1-part-2" {
		t.Fatalf("got %q", got)
	}
	des, _ := os.ReadDir(filepath.Join(s.root, "bk"))
	if len(des) != 1 {
		t.Fatalf("leftovers: %v", des)
	}
}

func TestSFTPPathTraversalRejected(t *testing.T) {
	s := startSFTP(t)
	b, _ := NewSFTPBackend(s.cfg())
	defer b.Close()
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(s.root, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"../secret", "..", ".", "a/../../secret", "/etc/passwd", "..\\secret", ".tmp-x", "a\x00b"} {
		if err := b.Put(ctx, n, strings.NewReader("x")); !errors.Is(err, ErrInvalidName) {
			t.Errorf("put %q: %v", n, err)
		}
		if _, err := b.Get(ctx, n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("get %q: %v", n, err)
		}
		if err := b.Delete(ctx, n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("delete %q: %v", n, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(s.root, "secret")); string(got) != "s" {
		t.Fatal("outside file touched")
	}
}

func TestSFTPReconnectAfterDrop(t *testing.T) {
	s := startSFTP(t)
	b, _ := NewSFTPBackend(s.cfg())
	defer b.Close()
	ctx := context.Background()
	if err := b.Put(ctx, "a", strings.NewReader("1")); err != nil {
		t.Fatal(err)
	}
	s.dropConns()
	time.Sleep(50 * time.Millisecond)
	if err := b.Put(ctx, "b", strings.NewReader("2")); err != nil {
		t.Fatalf("after drop: %v", err)
	}
}

func TestSFTPBoundedConcurrency(t *testing.T) {
	s := startSFTP(t)
	c := s.cfg()
	c.MaxConcurrency = 2
	b, _ := NewSFTPBackend(c)
	defer b.Close()
	ctx := context.Background()
	if err := b.Put(ctx, "o", strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	r1, err := b.Get(ctx, "o")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := b.Get(ctx, "o")
	if err != nil {
		t.Fatal(err)
	}
	// Both slots are held by open readers: a third op blocks until ctx ends.
	tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := b.Get(tctx, "o"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third get: %v", err)
	}
	r1.Close()
	r3, err := b.Get(ctx, "o")
	if err != nil {
		t.Fatal(err)
	}
	r3.Close()
	r2.Close()
}

func TestSFTPContextCancellation(t *testing.T) {
	s := startSFTP(t)
	b, _ := NewSFTPBackend(s.cfg())
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Put(ctx, "c", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("put: %v", err)
	}
	if _, err := b.List(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("list: %v", err)
	}
	if err := b.Delete(ctx, "c"); !errors.Is(err, context.Canceled) {
		t.Fatalf("delete: %v", err)
	}
	// Cancel mid-upload: temp file removed, no object.
	ctx2, cancel2 := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- b.Put(ctx2, "mid", pr) }()
	_, _ = pw.Write([]byte("abc"))
	cancel2()
	go func() { _, _ = pw.Write([]byte("def")) }()
	err := <-done
	pr.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-upload: %v", err)
	}
	des, _ := os.ReadDir(filepath.Join(s.root, "bk"))
	if len(des) != 0 {
		t.Fatalf("leftovers: %v", des)
	}
	// Cancel during dial/handshake of a server that never answers.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	c := s.cfg()
	c.Addr = ln.Addr().String()
	hb, _ := NewSFTPBackend(c)
	ctx3, cancel3 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel3()
	start := time.Now()
	if err := hb.Put(ctx3, "h", strings.NewReader("x")); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("handshake cancel: %v after %v", err, time.Since(start))
	}
}
