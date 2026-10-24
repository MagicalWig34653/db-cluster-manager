// SPDX-License-Identifier: AGPL-3.0-only

package managertls

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

type memoryStore struct {
	mu     sync.Mutex
	values map[string][]byte
}

func newMemory() *memoryStore { return &memoryStore{values: map[string][]byte{}} }
func (s *memoryStore) Get(ctx context.Context, k string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, ok := s.values[k]
	if !ok {
		return nil, ErrMissing
	}
	return bytes.Clone(v), nil
}
func (s *memoryStore) Put(ctx context.Context, k string, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.values[k] = bytes.Clone(v)
	return nil
}
func (s *memoryStore) Delete(ctx context.Context, k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(s.values, k)
	return nil
}

type handlerTransport struct{ h http.Handler }

func (rt handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	rt.h.ServeHTTP(w, r)
	return w.Result(), nil
}

// fakeCA implements the RFC 8555 directory/account/order/authorization/finalize
// lifecycle, and validates the actual presented HTTP or RFC 8737 challenge.
// Tests route the real x/crypto client through httptest recorders or a local
// httptest.Server. No public CA, DNS, Docker or infrastructure is contacted.
type fakeCA struct {
	t          *testing.T
	base, kind string
	replica    *Manager
	key        *ecdsa.PrivateKey
	ca         *x509.Certificate
	account    crypto.PublicKey
	cert       []byte
	issued     int
	accepted   bool
	fail       bool
	block      bool
	entered    chan struct{}
}

