// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
)

// TestPatchAgentDirectoryOverMTLS runs a real patch agent service behind the
// real mTLS listener (fake package manager) and reaches it through the
// manager's patch agent directory: report, then a fenced deny list that the
// agent applies to its own TLS listener.
func TestPatchAgentDirectoryOverMTLS(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	f.pki.ReplicaID = "m1"
	s := f.svc
	plain, _, err := f.pki.IssueEnrollmentToken("sw1", 0)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := f.enrollInfo(t, plain, "n1", agentrt.NativeInfo(false))
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
	state, err := patch.OpenAgentState(filepath.Join(dir, "patch-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := &patch.FakeRunner{Responses: map[string]patch.CmdResult{
		"uname -rm": {Stdout: "6.1.0-18-arm64 aarch64\n"},
		`dpkg-query -W -f=${Package}\t${Version}\t${Architecture}\n`: {Stdout: "openssl\t3.0.11-1~deb12u1\tarm64\n"},
		"apt list --upgradable": {Stdout: "Listing...\n"},
	}}
	svc := &patch.AgentService{Collector: patch.Collector{HostID: "n1", Runner: r, FS: patch.FakeFS{"/usr/bin/apt-get": "", "/etc/os-release": "ID=debian\nVERSION_ID=12\n"}},
		Guard: guard, State: state}
	pool, _ := holder.Credentials().CAPool()
	tc, err := agent.ServerTLSConfig(agent.MTLSServerConfig{Certificate: holder.Certificate, ClientCAs: pool,
		ManagerIdentity: certs.ManagerURI("inst-1"), Revoked: svc.Revoked})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: svc.Handler(nil), TLSConfig: tc, ErrorLog: log.New(io.Discard, "", 0)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	d := &PatchAgentDirectory{Dir: &AgentDirectory{Store: s.Store(), PKI: f.pki, NativePort: port, Timeout: 5 * time.Second}}
	ctx := context.Background()
	res, err := d.Do(ctx, "n1", patch.AgentCommand{Kind: patch.CmdReport})
	if err != nil || res.Report == nil || res.Report.Manager != patch.APT || res.Runtime != agentrt.Native {
		t.Fatal(res, err)
	}
	if h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "n1")); h.Value.AgentReports[agentrt.Native].Runtime != agentrt.Native {
		t.Fatalf("native report not stored: %+v", h.Value.AgentReports)
	}
	if _, err := d.Do(ctx, "n1", patch.AgentCommand{Kind: patch.CmdDenyList, Term: 4, DenyRevision: 2, DenySerials: []string{"ff"}}); err != nil {
		t.Fatal(err)
	}
	if !svc.Revoked(big.NewInt(0xff)) {
		t.Fatal("agent did not apply the deny list")
	}
	// A stale leader term is rejected by the agent.
	_, err = d.Do(ctx, "n1", patch.AgentCommand{Kind: patch.CmdDenyList, Term: 3, DenyRevision: 3, DenySerials: nil})
	if !errors.Is(err, patch.ErrStale) {
		t.Fatal("stale term accepted:", err)
	}
	// Unknown and unenrolled hosts are refused without a connection.
	if _, err := d.Do(ctx, "ghost", patch.AgentCommand{Kind: patch.CmdReport}); err == nil {
		t.Fatal("unknown host reached")
	}
}
