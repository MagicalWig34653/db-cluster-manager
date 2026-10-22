// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/alerts"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

// Monitoring is the manager's monitoring service. It implements
// api.Monitoring.
//
// Single writer: the metric store, the collector scheduler, the alert
// evaluator and the notification dispatcher run only while this replica
// leads (they are started by the leadership hook and bound to the
// leadership context). Followers neither collect nor evaluate. The store
// lives on the leader's local disk; after a failover the new leader starts
// with its own, possibly empty, store, and the former leader's history is
// not visible (ADR 0006: no metric replication).
//
// Dashboards live in the replicated store (CollDashboards) and work on every
// replica. The UI inbox and the event log are in memory on the leader.
//
// Settings (retention, disk budget, channels, scrape interval, disabled
// rules) are read when the leadership starts and on Reload.
type Monitoring struct {
	svc *Service
	cfg MonitoringConfig
	log *slog.Logger

	inbox *notify.Inbox
	nlog  *notify.Log

	mu      sync.RWMutex
	run     *monRun
	lastErr string
	reload  chan struct{}
}

// MonitoringConfig wires the monitoring service.
type MonitoringConfig struct {
	// DataDir holds the metric store (DataDir/metrics). Empty disables
	// collection, alerting and metric queries; dashboards still work.
	DataDir string
	// Checker evaluates read permissions for alert evaluation, which runs as
	// collect.SystemSubject. That subject must be bound to a role that can
	// read what the rules select. Required when DataDir is set.
	Checker authz.Checker

	// Sources of observations; a nil source disables its collector.
	Status       collect.StatusSource
	Stats        collect.StatsSource
	HTTP         collect.HTTPDoer
	ProxyTargets func() []collect.ProxyTarget
	// Targets lists database nodes; the default derives them from the
	// replicated cluster records.
	Targets func() []collect.Target

	// Rules replaces alerts.DefaultRules.
	Rules []alerts.Rule
	// Channels adds delivery channels besides "ui" and those configured in
	// the settings (a channel with the same name wins over settings).
	Channels []notify.Channel
	// ReadSecret reads secret files (default os.ReadFile).
	ReadSecret func(path string) ([]byte, error)
	// Keys unseals notification secrets stored through the API
	// (CollNotifySecrets); nil disables them.
	Keys KeyProvider

	// CollectInterval overrides the scrape interval of the settings (tests;
	// the settings minimum does not apply).
	CollectInterval time.Duration
	// EvalInterval, MaintainInterval and RetryInterval default to 30s, 1m
	// and 10s.
	EvalInterval     time.Duration
	MaintainInterval time.Duration
	RetryInterval    time.Duration
	// InboxSize and LogSize bound the in-memory inbox and event log
	// (defaults 500 and 5000).
	InboxSize, LogSize int

	// StoreTune adjusts the metric store options (tests).
	StoreTune func(*metrics.Options)
	Logger    *slog.Logger
	Now       func() time.Time
}

type monRun struct {
	store *metrics.Store
	sched *collect.Scheduler
	eval  *alerts.Evaluator
	disp  *notify.Dispatcher
}

var _ api.Monitoring = (*Monitoring)(nil)

