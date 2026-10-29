// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

// Fakes only: in-memory KV, a fake registry resolver, a fake service
// driver and a fixed clock. No registry, Docker or Swarm is contacted.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

const inst = "inst-1"

type fakeSource struct {
	mu   sync.Mutex
	svcs []ManagedService
}

func (f *fakeSource) Services(context.Context) ([]ManagedService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ManagedService(nil), f.svcs...), nil
}

func (f *fakeSource) setImage(id, img string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.svcs {
		if f.svcs[i].ID == id {
			f.svcs[i].Image = img
		}
	}
}

type fakeResolver struct {
	mu      sync.Mutex
	tags    map[string][]string // repo -> tags
	digests map[string]string   // repo:tag -> digest
	err     error
	calls   int
}

func (f *fakeResolver) Tags(_ context.Context, r Ref) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.tags[r.Repo], nil
}

func (f *fakeResolver) Digest(_ context.Context, r Ref) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	d, ok := f.digests[r.Repo+":"+r.Tag]
	if !ok {
		return "", errors.New("no such tag")
	}
	return d, nil
}

func (f *fakeResolver) set(repo, tag, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.digests == nil {
		f.digests = map[string]string{}
	}
	f.digests[repo+":"+tag] = digest
	found := false
	if f.tags == nil {
		f.tags = map[string][]string{}
	}
	for _, t := range f.tags[repo] {
		found = found || t == tag
	}
	if !found {
		f.tags[repo] = append(f.tags[repo], tag)
	}
}

type fakeDriver struct {
	mu        sync.Mutex
	startErr  error
	statuses  []OpStatus // popped in order; the last repeats
	updates   []string   // image of each StartUpdate
	rollbacks int
	approvals []string // actor:op
	strategy  Strategy
	polls     int
}

func (d *fakeDriver) StartUpdate(_ context.Context, actor, id, image string, s Strategy) (Started, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.startErr != nil {
		return Started{}, d.startErr
	}
	d.updates = append(d.updates, id+"="+image)
	d.strategy = s
	return Started{OperationID: "op-1", Digest: "dg"}, nil
}

func (d *fakeDriver) StartRollback(_ context.Context, _, _ string, _ Strategy) (Started, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rollbacks++
	return Started{OperationID: "op-rb", Digest: "dg"}, nil
}

func (d *fakeDriver) Approve(_ context.Context, actor, op, digest string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if digest != "dg" {
		return errors.New("bad digest")
	}
	d.approvals = append(d.approvals, actor+":"+op)
	return nil
}

func (d *fakeDriver) Status(context.Context, string) (OpStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.polls++
	if len(d.statuses) == 0 {
		return OpStatus{State: OpSucceeded, Runtime: agentrt.Container}, nil
	}
	s := d.statuses[0]
	if len(d.statuses) > 1 {
		d.statuses = d.statuses[1:]
	}
	return s, nil
}

type harness struct {
	t      *testing.T
	svc    *Service
	src    *fakeSource
	res    *fakeResolver
	drv    *fakeDriver
	now    time.Time
	events []notify.Event
	leader bool
}

var (
	dgOld = "sha256:" + hex64('1')
	dgNew = "sha256:" + hex64('2')
	dgMin = "sha256:" + hex64('3')
)

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, leader: true, now: time.Date(2026, 10, 28, 2, 30, 0, 0, time.UTC)}
	h.src = &fakeSource{svcs: []ManagedService{
		{ID: "web", Name: "web", Swarm: "s1", Type: "container", Image: "nginx:1.25.3", Labels: map[string]string{LabelInstallation: inst}},
	}}
	h.res = &fakeResolver{}
	h.res.set("library/nginx", "1.25.3", dgOld)
	h.drv = &fakeDriver{}
	h.svc = &Service{KV: &vuln.MemoryKV{}, Journal: opjournal.New(), Source: h.src, Driver: h.drv, Resolver: h.res, Installation: inst,
		Now:    func() time.Time { return h.now },
		Sleep:  func(context.Context, time.Duration) error { return nil },
		Term:   func() (uint64, error) { return 7, h.errIfFollower() },
		Notify: func(_ context.Context, e notify.Event) { h.events = append(h.events, e) }}
	return h
}

func (h *harness) errIfFollower() error {
	if !h.leader {
		return errors.New("not leader")
	}
	return nil
}

