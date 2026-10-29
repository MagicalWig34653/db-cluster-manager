// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
)

const inst = "inst-1"

type authority struct {
	ca       *certs.CA
	validity time.Duration
}

func (a *authority) SignAgent(ctx context.Context, host string, rt agentrt.Runtime, csr *x509.CertificateRequest) (*certs.Signed, error) {
	return a.ca.SignCSR(ctx, csr, certs.Request{Kind: certs.KindPeer, CommonName: "dbcm agent " + host,
		IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, URIs: []*url.URL{certs.RuntimeAgentURI(inst, host, string(rt))}, Validity: a.validity})
}
func (a *authority) CABundlePEM() []byte       { return a.ca.CertificatePEM() }
func (a *authority) ClientCAs() *x509.CertPool { return a.ca.Pool() }

type registry struct {
	mu       sync.Mutex
	tokens   map[string]*Token
	serials  map[string]string
	revoked  map[string]bool
	sourceOK func(host string, ip net.IP) bool
	now      time.Time
}

func (r *registry) ConsumeToken(_ context.Context, plain, host string, rt agentrt.Runtime, src net.IP) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, secret, err := ParseToken(plain)
	if err != nil {
		return err
	}
	t, ok := r.tokens[id]
	if !ok {
		return ErrTokenInvalid
	}
	if r.sourceOK != nil && !r.sourceOK(host, src) {
		return ErrSourceMismatch
	}
	c := *t
	if err := c.ConsumeRuntime(secret, host, rt, r.now); err != nil {
		return err
	}
	r.tokens[id] = &c
	return nil
}

func (r *registry) CurrentSerial(_ context.Context, host string, rt agentrt.Runtime) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := host
	if rt == agentrt.Native {
		key += "/native"
	}
	s, ok := r.serials[key]
	return s, ok, nil
}

func (r *registry) RecordIssued(_ context.Context, host string, rt agentrt.Runtime, c *x509.Certificate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := host
	if rt == agentrt.Native {
		key += "/native"
	}
	r.serials[key] = c.SerialNumber.Text(16)
	return nil
}

func (r *registry) Revoked(sn *big.Int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revoked[sn.Text(16)]
}

type env struct {
	ca    *certs.CA
	caNow time.Time
	reg   *registry
	url   string
	plain string
	fp    string
}

