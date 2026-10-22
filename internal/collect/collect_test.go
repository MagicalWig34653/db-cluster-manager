// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
)

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

type fakeStatus struct {
	vars map[string]map[string]string
	at   time.Time
	err  map[string]error
}

func (f *fakeStatus) Status(_ context.Context, t Target) (map[string]string, time.Time, error) {
	if e := f.err[t.Node]; e != nil {
		return nil, time.Time{}, e
	}
	return f.vars[t.Node], f.at, nil
}

func byName(es []metrics.Entry) map[string][]metrics.Entry {
	m := map[string][]metrics.Entry{}
	for _, e := range es {
		m[e.Labels.Name()] = append(m[e.Labels.Name()], e)
	}
	return m
}

func val(t *testing.T, es []metrics.Entry, name, node string) float64 {
	t.Helper()
	for _, e := range es {
		if e.Labels.Name() == name && (node == "" || e.Labels.Get(LabelNode) == node) {
			return e.Sample.V
		}
	}
	t.Fatalf("no sample %s node=%s", name, node)
	return 0
}

var healthyVars = map[string]string{
	"wsrep_cluster_size": "3", "wsrep_cluster_status": "Primary", "wsrep_local_state": "4",
	"wsrep_ready": "ON", "wsrep_connected": "ON", "wsrep_flow_control_paused": "0.25",
	"Threads_connected": "12", "Queries": "1000", "Slow_queries": "3", "Bytes_sent": "99", "Bytes_received": "55",
	"Innodb_buffer_pool_reads": "10", "Innodb_buffer_pool_read_requests": "1000",
	"Innodb_buffer_pool_pages_total": "100", "Innodb_buffer_pool_pages_free": "25", "Innodb_buffer_pool_pages_dirty": "5",
	"Seconds_Behind_Master": "NULL",
}