func (h *harness) kinds() []string {
	var out []string
	for _, e := range h.events {
		out = append(out, e.Kind)
	}
	return out
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

var ctx = context.Background()

// baseline records the baseline digest of a service (first check).
func (h *harness) baseline(id string) {
	h.t.Helper()
	if _, err := h.svc.Check(ctx, id, CheckOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) policy(p Policy) {
	h.t.Helper()
	if _, err := h.svc.SetPolicy(ctx, "admin", p); err != nil {
		h.t.Fatal(err)
	}
}

func nightWindow() []patch.Window {
	return []patch.Window{{Start: "02:00", DurationMinutes: 120}}
}

func TestOwnershipScope(t *testing.T) {
	h := newHarness(t)
	h.src.svcs = append(h.src.svcs,
		ManagedService{ID: "other", Name: "other", Type: "container", Image: "nginx:1.25.3", Labels: map[string]string{LabelInstallation: "someone-else"}},
		ManagedService{ID: "bare", Name: "bare", Type: "container", Image: "nginx:1.25.3"},
		ManagedService{ID: "optin", Name: "optin", Type: "container", Image: "nginx:1.25.3", Foreign: true, Labels: map[string]string{LabelImageUpdate: OptInValue}},
		ManagedService{ID: "foreign", Name: "foreign", Type: "container", Image: "nginx:1.25.3", Foreign: true})
	views, err := h.svc.Services(ctx)
	h.must(err)
	got := map[string]string{}
	for _, v := range views {
		got[v.Service.ID] = v.Scope
	}
	want := map[string]string{"web": ScopeOwned, "optin": ScopeOptIn}
	if len(got) != len(want) || got["web"] != ScopeOwned || got["optin"] != ScopeOptIn {
		t.Fatalf("services in scope: %v", got)
	}
	for _, id := range []string{"other", "bare", "foreign", "nope"} {
		if _, err := h.svc.Check(ctx, id, CheckOptions{}); err == nil {
			t.Fatalf("check of out-of-scope %q succeeded", id)
		}
		if _, err := h.svc.SetPolicy(ctx, "admin", Policy{ServiceID: id, Mode: ModeNotify}); err == nil {
			t.Fatalf("policy for out-of-scope %q accepted", id)
		}
	}
	if h.res.calls != 0 {
		t.Fatal("registry contacted for an out-of-scope service")
	}
}

func TestPolicyValidation(t *testing.T) {
	h := newHarness(t)
	h.src.svcs = append(h.src.svcs,
		ManagedService{ID: "db", Name: "db", Type: "container", Image: "mariadb:11.4.2", Labels: map[string]string{LabelInstallation: inst}},
		ManagedService{ID: "gal", Name: "gal", Type: "galera", Image: "mariadb:11.4.2", Labels: map[string]string{LabelInstallation: inst}},
		ManagedService{ID: "opt", Name: "opt", Type: "container", Image: "nginx:1.25.3", Foreign: true, Labels: map[string]string{LabelImageUpdate: OptInValue}})
	bad := []Policy{
		{ServiceID: "web", Mode: "yolo"},
		{ServiceID: "web", Mode: ModeNotify, Track: "everything"},
		{ServiceID: "web", Mode: ModeAuto, Track: TrackMajor, Windows: nightWindow()},
		{ServiceID: "web", Mode: ModeAuto, Track: TrackPatch},                          // no window
		{ServiceID: "db", Mode: ModeNotify, Track: TrackMajor},                         // never major for a database
		{ServiceID: "gal", Mode: ModeAuto, Track: TrackDigest, Windows: nightWindow()}, // Galera never auto
		{ServiceID: "opt", Mode: ModeAuto, Track: TrackDigest, Windows: nightWindow()}, // foreign: report only
		{ServiceID: "web", Mode: ModeNotify, CheckEveryMinutes: 1},
		{ServiceID: "web", Mode: ModeNotify, Strategy: Strategy{Order: "sideways"}},
		{ServiceID: "web", Mode: ModeNotify, Windows: []patch.Window{{Start: "25:00", DurationMinutes: 5}}},
	}
	for i, p := range bad {
		if _, err := h.svc.SetPolicy(ctx, "admin", p); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad policy %d accepted: %v", i, err)
		}
	}
	ok := []Policy{
		{ServiceID: "web", Mode: ModeAuto, Track: TrackPatch, Windows: nightWindow()},
		{ServiceID: "db", Mode: ModeNotify, Track: TrackMinor},
		{ServiceID: "gal", Mode: ModeNotify, Track: TrackMinor},
		{ServiceID: "opt", Mode: ModeNotify, Track: TrackDigest},
	}
	for _, p := range ok {
		if _, err := h.svc.SetPolicy(ctx, "admin", p); err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
	}
	p, err := h.svc.Policy("web")
	h.must(err)
	if p.CheckEveryMinutes != DefaultCheckEvery || p.WaitMinutes != DefaultWaitMinute {
		t.Fatalf("defaults: %+v", p)
	}
	if p, _ := h.svc.Policy("nothing-set"); p.Mode != ModeManual {
		t.Fatal("default is not manual")
	}
}

func TestDigestFollowRecordsBaselineThenProposes(t *testing.T) {
	h := newHarness(t)
	h.policy(Policy{ServiceID: "web", Mode: ModeNotify, Track: TrackDigest})
	h.baseline("web")
	plans, _ := h.svc.Plans()
	if len(plans) != 0 || len(h.events) != 1 || h.events[0].Kind != "imageupdate.policy_changed" {
		t.Fatalf("first check proposed something: %v", plans)
	}
	st, _ := h.svc.State("web")
	if st.Baseline == nil || st.Baseline.Digest != dgOld || st.Baseline.Tag != "1.25.3" {
		t.Fatalf("%+v", st.Baseline)
	}
	// The tag moves to a new digest.
	h.res.set("library/nginx", "1.25.3", dgNew)
	res, err := h.svc.Check(ctx, "web", CheckOptions{})
	h.must(err)
	if res.Plan == nil || res.Plan.Kind != KindDigest || res.Plan.State != PlanProposed {
		t.Fatalf("%+v", res)
	}
	if res.Plan.From.Digest != dgOld || res.Plan.To.Digest != dgNew || res.Plan.To.Ref != "nginx:1.25.3@"+dgNew || res.Plan.Route != RouteServiceUpdate {
		t.Fatalf("%+v", res.Plan)
	}
	if got := h.kinds(); len(got) != 2 || got[1] != "imageupdate.candidate" {
		t.Fatalf("events %v", got)
	}
	// Same candidate again: no second plan, no second notification.
	res, err = h.svc.Check(ctx, "web", CheckOptions{})
	h.must(err)
	if res.Plan == nil || len(h.events) != 2 {
		t.Fatalf("duplicate: %v %v", res, h.kinds())
	}
	if plans, _ := h.svc.Plans(); len(plans) != 1 {
		t.Fatalf("%d plans", len(plans))
	}
}

func TestDigestPinnedSpecIsItsOwnBaseline(t *testing.T) {
	h := newHarness(t)
	h.src.setImage("web", "nginx:1.25.3@"+dgOld)
	h.policy(Policy{ServiceID: "web", Mode: ModeNotify})
	h.res.set("library/nginx", "1.25.3", dgNew)
	res, err := h.svc.Check(ctx, "web", CheckOptions{})
	h.must(err)
	if res.Plan == nil || res.Plan.From.Digest != dgOld || res.Plan.To.Ref != "nginx:1.25.3@"+dgNew {
		t.Fatalf("%+v", res.Plan)
	}
}

func TestNewerVersionTagCandidateByTrack(t *testing.T) {
	h := newHarness(t)
	h.src.setImage("web", "nginx:1.25.3@"+dgOld)
	h.res.set("library/nginx", "1.25.4", dgNew)
	h.res.set("library/nginx", "1.26.0", dgMin)
	h.res.set("library/nginx", "2.0.0", "sha256:"+hex64('4'))
	h.policy(Policy{ServiceID: "web", Mode: ModeNotify, Track: TrackPatch})
	res, err := h.svc.Check(ctx, "web", CheckOptions{})
	h.must(err)
	if res.Plan == nil || res.Plan.Kind != KindPatch || res.Plan.To.Tag != "1.25.4" || res.Plan.To.Ref != "nginx:1.25.4@"+dgNew {
		t.Fatalf("%+v", res.Plan)
	}
	// A wider track supersedes the open plan with the better candidate.
	h.policy(Policy{ServiceID: "web", Mode: ModeNotify, Track: TrackMinor})
	res, err = h.svc.Check(ctx, "web", CheckOptions{})
	h.must(err)
	if res.Plan == nil || res.Plan.Kind != KindMinor || res.Plan.To.Tag != "1.26.0" {
		t.Fatalf("%+v", res.Plan)
	}
	plans, _ := h.svc.Plans()
	superseded := 0
	for _, p := range plans {
		if p.State == PlanSuperseded {
			superseded++
		}
	}
	if len(plans) != 2 || superseded != 1 {
		t.Fatalf("%+v", plans)
	}
}

func TestManualModeIsNotScheduled(t *testing.T) {
	h := newHarness(t)
	h.src.setImage("web", "nginx:1.25.3@"+dgOld)
	h.res.set("library/nginx", "1.25.3", dgNew)
	if n, err := h.svc.RunDue(ctx); err != nil || n != 0 || h.res.calls != 0 {
		t.Fatalf("manual service was checked: %d %v %d", n, err, h.res.calls)
	}
	h.policy(Policy{ServiceID: "web", Mode: ModeNotify})
	n, err := h.svc.RunDue(ctx)
	h.must(err)
	if n != 1 {
		t.Fatalf("checked %d", n)
	}
	// Not due again before the interval elapsed.
	if n, _ := h.svc.RunDue(ctx); n != 0 {
		t.Fatalf("checked again after %d", n)
	}
	h.now = h.now.Add(7 * time.Hour)
	if n, _ := h.svc.RunDue(ctx); n != 1 {
		t.Fatalf("not due after the interval: %d", n)
	}
}

func proposeDigest(h *harness, mode Mode) Plan {
	h.t.Helper()
	h.src.setImage("web", "nginx:1.25.3@"+dgOld)
	h.res.set("library/nginx", "1.25.3", dgNew)
	h.policy(Policy{ServiceID: "web", Mode: mode, Track: TrackDigest, Windows: nightWindow(),
		Strategy: Strategy{Order: "start-first", Parallelism: 1, MonitorSeconds: 20}})
	res, err := h.svc.Check(ctx, "web", CheckOptions{})
	h.must(err)
	if res.Plan == nil {
		h.t.Fatal("no plan")
	}
	return *res.Plan
}

func TestApproveAndApplyHappyPath(t *testing.T) {
	h := newHarness(t)
	p := proposeDigest(h, ModeNotify)
	if _, err := h.svc.Apply(ctx, "alice", p.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("applied an unapproved plan: %v", err)
	}
	if _, err := h.svc.Approve(ctx, "alice", p.ID, strings.Repeat("0", 64)); !errors.Is(err, ErrDigest) {
		t.Fatalf("wrong digest: %v", err)
	}
	p, err := h.svc.Approve(ctx, "alice", p.ID, p.Digest)
	h.must(err)
	if p.State != PlanApproved || p.ApprovedBy != "alice" || p.Auto {
		t.Fatalf("%+v", p)
	}
	if len(h.drv.updates) != 0 {
		t.Fatal("approval started the update")
	}
	h.drv.statuses = []OpStatus{{State: OpRunning}, {State: OpRunning}, {State: OpSucceeded, Runtime: agentrt.Container}}
	p, err = h.svc.Apply(ctx, "alice", p.ID)
	h.must(err)
	if p.State != PlanSucceeded || p.ExecutedBy != agentrt.Container || p.OperationID != "op-1" || p.FinishedAt.IsZero() {
		t.Fatalf("%+v", p)
	}
	if len(h.drv.updates) != 1 || h.drv.updates[0] != "web=nginx:1.25.3@"+dgNew {
		t.Fatalf("%v", h.drv.updates)
	}
	if h.drv.strategy.Order != "start-first" || h.drv.strategy.MonitorSeconds != 20 {
		t.Fatalf("strategy %+v", h.drv.strategy)
	}
	if len(h.drv.approvals) != 1 || h.drv.approvals[0] != "alice:op-1" {
		t.Fatalf("operation approvals %v", h.drv.approvals)
	}
	st, _ := h.svc.State("web")
	if st.Baseline == nil || st.Baseline.Digest != dgNew || st.Baseline.PlanID != p.ID {
		t.Fatalf("baseline %+v", st.Baseline)
	}
	op, err := h.svc.Journal.Get(p.JournalID)
	h.must(err)
	if len(op.Steps) != 2 || op.Steps[0].State != opjournal.Succeeded || op.Steps[1].State != opjournal.Succeeded {
		t.Fatalf("journal %+v", op)
	}
	if got := h.kinds(); got[len(got)-1] != "imageupdate.applied" {
		t.Fatalf("%v", got)
	}
	// An applied plan cannot be applied or approved again.
	if _, err := h.svc.Apply(ctx, "alice", p.ID); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestFailedHealthGateRollsBackAndBlocksRetry(t *testing.T) {
	h := newHarness(t)
	p := proposeDigest(h, ModeNotify)
	p, err := h.svc.Approve(ctx, "alice", p.ID, p.Digest)
	h.must(err)
	h.drv.statuses = []OpStatus{{State: OpRolledBack, Runtime: agentrt.Container, Detail: "update state rollback_completed"}}
	p, err = h.svc.Apply(ctx, "alice", p.ID)
	h.must(err)
	if p.State != PlanRolledBack || p.Rollback == nil || !p.Rollback.Automatic || p.Rollback.Restored != "nginx:1.25.3@"+dgOld {
		t.Fatalf("%+v", p)
	}
	st, _ := h.svc.State("web")
	if st.Baseline != nil && st.Baseline.Digest == dgNew {
		t.Fatal("baseline moved to a rolled-back image")
	}
	if got := h.kinds(); got[len(got)-1] != "imageupdate.rolled_back" {
		t.Fatalf("%v", got)
	}
	// The same target digest is not proposed again by the scheduled check.
	res, err := h.svc.Check(ctx, "web", CheckOptions{})
	h.must(err)
	if res.Plan != nil && res.Plan.State.open() {
		t.Fatalf("a rolled-back target was proposed again: %+v", res.Plan)
	}
	// An operator may force it.
	res, err = h.svc.Check(ctx, "web", CheckOptions{Force: true})
	h.must(err)
	if res.Plan == nil || !res.Plan.State.open() {
		t.Fatalf("forced check: %+v", res)
	}
}

func TestPlainFailureAndTimeout(t *testing.T) {
	h := newHarness(t)
	p := proposeDigest(h, ModeNotify)
	p, _ = h.svc.Approve(ctx, "alice", p.ID, p.Digest)
	h.drv.statuses = []OpStatus{{State: OpFailed, Detail: "step await-service failed; Bearer abc.def.ghi"}}
	p, err := h.svc.Apply(ctx, "alice", p.ID)
	h.must(err)
	if p.State != PlanFailed || strings.Contains(p.Error, "abc.def.ghi") {
		t.Fatalf("%+v", p)
	}

	h2 := newHarness(t)
	p = proposeDigest(h2, ModeNotify)
	p, _ = h2.svc.Approve(ctx, "alice", p.ID, p.Digest)
	h2.drv.statuses = []OpStatus{{State: OpRunning}}
	p, err = h2.svc.Apply(ctx, "alice", p.ID)
	h2.must(err)
	if p.State != PlanFailed || !strings.Contains(p.Error, "outcome unknown") {
		t.Fatalf("%+v", p)
	}
	if h2.drv.polls > 15*60/5+2 {
		t.Fatalf("polled %d times", h2.drv.polls)
	}
}

func TestBusyServiceKeepsPlanApproved(t *testing.T) {
	h := newHarness(t)
	p := proposeDigest(h, ModeNotify)
	p, _ = h.svc.Approve(ctx, "alice", p.ID, p.Digest)
	h.drv.startErr = ErrBusy
	if _, err := h.svc.Apply(ctx, "alice", p.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
	got, _ := h.svc.Plan(p.ID)
	if got.State != PlanApproved {
		t.Fatalf("%+v", got)
	}
	h.drv.startErr = errors.New("boom")
	if _, err := h.svc.Apply(ctx, "alice", p.ID); err == nil {
		t.Fatal("start failure not reported")
	}
	got, _ = h.svc.Plan(p.ID)
	if got.State != PlanFailed {
		t.Fatalf("%+v", got)
	}
}

func TestApplyChecksWindowStaleServiceAndLeadership(t *testing.T) {
	h := newHarness(t)
	p := proposeDigest(h, ModeNotify)
	p, _ = h.svc.Approve(ctx, "alice", p.ID, p.Digest)

	h.now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) // outside 02:00-04:00
	if _, err := h.svc.Apply(ctx, "alice", p.ID); !errors.Is(err, ErrOutsideWindow) {
		t.Fatalf("%v", err)
	}
	h.now = time.Date(2026, 10, 28, 2, 30, 0, 0, time.UTC)
	h.leader = false
	if _, err := h.svc.Apply(ctx, "alice", p.ID); err == nil {
		t.Fatal("applied without leadership")
	}
	h.leader = true
	if len(h.drv.updates) != 0 {
		t.Fatal("update started although a check failed")
	}
	// The service moved on by itself: the plan is stale and superseded.
	h.src.setImage("web", "nginx:1.25.9@"+dgMin)
	if _, err := h.svc.Apply(ctx, "alice", p.ID); !errors.Is(err, ErrStale) {
		t.Fatalf("%v", err)
	}
	got, _ := h.svc.Plan(p.ID)
	if got.State != PlanSuperseded {
		t.Fatalf("%+v", got)
	}
}

func TestAutoModeAppliesInsideWindowOnly(t *testing.T) {
	h := newHarness(t)
	h.now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) // outside the window
	p := proposeDigest(h, ModeAuto)
	if p.State != PlanApproved || !p.Auto || p.ApprovedBy != AutoActor {
		t.Fatalf("auto plan %+v", p)
	}
	if n, err := h.svc.RunDue(ctx); err != nil || len(h.drv.updates) != 0 {
		t.Fatalf("applied outside the window: %d %v %v", n, err, h.drv.updates)
	}
	h.now = time.Date(2026, 10, 29, 2, 10, 0, 0, time.UTC)
	if _, err := h.svc.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.drv.updates) != 1 || len(h.drv.approvals) != 1 || h.drv.approvals[0] != AutoActor+":op-1" {
		t.Fatalf("%v %v", h.drv.updates, h.drv.approvals)
	}
	got, _ := h.svc.Plan(p.ID)
	if got.State != PlanSucceeded {
		t.Fatalf("%+v", got)
	}
}

