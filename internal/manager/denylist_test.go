// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// noDocker is an agent.DockerAPI for commands that never touch Docker.
type noDocker struct{ agent.DockerAPI }

// countingAgents counts the commands it forwards.
type countingAgents struct {
	inner runner.AgentClient
	n     atomic.Int32
}

type fakeNativeDeny struct {
	calls int
	fail  bool
}

func (f *fakeNativeDeny) Do(_ context.Context, _ string, c patch.AgentCommand) (patch.AgentResult, error) {
	if c.Kind != patch.CmdDenyList || len(c.DenySerials) == 0 {
		return patch.AgentResult{}, errors.New("wrong native deny command")
	}
	f.calls++
	if f.fail {
		return patch.AgentResult{}, errors.New("native unavailable")
	}
	return patch.AgentResult{Runtime: agentrt.Native}, nil
}

func TestDenyListTracksEachRuntimeIndependently(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents = agents })
	s := c.leader()
	h := HostRecord{Swarm: "sw1", NodeID: "n1", Enrollment: EnrollEnrolled, AgentSerial: "a1", AgentIdentities: map[agentrt.Runtime]AgentIdentityRecord{agentrt.Native: {Serial: "b2"}}, AgentReports: map[agentrt.Runtime]agentrt.Info{agentrt.Container: agentrt.ContainerInfo(false), agentrt.Native: agentrt.NativeInfo(false)}}
	if _, err := Put(s.Store(), CollHosts, HostKey("sw1", "n1"), h, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(s.Store(), CollRevocations, "ff", certs.Revocation{Serial: "ff", RevokedAt: time.Now(), NotAfter: time.Now().Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	native := &fakeNativeDeny{fail: true}
	d := DenyListDistributor{Native: native}
	if err := d.DistributeOnce(context.Background(), s.Current()); err == nil {
		t.Fatal("native delivery failure hidden")
	}
	got, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "n1"))
	if got.Value.DenyListDigests[agentrt.Container] == "" || got.Value.DenyListDigests[agentrt.Native] != "" || len(agents.commands()) != 1 {
		t.Fatalf("acknowledgements mixed: %+v", got.Value)
	}
	native.fail = false
	if err := d.DistributeOnce(context.Background(), s.Current()); err != nil {
		t.Fatal(err)
	}
	got, _, _ = Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "n1"))
	if got.Value.DenyListDigests[agentrt.Native] != got.Value.DenyListDigests[agentrt.Container] || len(agents.commands()) != 1 || native.calls != 2 {
		t.Fatalf("retry did not isolate runtimes: %+v calls %d", got.Value, native.calls)
	}
}

func (c *countingAgents) Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error) {
	c.n.Add(1)
	return c.inner.Send(ctx, target, cmd)
}

