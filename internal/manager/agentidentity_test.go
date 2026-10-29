// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
)

// This checks the real PKI and transport verifier without listeners or VMs.
func TestRuntimeCertificatesHaveIndependentPins(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	if _, err := Put(s.Store(), CollSettings, SettingsKey, Settings{InstallationID: "inst-1"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(s.Store(), CollHosts, HostKey("sw1", "n1"), HostRecord{Swarm: "sw1", NodeID: "n1", Enrollment: EnrollEnrolled, Addr: "127.0.0.1"}, nil); err != nil {
		t.Fatal(err)
	}
	p := &AgentPKI{Store: s.Store(), Keys: testKey}
	issue := func(rt agentrt.Runtime) *certs.Signed {
		_, raw, err := certs.NewKeyAndCSR("test agent")
		if err != nil {
			t.Fatal(err)
		}
		csr, err := certs.ParseCSR(raw)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := p.SignAgent(context.Background(), "n1", rt, csr)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.RecordIssued(context.Background(), "n1", rt, cert.Cert); err != nil {
			t.Fatal(err)
		}
		return cert
	}
	container, native := issue(agentrt.Container), issue(agentrt.Native)
	ca, err := p.CA()
	if err != nil {
		t.Fatal(err)
	}
	verify := func(rt agentrt.Runtime, cert *certs.Signed) error {
		cfg, err := agent.ClientTLSConfig(agent.MTLSClientConfig{Certificate: func() (*tls.Certificate, error) { return &tls.Certificate{}, nil }, RootCAs: ca.Pool(), AgentIdentity: certs.RuntimeAgentURI("inst-1", "n1", string(rt)), CurrentSerial: func() (string, error) {
			serial, _, err := p.CurrentSerial(context.Background(), "n1", rt)
			return serial, err
		}, Revoked: p.Revoked})
		if err != nil {
			return err
		}
		return cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Cert, ca.Certificate()}})
	}
	if err := verify(agentrt.Container, container); err != nil {
		t.Fatal(err)
	}
	if err := verify(agentrt.Native, native); err != nil {
		t.Fatal(err)
	}
	if err := verify(agentrt.Container, native); err == nil {
		t.Fatal("native identity accepted in container slot")
	}
	if _, _, err := Update(s.Store(), CollHosts, HostKey("sw1", "n1"), func(cur HostRecord, _ bool) (HostRecord, error) {
		cur.DenyListDigest = "same"
		cur.DenyListDigests = map[agentrt.Runtime]string{agentrt.Container: "same", agentrt.Native: "same"}
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
	old := native
	native = issue(agentrt.Native)
	h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "n1"))
	if h.Value.DenyListDigests[agentrt.Native] != "" || h.Value.DenyListDigests[agentrt.Container] != "same" {
		t.Fatalf("renewal acknowledgements: %+v", h.Value.DenyListDigests)
	}
	if err := verify(agentrt.Container, container); err != nil {
		t.Fatalf("native renewal broke container: %v", err)
	}
	if err := verify(agentrt.Native, old); err == nil {
		t.Fatal("superseded native certificate accepted")
	}
	if err := verify(agentrt.Native, native); err != nil {
		t.Fatal(err)
	}
	if !p.Revoked(old.Cert.SerialNumber) || p.Revoked(container.Cert.SerialNumber) {
		t.Fatal("wrong serial denied")
	}
	oldContainer := container
	container = issue(agentrt.Container)
	h, _, _ = Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "n1"))
	if h.Value.DenyListDigests[agentrt.Container] != "" || h.Value.DenyListDigest != "" {
		t.Fatal("container renewal kept old deny-list acknowledgement")
	}
	if err := verify(agentrt.Native, native); err != nil {
		t.Fatalf("container renewal broke native: %v", err)
	}
	if err := verify(agentrt.Container, oldContainer); err == nil {
		t.Fatal("superseded container certificate accepted")
	}
	if err := verify(agentrt.Container, container); err != nil {
		t.Fatal(err)
	}
}