func TestAutoModeLimits(t *testing.T) {
	h := newHarness(t)
	h.src.svcs = append(h.src.svcs,
		ManagedService{ID: "db", Name: "db", Type: "container", Image: "mariadb:11.4.2@" + dgOld, Labels: map[string]string{LabelInstallation: inst}})
	h.res.set("library/mariadb", "11.4.2", dgOld)
	h.res.set("library/mariadb", "11.4.3", dgNew)
	h.res.set("library/mariadb", "11.5.1", dgMin)
	h.res.set("library/mariadb", "12.0.0", "sha256:"+hex64('4'))
	// Database, minor track, auto: the patch release is auto-approved only
	// when it is the best candidate; a minor jump needs a person.
	h.policy(Policy{ServiceID: "db", Mode: ModeAuto, Track: TrackMinor, Windows: nightWindow()})
	res, err := h.svc.Check(ctx, "db", CheckOptions{})
	h.must(err)
	if res.Plan == nil || res.Plan.Kind != KindMinor || res.Plan.To.Tag != "11.5.1" || res.Plan.State != PlanProposed || res.Plan.Auto {
		t.Fatalf("database minor jump must wait for a person: %+v", res.Plan)
	}
	if !res.Plan.Database {
		t.Fatal("database flag missing")
	}
	// Patch track on the database: auto.
	h.policy(Policy{ServiceID: "db", Mode: ModeAuto, Track: TrackPatch, Windows: nightWindow()})
	res, err = h.svc.Check(ctx, "db", CheckOptions{})
	h.must(err)
	if res.Plan == nil || res.Plan.Kind != KindPatch || res.Plan.State != PlanApproved || !res.Plan.Auto {
		t.Fatalf("%+v", res.Plan)
	}
}

