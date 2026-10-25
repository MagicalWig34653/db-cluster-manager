// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

type fakeWriterSource struct {
	hint WriterHint
	err  error
	acks []uint64
}

func (s *fakeWriterSource) Fetch(context.Context, CoordinationConfig) (WriterHint, error) {
	return s.hint, s.err
}
func (s *fakeWriterSource) Ack(_ context.Context, _ CoordinationConfig, e uint64) error {
	s.acks = append(s.acks, e)
	return nil
}

func newCoordinatedTestProxy(t *testing.T, id string, latencyA, latencyB time.Duration, source *fakeWriterSource) *Proxy {
	t.Helper()
	cfg := testConfig(node("a", "127.0.0.1:3306"), node("b", "127.0.0.1:3307"))
	cfg.Probe.Sync = SyncGalera
	cfg.Coordination = &CoordinationConfig{ClusterID: "c", ProxyID: id, ManagerURLs: []string{"http://127.0.0.1:9999"}, TokenFile: "/tmp/fake-token", Timeout: Duration(40 * time.Millisecond)}
	p, err := New(cfg, Options{WriterSource: source})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, ns := range p.nodes {
		ns.reported, ns.healthy, ns.sync, ns.lastSeen = true, true, routing.SyncSynced, now
		if i == 0 {
			ns.samples = []time.Duration{latencyA}
		} else {
			ns.samples = []time.Duration{latencyB}
		}
	}
	return p
}

func decideTestProxy(p *Proxy) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evaluateLocked(time.Now())
	return p.writer
}

func TestCoordinatedProxiesAgreeDespiteLocalLatencyAndPartition(t *testing.T) {
	s1 := &fakeWriterSource{hint: WriterHint{Epoch: 1, Writer: "a"}}
	s2 := &fakeWriterSource{hint: WriterHint{Epoch: 1, Writer: "a"}}
	p1 := newCoordinatedTestProxy(t, "p1", time.Millisecond, 10*time.Millisecond, s1)
	p2 := newCoordinatedTestProxy(t, "p2", 10*time.Millisecond, time.Millisecond, s2)
	for _, p := range []*Proxy{p1, p2} {
		p.syncWriterChoice(context.Background())
		if got := decideTestProxy(p); got != "a" {
			t.Fatalf("initial writer %q", got)
		}
	}
	// A partition at p2 and flapping local health cannot make it choose b.
	s2.err = errors.New("manager partition")
	p2.nodes[0].healthy = false
	p2.syncWriterChoice(context.Background())
	if got := decideTestProxy(p2); got != "" {
		t.Fatalf("partitioned proxy chose %q", got)
	}
	p2.nodes[0].healthy = true
	if got := decideTestProxy(p2); got != "a" {
		t.Fatalf("returned old writer %q", got)
	}
	// Manager freezes the choice. Both proxies stop writes, and a later
	// active hint changes them together after the coordinator has both ACKs.
	s1.hint, s2.hint = WriterHint{Epoch: 2, Pending: "b"}, WriterHint{Epoch: 2, Pending: "b"}
	s2.err = nil
	for _, p := range []*Proxy{p1, p2} {
		p.syncWriterChoice(context.Background())
		if got := decideTestProxy(p); got != "" {
			t.Fatalf("freeze writer %q", got)
		}
	}
	if len(s1.acks) != 1 || len(s2.acks) != 1 {
		t.Fatalf("acks: %v %v", s1.acks, s2.acks)
	}
	s1.hint, s2.hint = WriterHint{Epoch: 2, Writer: "b"}, WriterHint{Epoch: 2, Writer: "b"}
	for _, p := range []*Proxy{p1, p2} {
		p.syncWriterChoice(context.Background())
		if got := decideTestProxy(p); got != "b" {
			t.Fatalf("new writer %q", got)
		}
	}
	// A delayed freeze response cannot roll an active proxy back.
	s1.hint = WriterHint{Epoch: 2, Pending: "b"}
	p1.syncWriterChoice(context.Background())
	if got := decideTestProxy(p1); got != "b" {
		t.Fatalf("regressed to %q", got)
	}
}

