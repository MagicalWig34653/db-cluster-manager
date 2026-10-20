// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"context"
	"encoding/json"
	"math/rand"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

func TestWriteEndpointRoutesToWriterAndEchoes(t *testing.T) {
	a := newFakeBackend(t, "a")
	cfg := testConfig(node("a", a.addr()))
	p := startProxy(t, cfg, Options{})
	waitWriter(t, p, "a", 2*time.Second)

	c, id := dialID(t, p.Addr(EndpointWrite))
	if id != "a" {
		t.Fatalf("connected to %q, want a", id)
	}
	for i := range 3 {
		if err := c.echo(strings.Repeat("x", 1+i*5000)); err != nil {
			t.Fatalf("echo %d: %v", i, err)
		}
	}
}

// TestFailoverWhenWriterListenerCloses: with the real TCP prober, closing
// the writer's listener makes new connections reach the other node within
// one probe interval plus probe timeout (scheduling slack allowed).
func TestFailoverWhenWriterListenerCloses(t *testing.T) {
	a := newFakeBackend(t, "a")
	b := newFakeBackend(t, "b")
	cfg := testConfig(node("a", a.addr()), node("b", b.addr()))
	cfg.Policy.Mode = "prefer-node"
	cfg.Policy.PreferredID = "a"
	p := startProxy(t, cfg, Options{})
	waitWriter(t, p, "a", 2*time.Second)

	a.closeListener()
	closed := time.Now()
	var reached time.Duration
	for {
		if time.Since(closed) > 2*time.Second {
			t.Fatal("new connections never reached b")
		}
		if _, id := dialID(t, p.Addr(EndpointWrite)); id == "b" {
			reached = time.Since(closed)
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	limit := cfg.Probe.Interval.D() + cfg.Probe.Timeout.D() + 150*time.Millisecond
	t.Logf("failover reached b after %s (probe interval %s)", reached, cfg.Probe.Interval.D())
	if reached > limit {
		t.Fatalf("failover took %s, want <= %s", reached, limit)
	}
	st := p.Status()
	if st.Writer != "b" {
		t.Fatalf("status writer = %s", st.Writer)
	}
}

// scripted is a deterministic in-memory prober.
type scripted struct {
	mu sync.Mutex
	fn map[string]func() ProbeResult
}

func (s *scripted) set(addr string, f func() ProbeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fn == nil {
		s.fn = map[string]func() ProbeResult{}
	}
	s.fn[addr] = f
}

func (s *scripted) Probe(ctx context.Context, addr string) ProbeResult {
	s.mu.Lock()
	f := s.fn[addr]
	s.mu.Unlock()
	if f == nil {
		return ProbeResult{Err: context.DeadlineExceeded}
	}
	return f()
}

func healthy(lat time.Duration) func() ProbeResult {
	return func() ProbeResult { return ProbeResult{Healthy: true, Sync: routing.SyncSynced, Latency: lat} }
}

func down() ProbeResult { return ProbeResult{Err: net.ErrClosed} }

func TestDrainClosesOldWriterConnectionsAfterDeadline(t *testing.T) {
	a := newFakeBackend(t, "a")
	b := newFakeBackend(t, "b")
	pr := &scripted{}
	pr.set(a.addr(), healthy(time.Millisecond))
	pr.set(b.addr(), healthy(5*time.Millisecond))
	cfg := testConfig(node("a", a.addr()), node("b", b.addr()))
	drain := 400 * time.Millisecond
	cfg.Policy.DrainDeadline = Duration(drain)
	p := startProxy(t, cfg, Options{Prober: pr})
	waitWriter(t, p, "a", 2*time.Second)

	old, id := dialID(t, p.Addr(EndpointWrite))
	if id != "a" {
		t.Fatalf("first connection went to %q", id)
	}
	pr.set(a.addr(), down) // a fails its probes; its server still serves old conns
	waitWriter(t, p, "b", 2*time.Second)
	switched := time.Now()

	if _, id := dialID(t, p.Addr(EndpointWrite)); id != "b" {
		t.Fatalf("new connection after switch went to %q, want b", id)
	}
	if st := p.Status(); st.Nodes[0].Draining != 1 {
		t.Fatalf("status draining for a = %d, want 1", st.Nodes[0].Draining)
	}
	time.Sleep(drain / 3)
	if err := old.echo("still-alive"); err != nil {
		t.Fatalf("old connection closed before the drain deadline: %v", err)
	}
	at, ok := old.waitClosed(drain + time.Second)
	if !ok {
		t.Fatal("old connection was not closed after the drain deadline")
	}
	// The deadline is computed at the decision, which is at most one round
	// before we observed the switch.
	if el := at.Sub(switched); el < drain-cfg.Probe.Interval.D()-50*time.Millisecond {
		t.Fatalf("closed after %s, before the drain deadline %s", el, drain)
	}
}

func TestDrainCancelledWhenNodeReturns(t *testing.T) {
	a := newFakeBackend(t, "a")
	b := newFakeBackend(t, "b")
	pr := &scripted{}
	pr.set(a.addr(), healthy(time.Millisecond))
	pr.set(b.addr(), healthy(5*time.Millisecond))
	cfg := testConfig(node("a", a.addr()), node("b", b.addr()))
	cfg.Policy.Mode = "prefer-node"
	cfg.Policy.PreferredID = "a"
	cfg.Policy.SwitchConsecutive = 1
	cfg.Policy.DrainDeadline = Duration(500 * time.Millisecond)
	p := startProxy(t, cfg, Options{Prober: pr})
	waitWriter(t, p, "a", 2*time.Second)
	old, _ := dialID(t, p.Addr(EndpointWrite))
	pr.set(a.addr(), down)
	waitWriter(t, p, "b", 2*time.Second)
	pr.set(a.addr(), healthy(time.Millisecond))
	waitWriter(t, p, "a", 2*time.Second)
	if _, closed := old.waitClosed(800 * time.Millisecond); closed {
		t.Fatal("connection to a was closed although a became writer again before the deadline")
	}
}

// TestNoFlappingWithJitter drives probe rounds directly with seeded jitter
// around two close latencies. With the default hysteresis the writer never
// moves; the control run without hysteresis shows the input does flap.
func TestNoFlappingWithJitter(t *testing.T) {
	run := func(margin float64, consecutive int) int {
		pr := &scripted{}
		var mu sync.Mutex
		rnd := rand.New(rand.NewSource(42))
		jitter := func(base time.Duration) func() ProbeResult {
			return func() ProbeResult {
				mu.Lock()
				j := time.Duration(rnd.Int63n(int64(4*time.Millisecond))) - 2*time.Millisecond
				mu.Unlock()
				return ProbeResult{Healthy: true, Sync: routing.SyncSynced, Latency: base + j}
			}
		}
		pr.set("10.0.0.1:3306", jitter(10*time.Millisecond))
		pr.set("10.0.0.2:3306", jitter(9500*time.Microsecond))
		cfg := testConfig(node("a", "10.0.0.1:3306"), node("b", "10.0.0.2:3306"))
		cfg.Policy.Mode = "lowest-latency"
		cfg.Policy.SwitchMargin = margin
		cfg.Policy.SwitchConsecutive = consecutive
		p, err := New(cfg, Options{Prober: pr})
		if err != nil {
			t.Fatal(err)
		}
		defer p.cancel()
		for range 300 {
			p.probeRound(p.ctx)
		}
		p.probeWG.Wait()
		return p.Status().WriterChanges - 1 // the first selection is not a flap
	}
	if flaps := run(0, 0); flaps != 0 {
		t.Fatalf("writer flapped %d times with default hysteresis", flaps)
	}
	if flaps := run(0.001, 1); flaps == 0 {
		t.Fatal("control run without hysteresis did not flap; the test input is too weak")
	} else {
		t.Logf("control run without hysteresis: %d writer changes", flaps)
	}
}

func TestStaleProbeMakesNodeUnknown(t *testing.T) {
	a := newFakeBackend(t, "a")
	release := make(chan struct{})
	var hang atomic.Bool
	pr := ProberFunc(func(ctx context.Context, addr string) ProbeResult {
		if hang.Load() {
			<-release // ignores ctx on purpose: no report at all
		}
		return ProbeResult{Healthy: true, Sync: routing.SyncSynced, Latency: time.Millisecond}
	})
	cfg := testConfig(node("a", a.addr()))
	p := startProxy(t, cfg, Options{Prober: pr})
	defer close(release) // runs before the Shutdown cleanup
	waitWriter(t, p, "a", 2*time.Second)
	hang.Store(true)
	waitWriter(t, p, "none", 3*time.Second)
	st := p.Status()
	if st.Nodes[0].Health != Unknown {
		t.Fatalf("stale node health = %q, want unknown", st.Nodes[0].Health)
	}
	if !strings.Contains(strings.Join(st.Reasons, ";"), "stale") {
		t.Fatalf("reasons do not mention staleness: %v", st.Reasons)
	}
	if code, msg := dialErr(t, p.Addr(EndpointWrite)); code != erUnknownError || !strings.Contains(msg, "no usable writer") {
		t.Fatalf("got error %d %q", code, msg)
	}
}

func TestGaleraSyncStubFailsClosed(t *testing.T) {
	a := newFakeBackend(t, "a")
	cfg := testConfig(node("a", a.addr()))
	cfg.Probe.Sync = SyncGalera
	p := startProxy(t, cfg, Options{})
	waitFor(t, 2*time.Second, "first decision", func() bool { return p.Status().DecidedAt != nil })
	st := p.Status()
	if st.HasWriter {
		t.Fatalf("writer %s chosen without sync state", st.Writer)
	}
	n := st.Nodes[0]
	if n.Health != "healthy" || n.Sync != "unknown" || !strings.Contains(n.LastError, "not implemented") {
		t.Fatalf("node status = %+v", n)
	}
	dialErr(t, p.Addr(EndpointWrite))
}

func TestConnectionLimits(t *testing.T) {
	a := newFakeBackend(t, "a")
	t.Run("per listener", func(t *testing.T) {
		cfg := testConfig(node("a", a.addr()))
		cfg.Limits.MaxConnections = 2
		p := startProxy(t, cfg, Options{})
		waitWriter(t, p, "a", 2*time.Second)
		c1, _ := dialID(t, p.Addr(EndpointWrite))
		c2, _ := dialID(t, p.Addr(EndpointWrite))
		if c1 == nil || c2 == nil {
			t.Fatal("connections below the limit were refused")
		}
		if code, _ := dialErr(t, p.Addr(EndpointWrite)); code != erConCountError {
			t.Fatalf("over limit: code %d, want %d", code, erConCountError)
		}
		_ = c1.c.Close()
		waitFor(t, time.Second, "slot released", func() bool { return p.Status().Connections.Write == 1 })
		if c, id := dialID(t, p.Addr(EndpointWrite)); c == nil || id != "a" {
			t.Fatal("connection refused after a slot was released")
		}
	})
	t.Run("per client", func(t *testing.T) {
		cfg := testConfig(node("a", a.addr()))
		cfg.Limits.MaxConnectionsPerClient = 1
		p := startProxy(t, cfg, Options{})
		waitWriter(t, p, "a", 2*time.Second)
		if c, _ := dialID(t, p.Addr(EndpointWrite)); c == nil {
			t.Fatal("first connection refused")
		}
		if code, msg := dialErr(t, p.Addr(EndpointWrite)); code != erConCountError || !strings.Contains(msg, "client") {
			t.Fatalf("got %d %q", code, msg)
		}
	})
}

func TestIdleTimeout(t *testing.T) {
	a := newFakeBackend(t, "a")
	cfg := testConfig(node("a", a.addr()))
	idle := 250 * time.Millisecond
	cfg.Limits.IdleTimeout = Duration(idle)
	p := startProxy(t, cfg, Options{})
	waitWriter(t, p, "a", 2*time.Second)

	idleConn, _ := dialID(t, p.Addr(EndpointWrite))
	active, _ := dialID(t, p.Addr(EndpointWrite))
	start := time.Now()
	for time.Since(start) < 3*idle {
		if err := active.echo("ping"); err != nil {
			t.Fatalf("active connection failed after %s: %v", time.Since(start), err)
		}
		time.Sleep(idle / 5)
	}
	if _, ok := idleConn.waitClosed(time.Second); !ok {
		t.Fatal("idle connection not closed")
	}
}

func TestDialFailureRefusesAndKicksProbe(t *testing.T) {
	a := newFakeBackend(t, "a")
	addr := a.addr()
	pr := &scripted{}
	pr.set(addr, healthy(time.Millisecond)) // claims healthy though it is gone
	cfg := testConfig(node("a", addr))
	cfg.Limits.DialTimeout = Duration(200 * time.Millisecond)
	p := startProxy(t, cfg, Options{Prober: pr})
	waitWriter(t, p, "a", 2*time.Second)
	a.shutdown()
	start := time.Now()
	code, msg := dialErr(t, p.Addr(EndpointWrite))
	if code != erUnknownError || !strings.Contains(msg, "unreachable") {
		t.Fatalf("got %d %q", code, msg)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("refusal took %s", el)
	}
}

func TestReadEndpoint(t *testing.T) {
	a := newFakeBackend(t, "a")
	b := newFakeBackend(t, "b")
	c := newFakeBackend(t, "c")
	pr := &scripted{}
	pr.set(a.addr(), healthy(time.Millisecond))
	pr.set(b.addr(), healthy(2*time.Millisecond))
	pr.set(c.addr(), healthy(3*time.Millisecond))
	cfg := testConfig(node("a", a.addr()), node("b", b.addr()), NodeConfig{ID: "c", Address: c.addr(), Role: "reader"})
	cfg.ReadListen = "127.0.0.1:0"
	cfg.Policy.Read = "exclude-writer"
	p := startProxy(t, cfg, Options{Prober: pr})
	waitWriter(t, p, "a", 2*time.Second)
	waitFor(t, time.Second, "readers", func() bool { return len(p.Status().Readers) == 2 })
	seen := map[string]bool{}
	for range 4 {
		_, id := dialID(t, p.Addr(EndpointRead))
		seen[id] = true
	}
	if !seen["b"] || !seen["c"] || seen["a"] {
		t.Fatalf("read endpoint reached %v, want b and c only", seen)
	}
	// c leaves the reader set: its read connection is drained.
	rc, id := dialID(t, p.Addr(EndpointRead))
	for id != "c" {
		rc, id = dialID(t, p.Addr(EndpointRead))
	}
	pr.set(c.addr(), down)
	if _, ok := rc.waitClosed(2 * time.Second); !ok {
		t.Fatal("read connection to removed reader not drained")
	}
}

func TestStatusEndpoint(t *testing.T) {
	a := newFakeBackend(t, "a")
	block := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	pr := ProberFunc(func(ctx context.Context, addr string) ProbeResult {
		if first.Load() {
			select {
			case <-block:
			case <-ctx.Done():
				return ProbeResult{Err: ctx.Err()}
			}
		}
		return ProbeResult{Healthy: true, Sync: routing.SyncSynced, Latency: 2 * time.Millisecond}
	})
	cfg := testConfig(node("a", a.addr()), NodeConfig{ID: "s", Address: "127.0.0.1:1", Role: "standby"})
	cfg.ControlListen = "127.0.0.1:0"
	p, err := New(cfg, Options{Prober: pr})
	if err != nil {
		t.Fatal(err)
	}
	// Before any probe round: everything is unknown.
	st := p.Status()
	if st.Writer != Unknown || st.Nodes[0].Health != Unknown || st.Nodes[0].LatencyEWMA != Unknown || st.Nodes[0].Age != Unknown {
		t.Fatalf("initial status not unknown: %+v", st)
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	}()
	close(block)
	first.Store(false)
	waitWriter(t, p, "a", 2*time.Second)

	resp, err := http.Get("http://" + p.ControlAddr() + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got Status
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Writer != "a" || got.WriterAddress != a.addr() || len(got.Reasons) == 0 || got.DecidedAt == nil {
		t.Fatalf("status = %+v", got)
	}
	n := got.Nodes[0]
	if n.Health != "healthy" || n.Sync != "synced" || n.LatencyEWMA == Unknown || n.LastSeen == nil || n.Age == Unknown {
		t.Fatalf("node a = %+v", n)
	}
	if got.Nodes[1].Role != "standby" {
		t.Fatalf("node s = %+v", got.Nodes[1])
	}
	if r, err := http.Post("http://"+p.ControlAddr()+"/status", "text/plain", nil); err == nil {
		r.Body.Close()
		if r.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST /status = %d", r.StatusCode)
		}
	}
}

func TestReloadKeepsWriterAndAppliesPolicy(t *testing.T) {
	a := newFakeBackend(t, "a")
	b := newFakeBackend(t, "b")
	pr := &scripted{}
	pr.set(a.addr(), healthy(time.Millisecond))
	pr.set(b.addr(), healthy(time.Millisecond))
	cfg := testConfig(node("a", a.addr()), node("b", b.addr()))
	p := startProxy(t, cfg, Options{Prober: pr})
	waitWriter(t, p, "a", 2*time.Second)
	old, _ := dialID(t, p.Addr(EndpointWrite))

	// Unchanged policy with a new probe interval keeps the writer.
	same := cfg
	same.Probe.Interval = Duration(30 * time.Millisecond)
	same.Probe.Timeout = Duration(30 * time.Millisecond)
	if err := p.Reload(same); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if w := p.Status().Writer; w != "a" {
		t.Fatalf("reload moved the writer to %s", w)
	}

	next := same
	next.Policy.Mode = "prefer-node"
	next.Policy.PreferredID = "b"
	if err := p.Reload(next); err != nil {
		t.Fatal(err)
	}
	waitWriter(t, p, "b", 2*time.Second)
	if _, ok := old.waitClosed(2 * time.Second); !ok {
		t.Fatal("old writer connection not drained after reload")
	}

	bad := next
	bad.WriteListen = "127.0.0.1:1"
	if err := p.Reload(bad); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("listen change: err = %v", err)
	}
	removed := next
	removed.Nodes = []NodeConfig{node("a", a.addr())}
	removed.Policy = PolicyConfig{StaleAfter: next.Policy.StaleAfter, DrainDeadline: next.Policy.DrainDeadline}
	if err := p.Reload(removed); err != nil {
		t.Fatal(err)
	}
	waitWriter(t, p, "a", 2*time.Second)
	if n := len(p.Status().Nodes); n != 1 {
		t.Fatalf("nodes after removal = %d", n)
	}
}

func TestGracefulShutdownWaitsThenCloses(t *testing.T) {
	a := newFakeBackend(t, "a")
	cfg := testConfig(node("a", a.addr()))
	p, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	waitWriter(t, p, "a", 2*time.Second)
	c, _ := dialID(t, p.Addr(EndpointWrite))
	addr := p.Addr(EndpointWrite)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := p.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown returned nil although a connection outlived the grace period")
	}
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Fatalf("shutdown did not wait for the grace period (%s)", el)
	}
	if _, ok := c.waitClosed(time.Second); !ok {
		t.Fatal("connection still open after shutdown")
	}
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("listener still accepting after shutdown")
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	a := newFakeBackend(t, "a")
	time.Sleep(20 * time.Millisecond)
	base := runtime.NumGoroutine()
	cfg := testConfig(node("a", a.addr()))
	cfg.ControlListen = "127.0.0.1:0"
	cfg.Limits.MaxConnections = 3
	p, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	waitWriter(t, p, "a", 2*time.Second)
	var conns []*client
	for range 3 {
		c, _ := dialID(t, p.Addr(EndpointWrite))
		conns = append(conns, c)
	}
	dialErr(t, p.Addr(EndpointWrite)) // over the limit
	_ = conns[0].echo("x")
	_ = conns[1].c.Close()
	if r, err := http.Get("http://" + p.ControlAddr() + "/status"); err == nil {
		r.Body.Close()
	}
	http.DefaultClient.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = p.Shutdown(ctx)
	for _, c := range conns {
		_ = c.c.Close()
	}
	// The fake backend's per-connection goroutines end when the proxy
	// closed its side.
	if n, ok := goroutinesSettle(base, 3*time.Second); !ok {
		t.Fatalf("goroutines: %d after shutdown, baseline %d", n, base)
	}
}