func TestGaleraGoesToPlannerAndNeverAuto(t *testing.T) {
	h := newHarness(t)
	h.src.svcs = append(h.src.svcs,
		ManagedService{ID: "gal", Name: "gal", Type: "galera", Image: "mariadb:11.4.2@" + dgOld, Labels: map[string]string{LabelInstallation: inst}})
	h.res.set("library/mariadb", "11.4.2", dgOld)
	h.res.set("library/mariadb", "11.4.3", dgNew)
	h.policy(Policy{ServiceID: "gal", Mode: ModeNotify, Track: TrackPatch})
	res, err := h.svc.Check(ctx, "gal", CheckOptions{})
	h.must(err)
	if res.Plan == nil || res.Plan.Route != RoutePlanner || !res.Plan.Database {
		t.Fatalf("%+v", res.Plan)
	}
	if _, err := h.svc.Approve(ctx, "alice", res.Plan.ID, res.Plan.Digest); !errors.Is(err, ErrRoutedToPlanner) {
		t.Fatalf("%v", err)
	}
	if len(h.drv.updates) != 0 {
		t.Fatal("galera update started")
	}
	if _, err := h.svc.Apply(ctx, "alice", res.Plan.ID); err == nil {
		t.Fatal("galera plan applied")
	}
	// Even a stored auto policy cannot approve it (a policy written by an
	// older version, bypassing validation).
	pl := res.Plan
	pl.State, pl.Auto = PlanApproved, true
	h.svc.putPlanForTest(*pl)
	if _, err := h.svc.Apply(ctx, "alice", pl.ID); !errors.Is(err, ErrRoutedToPlanner) {
		t.Fatalf("%v", err)
	}
}