func newFakeCA(t *testing.T, kind string) *fakeCA {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake ACME CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeCA{t: t, base: "https://fake-ca.invalid", kind: kind, key: k, ca: ca}
}
func (f *fakeCA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Replay-Nonce", "fake-nonce")
	w.Header().Set("Content-Type", "application/json")
	respond := func(status int, v any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
	var payload []byte
	if r.Method == http.MethodPost {
		var j struct {
			Payload string `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
			f.t.Error(err)
			w.WriteHeader(400)
			return
		}
		var err error
		payload, err = base64.RawURLEncoding.DecodeString(j.Payload)
		if err != nil {
			f.t.Error(err)
			w.WriteHeader(400)
			return
		}
	}
	order := func(status string) map[string]any {
		return map[string]any{"status": status, "identifiers": []map[string]string{{"type": "dns", "value": "manager.example.test"}}, "authorizations": []string{f.base + "/authz"}, "finalize": f.base + "/finalize", "certificate": f.base + "/cert"}
	}
	switch r.URL.Path {
	case "/directory":
		respond(200, map[string]string{"newNonce": f.base + "/nonce", "newAccount": f.base + "/account", "newOrder": f.base + "/order"})
	case "/nonce":
		w.WriteHeader(200)
	case "/account":
		w.Header().Set("Location", f.base+"/account/1")
		respond(201, map[string]any{"status": "valid", "contact": []string{"mailto:ops@example.test"}})
	case "/order":
		if f.block {
			close(f.entered)
			<-r.Context().Done()
			return
		}
		if f.fail {
			respond(400, map[string]string{"type": "urn:ietf:params:acme:error:rejectedIdentifier", "detail": "fake rejection"})
			return
		}
		w.Header().Set("Location", f.base+"/order/1")
		respond(201, order("pending"))
	case "/authz":
		status := "pending"
		if f.accepted {
			status = "valid"
		}
		respond(200, map[string]any{"status": status, "identifier": map[string]string{"type": "dns", "value": "manager.example.test"}, "challenges": []map[string]string{{"type": f.kind, "url": f.base + "/challenge", "token": "test-token", "status": status}}})
	case "/challenge":
		if !f.checkChallenge() {
			respond(400, map[string]string{"type": "urn:ietf:params:acme:error:unauthorized"})
			return
		}
		f.accepted = true
		respond(200, map[string]string{"type": f.kind, "url": f.base + "/challenge", "token": "test-token", "status": "valid"})
	case "/finalize":
		if !f.accepted {
			f.t.Error("finalized before challenge validation")
		}
		var req struct {
			CSR string `json:"csr"`
		}
		_ = json.Unmarshal(payload, &req)
		raw, err := base64.RawURLEncoding.DecodeString(req.CSR)
		if err != nil {
			f.t.Error(err)
			w.WriteHeader(400)
			return
		}
		csr, err := x509.ParseCertificateRequest(raw)
		if err != nil {
			f.t.Error(err)
			w.WriteHeader(400)
			return
		}
		if err := csr.CheckSignature(); err != nil {
			f.t.Error(err)
		}
		tpl := &x509.Certificate{SerialNumber: big.NewInt(int64(2 + f.issued)), DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, tpl, f.ca, csr.PublicKey, f.key)
		if err != nil {
			f.t.Error(err)
		}
		f.cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		f.issued++
		w.Header().Set("Location", f.base+"/order/1")
		respond(200, order("valid"))
	case "/cert":
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write(f.cert)
	default:
		f.t.Errorf("unexpected fake CA request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}

func (f *fakeCA) checkChallenge() bool {
	f.t.Helper()
	thumb, err := acme.JWKThumbprint(f.account)
	if err != nil {
		f.t.Error(err)
		return false
	}
	response := "test-token." + thumb
	if f.kind == "http-01" {
		r := httptest.NewRequest("GET", "http://manager.example.test/.well-known/acme-challenge/test-token", nil)
		w := httptest.NewRecorder()
		f.replica.HTTPHandler().ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != response {
			f.t.Errorf("HTTP challenge: %d %q", w.Code, w.Body.String())
			return false
		}
		return true
	}
	c, err := f.replica.GetCertificate(&tls.ClientHelloInfo{ServerName: "manager.example.test", SupportedProtos: []string{acme.ALPNProto}})
	if err != nil {
		f.t.Error(err)
		return false
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		f.t.Error(err)
		return false
	}
	if err := leaf.VerifyHostname("manager.example.test"); err != nil {
		f.t.Error(err)
		return false
	}
	hash := sha256.Sum256([]byte(response))
	for _, ext := range leaf.Extensions {
		if ext.Id.String() == "1.3.6.1.5.5.7.1.31" && ext.Critical && bytes.Equal(ext.Value, append([]byte{4, 32}, hash[:]...)) {
			return true
		}
	}
	f.t.Error("missing critical acmeIdentifier digest")
	return false
}

func setupFake(t *testing.T, kind string, network bool) (*Manager, *Manager, *fakeCA, *memoryStore) {
	t.Helper()
	ca := newFakeCA(t, kind)
	var transport http.RoundTripper = handlerTransport{h: ca}
	if network {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("sandbox blocks local fake ACME listener: %v", err)
		}
		server := &httptest.Server{Listener: l, Config: &http.Server{Handler: ca}}
		server.Start()
		t.Cleanup(server.Close)
		ca.base = server.URL
		transport = server.Client().Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
	}
	factory := func(directory string, key crypto.Signer) Client {
		ca.account = key.Public()
		return &protocolClient{c: &acme.Client{DirectoryURL: directory, Key: key, HTTPClient: &http.Client{Transport: transport, Timeout: time.Second}}}
	}
	// Production config still requires HTTPS; socket tests replace only the
	// transport endpoint, as a local fake does not need a trusted public name.
	cfg := Config{DirectoryURL: "https://fake-ca.invalid/directory", Email: "ops@example.test", Domains: []string{"manager.example.test"}, Challenge: kind}
	wrapped := func(_ string, key crypto.Signer) Client { return factory(ca.base+"/directory", key) }
	s := newMemory()
	m, err := New(cfg, s, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	follower, err := New(cfg, s, func(string, crypto.Signer) Client { t.Fatal("follower contacted CA"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	ca.replica = follower
	return m, follower, ca, s
}

func TestRFC8555Lifecycle(t *testing.T) {
	for _, kind := range []string{"http-01", "tls-alpn-01"} {
		t.Run(kind, func(t *testing.T) { testLifecycle(t, kind, false) })
	}
}
func TestRFC8555HTTptestServer(t *testing.T) {
	for _, kind := range []string{"http-01", "tls-alpn-01"} {
		t.Run(kind, func(t *testing.T) { testLifecycle(t, kind, true) })
	}
}
func testLifecycle(t *testing.T, kind string, network bool) {
	m, follower, ca, s := setupFake(t, kind, network)
	ctx := context.Background()
	if err := m.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if ca.issued != 1 {
		t.Fatalf("issued %d", ca.issued)
	}
	for _, reader := range []*Manager{m, follower} {
		c, err := reader.GetCertificate(&tls.ClientHelloInfo{ServerName: "manager.example.test"})
		if err != nil {
			t.Fatal(err)
		}
		if c.Leaf.VerifyHostname("manager.example.test") != nil {
			t.Fatal("wrong certificate")
		}
		if _, err := reader.GetCertificate(&tls.ClientHelloInfo{ServerName: "unconfigured.example.test"}); err == nil {
			t.Fatal("unconfigured SNI accepted")
		}
		if _, err := reader.GetCertificate(&tls.ClientHelloInfo{ServerName: "manager.example.test", SupportedProtos: []string{acme.ALPNProto}}); err == nil {
			t.Fatal("stale challenge certificate served")
		}
	}
	if err := m.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if ca.issued != 1 {
		t.Fatal("unnecessary renewal")
	}
	// Make the shared certificate due, then create a fresh worker as on election.
	var b bundle
	if err := m.get(ctx, "certificate", &b); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(b.Certificate, b.Key)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(99), DNSNames: m.config.Domains, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.ca, cert.PrivateKey.(crypto.Signer).Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	b.Certificate = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := m.put(ctx, "certificate", b); err != nil {
		t.Fatal(err)
	}
	accountBefore, err := s.Get(ctx, m.key("account"))
	if err != nil {
		t.Fatal(err)
	}
	// A CA outage must not replace the still-valid certificate.
	ca.fail = true
	if err := m.Ensure(ctx); err == nil {
		t.Fatal("expected CA failure")
	}
	if _, err := follower.GetCertificate(&tls.ClientHelloInfo{ServerName: "manager.example.test"}); err != nil {
		t.Fatal(err)
	}
	ca.fail = false
	ca.accepted = false
	successor, err := New(m.config, s, m.factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := successor.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	accountAfter, _ := s.Get(ctx, m.key("account"))
	if !bytes.Equal(accountBefore, accountAfter) || ca.issued != 2 {
		t.Fatal("successor did not reuse account/renew")
	}
}

func TestCancellationStopsWorker(t *testing.T) {
	m, _, ca, _ := setupFake(t, "http-01", false)
	ca.block = true
	ca.entered = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx, nil); close(done) }()
	select {
	case <-ca.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not reach CA")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop on leadership loss")
	}
}

func TestChallengeHandlerScope(t *testing.T) {
	m, _, _, _ := setupFake(t, "http-01", false)
	ctx := context.Background()
	r := challengeRecord{Domain: "manager.example.test", Token: "token", Response: "proof", Expires: time.Now().Add(time.Minute)}
	if err := m.put(ctx, "challenge/manager.example.test", r); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, host, path string
		code               int
	}{
		{"GET", "manager.example.test", "/.well-known/acme-challenge/token", 200},
		{"GET", "manager.example.test:80", "/.well-known/acme-challenge/token", 200},
		{"POST", "manager.example.test", "/.well-known/acme-challenge/token", 404},
		{"GET", "other.example.test", "/.well-known/acme-challenge/token", 404},
		{"GET", "manager.example.test", "/api/v1/health", 404},
		{"GET", "manager.example.test", "/.well-known/acme-challenge/wrong", 404},
	} {
		req := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		w := httptest.NewRecorder()
		m.HTTPHandler().ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Errorf("%+v: %d", tc, w.Code)
		}
	}
	r.Expires = time.Now().Add(-time.Minute)
	_ = m.put(ctx, "challenge/manager.example.test", r)
	w := httptest.NewRecorder()
	m.HTTPHandler().ServeHTTP(w, httptest.NewRequest("GET", "http://manager.example.test/.well-known/acme-challenge/token", nil))
	if w.Code != 404 {
		t.Fatal("expired challenge served")
	}
}

func TestConfigValidation(t *testing.T) {
	good := Config{Email: "ops@example.test", Domains: []string{"manager.example.test"}}
	if good.Defaults().DirectoryURL != StagingDirectory || good.Validate() != nil {
		t.Fatal("staging default invalid")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.DirectoryURL = "http://ca.example.test/directory" },
		func(c *Config) { c.Email = "" }, func(c *Config) { c.Domains = nil },
		func(c *Config) { c.Domains = []string{"*.example.test"} },
		func(c *Config) { c.Domains = []string{"manager.example.test", "manager.example.test"} },
		func(c *Config) { c.Domains = []string{"127.0.0.1"} },
		func(c *Config) { c.Challenge = "dns-01" }, func(c *Config) { c.HTTPListen = "invalid" },
	} {
		c := good
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

// Ensure the test transport doesn't accidentally escape to a real HTTP client.
var _ http.RoundTripper = handlerTransport{}

func TestTLSHandshakeWithSharedCertificate(t *testing.T) {
	m, follower, ca, _ := setupFake(t, "tls-alpn-01", false)
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.ca)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	_ = left.SetDeadline(time.Now().Add(2 * time.Second))
	_ = right.SetDeadline(time.Now().Add(2 * time.Second))
	server := tls.Server(left, follower.TLSConfig())
	client := tls.Client(right, &tls.Config{RootCAs: roots, ServerName: "manager.example.test", MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}})
	done := make(chan error, 1)
	go func() { done <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if client.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("ordinary TLS ALPN failed")
	}
}

func TestALPNIsolationAndExpiredProof(t *testing.T) {
	m, _, _, _ := setupFake(t, "tls-alpn-01", false)
	client := NewClient("https://fake-ca.invalid/directory", func() crypto.Signer {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}())
	cert, err := client.ALPNCertificate("test-token", "manager.example.test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodeCertificate(cert)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := challengeRecord{Domain: "manager.example.test", Bundle: b, Expires: time.Now().Add(time.Minute)}
	if err := m.put(ctx, "challenge/manager.example.test", r); err != nil {
		t.Fatal(err)
	}
	for _, protos := range [][]string{nil, {"h2"}, {acme.ALPNProto, "h2"}} {
		if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "manager.example.test", SupportedProtos: protos}); err == nil {
			t.Fatalf("challenge leaked with ALPN %v", protos)
		}
	}
	if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "manager.example.test", SupportedProtos: []string{acme.ALPNProto}}); err != nil {
		t.Fatal(err)
	}
	r.Expires = time.Now().Add(-time.Second)
	if err := m.put(ctx, "challenge/manager.example.test", r); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "manager.example.test", SupportedProtos: []string{acme.ALPNProto}}); err == nil {
		t.Fatal("expired ALPN proof served")
	}
}