// NewMonitoring creates the service and registers its leadership hook on svc.
// Call it before svc.Start.
func NewMonitoring(svc *Service, cfg MonitoringConfig) (*Monitoring, error) {
	if svc == nil {
		return nil, errors.New("manager: monitoring needs a Service")
	}
	if cfg.DataDir != "" && cfg.Checker == nil {
		return nil, errors.New("manager: monitoring with a data dir needs a Checker")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReadSecret == nil {
		cfg.ReadSecret = os.ReadFile
	}
	if cfg.EvalInterval <= 0 {
		cfg.EvalInterval = 30 * time.Second
	}
	if cfg.MaintainInterval <= 0 {
		cfg.MaintainInterval = time.Minute
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 10 * time.Second
	}
	if cfg.InboxSize <= 0 {
		cfg.InboxSize = 500
	}
	if cfg.LogSize <= 0 {
		cfg.LogSize = 5000
	}
	if cfg.Rules == nil {
		cfg.Rules = alerts.DefaultRules()
	}
	for _, r := range cfg.Rules {
		if err := r.Validate(); err != nil {
			return nil, err
		}
	}
	m := &Monitoring{
		svc: svc, cfg: cfg, log: cfg.Logger, reload: make(chan struct{}, 1),
		inbox: notify.NewInbox(cfg.InboxSize, cfg.Now),
		nlog:  notify.NewLog(cfg.LogSize, cfg.Now),
	}
	svc.OnLeadership(m.onLeadership)
	return m, nil
}

func (m *Monitoring) onLeadership(l *Leadership) {
	if l == nil || m.cfg.DataDir == "" {
		return // stepping down cancels the term context; runTerm cleans up
	}
	l.Go(m.runTerm)
}

// Reload re-reads the settings and restarts the leader components.
func (m *Monitoring) Reload() {
	select {
	case m.reload <- struct{}{}:
	default:
	}
}

// MonitoringStatus is a snapshot for diagnostics.
type MonitoringStatus struct {
	Running   bool
	LastError string
	Store     metrics.Stats
	Jobs      []collect.JobStats
	Alerts    []alerts.Instance
}

// Status returns a snapshot. Running is false on followers.
func (m *Monitoring) Status() MonitoringStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := MonitoringStatus{LastError: m.lastErr}
	if m.run != nil {
		st.Running = true
		st.Store = m.run.store.Stats()
		st.Jobs = m.run.sched.Stats()
		st.Alerts = m.run.eval.Instances()
	}
	return st
}

func (m *Monitoring) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastErr = ""
	} else {
		m.lastErr = err.Error()
	}
}

// settings loads the effective, validated monitoring settings.
func (m *Monitoring) settings() (MonitoringSettings, error) {
	v, ok, err := Get[Settings](m.svc.store, CollSettings, SettingsKey, Consistent())
	if err != nil {
		return MonitoringSettings{}, err
	}
	var ms MonitoringSettings
	if ok {
		ms = v.Value.Monitoring
	}
	if err := ms.Validate(); err != nil {
		return MonitoringSettings{}, fmt.Errorf("invalid monitoring settings: %w", err)
	}
	return ms.Effective(), nil
}

func (m *Monitoring) runTerm(ctx context.Context) {
	for ctx.Err() == nil {
		reload := m.serve(ctx)
		if reload {
			continue
		}
		return
	}
}

// serve builds and runs the components until ctx ends (returns false) or a
// reload is requested (returns true). A build failure is retried.
func (m *Monitoring) serve(ctx context.Context) (reload bool) {
	for {
		run, rctx, cancel, err := m.build(ctx)
		if err == nil {
			return m.operate(ctx, run, rctx, cancel)
		}
		m.setErr(err)
		m.log.Warn("manager: monitoring not started", "err", err)
		t := time.NewTimer(m.cfg.RetryInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-m.reload:
			t.Stop()
		case <-t.C:
		}
	}
}

