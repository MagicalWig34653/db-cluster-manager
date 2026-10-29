// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe/fakemysql"
)

const monitorPW = "mon-pw-7x"

func TestParseFlagsMTLS(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	base := []string{"-listen", "0.0.0.0:7443", "-state", "/var/lib/a", "-installation", "inst-1", "-volume-base", "/srv/dbcm",
		"-manager-url", "https://10.0.0.4:7444", "-ca-fingerprint", fp, "-host-id", "node1", "-enroll-token-file", "/run/secrets/t"}
	o, err := parseFlags(base)
	if err != nil || o.insecureLab || o.hostID != "node1" {
		t.Fatalf("parsed %+v %v", o, err)
	}
	t.Setenv(hostIDEnv, "from-env")
	if o, err := parseFlags(base[:len(base)-4]); err != nil || o.hostID != "from-env" {
		t.Fatalf("host ID from env: %+v %v", o, err)
	}
	t.Setenv(hostIDEnv, "")
	drop := func(k string) []string {
		var out []string
		for i := 0; i < len(base); i += 2 {
			if base[i] != k {
				out = append(out, base[i], base[i+1])
			}
		}
		return out
	}
	with := func(k, v string) []string { return append(drop(k), k, v) }
	for name, args := range map[string][]string{
		"no manager url":    drop("-manager-url"),
		"http manager url":  with("-manager-url", "http://10.0.0.4:7444"),
		"no fingerprint":    drop("-ca-fingerprint"),
		"bad fingerprint":   with("-ca-fingerprint", "abc"),
		"no host id":        drop("-host-id"),
		"bad host id":       with("-host-id", "a/b"),
		"monitor user only": append(append([]string{}, base...), "-monitor-user", "m"),
		// The lab flag keeps its loopback restriction.
		"lab on all interfaces": append(append([]string{}, base...), "-insecure-lab"),
	} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// enrollServer is a minimal manager enrollment endpoint for the agent run.
type enrollServer struct {
	ca      *certs.CA
	url     string
	plain   string
	mu      sync.Mutex
	used    map[string]bool
	serials map[string]string
}

func (e *enrollServer) SignAgent(ctx context.Context, host string, rt agentrt.Runtime, csr *x509.CertificateRequest) (*certs.Signed, error) {
	return e.ca.SignCSR(ctx, csr, certs.Request{Kind: certs.KindPeer, CommonName: host, IPs: []net.IP{net.IPv4(127, 0, 0, 1)},
		URIs: []*url.URL{certs.RuntimeAgentURI("inst-1", host, string(rt))}, Validity: time.Hour})
}
func (e *enrollServer) CABundlePEM() []byte       { return e.ca.CertificatePEM() }
func (e *enrollServer) ClientCAs() *x509.CertPool { return e.ca.Pool() }
func (e *enrollServer) ConsumeToken(_ context.Context, plain, host string, rt agentrt.Runtime, _ net.IP) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if plain != e.plain {
		return enroll.ErrTokenInvalid
	}
	key := host + "/" + string(rt)
	if e.used[key] {
		return enroll.ErrTokenReplayed
	}
	e.used[key] = true
	return nil
}
func (e *enrollServer) CurrentSerial(_ context.Context, host string, rt agentrt.Runtime) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.serials[host+"/"+string(rt)]
	return s, ok, nil
}
func (e *enrollServer) RecordIssued(_ context.Context, host string, rt agentrt.Runtime, c *x509.Certificate) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.serials[host+"/"+string(rt)] = c.SerialNumber.Text(16)
	return nil
}
func (e *enrollServer) Revoked(*big.Int) bool { return false }

