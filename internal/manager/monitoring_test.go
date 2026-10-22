// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/alerts"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

type fakeStatusSrc struct {
	mu      sync.Mutex
	primary bool
	calls   atomic.Int32
}

func (f *fakeStatusSrc) Status(_ context.Context, t collect.Target) (map[string]string, time.Time, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	st := "Primary"
	if !f.primary {
		st = "non-Primary"
	}
	return map[string]string{"wsrep_cluster_size": "3", "wsrep_cluster_status": st, "wsrep_ready": "ON", "wsrep_connected": "ON", "Threads_connected": "7"}, time.Now(), nil
}

func (f *fakeStatusSrc) set(primary bool) {
	f.mu.Lock()
	f.primary = primary
	f.mu.Unlock()
}

type recChannel struct {
	name string
	mu   sync.Mutex
	got  []notify.Event
}

func (c *recChannel) Name() string { return c.name }
func (c *recChannel) Send(_ context.Context, e notify.Event) error {
	c.mu.Lock()
	c.got = append(c.got, e)
	c.mu.Unlock()
	return nil
}
func (c *recChannel) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.got) }

func systemPolicy(t *testing.T) *authz.Policy {
	t.Helper()
	p := authz.NewPolicy()
	if err := p.Bind(authz.Binding{Subject: collect.SystemSubject, Role: authz.RoleManagerSystem, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	return p
}

func fastRule() []alerts.Rule {
	return []alerts.Rule{{ID: "not-primary", Summary: "node not primary", Selector: collect.MGaleraPrimary, Cmp: alerts.Below, Threshold: 1, Severity: notify.SeverityCritical}}
}

// monCluster builds n replicas, each with its own Monitoring and data dir.
func monCluster(t *testing.T, n int, status collect.StatusSource, mut func(id string, c *MonitoringConfig)) (*cluster, map[string]*Monitoring) {
	t.Helper()
	mons := map[string]*Monitoring{}
	var mu sync.Mutex
	pol := systemPolicy(t)
	c := newClusterWith(t, n, nil, func(id string, s *Service) {
		cfg := MonitoringConfig{
			DataDir: t.TempDir(), Checker: pol, Status: status, CollectInterval: 20 * time.Millisecond,
			EvalInterval: 20 * time.Millisecond, MaintainInterval: 50 * time.Millisecond, RetryInterval: 20 * time.Millisecond,
			Rules: fastRule(),
		}
		if mut != nil {
			mut(id, &cfg)
		}
		m, err := NewMonitoring(s, cfg)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		mons[id] = m
		mu.Unlock()
	})
	return c, mons
}

func seedCluster(t *testing.T, s *Service) {
	t.Helper()
	rec := ClusterRecord{ID: "c1", Swarm: "s1", Name: "orders", Size: 3, Nodes: []galera.Node{{Slot: "n1"}, {Slot: "n2"}, {Slot: "n3"}}}
	if _, err := Put(s.Store(), CollClusters, "c1", rec, nil); err != nil {
		t.Fatal(err)
	}
}

func queryNow(m *Monitoring, name string) (*metrics.Result, error) {
	sel, _ := metrics.ParseSelector(name)
	now := time.Now().UnixMilli()
	return m.Query(context.Background(), metrics.Query{Selector: sel, Start: now - 10_000, End: now, Step: 1000, Window: 10_000, Func: metrics.FuncLast, Authorizer: metrics.AllowAll})
}

func TestMonitoringRunsOnlyOnLeaderAndFollowsFailover(t *testing.T) {
	src := &fakeStatusSrc{primary: true}
	c, mons := monCluster(t, 3, src, nil)
	l := c.leader()
	seedCluster(t, l)
	ml := mons[l.cfg.NodeID]
	waitFor(t, "leader collecting", func() bool {
		r, err := queryNow(ml, collect.MGaleraClusterSize)
		return err == nil && len(r.Series) == 3
	})
	if st := ml.Status(); !st.Running || len(st.Jobs) != 1 || st.Jobs[0].Name != "galera" {
		t.Fatalf("%+v", st)
	}
	for _, s := range c.live(l.cfg.NodeID) {
		f := mons[s.cfg.NodeID]
		if f.Status().Running {
			t.Fatal("follower runs monitoring")
		}
		if _, err := queryNow(f, collect.MGaleraClusterSize); !errors.Is(err, api.ErrUnavailable) {
			t.Fatalf("follower query err = %v", err)
		}
		if _, err := f.Notifications(context.Background()); !errors.Is(err, api.ErrUnavailable) {
			t.Fatalf("follower notifications err = %v", err)
		}
	}
	c.close(l)
	nl := c.leader(l.cfg.NodeID)
	nm := mons[nl.cfg.NodeID]
	waitFor(t, "new leader collecting", func() bool { return nm.Status().Running })
	if mons[l.cfg.NodeID].Status().Running {
		t.Fatal("closed leader still running")
	}
	// The old leader's history is not visible on the new leader (documented).
	waitFor(t, "new leader has its own data", func() bool {
		r, err := queryNow(nm, collect.MGaleraClusterSize)
		return err == nil && len(r.Series) == 3
	})
}

func TestAlertsReachInboxAndExternalChannelsFromSettings(t *testing.T) {
	src := &fakeStatusSrc{primary: true}
	ext := &recChannel{name: "pager"}
	c, mons := monCluster(t, 1, src, func(_ string, cfg *MonitoringConfig) { cfg.Channels = []notify.Channel{ext} })
	l := c.leader()
	m := mons[l.cfg.NodeID]
	seedCluster(t, l)
	waitFor(t, "healthy data", func() bool { r, err := queryNow(m, collect.MGaleraPrimary); return err == nil && len(r.Series) == 3 })
	ctx := context.Background()
	time.Sleep(100 * time.Millisecond)
	if n, _ := m.Notifications(ctx); len(n) != 0 {
		for _, x := range n {
			if x.Kind == alerts.KindFiring {
				t.Fatalf("healthy cluster must not fire: %+v", n)
			}
		}
	}
	src.set(false)
	var notes []api.Notification
	waitFor(t, "firing notifications", func() bool {
		notes, _ = m.Notifications(ctx)
		n := 0
		for _, x := range notes {
			if x.Kind == alerts.KindFiring {
				n++
			}
		}
		return n >= 3
	})
	var fire api.Notification
	for _, x := range notes {
		if x.Kind == alerts.KindFiring {
			fire = x
		}
	}
	if fire.Swarm != "s1" || fire.Cluster != "c1" || fire.Severity != "critical" || fire.Acked {
		t.Fatalf("%+v", fire)
	}
	waitFor(t, "external channel", func() bool { return ext.count() >= 3 })
	if err := m.AckNotification(ctx, fire.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	got, err := m.Notification(ctx, fire.ID)
	if err != nil || !got.Acked || !got.Read {
		t.Fatalf("%+v %v", got, err)
	}
	if err := m.AckNotification(ctx, "nope", "alice"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if _, err := m.Notification(ctx, "nope"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	// recovery produces a resolved notification
	src.set(true)
	waitFor(t, "resolved", func() bool {
		notes, _ = m.Notifications(ctx)
		for _, x := range notes {
			if x.Kind == alerts.KindResolved {
				return true
			}
		}
		return false
	})
}

func TestSettingsDriveRetentionBudgetAndWebhook(t *testing.T) {
	hits := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits <- r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "hook.secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := &fakeStatusSrc{primary: false}
	c, mons := monCluster(t, 1, src, nil)
	l := c.leader()
	m := mons[l.cfg.NodeID]
	seedCluster(t, l)
	set := Settings{InstallationID: "i", VolumeBase: "/v", Monitoring: MonitoringSettings{
		RawRetentionDays: 3, AggRetentionDays: 30, MaxBytes: 128 << 20,
		Notifications: NotificationSettings{MinSeverity: "error", Webhook: &WebhookSettings{URL: srv.URL, SecretFile: secret, AllowPrivate: true}},
	}}
	if _, err := Put(l.Store(), CollSettings, SettingsKey, set, nil); err != nil {
		t.Fatal(err)
	}
	m.Reload()
	waitFor(t, "reloaded with the budget", func() bool { st := m.Status(); return st.Running && st.Store.MaxBytes == 128<<20 })
	select {
	case ct := <-hits:
		if ct != "application/json" {
			t.Fatalf("content type %q", ct)
		}
	case <-time.After(3 * time.Second):
		for _, e := range m.nlog.Query(notify.Query{}) {
			t.Logf("%s %s %v", e.Kind, e.Message, e.Details)
		}
		t.Fatal("webhook not called")
	}
	if e := m.Status().LastError; e != "" {
		t.Fatalf("unexpected error %q", e)
	}
}

func TestInvalidSettingsKeepMonitoringOffUntilFixed(t *testing.T) {
	src := &fakeStatusSrc{primary: true}
	c, mons := monCluster(t, 1, src, nil)
	l := c.leader()
	m := mons[l.cfg.NodeID]
	waitFor(t, "running with defaults", func() bool { return m.Status().Running })
	bad := Settings{InstallationID: "i", VolumeBase: "/v", Monitoring: MonitoringSettings{RawRetentionDays: 10, AggRetentionDays: 5}}
	if _, err := Put(l.Store(), CollSettings, SettingsKey, bad, nil); err != nil {
		t.Fatal(err)
	}
	m.Reload()
	waitFor(t, "stopped with an error", func() bool { st := m.Status(); return !st.Running && st.LastError != "" })
	good := bad
	good.Monitoring = MonitoringSettings{}
	if _, err := Put(l.Store(), CollSettings, SettingsKey, good, nil); err != nil {
		// version conflict: overwrite using Update
		if _, _, err := Update(l.Store(), CollSettings, SettingsKey, func(Settings, bool) (Settings, error) { return good, nil }); err != nil {
			t.Fatal(err)
		}
	}
	m.Reload()
	waitFor(t, "running again", func() bool { st := m.Status(); return st.Running && st.LastError == "" })
}

func TestUnreadableChannelSecretSkipsChannelOnly(t *testing.T) {
	src := &fakeStatusSrc{primary: false}
	c, mons := monCluster(t, 1, src, nil)
	l := c.leader()
	m := mons[l.cfg.NodeID]
	seedCluster(t, l)
	set := Settings{InstallationID: "i", VolumeBase: "/v", Monitoring: MonitoringSettings{
		Notifications: NotificationSettings{Webhook: &WebhookSettings{URL: "https://hooks.example.test/x", SecretFile: "/nonexistent/secret"}},
	}}
	if _, err := Put(l.Store(), CollSettings, SettingsKey, set, nil); err != nil {
		t.Fatal(err)
	}
	m.Reload()
	waitFor(t, "channel error reported", func() bool { st := m.Status(); return st.Running && st.LastError != "" })
	waitFor(t, "inbox still works", func() bool { n, _ := m.Notifications(context.Background()); return len(n) > 0 })
}

func TestMonitoringSettingsValidation(t *testing.T) {
	if err := (MonitoringSettings{}).Validate(); err != nil {
		t.Fatal(err)
	}
	e := MonitoringSettings{}.Effective()
	if e.RawRetentionDays != 7 || e.AggRetentionDays != 90 || e.MaxBytes != 2<<30 || e.CollectIntervalSeconds != 15 {
		t.Fatalf("%+v", e)
	}
	for name, s := range map[string]MonitoringSettings{
		"agg<raw":      {RawRetentionDays: 10, AggRetentionDays: 5},
		"small budget": {MaxBytes: 1024},
		"fast scrape":  {CollectIntervalSeconds: 1},
		"severity":     {Notifications: NotificationSettings{MinSeverity: "loud"}},
		"webhook":      {Notifications: NotificationSettings{Webhook: &WebhookSettings{URL: "x"}}},
		"email":        {Notifications: NotificationSettings{Email: &EmailSettings{Addr: "h:25"}}},
		"negative":     {RawRetentionDays: -1},
	} {
		if s.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// a long raw retention lifts the default aggregated retention
	if e := (MonitoringSettings{RawRetentionDays: 120}).Effective(); e.AggRetentionDays != 120 {
		t.Fatalf("%+v", e)
	}
}

func TestDashboardsInReplicatedStore(t *testing.T) {
	c := newCluster(t, 3, nil)
	l := c.leader()
	m, err := NewMonitoring(l, MonitoringConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fm, _ := NewMonitoring(c.follower(l), MonitoringConfig{})
	ctx := context.Background()
	d := metrics.Dashboard{ID: "d1", Title: "T", Owner: "alice", Shared: true, Panels: []metrics.Panel{{
		ID: "p", Title: "P", Kind: "line", Selector: "m", Func: "last", StepSeconds: 15, W: 6, H: 4}}}
	if err := m.CreateDashboard(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateDashboard(ctx, d); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("err %v", err)
	}
	invalid := d
	invalid.ID, invalid.Panels = "d2", []metrics.Panel{{ID: "p"}}
	if err := m.CreateDashboard(ctx, invalid); !errors.Is(err, metrics.ErrInvalidDashboard) {
		t.Fatalf("err %v", err)
	}
	waitFor(t, "visible on a follower", func() bool {
		l, err := fm.Dashboards(ctx)
		return err == nil && len(l) == 1 && l[0].ID == "d1"
	})
	upd := d
	upd.Title, upd.Owner = "New", "mallory"
	if err := m.UpdateDashboard(ctx, upd); err != nil {
		t.Fatal(err)
	}
	got, err := m.Dashboard(ctx, "d1")
	if err != nil || got.Title != "New" || got.Owner != "alice" {
		t.Fatalf("%+v %v", got, err)
	}
	missing := d
	missing.ID = "zz"
	if err := m.UpdateDashboard(ctx, missing); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if err := m.DeleteDashboard(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteDashboard(ctx, "d1"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if _, err := m.Dashboard(ctx, "d1"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestMonitoringWithoutDataDirOnlyServesDashboards(t *testing.T) {
	c := newCluster(t, 1, nil)
	l := c.leader()
	m, err := NewMonitoring(l, MonitoringConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queryNow(m, "m"); !errors.Is(err, api.ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	if _, err := NewMonitoring(l, MonitoringConfig{DataDir: t.TempDir()}); err == nil {
		t.Fatal("a data dir without a Checker must be rejected")
	}
	if _, err := NewMonitoring(l, MonitoringConfig{Rules: []alerts.Rule{{ID: "bad"}}}); err == nil {
		t.Fatal("invalid rules must be rejected")
	}
}