func (m *Monitoring) build(ctx context.Context) (*monRun, context.Context, context.CancelFunc, error) {
	set, err := m.settings()
	if err != nil {
		return nil, nil, nil, err
	}
	opts := metrics.DefaultOptions(filepath.Join(m.cfg.DataDir, "metrics"))
	opts.RawRetention, opts.AggRetention, opts.MaxBytes = set.rawRetention(), set.aggRetention(), set.MaxBytes
	opts.Now = m.cfg.Now
	if m.cfg.StoreTune != nil {
		m.cfg.StoreTune(&opts)
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, nil, nil, err
	}
	store, err := metrics.Open(opts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open metric store: %w", err)
	}
	fail := func(err error) (*monRun, context.Context, context.CancelFunc, error) {
		_ = store.Close()
		return nil, nil, nil, err
	}
	interval := time.Duration(set.CollectIntervalSeconds) * time.Second
	if m.cfg.CollectInterval > 0 {
		interval = m.cfg.CollectInterval
	}
	var jobs []collect.Job
	targets := m.cfg.Targets
	if targets == nil {
		targets = m.clusterTargets
	}
	if m.cfg.Status != nil {
		g, err := collect.NewGaleraCollector(collect.GaleraConfig{Source: m.cfg.Status, Targets: targets, StaleAfter: 4 * interval})
		if err != nil {
			return fail(err)
		}
		jobs = append(jobs, collect.Job{Collector: g, Interval: interval})
	}
	if m.cfg.Stats != nil {
		h, err := collect.NewHostCollector(collect.HostConfig{Source: m.cfg.Stats, StaleAfter: 4 * interval})
		if err != nil {
			return fail(err)
		}
		jobs = append(jobs, collect.Job{Collector: h, Interval: interval})
	}
	if m.cfg.HTTP != nil && m.cfg.ProxyTargets != nil {
		p, err := collect.NewProxyCollector(collect.ProxyConfig{Targets: m.cfg.ProxyTargets, Client: m.cfg.HTTP, StaleAfter: 4 * interval})
		if err != nil {
			return fail(err)
		}
		jobs = append(jobs, collect.Job{Collector: p, Interval: interval})
	}
	sched, err := collect.NewScheduler(collect.SchedulerConfig{Sink: store, Jobs: jobs, Now: m.cfg.Now})
	if err != nil {
		return fail(err)
	}
	eval, err := alerts.New(alerts.Config{
		Rules:      alerts.Without(m.cfg.Rules, set.DisabledRules...),
		Querier:    store,
		Authorizer: collect.SeriesAuthorizer(m.cfg.Checker, collect.SystemSubject),
		Now:        m.cfg.Now,
	})
	if err != nil {
		return fail(err)
	}
	disp, err := m.dispatcher(set)
	if err != nil {
		return fail(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	return &monRun{store: store, sched: sched, eval: eval, disp: disp}, rctx, cancel, nil
}

// dispatcher wires the channels from the settings. A channel that cannot be
// built (unreadable secret, invalid address) is skipped and reported through
// Status().LastError; the UI inbox is always present.
func (m *Monitoring) dispatcher(set MonitoringSettings) (*notify.Dispatcher, error) {
	min, err := parseMinSeverity(set.Notifications.MinSeverity)
	if err != nil {
		return nil, err
	}
	channels := []notify.Channel{m.inbox}
	var external []string
	var problems []error
	have := map[string]bool{"ui": true}
	for _, c := range m.cfg.Channels {
		if !have[c.Name()] {
			channels = append(channels, c)
			external = append(external, c.Name())
			have[c.Name()] = true
		}
	}
	if w := set.Notifications.Webhook; w != nil && !w.Disabled && !have["webhook"] {
		ch, err := m.webhookChannel(w)
		if err == nil {
			channels, external = append(channels, ch), append(external, ch.Name())
		} else {
			problems = append(problems, fmt.Errorf("webhook channel: %w", err))
		}
	}
	if x := set.Notifications.Email; x != nil && !x.Disabled && !have["email"] {
		ch, err := m.emailChannel(x)
		if err == nil {
			channels, external = append(channels, ch), append(external, ch.Name())
		} else {
			problems = append(problems, fmt.Errorf("email channel: %w", err))
		}
	}
	if len(problems) > 0 {
		e := errors.Join(problems...)
		m.setErr(e)
		m.log.Warn("manager: notification channels skipped", "err", e)
	} else {
		m.setErr(nil)
	}
	rules := []notify.Rule{{Name: "inbox", Channels: []string{"ui"}}}
	if len(external) > 0 {
		rules = append(rules, notify.Rule{Name: "external", Match: notify.Matcher{MinSeverity: min}, Channels: external})
	}
	return notify.NewDispatcher(m.nlog, notify.NewRouter(rules, m.cfg.Now), m.cfg.Now, channels...), nil
}

// storedSecret reads a notification secret set through the API.
func (m *Monitoring) storedSecret(key string) (string, error) {
	if m.cfg.Keys == nil {
		return "", errors.New("no data key to unseal stored secrets")
	}
	v, ok, err := GetSealed[string](m.svc.store, m.cfg.Keys, CollNotifySecrets, key)
	if err != nil {
		return "", err
	}
	if !ok || v == "" {
		return "", fmt.Errorf("stored %s secret missing", key)
	}
	return v, nil
}

// webhookChannel builds the webhook channel of the settings.
func (m *Monitoring) webhookChannel(w *WebhookSettings) (*notify.Webhook, error) {
	var secret string
	if w.SecretFile != "" {
		b, err := m.cfg.ReadSecret(w.SecretFile)
		if err != nil {
			return nil, err
		}
		secret = strings.TrimSpace(string(b))
	} else {
		var err error
		if secret, err = m.storedSecret("webhook"); err != nil {
			return nil, err
		}
	}
	return notify.NewWebhook(notify.WebhookConfig{
		URL: w.URL, Secret: []byte(secret), AllowPrivate: w.AllowPrivate, MaxRetries: min2(w.MaxRetries, 5),
	})
}

// emailChannel builds the email channel of the settings.
func (m *Monitoring) emailChannel(x *EmailSettings) (*notify.Email, error) {
	cfg := notify.EmailConfig{Addr: x.Addr, From: x.From, To: x.To, Username: x.Username, AllowInsecure: x.AllowInsecure}
	switch {
	case x.PasswordFile != "":
		pw, err := m.cfg.ReadSecret(x.PasswordFile)
		if err != nil {
			return nil, err
		}
		cfg.Password = strings.TrimSpace(string(pw))
	case x.PasswordStored:
		pw, err := m.storedSecret("email")
		if err != nil {
			return nil, err
		}
		cfg.Password = pw
	}
	return notify.NewEmail(cfg)
}

func min2(a, b int) int {
	if a > b {
		return b
	}
	return a
}

func (m *Monitoring) operate(ctx context.Context, run *monRun, rctx context.Context, cancel context.CancelFunc) (reload bool) {
	m.mu.Lock()
	m.run = run
	m.mu.Unlock()
	var wg sync.WaitGroup
	loop := func(every time.Duration, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-rctx.Done():
					return
				case <-t.C:
					fn()
				}
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); run.sched.Run(rctx) }()
	loop(m.cfg.EvalInterval, func() {
		events, err := run.eval.Evaluate(rctx)
		if err != nil && rctx.Err() == nil {
			m.log.Warn("manager: alert evaluation", "err", err)
		}
		for _, e := range events {
			run.disp.Handle(rctx, e)
		}
		run.disp.Escalate(rctx)
	})
	loop(m.cfg.MaintainInterval, func() {
		if err := run.store.Maintain(rctx); err != nil && rctx.Err() == nil {
			m.log.Warn("manager: metric store maintenance", "err", err)
		}
	})
	select {
	case <-ctx.Done():
	case <-m.reload:
		reload = true
	}
	cancel()
	wg.Wait()
	// Wait for in-flight queries (read locks) before closing the store.
	m.mu.Lock()
	m.run = nil
	m.mu.Unlock()
	if err := run.store.Close(); err != nil {
		m.log.Warn("manager: metric store close", "err", err)
	}
	return reload
}

