// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
)

// pkiFixture runs the enrollment endpoint of a single-node manager on
// loopback TLS and holds host sw1/n1 at 127.0.0.1.
type pkiFixture struct {
	svc *Service
	pki *AgentPKI
	url string
}

func newPKIFixture(t *testing.T, hostAddr string) *pkiFixture {
	t.Helper()
	c := newCluster(t, 1, nil)
	s := c.leader()
	if _, err := Put(s.Store(), CollSettings, SettingsKey, Settings{InstallationID: "inst-1", VolumeBase: "/srv/dbcm"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(s.Store(), CollHosts, HostKey("sw1", "n1"), HostRecord{Swarm: "sw1", NodeID: "n1", Enrollment: EnrollEnrolled, Addr: hostAddr}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(s.Store(), CollHosts, HostKey("sw1", "n2"), HostRecord{Swarm: "sw1", NodeID: "n2", Enrollment: EnrollDiscovered, Addr: "127.0.0.1"}, nil); err != nil {
		t.Fatal(err)
	}
	p := &AgentPKI{Store: s.Store(), Keys: testKey, RequireSourceAddr: true, ServerIPs: []net.IP{net.IPv4(127, 0, 0, 1)}}
	srv := &enroll.Server{Authority: p, Registry: p, Installation: "inst-1"}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler(), TLSConfig: srv.TLSConfig(p.EnrollmentServerCertificate),
		ReadHeaderTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = hs.ServeTLS(l, "", "") }()
	t.Cleanup(func() { _ = hs.Close() })
	return &pkiFixture{svc: s, pki: p, url: "https://" + l.Addr().String()}
}

func (f *pkiFixture) enroll(t *testing.T, plain, host string) (*enroll.Credentials, error) {
	t.Helper()
	fp, err := f.pki.CAFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return enroll.Enroll(context.Background(), enroll.AgentConfig{ManagerURL: f.url, Installation: "inst-1", HostID: host,
		CAFingerprint: fp, Token: plain, Timeout: 5 * time.Second})
}

// startAgent serves h over mTLS with the enrolled credentials.
func startAgent(t *testing.T, h agent.Handler, holder *enroll.Holder) int {
	t.Helper()
	pool, err := holder.Credentials().CAPool()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := agent.NewMTLSServer(h, agent.MTLSServerConfig{Certificate: holder.Certificate, ClientCAs: pool, ManagerIdentity: certs.ManagerURI("inst-1")})
	if err != nil {
		t.Fatal(err)
	}
	srv.ErrorLog = log.New(io.Discard, "", 0)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(l, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

func serialOf(t *testing.T, c *enroll.Credentials) string {
	t.Helper()
	kp, err := c.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return kp.Leaf.SerialNumber.Text(16)
}

func TestAgentEnrollmentCommandsRenewalRevocation(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	s := f.svc
	ctx := context.Background()
	plain, rec, err := f.pki.IssueEnrollmentToken("sw1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Token.Digest, plain) || len(rec.Token.AllowedHosts) != 1 || rec.Token.AllowedHosts[0] != "n1" {
		t.Fatalf("token record %+v", rec)
	}
	// A discovered (not enrolled) host cannot use it.
	if _, err := f.enroll(t, plain, "n2"); !errors.Is(err, enroll.ErrRejected) {
		t.Fatalf("not enrolled host: %v", err)
	}
	creds, err := f.enroll(t, plain, "n1")
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "n1"))
	if h.Value.AgentSerial != serialOf(t, creds) || h.Value.AgentNotAfter.IsZero() {
		t.Fatalf("identity not recorded: %+v", h.Value)
	}
	if _, err := f.enroll(t, plain, "n1"); !errors.Is(err, enroll.ErrRejected) {
		t.Fatalf("replayed token: %v", err)
	}

	holder, err := enroll.NewHolder(creds)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []agent.Command
	port := startAgent(t, handlerFunc(func(_ context.Context, cmd agent.Command) (agent.Result, error) {
		mu.Lock()
		got = append(got, cmd)
		mu.Unlock()
		return agent.Result{OpID: cmd.OpID, StepID: cmd.StepID, Kind: cmd.Kind, Status: agent.StatusSucceeded}, nil
	}), holder)
	dir := &AgentDirectory{Store: s.Store(), PKI: f.pki, Port: port, Timeout: 5 * time.Second}
	cmd := agent.Command{Term: 1, OpID: "o", StepID: "s1", Kind: agent.KindInspect}
	if res, err := dir.Send(ctx, "n1", cmd); err != nil || res.Status != agent.StatusSucceeded {
		t.Fatalf("command over mTLS: %+v %v", res, err)
	}
	if base, _ := dir.Resolve("n1"); base != "https://127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("resolved %q", base)
	}

	// Renewal rotates the identity and denies the old serial.
	old := serialOf(t, creds)
	next, err := enroll.Renew(ctx, enroll.RenewConfig{ManagerURL: f.url, Installation: "inst-1", HostID: "n1", Current: creds, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Set(next); err != nil {
		t.Fatal(err)
	}
	h, _, _ = Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "n1"))
	if h.Value.AgentSerial != serialOf(t, next) || !f.pki.Revoked(mustSerial(t, old)) {
		t.Fatalf("renewal not recorded: %+v", h.Value)
	}
	dir2 := &AgentDirectory{Store: s.Store(), PKI: f.pki, Port: port, Timeout: 5 * time.Second}
	if _, err := dir2.Send(ctx, "n1", agent.Command{Term: 1, OpID: "o", StepID: "s2", Kind: agent.KindInspect}); err != nil {
		t.Fatalf("command after renewal: %v", err)
	}
	// The superseded agent certificate is refused by the manager's client.
	if err := holder.Set(creds); err != nil {
		t.Fatal(err)
	}
	dir3 := &AgentDirectory{Store: s.Store(), PKI: f.pki, Port: port, Timeout: 5 * time.Second}
	if _, err := dir3.Send(ctx, "n1", agent.Command{Term: 1, OpID: "o", StepID: "s3", Kind: agent.KindInspect}); err == nil {
		t.Fatal("agent with a superseded certificate accepted")
	}
	_ = holder.Set(next)

	// Revoking the enrollment revokes the identity.
	ops := opsFor(t, s)
	sw := &Swarms{Svc: s, Authorizer: ops.Authorizer}
	if _, err := sw.SetEnrollment(adminSubject, "sw1", "n1", false, ""); err != nil {
		t.Fatal(err)
	}
	if !f.pki.Revoked(mustSerial(t, serialOf(t, next))) {
		t.Fatal("serial of a revoked host not denied")
	}
	if _, err := dir2.Send(ctx, "n1", agent.Command{Term: 1, OpID: "o", StepID: "s4", Kind: agent.KindInspect}); !errors.Is(err, agent.ErrInvalidCommand) {
		t.Fatalf("revoked host reachable: %v", err)
	}
	if _, err := enroll.Renew(ctx, enroll.RenewConfig{ManagerURL: f.url, Installation: "inst-1", HostID: "n1", Current: next, Timeout: 5 * time.Second}); err == nil {
		t.Fatal("revoked agent renewed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("agent received %d commands, want 2", len(got))
	}
}

func mustSerial(t *testing.T, hex string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(hex, 16)
	if !ok {
		t.Fatalf("serial %q", hex)
	}
	return n
}

func TestEnrollmentSourceAddressAndCAStability(t *testing.T) {
	f := newPKIFixture(t, "10.9.9.9")
	plain, _, err := f.pki.IssueEnrollmentToken("sw1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.enroll(t, plain, "n1"); !errors.Is(err, enroll.ErrRejected) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("enrollment from another address: %v", err)
	}
	recs, _ := List[EnrollTokenRecord](f.svc.Store(), CollEnrollTokens)
	if len(recs) != 1 || len(recs[0].Value.Token.Used) != 0 {
		t.Fatalf("token consumed by a refused request: %+v", recs)
	}
	// The CA is persistent: a second PKI on the same store loads it.
	fp1, _ := f.pki.CAFingerprint()
	p2 := &AgentPKI{Store: f.svc.Store(), Keys: testKey}
	fp2, err := p2.CAFingerprint()
	if err != nil || fp1 != fp2 {
		t.Fatalf("CA not shared: %s %s %v", fp1, fp2, err)
	}
	// The stored CA document is sealed.
	doc, ok, _ := f.svc.Store().Raw(CollPKI, PKIKeyAgentCA)
	if !ok || strings.Contains(string(doc.Value), "PRIVATE") || strings.Contains(string(doc.Value), "passphrase") {
		t.Fatalf("CA document not sealed: %s", doc.Value)
	}
	// Without the data key the CA cannot be loaded.
	p3 := &AgentPKI{Store: f.svc.Store(), Keys: StaticKey{ID: "k1", Bytes: []byte("ffffffffffffffffffffffffffffffff")}}
	if _, err := p3.CA(); err == nil {
		t.Fatal("CA loaded with a wrong data key")
	}
}

func TestAgentDirectoryRefusesUnresolvableHosts(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	st := f.svc.Store()
	for key, h := range map[string]HostRecord{
		"no-serial": {Swarm: "sw1", NodeID: "a", Enrollment: EnrollEnrolled, Addr: "127.0.0.1"},
		"no-addr":   {Swarm: "sw1", NodeID: "b", Enrollment: EnrollEnrolled, AgentSerial: "1"},
		"zero-addr": {Swarm: "sw1", NodeID: "c", Enrollment: EnrollEnrolled, AgentSerial: "1", Addr: "0.0.0.0"},
		"revoked":   {Swarm: "sw1", NodeID: "d", Enrollment: EnrollRevoked, AgentSerial: "1", Addr: "127.0.0.1"},
	} {
		if _, err := Put(st, CollHosts, HostKey("sw1", key), h, nil); err != nil {
			t.Fatal(err)
		}
	}
	dir := &AgentDirectory{Store: st, PKI: f.pki}
	for _, target := range []string{"a", "b", "c", "d", "unknown"} {
		if _, err := dir.Send(context.Background(), target, agent.Command{}); !errors.Is(err, agent.ErrInvalidCommand) {
			t.Errorf("%s: %v", target, err)
		}
	}
}

func TestManagerCertificatesCarryIdentities(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	for name, get := range map[string]func() (*tls.Certificate, error){
		"client": f.pki.ManagerClientCertificate, "server": f.pki.EnrollmentServerCertificate,
	} {
		c, err := get()
		if err != nil {
			t.Fatal(err)
		}
		if !certs.HasURI(c.Leaf, certs.ManagerURI("inst-1")) {
			t.Errorf("%s certificate lacks the manager identity", name)
		}
		again, _ := get()
		if again != c {
			t.Errorf("%s certificate not cached", name)
		}
	}
}