func newEnv(t *testing.T, serverURI *url.URL) *env {
	t.Helper()
	e := &env{caNow: time.Now().Add(-24 * time.Hour)}
	ca, err := certs.NewCA(certs.CAOptions{Validity: 72 * time.Hour, Now: func() time.Time { return e.caNow }})
	if err != nil {
		t.Fatal(err)
	}
	e.caNow = time.Now()
	e.ca = ca
	e.fp = certs.Fingerprint(ca.Certificate())
	e.reg = &registry{tokens: map[string]*Token{}, serials: map[string]string{}, revoked: map[string]bool{}, now: time.Now()}
	tok, plain, err := NewToken(inst, []string{"node1", "node2"}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e.reg.tokens[tok.ID], e.plain = &tok, plain
	srvCert, err := ca.Issue(context.Background(), certs.Request{Kind: certs.KindServer, CommonName: "manager",
		IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, URIs: []*url.URL{serverURI}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	kp, err := srvCert.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Authority: &authority{ca: ca, validity: time.Hour}, Registry: e.reg, Installation: inst}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: s.Handler(), TLSConfig: s.TLSConfig(func() (*tls.Certificate, error) { return &kp, nil }),
		ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.ServeTLS(l, "", "") }()
	t.Cleanup(func() { _ = hs.Close() })
	e.url = "https://" + l.Addr().String()
	return e
}

func (e *env) cfg(host string) AgentConfig {
	return AgentConfig{ManagerURL: e.url, Installation: inst, HostID: host, CAFingerprint: e.fp, Token: e.plain, Timeout: 5 * time.Second}
}

func (e *env) renewCfg(host string, cur *Credentials) RenewConfig {
	return RenewConfig{ManagerURL: e.url, Installation: inst, HostID: host, Current: cur, Timeout: 5 * time.Second}
}

func TestEnrollAndRenew(t *testing.T) {
	e := newEnv(t, certs.ManagerURI(inst))
	ctx := context.Background()
	cr, err := Enroll(ctx, e.cfg("node1"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := cr.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if !certs.HasURI(cert.Leaf, certs.AgentURI(inst, "node1")) || e.reg.serials["node1"] != cert.Leaf.SerialNumber.Text(16) {
		t.Fatalf("identity not issued/recorded: %v %v", cert.Leaf.URIs, e.reg.serials)
	}
	if !strings.Contains(string(cr.KeyPEM), "PRIVATE KEY") {
		t.Fatal("no key")
	}

	next, err := Renew(ctx, e.renewCfg("node1", cr))
	if err != nil {
		t.Fatal(err)
	}
	nc, _ := next.TLSCertificate()
	if nc.Leaf.SerialNumber.Cmp(cert.Leaf.SerialNumber) == 0 || string(next.KeyPEM) == string(cr.KeyPEM) {
		t.Fatal("renewal did not rotate key and serial")
	}
	if e.reg.serials["node1"] != nc.Leaf.SerialNumber.Text(16) {
		t.Fatal("renewed serial not recorded")
	}
	// The superseded certificate cannot renew again.
	if _, err := Renew(ctx, e.renewCfg("node1", cr)); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("renewal with superseded certificate: %v", err)
	}
}

func TestEnrollTokenRules(t *testing.T) {
	e := newEnv(t, certs.ManagerURI(inst))
	ctx := context.Background()
	if _, err := Enroll(ctx, e.cfg("node1")); err != nil {
		t.Fatal(err)
	}
	// Replay by the same host.
	if _, err := Enroll(ctx, e.cfg("node1")); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("replay: %v", err)
	}
	// A second allowed host may use it once.
	if _, err := Enroll(ctx, e.cfg("node2")); err != nil {
		t.Fatalf("second host: %v", err)
	}
	// Host not covered.
	if _, err := Enroll(ctx, e.cfg("node3")); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("other host: %v", err)
	}
	// Wrong secret, malformed, unknown ID.
	id, _, _ := ParseToken(e.plain)
	for name, tok := range map[string]string{
		"wrong secret": "dbcm-et1." + id + "." + strings.Repeat("A", 43),
		"malformed":    "hello",
		"unknown id":   "dbcm-et1.ffff." + strings.Repeat("A", 43),
	} {
		c := e.cfg("node1")
		c.Token = tok
		if _, err := Enroll(ctx, c); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "401") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Expired.
	e.reg.now = time.Now().Add(2 * time.Hour)
	tok, plain, _ := NewToken(inst, []string{"node4"}, time.Now(), time.Hour)
	e.reg.tokens[tok.ID] = &tok
	c := e.cfg("node4")
	c.Token = plain
	if _, err := Enroll(ctx, c); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expired token: %v", err)
	}
}

func TestEnrollSourceMismatch(t *testing.T) {
	e := newEnv(t, certs.ManagerURI(inst))
	e.reg.sourceOK = func(string, net.IP) bool { return false }
	if _, err := Enroll(context.Background(), e.cfg("node1")); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("source mismatch: %v", err)
	}
}