func TestCoordinatedFreezeClosesExistingWriteConnections(t *testing.T) {
	s := &fakeWriterSource{hint: WriterHint{Epoch: 1, Writer: "a"}}
	p := newCoordinatedTestProxy(t, "p1", time.Millisecond, 2*time.Millisecond, s)
	p.syncWriterChoice(context.Background())
	decideTestProxy(p)
	client, peer := net.Pipe()
	defer peer.Close()
	c := &tconn{ep: EndpointWrite, node: "a", client: client}
	p.conns[c] = struct{}{}
	s.hint = WriterHint{Epoch: 2, Pending: "b"}
	p.syncWriterChoice(context.Background())
	if !c.closed || len(s.acks) != 1 {
		t.Fatalf("connection closed=%v, ACKs=%v", c.closed, s.acks)
	}
}

func TestCoordinatedProbeRoundsUseFakeBackendEvidence(t *testing.T) {
	s := &fakeWriterSource{hint: WriterHint{Epoch: 1, Writer: "a"}}
	cfg := testConfig(node("a", "127.0.0.1:3306"), node("b", "127.0.0.1:3307"))
	cfg.Probe.Sync = SyncGalera
	cfg.Coordination = &CoordinationConfig{ClusterID: "c", ProxyID: "p1", ManagerURLs: []string{"http://127.0.0.1:9999"}, TokenFile: "/tmp/fake-token", Timeout: Duration(40 * time.Millisecond)}
	backend := &scripted{}
	backend.set(cfg.Nodes[0].Address, healthy(5*time.Millisecond))
	backend.set(cfg.Nodes[1].Address, healthy(time.Millisecond)) // locally faster, but not the writer
	p, err := New(cfg, Options{WriterSource: s, Prober: backend})
	if err != nil {
		t.Fatal(err)
	}
	p.probeRound(context.Background())
	if got := p.Status().Writer; got != "a" {
		t.Fatalf("chose locally faster backend: %q", got)
	}
	backend.set(cfg.Nodes[0].Address, func() ProbeResult { return down() })
	p.probeRound(context.Background())
	if got := p.Status().Writer; got != "none" {
		t.Fatalf("failed over locally to %q", got)
	}
	backend.set(cfg.Nodes[0].Address, healthy(5*time.Millisecond))
	p.probeRound(context.Background())
	if got := p.Status().Writer; got != "a" {
		t.Fatalf("flap returned %q", got)
	}
}

func TestCoordinationConfigRejectsUnsafeManagerURLAndLocalPolicy(t *testing.T) {
	c := testConfig(node("a", "127.0.0.1:3306"))
	c.Probe.Sync = SyncGalera
	c.Coordination = &CoordinationConfig{ClusterID: "c", ProxyID: "p", ManagerURLs: []string{"http://manager.example.test"}, TokenFile: "/tmp/token", Timeout: Duration(40 * time.Millisecond)}
	if err := c.Validate(); err == nil {
		t.Fatal("non-TLS remote manager accepted")
	}
	c.Coordination.ManagerURLs = []string{"https://manager.example.test"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Policy.Mode = "lowest-latency"
	if err := c.Validate(); err == nil {
		t.Fatal("local writer policy accepted")
	}
	c.Policy.Mode = "single-writer-stable"
	c.Policy.Read = "include-writer"
	c.ReadListen = "127.0.0.1:3307"
	if err := c.Validate(); err == nil {
		t.Fatal("writable read endpoint accepted")
	}
	c.Policy.Read = "disabled"
	c.ReadListen = ""
	c.Probe.Sync = SyncAssumeSynced
	if err := c.Validate(); err == nil {
		t.Fatal("assumed Galera state accepted")
	}
}