func TestGaleraMapping(t *testing.T) {
	src := &fakeStatus{vars: map[string]map[string]string{"n1": healthyVars}, at: t0}
	g, err := NewGaleraCollector(GaleraConfig{Source: src, Targets: func() []Target {
		return []Target{{Swarm: "s", Cluster: "c", Node: "n1", ExpectedSize: 5}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	es, err := g.Collect(context.Background(), t0)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{
		MTargetUp: 1, MGaleraClusterSize: 3, MGaleraSizeDeficit: 2, MGaleraPrimary: 1, MGaleraReady: 1,
		MGaleraFlowControlPaused: 0.25, MDBThreadsConnected: 12, MDBQueries: 1000, MDBSlowQueries: 3,
		MDBBufferPoolHitRatio: 0.99, MDBBufferPoolUsedRatio: 0.75, MDBBufferPoolDirtyRatio: 0.05,
	} {
		if got := val(t, es, name, ""); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if _, ok := byName(es)[MDBReplicationLagSeconds]; ok {
		t.Error("NULL replication lag must not become a sample")
	}
	if _, ok := byName(es)[MGaleraRecvQueueAvg]; ok {
		t.Error("absent variable must not become a sample")
	}
	for _, e := range es {
		if e.Sample.T != t0.UnixMilli() {
			t.Fatal("timestamp")
		}
		if _, ok := Lookup(e.Labels.Name()); !ok {
			t.Errorf("%s emitted but not in the catalog", e.Labels.Name())
		}
	}
}

func TestGaleraStaleAndErrorsProduceOnlyUp0(t *testing.T) {
	src := &fakeStatus{
		vars: map[string]map[string]string{"old": healthyVars, "empty": {}, "bad": healthyVars},
		at:   t0.Add(-5 * time.Minute),
		err:  map[string]error{"bad": errors.New("boom")},
	}
	g, _ := NewGaleraCollector(GaleraConfig{Source: src, Targets: func() []Target {
		return []Target{{Swarm: "s", Cluster: "c", Node: "old"}, {Swarm: "s", Cluster: "c", Node: "empty"}, {Swarm: "s", Cluster: "c", Node: "bad"}}
	}})
	es, err := g.Collect(context.Background(), t0)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if len(es) != 3 {
		t.Fatalf("want 3 samples, got %d", len(es))
	}
	for _, e := range es {
		if e.Labels.Name() != MTargetUp || e.Sample.V != 0 {
			t.Fatalf("unexpected %v", e)
		}
	}
}

func TestGaleraUnparsableAndLimits(t *testing.T) {
	v := map[string]string{"wsrep_cluster_size": "x", "wsrep_cluster_status": "weird", "wsrep_ready": "maybe", "Threads_connected": "NaN"}
	src := &fakeStatus{vars: map[string]map[string]string{"n": v}, at: t0}
	g, _ := NewGaleraCollector(GaleraConfig{Source: src, MaxTargets: 1, Targets: func() []Target {
		return []Target{{Swarm: "s", Cluster: "c", Node: "n"}, {Swarm: "s", Cluster: "c", Node: "dropped"}}
	}})
	es, err := g.Collect(context.Background(), t0)
	if err == nil {
		t.Fatal("target limit must be reported")
	}
	if len(es) != 1 || es[0].Labels.Name() != MTargetUp || es[0].Sample.V != 1 {
		t.Fatalf("only up=1 expected, got %v", es)
	}
}

type fakeStats struct {
	snap Snapshot
	err  error
}

func (f fakeStats) Stats(context.Context) (Snapshot, error) { return f.snap, f.err }

func mustHost(t *testing.T, s StatsSource) *HostCollector {
	t.Helper()
	h, err := NewHostCollector(HostConfig{Source: s})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHostCollector(t *testing.T) {
	snap := Snapshot{ObservedAt: t0, Containers: []ContainerStats{
		{Swarm: "s", Cluster: "c", Node: "n1", Host: "h1", Container: "db1", CPURatio: 0.5, MemUsed: 50, MemLimit: 100},
		{Swarm: "s", Cluster: "c", Node: "n2", Host: "h1", Container: "db2", MemUsed: 5},
	}, Hosts: []HostStats{{Swarm: "s", Host: "h1", CPURatio: 0.2, MemRatio: 1.5, DiskUsed: 90, DiskTotal: 100}, {Swarm: "s", Host: "h2"}}}
	es, err := mustHost(t, fakeStats{snap: snap}).Collect(context.Background(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if val(t, es, MContainerMemRatio, "n1") != 0.5 || val(t, es, MHostDiskRatio, "") != 0.9 || val(t, es, MHostMemRatio, "") != 1 {
		t.Fatal("ratios")
	}
	if n := len(byName(es)[MContainerMemRatio]); n != 1 {
		t.Fatalf("unlimited container must not get a ratio, got %d", n)
	}
	if n := len(byName(es)[MHostDiskRatio]); n != 1 {
		t.Fatalf("host without disk info must not get a ratio, got %d", n)
	}
	es, err = mustHost(t, fakeStats{err: errors.New("docker down")}).Collect(context.Background(), t0)
	if err == nil || len(es) != 1 || es[0].Sample.V != 0 {
		t.Fatalf("error: %v %v", es, err)
	}
	snap.ObservedAt = t0.Add(-time.Hour)
	es, _ = mustHost(t, fakeStats{snap: snap}).Collect(context.Background(), t0)
	if len(es) != 1 || es[0].Sample.V != 0 {
		t.Fatalf("stale: %v", es)
	}
}

type fakeDoer struct {
	body   string
	status int
	err    error
}

func (f fakeDoer) Do(*http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	rec := httptest.NewRecorder()
	rec.WriteHeader(f.status)
	_, _ = io.WriteString(rec, f.body)
	return rec.Result(), nil
}

func proxyDoc(now time.Time) string {
	return `{"now":"` + now.Format(time.RFC3339) + `","writer":"n1","has_writer":true,"readers":["n2"],"writer_changes":2,
"connections":{"write":3,"read":4},"nodes":[{"id":"n1","health":"healthy","connections":3,"draining":1},{"id":"n2","health":"unknown","connections":4}]}`
}

func TestProxyCollector(t *testing.T) {
	tg := func() []ProxyTarget {
		return []ProxyTarget{{Swarm: "s", Cluster: "c", Name: "p1", URL: "http://x/status"}}
	}
	mk := func(d fakeDoer) *ProxyCollector {
		p, err := NewProxyCollector(ProxyConfig{Targets: tg, Client: d})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	es, err := mk(fakeDoer{body: proxyDoc(t0), status: 200}).Collect(context.Background(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if val(t, es, MProxyHasWriter, "") != 1 || val(t, es, MProxyWriterChanges, "") != 2 || val(t, es, MProxyNodeHealthy, "n1") != 1 {
		t.Fatal("values")
	}
	if n := len(byName(es)[MProxyNodeHealthy]); n != 1 {
		t.Fatalf("unknown node health must not be sampled, got %d", n)
	}
	for _, d := range []fakeDoer{{err: errors.New("refused")}, {body: "x", status: 500}, {body: "{", status: 200}, {body: proxyDoc(t0.Add(-time.Hour)), status: 200}} {
		es, _ := mk(d).Collect(context.Background(), t0)
		if len(es) != 1 || es[0].Labels.Name() != MTargetUp || es[0].Sample.V != 0 {
			t.Fatalf("%+v -> %v", d, es)
		}
	}
}

func TestCatalogIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Catalog {
		if seen[d.Name] || !strings.HasPrefix(d.Name, "dcm_") || d.Help == "" || d.Unit == "" {
			t.Errorf("bad catalog entry %+v", d)
		}
		if d.Type == Counter && !strings.HasSuffix(d.Name, "_total") {
			t.Errorf("counter %s must end in _total", d.Name)
		}
		seen[d.Name] = true
	}
}

type allowPolicy struct{ ok func(authz.Resource) bool }

func (a allowPolicy) Check(_ authz.Subject, _ authz.Action, r authz.Resource) authz.Decision {
	return authz.Decision{Allowed: a.ok(r)}
}

func TestResourceOfAndAuthorizer(t *testing.T) {
	cl := metrics.MustLabels("m", LabelSwarm, "s", LabelCluster, "c")
	host := metrics.MustLabels("m", LabelSwarm, "s", LabelHost, "h")
	self := metrics.MustLabels("m", LabelCollector, "galera")
	broken := metrics.MustLabels("m", LabelCluster, "c")
	if r := ResourceOf(cl); r.Type != authz.ResourceCluster || r.Scope != authz.ClusterScope("s", "c") {
		t.Fatal(r)
	}
	if r := ResourceOf(host); r.Type != authz.ResourceHost {
		t.Fatal(r)
	}
	if r := ResourceOf(self); r.Type != authz.ResourceSettings {
		t.Fatal(r)
	}
	u := authz.Subject{ID: "u", Kind: authz.SubjectUser}
	a := SeriesAuthorizer(allowPolicy{ok: func(authz.Resource) bool { return true }}, u)
	if !a(cl) || a(broken) {
		t.Fatal("authorizer")
	}
	only := SeriesAuthorizer(allowPolicy{ok: func(r authz.Resource) bool { return r.Scope.Swarm == "s" && r.Type == authz.ResourceCluster }}, u)
	if !only(cl) || only(host) || only(self) {
		t.Fatal("scoped authorizer")
	}
}

// --- scheduler ---

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type sink struct {
	mu      sync.Mutex
	entries []metrics.Entry
	err     error
}

func (s *sink) Append(e []metrics.Entry) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.entries = append(s.entries, e...)
	return len(e), nil
}

func (s *sink) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *sink) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.entries {
		if e.Labels.Name() == name {
			n++
		}
	}
	return n
}

type fnCollector struct {
	name string
	fn   func(ctx context.Context, now time.Time) ([]metrics.Entry, error)
}

func (f fnCollector) Name() string { return f.name }
func (f fnCollector) Collect(ctx context.Context, now time.Time) ([]metrics.Entry, error) {
	return f.fn(ctx, now)
}

func one(now time.Time) []metrics.Entry {
	return []metrics.Entry{{Labels: metrics.MustLabels("x_total"), Sample: metrics.Sample{T: now.UnixMilli(), V: 1}}}
}

func TestSchedulerIntervalJitterAndSelfMetrics(t *testing.T) {
	clk := &clock{t: t0}
	sk := &sink{}
	var runs atomic.Int32
	mk := func(name string) Job {
		return Job{Interval: time.Minute, Collector: fnCollector{name, func(_ context.Context, now time.Time) ([]metrics.Entry, error) {
			runs.Add(1)
			return one(now), nil
		}}}
	}
	s, err := NewScheduler(SchedulerConfig{Sink: sk, Now: clk.Now, Jobs: []Job{mk("a"), mk("b")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range s.jobs {
		if off := j.next.Sub(t0); off < 0 || off >= 6*time.Second {
			t.Fatalf("jitter %v", off)
		}
	}
	clk.Add(7 * time.Second)
	s.RunDue(context.Background())
	if runs.Load() != 2 {
		t.Fatalf("runs = %d", runs.Load())
	}
	s.RunDue(context.Background()) // nothing due
	if runs.Load() != 2 {
		t.Fatal("ran twice in one interval")
	}
	clk.Add(time.Minute)
	s.RunDue(context.Background())
	if runs.Load() != 4 {
		t.Fatalf("runs = %d", runs.Load())
	}
	if sk.count(MCollectDuration) != 4 || sk.count(MCollectLastSuccess) != 4 {
		t.Fatalf("self metrics missing: %d", sk.count(MCollectDuration))
	}
	st := s.Stats()
	if st[0].Name != "a" || st[0].Errors != 0 || st[0].LastSamples != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestSchedulerTimeoutDoesNotStack(t *testing.T) {
	clk := &clock{t: t0}
	release := make(chan struct{})
	var starts atomic.Int32
	c := fnCollector{"slow", func(ctx context.Context, _ time.Time) ([]metrics.Entry, error) {
		starts.Add(1)
		<-release // ignores ctx on purpose
		return nil, nil
	}}
	s, _ := NewScheduler(SchedulerConfig{Sink: &sink{}, Now: clk.Now, JitterFraction: 0.0001, Jobs: []Job{{Collector: c, Interval: time.Minute, Timeout: 30 * time.Millisecond}}})
	clk.Add(time.Second)
	s.RunDue(context.Background())
	clk.Add(time.Minute)
	s.RunDue(context.Background())
	if starts.Load() != 1 {
		t.Fatalf("started %d times while the first run was still in flight", starts.Load())
	}
	st := s.Stats()[0]
	if st.Errors != 1 || st.Skipped != 1 || !strings.Contains(st.LastError, "deadline") {
		t.Fatalf("%+v", st)
	}
	close(release)
}

func TestSchedulerPartialResultAndError(t *testing.T) {
	clk := &clock{t: t0}
	sk := &sink{}
	c := fnCollector{"p", func(_ context.Context, now time.Time) ([]metrics.Entry, error) {
		return one(now), errors.New("half failed")
	}}
	s, _ := NewScheduler(SchedulerConfig{Sink: sk, Now: clk.Now, Jobs: []Job{{Collector: c, Interval: time.Second}}})
	clk.Add(time.Second)
	s.RunDue(context.Background())
	if sk.count("x_total") != 1 {
		t.Fatal("partial samples must be stored")
	}
	if st := s.Stats()[0]; st.Errors != 1 || st.LastError != "half failed" || !st.LastSuccess.IsZero() {
		t.Fatalf("%+v", st)
	}
}

func TestSchedulerPanicIsContained(t *testing.T) {
	clk := &clock{t: t0}
	c := fnCollector{"p", func(context.Context, time.Time) ([]metrics.Entry, error) { panic("x") }}
	s, _ := NewScheduler(SchedulerConfig{Sink: &sink{}, Now: clk.Now, Jobs: []Job{{Collector: c, Interval: time.Second}}})
	clk.Add(time.Second)
	s.RunDue(context.Background())
	if s.Stats()[0].Errors != 1 {
		t.Fatal("panic must count as error")
	}
}

func TestSchedulerBackpressureBacksOffAndRecovers(t *testing.T) {
	clk := &clock{t: t0}
	sk := &sink{err: metrics.ErrDiskBudget}
	var runs atomic.Int32
	c := fnCollector{"bp", func(_ context.Context, now time.Time) ([]metrics.Entry, error) {
		runs.Add(1)
		return one(now), nil
	}}
	s, _ := NewScheduler(SchedulerConfig{Sink: sk, Now: clk.Now, BackoffBase: 10 * time.Second, BackoffMax: 40 * time.Second,
		Jobs: []Job{{Collector: c, Interval: time.Second}}})
	step := func() { clk.Add(time.Second); s.RunDue(context.Background()) }
	step() // run 1, rejected, backoff 10s
	if st := s.Stats()[0]; st.Backoff != 10*time.Second || st.Rejected != 1 {
		t.Fatalf("%+v", st)
	}
	for range 5 {
		step()
	}
	if runs.Load() != 1 || s.Stats()[0].Skipped != 5 {
		t.Fatalf("runs=%d stats=%+v", runs.Load(), s.Stats()[0])
	}
	for range 6 {
		step()
	}
	if runs.Load() != 2 || s.Stats()[0].Backoff != 20*time.Second {
		t.Fatalf("backoff must double: runs=%d %+v", runs.Load(), s.Stats()[0])
	}
	sk.setErr(nil)
	for range 25 {
		step()
	}
	if st := s.Stats()[0]; st.Backoff != 0 {
		t.Fatalf("backoff must reset after success: %+v", st)
	}
}

func TestSchedulerValidation(t *testing.T) {
	if _, err := NewScheduler(SchedulerConfig{}); err == nil {
		t.Fatal("sink required")
	}
	c := fnCollector{"a", nil}
	if _, err := NewScheduler(SchedulerConfig{Sink: &sink{}, Jobs: []Job{{Collector: c, Interval: time.Second}, {Collector: c, Interval: time.Second}}}); err == nil {
		t.Fatal("duplicate")
	}
	if _, err := NewScheduler(SchedulerConfig{Sink: &sink{}, Jobs: []Job{{Collector: c}}}); err == nil {
		t.Fatal("interval")
	}
}

func TestSchedulerRunLoopStops(t *testing.T) {
	clk := &clock{t: t0}
	var runs atomic.Int32
	c := fnCollector{"l", func(_ context.Context, now time.Time) ([]metrics.Entry, error) { runs.Add(1); return one(now), nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, _ := NewScheduler(SchedulerConfig{Sink: &sink{}, Now: clk.Now, JitterFraction: 0.0001, Jobs: []Job{{Collector: c, Interval: time.Second}},
		Sleep: func(context.Context, time.Duration) {
			clk.Add(time.Second)
			if runs.Load() >= 3 {
				cancel()
			}
		}})
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if runs.Load() < 3 {
		t.Fatalf("runs %d", runs.Load())
	}
}

func TestSamplesAreAcceptedByTheStore(t *testing.T) {
	st, err := metrics.Open(metrics.DefaultOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	src := &fakeStatus{vars: map[string]map[string]string{"n1": healthyVars}, at: now}
	g, _ := NewGaleraCollector(GaleraConfig{Source: src, Targets: func() []Target { return []Target{{Swarm: "s", Cluster: "c", Node: "n1"}} }})
	es, err := g.Collect(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.Append(es); err != nil || n != len(es) {
		t.Fatalf("append %d/%d: %v", n, len(es), err)
	}
}