// clusterTargets derives the Galera targets from the replicated cluster
// records: one per node slot, with the desired size for the size-deficit
// metric.
func (m *Monitoring) clusterTargets() []collect.Target {
	recs, err := List[ClusterRecord](m.svc.store, CollClusters)
	if err != nil {
		return nil
	}
	var out []collect.Target
	for _, r := range recs {
		for _, n := range r.Value.Nodes {
			out = append(out, collect.Target{Swarm: r.Value.Swarm, Cluster: r.Value.ID, Node: n.Slot, ExpectedSize: r.Value.Size})
		}
	}
	return out
}

// Query implements api.Monitoring. Only the leader holds metric data.
func (m *Monitoring) Query(ctx context.Context, q metrics.Query) (*metrics.Result, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.run == nil {
		return nil, api.ErrUnavailable
	}
	return m.run.store.Query(ctx, q)
}

// --- dashboards (replicated store) ---

func mapStoreErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrConflict):
		return api.ErrConflict
	case errors.Is(err, ErrNotFound):
		return api.ErrNotFound
	case errors.Is(err, ErrNotLeader):
		return fmt.Errorf("%w: %v", api.ErrUnavailable, err)
	}
	return err
}

// Dashboards implements api.Monitoring.
func (m *Monitoring) Dashboards(context.Context) ([]metrics.Dashboard, error) {
	docs, err := List[metrics.Dashboard](m.svc.store, CollDashboards)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	out := make([]metrics.Dashboard, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Value)
	}
	return out, nil
}

// Dashboard implements api.Monitoring.
func (m *Monitoring) Dashboard(_ context.Context, id string) (metrics.Dashboard, error) {
	d, ok, err := Get[metrics.Dashboard](m.svc.store, CollDashboards, id)
	if err != nil {
		return metrics.Dashboard{}, mapStoreErr(err)
	}
	if !ok {
		return metrics.Dashboard{}, api.ErrNotFound
	}
	return d.Value, nil
}