func TestForeignOptInOnlyReports(t *testing.T) {
	h := newHarness(t)
	h.src.svcs = append(h.src.svcs,
		ManagedService{ID: "opt", Name: "opt", Type: "container", Image: "redis:7.2.1@" + dgOld, Foreign: true, Labels: map[string]string{LabelImageUpdate: OptInValue}})
	h.res.set("library/redis", "7.2.1", dgNew)
	h.policy(Policy{ServiceID: "opt", Mode: ModeNotify, Track: TrackDigest})
	res, err := h.svc.Check(ctx, "opt", CheckOptions{})
	h.must(err)
	if res.Plan == nil || res.Plan.Route != RouteReportOnly {
		t.Fatalf("%+v", res.Plan)
	}
	if _, err := h.svc.Approve(ctx, "alice", res.Plan.ID, res.Plan.Digest); !errors.Is(err, ErrForeign) {
		t.Fatalf("%v", err)
	}
}

func TestRollbackAfterSuccess(t *testing.T) {
	h := newHarness(t)
	p := proposeDigest(h, ModeNotify)
	p, _ = h.svc.Approve(ctx, "alice", p.ID, p.Digest)
	p, err := h.svc.Apply(ctx, "alice", p.ID)
	h.must(err)
	h.src.setImage("web", p.To.Ref) // what the manager records after the update
	rb, err := h.svc.ProposeRollback(ctx, "bob", p.ID)
	h.must(err)
	if rb.Kind != KindRollback || rb.Source != SourceRollback || rb.Reference != p.ID || rb.State != PlanProposed || rb.To.Ref != p.From.Ref {
		t.Fatalf("%+v", rb)
	}
	if h.drv.rollbacks != 0 {
		t.Fatal("proposing a rollback started it")
	}
	rb, _ = h.svc.Approve(ctx, "bob", rb.ID, rb.Digest)
	h.drv.statuses = []OpStatus{{State: OpSucceeded, Runtime: agentrt.Container}}
	h.src.setImage("web", p.To.Ref)
	rb, err = h.svc.Apply(ctx, "bob", rb.ID)
	h.must(err)
	if rb.State != PlanSucceeded || h.drv.rollbacks != 1 {
		t.Fatalf("%+v rollbacks=%d", rb, h.drv.rollbacks)
	}
	orig, _ := h.svc.Plan(p.ID)
	if orig.Rollback == nil || orig.Rollback.Automatic || orig.Rollback.PlanID != rb.ID {
		t.Fatalf("%+v", orig.Rollback)
	}
	// Only a succeeded plan whose image still runs can be rolled back.
	if _, err := h.svc.ProposeRollback(ctx, "bob", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	h2 := newHarness(t)
	q := proposeDigest(h2, ModeNotify)
	if _, err := h2.svc.ProposeRollback(ctx, "bob", q.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("rollback of a plan that never ran: %v", err)
	}
}

func TestBuiltImageOffer(t *testing.T) {
	h := newHarness(t)
	h.src.svcs = append(h.src.svcs,
		ManagedService{ID: "site", Name: "site", Type: "container", Image: "ghcr.io/acme/web:2.0.1@" + dgOld, Labels: map[string]string{LabelInstallation: inst}})
	job := imagebuild.Job{ID: "ib-1", Status: imagebuild.JobSucceeded, Reference: "ghcr.io/acme/web:2.0.2", ImageDigest: dgNew}
	b, err := BuiltFromJob(job, "finding-9")
	h.must(err)
	plans, err := h.svc.OfferBuilt(ctx, "system", b)
	h.must(err)
	if len(plans) != 1 || plans[0].ServiceID != "site" || plans[0].Kind != KindBuilt || plans[0].Source != SourceImageBuild ||
		plans[0].To.Ref != "ghcr.io/acme/web:2.0.2@"+dgNew || plans[0].State != PlanProposed || plans[0].Auto {
		t.Fatalf("%+v", plans)
	}
	// Offered again: no second plan.
	if again, _ := h.svc.OfferBuilt(ctx, "system", b); len(again) != 1 || again[0].ID != plans[0].ID {
		t.Fatalf("%+v", again)
	}
	// A built image is never auto-approved, even with an auto policy.
	h.policy(Policy{ServiceID: "site", Mode: ModeAuto, Track: TrackPatch, Windows: nightWindow()})
	b2, _ := BuiltFromJob(imagebuild.Job{ID: "ib-2", Status: imagebuild.JobSucceeded, Reference: "ghcr.io/acme/web:2.0.3", ImageDigest: "sha256:" + hex64('5')}, "")
	plans, err = h.svc.OfferBuilt(ctx, "system", b2)
	h.must(err)
	if len(plans) != 1 || plans[0].State != PlanProposed || plans[0].Auto {
		t.Fatalf("%+v", plans)
	}
	if _, err := BuiltFromJob(imagebuild.Job{ID: "ib-3", Status: imagebuild.JobFailed, Reference: "ghcr.io/acme/web:2.0.4"}, ""); err == nil {
		t.Fatal("failed build accepted")
	}
	if _, err := BuiltFromJob(imagebuild.Job{ID: "ib-4", Status: imagebuild.JobSucceeded, Reference: "ghcr.io/acme/web:2.0.4"}, ""); err == nil {
		t.Fatal("build without digest accepted")
	}
	// Other repositories are not offered.
	b3, _ := BuiltFromJob(imagebuild.Job{ID: "ib-5", Status: imagebuild.JobSucceeded, Reference: "ghcr.io/acme/other:1.0.0", ImageDigest: dgNew}, "")
	if plans, _ := h.svc.OfferBuilt(ctx, "system", b3); len(plans) != 0 {
		t.Fatalf("%+v", plans)
	}
}

func TestRegistryErrorsAreRedactedAndNotifiedOnce(t *testing.T) {
	h := newHarness(t)
	h.policy(Policy{ServiceID: "web", Mode: ModeNotify})
	h.res.err = errors.New("401 Authorization: Bearer s3cr3t-token password=hunter2")
	for i := 0; i < 2; i++ {
		if _, err := h.svc.Check(ctx, "web", CheckOptions{}); err == nil {
			t.Fatal("registry failure not reported")
		}
	}
	st, _ := h.svc.State("web")
	all := st.LastError
	for _, e := range h.events {
		all += e.Message
		for _, v := range e.Details {
			all += v
		}
	}
	if strings.Contains(all, "s3cr3t-token") || strings.Contains(all, "hunter2") {
		t.Fatalf("secret leaked: %s", all)
	}
	failed := 0
	for _, k := range h.kinds() {
		if k == "imageupdate.check_failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("check_failed notified %d times", failed)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	h := newHarness(t)
	p := proposeDigest(h, ModeNotify)
	p, _ = h.svc.Approve(ctx, "alice", p.ID, p.Digest)
	p.State = PlanRunning
	h.svc.putPlanForTest(p)
	n, err := h.svc.RecoverInterrupted(ctx)
	h.must(err)
	got, _ := h.svc.Plan(p.ID)
	if n != 1 || got.State != PlanFailed || !strings.Contains(got.Error, "outcome unknown") {
		t.Fatalf("%d %+v", n, got)
	}
	if len(h.drv.updates) != 0 {
		t.Fatal("recovery contacted the driver")
	}
}

func TestDeclineAndUnknownPlan(t *testing.T) {
	h := newHarness(t)
	p := proposeDigest(h, ModeNotify)
	d, err := h.svc.Decline(ctx, "alice", p.ID)
	h.must(err)
	if d.State != PlanDeclined {
		t.Fatal(d.State)
	}
	if _, err := h.svc.Approve(ctx, "alice", p.ID, p.Digest); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := h.svc.Plan("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// A declined target is not proposed again by the scheduled check.
	res, _ := h.svc.Check(ctx, "web", CheckOptions{})
	if res.Plan != nil && res.Plan.State.open() {
		t.Fatalf("%+v", res.Plan)
	}
}