func newEnrollServer(t *testing.T) *enrollServer {
	t.Helper()
	ca, err := certs.NewCA(certs.CAOptions{Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	e := &enrollServer{ca: ca, plain: "dbcm-et1.abc." + strings.Repeat("x", 43), used: map[string]bool{}, serials: map[string]string{}}
	iss, err := ca.Issue(context.Background(), certs.Request{Kind: certs.KindServer, CommonName: "m", IPs: []net.IP{net.IPv4(127, 0, 0, 1)},
		URIs: []*url.URL{certs.ManagerURI("inst-1")}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	kp, _ := iss.TLSCertificate()
	srv := &enroll.Server{Authority: e, Registry: e, Installation: "inst-1"}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler(), TLSConfig: srv.TLSConfig(func() (*tls.Certificate, error) { return &kp, nil }),
		ReadHeaderTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = hs.ServeTLS(l, "", "") }()
	t.Cleanup(func() { _ = hs.Close() })
	e.url = "https://" + l.Addr().String()
	return e
}

func TestRunMTLSEnrollsServesAndCollectsHealth(t *testing.T) {
	fd := newFakeDocker(t)
	es := newEnrollServer(t)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	pwFile := filepath.Join(dir, "pw")
	_ = os.WriteFile(tokenFile, []byte(es.plain+"\n"), 0o600)
	_ = os.WriteFile(pwFile, []byte(monitorPW+"\n"), 0o600)
	db := fakemysql.New(t, "dbcm_monitor", monitorPW, fakemysql.SyncedVars())
	state := filepath.Join(dir, "state")
	args := []string{"-listen", "127.0.0.1:0", "-docker", "unix://" + fd.sock, "-state", state, "-installation", "inst-1",
		"-volume-base", t.TempDir(), "-manager-url", es.url, "-ca-fingerprint", certs.Fingerprint(es.ca.Certificate()),
		"-host-id", "node1", "-enroll-token-file", tokenFile, "-monitor-user", "dbcm_monitor", "-monitor-password-file", pwFile}
	start := func() (string, func() error) {
		o, err := parseFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		addrc, errc := make(chan string, 1), make(chan error, 1)
		go func() { errc <- run(ctx, o, io.Discard, func(a string) { addrc <- a }) }()
		select {
		case a := <-addrc:
			return a, func() error { cancel(); return <-errc }
		case err := <-errc:
			cancel()
			t.Fatalf("run: %v", err)
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatal("agent did not start")
		}
		return "", nil
	}
	addr, stop := start()
	if fi, err := os.Stat(filepath.Join(state, tlsDirName, enroll.KeyFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key not stored privately: %v %v", fi, err)
	}
	mgr, err := es.ca.Issue(context.Background(), certs.Request{Kind: certs.KindClient, CommonName: "manager",
		URIs: []*url.URL{certs.ManagerURI("inst-1")}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	mkp, _ := mgr.TLSCertificate()
	c, err := agent.NewMTLSClient("https://"+addr, agent.MTLSClientConfig{
		Certificate: func() (*tls.Certificate, error) { return &mkp, nil }, RootCAs: es.ca.Pool(),
		AgentIdentity: certs.AgentURI("inst-1", "node1"), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ha, _ := json.Marshal(agent.CollectHealthArgs{Address: db.Addr()})
	res, err := c.Handle(context.Background(), agent.Command{Term: 1, OpID: "health", StepID: "h1", Kind: agent.KindCollectHealth, Args: ha})
	if err != nil || res.Status != agent.StatusSucceeded || !strings.Contains(string(res.Output), "wsrep_local_state_comment") {
		t.Fatalf("collect-health over mTLS: %+v %v", res, err)
	}
	// The lab client without a certificate is refused.
	if _, err := (&agent.LabClient{BaseURL: "http://" + addr}).Handle(context.Background(), agent.Command{}); err == nil {
		t.Fatal("plain HTTP accepted")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// A restart reuses the stored credentials: no second enrollment (the
	// token is already used, so a second one would fail).
	_, stop = start()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestRunMTLSStopsOnRejectedEnrollment(t *testing.T) {
	fd := newFakeDocker(t)
	es := newEnrollServer(t)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	_ = os.WriteFile(tokenFile, []byte("dbcm-et1.abc."+strings.Repeat("y", 43)), 0o600)
	o, err := parseFlags([]string{"-listen", "127.0.0.1:0", "-docker", "unix://" + fd.sock, "-state", filepath.Join(dir, "s"),
		"-installation", "inst-1", "-volume-base", dir, "-manager-url", es.url, "-ca-fingerprint", certs.Fingerprint(es.ca.Certificate()),
		"-host-id", "node1", "-enroll-token-file", tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, o, io.Discard, nil); !errors.Is(err, enroll.ErrRejected) {
		t.Fatalf("run with a wrong token: %v", err)
	}
}