func TestEnrollRejectsUnpinnedManager(t *testing.T) {
	e := newEnv(t, certs.ManagerURI(inst))
	other, err := certs.NewCA(certs.CAOptions{Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c := e.cfg("node1")
	c.CAFingerprint = certs.Fingerprint(other.Certificate())
	if _, err := Enroll(context.Background(), c); !errors.Is(err, ErrManagerIdentity) {
		t.Fatalf("wrong pin: %v", err)
	}
	c.CAFingerprint = "not-a-fingerprint"
	if _, err := Enroll(context.Background(), c); err == nil {
		t.Fatal("invalid pin accepted")
	}
	if len(e.reg.tokens[firstID(e)].Used) != 0 {
		t.Fatal("token consumed by a request that never reached the manager")
	}
}

func firstID(e *env) string {
	id, _, _ := ParseToken(e.plain)
	return id
}

func TestEnrollRejectsServerWithoutManagerIdentity(t *testing.T) {
	// Same CA, but the server certificate is an agent's.
	e := newEnv(t, certs.AgentURI(inst, "node9"))
	if _, err := Enroll(context.Background(), e.cfg("node1")); !errors.Is(err, ErrManagerIdentity) {
		t.Fatalf("server without manager identity: %v", err)
	}
}

func TestRenewRejections(t *testing.T) {
	e := newEnv(t, certs.ManagerURI(inst))
	ctx := context.Background()
	cr, err := Enroll(ctx, e.cfg("node1"))
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := cr.TLSCertificate()

	// Revoked serial.
	e.reg.revoked[cert.Leaf.SerialNumber.Text(16)] = true
	if _, err := Renew(ctx, e.renewCfg("node1", cr)); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked: %v", err)
	}
	delete(e.reg.revoked, cert.Leaf.SerialNumber.Text(16))

	// Expired certificate: the TLS layer refuses it.
	// Even when it is the recorded serial.
	current := e.reg.serials["node1"]
	e.caNow = time.Now().Add(-3 * time.Hour)
	expired := expiredCreds(t, e)
	e.caNow = time.Now()
	if _, err := Renew(ctx, e.renewCfg("node1", expired)); err == nil {
		t.Fatal("expired certificate renewed")
	}
	e.reg.serials["node1"] = current

	// A certificate from another CA.
	other, _ := certs.NewCA(certs.CAOptions{Validity: time.Hour})
	key, csr2, _ := certs.NewKeyAndCSR("x")
	p2, _ := certs.ParseCSR(csr2)
	s2, err := other.SignCSR(ctx, p2, certs.Request{Kind: certs.KindPeer, CommonName: "x", URIs: []*url.URL{certs.AgentURI(inst, "node1")}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	kp, _ := certs.MarshalKeyPEM(key)
	foreign := &Credentials{KeyPEM: kp, ChainPEM: s2.ChainPEM, CAPEM: cr.CAPEM}
	if _, err := Renew(ctx, e.renewCfg("node1", foreign)); err == nil {
		t.Fatal("foreign certificate renewed")
	}
	// Still works with the current one.
	if _, err := Renew(ctx, e.renewCfg("node1", cr)); err != nil {
		t.Fatal(err)
	}
}

func expiredCreds(t *testing.T, e *env) *Credentials {
	t.Helper()
	key, csr, _ := certs.NewKeyAndCSR("x")
	p, _ := certs.ParseCSR(csr)
	s, err := e.ca.SignCSR(context.Background(), p, certs.Request{Kind: certs.KindPeer, CommonName: "x",
		URIs: []*url.URL{certs.AgentURI(inst, "node1")}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	e.reg.serials["node1"] = s.Cert.SerialNumber.Text(16)
	kp, _ := certs.MarshalKeyPEM(key)
	return &Credentials{KeyPEM: kp, ChainPEM: s.ChainPEM, CAPEM: e.ca.CertificatePEM()}
}

func TestServerRequestValidation(t *testing.T) {
	e := newEnv(t, certs.ManagerURI(inst))
	tc, _ := pinnedTLS(e.fp, inst, time.Now, nil)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 5 * time.Second}
	for name, tc := range map[string]struct {
		method, ctype, body string
		want                int
	}{
		"get":        {http.MethodGet, "application/json", "", http.StatusMethodNotAllowed},
		"media":      {http.MethodPost, "text/plain", "{}", http.StatusUnsupportedMediaType},
		"unknown":    {http.MethodPost, "application/json", `{"x":1}`, http.StatusBadRequest},
		"host id":    {http.MethodPost, "application/json", `{"token":"a","host_id":"a/b","csr":""}`, http.StatusBadRequest},
		"bad csr":    {http.MethodPost, "application/json", `{"token":"a","host_id":"n","csr":"x"}`, http.StatusBadRequest},
		"renew cert": {http.MethodPost, "application/json", `{"csr":"x"}`, http.StatusUnauthorized},
	} {
		path := PathEnroll
		if name == "renew cert" {
			path = PathRenew
		}
		req, _ := http.NewRequest(tc.method, e.url+path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.ctype)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: HTTP %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
	if len(e.reg.tokens[firstID(e)].Used) != 0 {
		t.Fatal("invalid requests consumed the token")
	}
}

func TestTokenConsume(t *testing.T) {
	now := time.Now()
	tok, plain, err := NewToken(inst, []string{"a"}, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tok.Digest, plain) {
		t.Fatal("plain token stored")
	}
	_, secret, err := ParseToken(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.Consume(secret, "a", now); err != nil {
		t.Fatal(err)
	}
	if err := tok.Consume(secret, "a", now); !errors.Is(err, ErrTokenReplayed) {
		t.Fatalf("replay: %v", err)
	}
	tok2, plain2, _ := NewToken(inst, []string{"a"}, now, time.Minute)
	_, s2, _ := ParseToken(plain2)
	tok2.Revoked = true
	if err := tok2.Consume(s2, "a", now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("revoked: %v", err)
	}
	if _, _, err := NewToken("", nil, now, time.Minute); err == nil {
		t.Fatal("token without installation")
	}
}

func TestTokenCanBeConsumedOncePerRuntime(t *testing.T) {
	now := time.Now()
	tok, plain, err := NewToken(inst, []string{"node1"}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := ParseToken(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.ConsumeRuntime(secret, "node1", agentrt.Container, now); err != nil {
		t.Fatal(err)
	}
	if err := tok.ConsumeRuntime(secret, "node1", agentrt.Native, now); err != nil {
		t.Fatal(err)
	}
	if err := tok.ConsumeRuntime(secret, "node1", agentrt.Native, now); !errors.Is(err, ErrTokenReplayed) {
		t.Fatalf("native replay: %v", err)
	}
	if len(tok.Used) != 2 || tok.Used["node1"].IsZero() || tok.Used["node1/native"].IsZero() {
		t.Fatalf("used slots: %+v", tok.Used)
	}
}

func TestFileStoreAndRenewer(t *testing.T) {
	e := newEnv(t, certs.ManagerURI(inst))
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "creds")
	st := FileStore{Dir: dir}
	if _, err := st.Load(); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("empty store: %v", err)
	}
	cr, err := Enroll(ctx, e.cfg("node1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(cr); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v %v", fi.Mode(), err)
	}
	loaded, err := st.Load()
	if err != nil || string(loaded.ChainPEM) != string(cr.ChainPEM) {
		t.Fatalf("load: %v", err)
	}
	h, err := NewHolder(loaded)
	if err != nil {
		t.Fatal(err)
	}
	r := &Renewer{Config: e.renewCfg("node1", nil), Holder: h, Store: st, Policy: certs.Policy{Fraction: 0.1}}
	if renewed, err := r.Once(ctx); err != nil || renewed {
		t.Fatalf("fresh certificate renewed: %v %v", renewed, err)
	}
	r.Policy = certs.Policy{Fraction: 1}
	renewed, err := r.Once(ctx)
	if err != nil || !renewed {
		t.Fatalf("renewal: %v %v", renewed, err)
	}
	c1, _ := h.Certificate()
	again, _ := st.Load()
	c2, _ := again.TLSCertificate()
	if c1.Leaf.SerialNumber.Cmp(c2.Leaf.SerialNumber) != 0 || e.reg.serials["node1"] != c1.Leaf.SerialNumber.Text(16) {
		t.Fatal("holder, store and registry disagree after renewal")
	}
	// Mismatched files are detected.
	other, _ := Enroll(ctx, e.cfg("node2"))
	if err := os.WriteFile(filepath.Join(dir, KeyFile), other.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(); err == nil {
		t.Fatal("mismatched key accepted")
	}
}