// CreateDashboard implements api.Monitoring.
func (m *Monitoring) CreateDashboard(_ context.Context, d metrics.Dashboard) error {
	if err := d.Validate(); err != nil {
		return err
	}
	_, err := Put(m.svc.store, CollDashboards, d.ID, d, IfVersion(0))
	return mapStoreErr(err)
}

// UpdateDashboard implements api.Monitoring. The owner and template flag of
// the stored dashboard are kept.
func (m *Monitoring) UpdateDashboard(_ context.Context, d metrics.Dashboard) error {
	if err := d.Validate(); err != nil {
		return err
	}
	_, _, err := Update(m.svc.store, CollDashboards, d.ID, func(cur metrics.Dashboard, exists bool) (metrics.Dashboard, error) {
		if !exists {
			return cur, ErrNotFound
		}
		d.Owner, d.Template = cur.Owner, cur.Template
		return d, nil
	})
	return mapStoreErr(err)
}

// DeleteDashboard implements api.Monitoring.
func (m *Monitoring) DeleteDashboard(_ context.Context, id string) error {
	if _, ok, err := Get[metrics.Dashboard](m.svc.store, CollDashboards, id, Consistent()); err != nil {
		return mapStoreErr(err)
	} else if !ok {
		return api.ErrNotFound
	}
	return mapStoreErr(Delete(m.svc.store, CollDashboards, id, nil))
}

// --- notifications (leader memory) ---

func toNotification(it notify.InboxItem) api.Notification {
	e := it.Event
	return api.Notification{
		ID: e.ID, Time: e.Time, Severity: e.Severity.String(), Kind: e.Kind, Resource: e.Resource, Message: e.Message,
		Swarm: e.Details["swarm"], Cluster: e.Details["cluster"], Details: e.Details,
		Read: it.Read, Acked: it.Acked, AckedAt: it.AckedAt,
	}
}

func (m *Monitoring) requireLeader() error {
	if !m.svc.IsLeader() {
		return fmt.Errorf("%w: notifications are held by the leading manager", api.ErrUnavailable)
	}
	return nil
}

// Notifications implements api.Monitoring.
func (m *Monitoring) Notifications(context.Context) ([]api.Notification, error) {
	if err := m.requireLeader(); err != nil {
		return nil, err
	}
	items := m.inbox.List(false)
	out := make([]api.Notification, 0, len(items))
	for _, it := range items {
		out = append(out, toNotification(it))
	}
	return out, nil
}

// Notification implements api.Monitoring.
func (m *Monitoring) Notification(ctx context.Context, id string) (api.Notification, error) {
	items, err := m.Notifications(ctx)
	if err != nil {
		return api.Notification{}, err
	}
	for _, n := range items {
		if n.ID == id {
			return n, nil
		}
	}
	return api.Notification{}, api.ErrNotFound
}

// AckNotification implements api.Monitoring. The acknowledgement is held in
// memory with the inbox.
func (m *Monitoring) AckNotification(_ context.Context, id, by string) error {
	if err := m.requireLeader(); err != nil {
		return err
	}
	if !m.inbox.Ack(id) {
		return api.ErrNotFound
	}
	m.nlog.Append(notify.Event{Severity: notify.SeverityInfo, Kind: notify.KindPrefix + "ack", Message: "notification acknowledged",
		Details: map[string]string{"event_id": id, "by": by}})
	return nil
}

// MarkNotificationRead implements api.Monitoring.
func (m *Monitoring) MarkNotificationRead(_ context.Context, id string) error {
	if err := m.requireLeader(); err != nil {
		return err
	}
	if !m.inbox.MarkRead(id) {
		return api.ErrNotFound
	}
	return nil
}

// Notify delivers an event (for example from other subsystems) through the
// running dispatcher. It reports false when this replica is not running the
// monitoring components.
func (m *Monitoring) Notify(ctx context.Context, e notify.Event) bool {
	m.mu.RLock()
	run := m.run
	m.mu.RUnlock()
	if run == nil {
		return false
	}
	run.disp.Handle(ctx, e)
	return true
}