// TestDenyListReachesAgentAndBlocksRevokedManagerCertificate runs the
// enrollment endpoint, a real agent executor behind the real mTLS listener
// and the manager's agent directory on loopback: a renewal denies the old
// agent serial, the distributor delivers the list once per change, and
// revoking a replica's manager certificates makes the agent refuse that
// certificate's TLS handshake while the reissued one still works.
func TestDenyListReachesAgentAndBlocksRevokedManagerCertificate(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	f.pki.ReplicaID = "m1"
	s := f.svc
	ctx := context.Background()
	plain, _, err := f.pki.IssueEnrollmentToken("sw1", 0)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := f.enroll(t, plain, "n1")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := enroll.NewHolder(creds)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	guard, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(dir, "term")))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := agent.NewExecutor(agent.Config{InstallationID: "inst-1", StatePath: filepath.Join(dir, "state.json"),
		VolumeBase: dir, Guard: guard, Docker: noDocker{}})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := holder.Credentials().CAPool()
	srv, err := agent.NewMTLSServer(ex, agent.MTLSServerConfig{Certificate: holder.Certificate, ClientCAs: pool,
		ManagerIdentity: certs.ManagerURI("inst-1"), Revoked: ex.Revoked})
	if err != nil {
		t.Fatal(err)
	}
	srv.ErrorLog = log.New(io.Discard, "", 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	agents := &countingAgents{inner: &AgentDirectory{Store: s.Store(), PKI: f.pki, Port: port, Timeout: 5 * time.Second}}
	cur := s.Current()
	l := &Leadership{Term: cur.Term, Agents: agents, svc: s}
	var dd DenyListDistributor

	// Renewal: the old agent serial is denied and distributed.
	old := serialOf(t, creds)
	next, err := enroll.Renew(ctx, enroll.RenewConfig{ManagerURL: f.url, Installation: "inst-1", HostID: "n1", Current: creds, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Set(next); err != nil {
		t.Fatal(err)
	}
	if err := dd.DistributeOnce(ctx, l); err != nil {
		t.Fatal(err)
	}
	if !ex.Revoked(mustSerial(t, old)) {
		t.Fatal("agent does not know the superseded serial")
	}
	if err := dd.DistributeOnce(ctx, l); err != nil || agents.n.Load() != 1 {
		t.Fatalf("unchanged list sent again: %d %v", agents.n.Load(), err)
	}
	h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "n1"))
	dl, _ := CurrentDenyList(s.Store())
	if h.Value.DenyListDigest != dl.Digest || len(dl.Serials) != 1 {
		t.Fatalf("host digest %q list %+v", h.Value.DenyListDigest, dl)
	}

	// Revoke the replica's manager certificates.
	oldClient, err := f.pki.ManagerClientCertificate()
	if err != nil {
		t.Fatal(err)
	}
	ops := opsFor(t, s)
	if err := ops.Authorizer.Policy().Bind(authz.Binding{Subject: operatorSubject, Role: authz.RoleOperator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pki.RevokeReplicaCertificates(operatorSubject, ops.Authorizer, "m1", "test"); err == nil {
		t.Fatal("operator revoked manager certificates")
	}
	n, err := f.pki.RevokeReplicaCertificates(adminSubject, ops.Authorizer, "m1", "replica compromised")
	if err != nil || n < 1 {
		t.Fatalf("revoked %d: %v", n, err)
	}
	if !f.pki.Revoked(oldClient.Leaf.SerialNumber) {
		t.Fatal("client certificate not denied")
	}
	// A fresh directory reissues the client certificate and delivers the
	// grown list.
	agents.inner = &AgentDirectory{Store: s.Store(), PKI: f.pki, Port: port, Timeout: 5 * time.Second}
	if err := dd.DistributeOnce(ctx, l); err != nil {
		t.Fatal(err)
	}
	newClient, _ := f.pki.ManagerClientCertificate()
	if newClient.Leaf.SerialNumber.Cmp(oldClient.Leaf.SerialNumber) == 0 || !ex.Revoked(oldClient.Leaf.SerialNumber) {
		t.Fatal("client certificate not reissued or not denied on the agent")
	}
	get := func(c *tls.Certificate) error {
		cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*c}, InsecureSkipVerify: true}}} //nolint:gosec // server side under test
		resp, err := cl.Get("https://127.0.0.1:" + strconv.Itoa(port) + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := get(oldClient); err == nil {
		t.Fatal("agent accepted a revoked manager certificate")
	}
	if err := get(newClient); err != nil {
		t.Fatalf("agent refused the reissued certificate: %v", err)
	}
}

func TestDenyListCompactionSurvivesLeaderChange(t *testing.T) {
	c := newCluster(t, 3, nil)
	leader := c.leader()
	now := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	for serial, expiry := range map[string]time.Time{
		"aa": now.Add(-time.Hour), "bb": now.Add(time.Hour), "cc": {},
	} {
		if _, err := Put(leader.Store(), CollRevocations, serial, certs.Revocation{
			Serial: serial, RevokedAt: now.Add(-2 * time.Hour), NotAfter: expiry}, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := CurrentDenyList(leader.Store())
	if err != nil {
		t.Fatal(err)
	}
	compact := DenyListCompactor{Now: func() time.Time { return now }}
	if n, err := compact.CompactOnce(leader.Store()); err != nil || n != 1 {
		t.Fatalf("compact: %d %v", n, err)
	}
	after, err := CurrentDenyList(leader.Store())
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision <= before.Revision || len(after.Serials) != 2 || after.Serials[0] != "bb" || after.Serials[1] != "cc" {
		t.Fatalf("after compaction: %+v; before: %+v", after, before)
	}
	c.close(leader)
	next := c.leader(leader.cfg.NodeID)
	got, err := CurrentDenyList(next.Store())
	if err != nil || got.Revision != after.Revision || got.Digest != after.Digest {
		t.Fatalf("new leader lost compacted state: %+v %v", got, err)
	}
}
