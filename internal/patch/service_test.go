// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// fakeHost is an in-memory host behind a HostAgent.
type fakeHost struct {
	mu       sync.Mutex
	report   HostReport
	cmds     []AgentCommand
	applyErr error
	pending  map[string]string // name -> available version
}

func (f *fakeHost) Do(_ context.Context, id string, c AgentCommand) (AgentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, c)
	switch c.Kind {
	case CmdReport:
		r := f.report
		r.Updates = nil
		for _, u := range f.report.Updates {
			if _, ok := f.pending[u.Name]; ok {
				r.Updates = append(r.Updates, u)
			}
		}
		return AgentResult{Report: &r}, nil
	case CmdSimulate:
		sim := Simulation{Source: "native"}
		for _, p := range c.Packages {
			sim.Changes = append(sim.Changes, Change{Name: p, To: f.pending[p]})
		}
		return AgentResult{Simulation: &sim}, nil
	case CmdApply:
		if f.applyErr != nil {
			return AgentResult{}, f.applyErr
		}
		for _, p := range c.Packages {
			delete(f.pending, p)
		}
		return AgentResult{Output: "done"}, nil
	case CmdReboot:
		return AgentResult{}, nil
	}
	return AgentResult{}, ErrInvalid
}

func (f *fakeHost) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cmds {
		if c.Kind == kind {
			n++
		}
	}
	return n
}

type denyGuard struct{ err error }

func (d denyGuard) Check(context.Context, string, Action, string) error { return d.err }

type fixture struct {
	svc    *Service
	host   *fakeHost
	events []notify.Event
	now    time.Time
	term   uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC), term: 7}
	f.host = &fakeHost{pending: map[string]string{"openssl": "3.0.11-1~deb12u2", "tzdata": "2024a", "curl": "7.88.1-10+deb12u5"}}
	f.host.report = HostReport{HostID: "h1", CollectedAt: f.now, OS: OSInfo{ID: "debian", VersionID: "12", Kernel: "6.1.0-18-arm64"}, Manager: APT, RebootRequired: No, SecurityKnown: true,
		Installed: []Package{{Name: "openssl", Version: "3.0.11-1~deb12u1", Arch: "arm64"}},
		Updates: []Update{{Name: "openssl", Available: "3.0.11-1~deb12u2", Installed: "3.0.11-1~deb12u1", Security: true},
			{Name: "tzdata", Available: "2024a"}, {Name: "curl", Available: "7.88.1-10+deb12u5", Security: true}}}
	f.svc = &Service{KV: &vuln.MemoryKV{}, Journal: opjournal.New(), Agents: f.host, Guard: AllowAllGuard{},
		Term: func() (uint64, error) { return f.term, nil }, Now: func() time.Time { return f.now },
		Notify: func(_ context.Context, e notify.Event) { f.events = append(f.events, e) }}
	return f
}

func (f *fixture) setup(t *testing.T, pol *Policy) {
	t.Helper()
	if _, err := f.svc.UpsertHost(Host{ID: "h1", Policy: pol}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Poll(context.Background(), "h1", false); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) kinds() []string {
	var k []string
	for _, e := range f.events {
		k = append(k, e.Kind)
	}
	return k
}

var approval = &Policy{Mode: ModeApprovalRequired}

func TestReportOnlyByDefaultCannotBeApproved(t *testing.T) {
	f := newFixture(t)
	f.setup(t, nil)
	p, err := f.svc.Propose(context.Background(), "alice", "h1", ProposeInput{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(context.Background(), "alice", p.ID, p.Digest); !errors.Is(err, ErrReportOnly) {
		t.Fatal("report-only host approved:", err)
	}
	if f.host.count(CmdApply) != 0 {
		t.Fatal("apply sent")
	}
}

func TestSecurityEventOnlyForNewSecurityUpdates(t *testing.T) {
	f := newFixture(t)
	f.setup(t, nil)
	if got := f.kinds(); len(got) != 1 || got[0] != "patch.security_updates" {
		t.Fatalf("%v", got)
	}
	if _, err := f.svc.Poll(context.Background(), "h1", false); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 1 {
		t.Fatalf("same state alerted again: %v", f.kinds())
	}
	f.host.report.Updates = append(f.host.report.Updates, Update{Name: "libc6", Available: "2.36-9+deb12u4", Security: true})
	f.host.pending["libc6"] = "x"
	f.now = f.now.Add(time.Hour)
	f.svc.Poll(context.Background(), "h1", false)
	if len(f.events) != 2 || f.events[1].Details["packages"] != "libc6" {
		t.Fatalf("%+v", f.events)
	}
	hist, _ := f.svc.History("h1")
	if len(hist) != 2 || len(hist[0].Diff.NewSecurity) != 1 {
		t.Fatalf("%+v", hist)
	}
}

func TestApplyHappyPath(t *testing.T) {
	f := newFixture(t)
	f.setup(t, approval)
	ctx := context.Background()
	p, err := f.svc.Propose(ctx, "alice", "h1", ProposeInput{SecurityOnly: true})
	if err != nil || len(p.Packages) != 2 || p.Packages[0] != "curl" {
		t.Fatal(p, err)
	}
	if _, err := f.svc.Apply(ctx, p.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("apply without approval:", err)
	}
	if _, err := f.svc.Approve(ctx, "bob", p.ID, "wrong"); !errors.Is(err, ErrDigest) {
		t.Fatal("wrong digest accepted:", err)
	}
	if _, err := f.svc.Approve(ctx, "bob", p.ID, p.Digest); err != nil {
		t.Fatal(err)
	}
	done, err := f.svc.Apply(ctx, p.ID)
	if err != nil || done.State != PlanSucceeded || len(done.Remaining) != 0 {
		t.Fatal(done, err)
	}
	var apply AgentCommand
	for _, c := range f.host.cmds {
		if c.Kind == CmdApply {
			apply = c
		}
	}
	if apply.Term != 7 || apply.StepID == "" || strings.Join(apply.Packages, ",") != "curl,openssl" {
		t.Fatalf("%+v", apply)
	}
	op, err := f.svc.Journal.Get(done.OpID)
	if err != nil || op.Steps[0].State != opjournal.Succeeded || op.Steps[1].State != opjournal.Succeeded {
		t.Fatal(op, err)
	}
	if _, err := f.svc.Apply(ctx, p.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("a finished plan ran twice:", err)
	}
	rep, _ := f.svc.Report("h1")
	if len(rep.Updates) != 1 || rep.Updates[0].Name != "tzdata" {
		t.Fatalf("%+v", rep.Updates)
	}
	if f.host.count(CmdReboot) != 0 {
		t.Fatal("reboot without policy")
	}
}

func TestSecurityOnlyPolicyRefusesOtherPackages(t *testing.T) {
	f := newFixture(t)
	f.setup(t, &Policy{Mode: ModeApprovalRequired, SecurityOnly: true})
	if _, err := f.svc.Propose(context.Background(), "a", "h1", ProposeInput{Packages: []string{"tzdata"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	p, err := f.svc.Propose(context.Background(), "a", "h1", ProposeInput{})
	if err != nil || !p.SecurityOnly || len(p.Packages) != 2 {
		t.Fatal(p, err)
	}
}

func TestMaintenanceWindow(t *testing.T) {
	f := newFixture(t) // 03:00 UTC
	pol := &Policy{Mode: ModeApprovalRequired, Windows: []Window{{Start: "22:00", DurationMinutes: 120}}}
	f.setup(t, pol)
	ctx := context.Background()
	p, _ := f.svc.Propose(ctx, "a", "h1", ProposeInput{})
	f.svc.Approve(ctx, "b", p.ID, p.Digest)
	if _, err := f.svc.Apply(ctx, p.ID); !errors.Is(err, ErrOutsideWindow) {
		t.Fatal(err)
	}
	if f.host.count(CmdApply) != 0 {
		t.Fatal("apply sent outside window")
	}
	f.now = time.Date(2026, 10, 27, 23, 0, 0, 0, time.UTC)
	if got, err := f.svc.Apply(ctx, p.ID); err != nil || got.State != PlanSucceeded {
		t.Fatal(got, err)
	}
}

func TestWindowEvaluation(t *testing.T) {
	overnight := Policy{Windows: []Window{{Days: []string{"sat"}, Start: "23:00", DurationMinutes: 180, Zone: "Europe/Berlin"}}}
	// Sat 2026-10-31 23:30 Berlin (CET after the DST change on 25 Oct) = 22:30 UTC.
	in := time.Date(2026, 10, 31, 22, 30, 0, 0, time.UTC)
	// Sun 01:00 Berlin = Sat 00:00 UTC? Sun 2026-11-01 01:00 CET = 00:00 UTC.
	cont := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	out := time.Date(2026, 11, 1, 2, 30, 0, 0, time.UTC)
	if !overnight.InWindow(in) || !overnight.InWindow(cont) || overnight.InWindow(out) {
		t.Fatal(overnight.InWindow(in), overnight.InWindow(cont), overnight.InWindow(out))
	}
	if next, ok := overnight.NextWindow(out); !ok || next.Weekday() != time.Saturday {
		t.Fatal(next, ok)
	}
	if !(Policy{}).InWindow(out) {
		t.Fatal("no windows must mean unrestricted")
	}
	for _, bad := range []Policy{
		{Windows: []Window{{Start: "25:00", DurationMinutes: 10}}}, {Windows: []Window{{Start: "10:00", DurationMinutes: 0}}},
		{Windows: []Window{{Start: "10:00", DurationMinutes: 10, Zone: "Mars/Base"}}}, {Windows: []Window{{Days: []string{"xyz"}, Start: "10:00", DurationMinutes: 10}}},
		{Mode: "auto"}, {Reboot: RebootAuto}, {ReportEveryMinutes: 1},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestPolicyResolutionMostSpecificWins(t *testing.T) {
	def := Policy{Mode: ModeReportOnly}
	grp := &Policy{Mode: ModeApprovalRequired, SecurityOnly: true}
	host := &Policy{Mode: ModeReportOnly}
	if Resolve(host, grp, def).Mode != ModeReportOnly || Resolve(nil, grp, def).SecurityOnly != true || Resolve(nil, nil, def).Reboot != RebootNever {
		t.Fatal("resolution")
	}
}

func TestGaleraGuardBlocksApplyAndReboot(t *testing.T) {
	f := newFixture(t)
	f.svc.Guard = denyGuard{ErrGaleraBlocked}
	f.setup(t, &Policy{Mode: ModeApprovalRequired, Reboot: RebootWithPlan, Windows: []Window{{Start: "00:00", DurationMinutes: 1440}}})
	ctx := context.Background()
	p, _ := f.svc.Propose(ctx, "a", "h1", ProposeInput{Reboot: true})
	f.svc.Approve(ctx, "b", p.ID, p.Digest)
	if _, err := f.svc.Apply(ctx, p.ID); !errors.Is(err, ErrGaleraBlocked) {
		t.Fatal(err)
	}
	if f.host.count(CmdApply) != 0 || f.host.count(CmdReboot) != 0 {
		t.Fatal("blocked host was touched")
	}
}

type dir map[string]string

func (d dir) Member(_ context.Context, id string) (string, bool, error) {
	c, ok := d[id]
	if c == "ERR" {
		return "", false, errors.New("lookup failed")
	}
	return c, ok, nil
}

type coord struct{ ok bool }

func (c coord) Permits(_ context.Context, coordination, cluster, host string, a Action) (bool, error) {
	return c.ok && coordination == "roll-1" && cluster == "gal", nil
}

func TestGaleraGuardRules(t *testing.T) {
	ctx := context.Background()
	g := GaleraGuard{Directory: dir{"m1": "gal", "bad": "ERR"}, Coordinator: coord{true}}
	if err := g.Check(ctx, "plain", ActionApply, ""); err != nil {
		t.Fatal("non-member blocked:", err)
	}
	if err := g.Check(ctx, "m1", ActionApply, ""); !errors.Is(err, ErrGaleraBlocked) {
		t.Fatal("uncoordinated member allowed")
	}
	if err := g.Check(ctx, "m1", ActionReboot, "other"); !errors.Is(err, ErrGaleraBlocked) {
		t.Fatal("foreign coordination allowed")
	}
	if err := g.Check(ctx, "m1", ActionReboot, "roll-1"); err != nil {
		t.Fatal("coordinated member blocked:", err)
	}
	if err := g.Check(ctx, "bad", ActionApply, "roll-1"); !errors.Is(err, ErrGaleraBlocked) {
		t.Fatal("unknown membership must fail closed")
	}
	if err := (GaleraGuard{}).Check(ctx, "x", ActionApply, ""); !errors.Is(err, ErrGaleraBlocked) {
		t.Fatal("missing directory must fail closed")
	}
	if err := (GaleraGuard{Directory: dir{"m1": "gal"}, Coordinator: coord{false}}).Check(ctx, "m1", ActionApply, "roll-1"); !errors.Is(err, ErrGaleraBlocked) {
		t.Fatal("denied coordination allowed")
	}
}

func TestRebootRules(t *testing.T) {
	win := []Window{{Start: "00:00", DurationMinutes: 1440}}
	run := func(pol Policy, planReboot bool, required Tristate) string {
		f := newFixture(t)
		f.host.report.RebootRequired = required
		f.setup(t, &pol)
		ctx := context.Background()
		p, err := f.svc.Propose(ctx, "a", "h1", ProposeInput{Reboot: planReboot})
		if err != nil {
			return "propose:" + err.Error()
		}
		f.svc.Approve(ctx, "b", p.ID, p.Digest)
		done, err := f.svc.Apply(ctx, p.ID)
		if err != nil {
			return "apply:" + err.Error()
		}
		return fmt.Sprintf("%d|%s", f.host.count(CmdReboot), done.RebootStatus)
	}
	if got := run(Policy{Mode: ModeApprovalRequired}, true, Yes); !strings.HasPrefix(got, "propose:") {
		t.Fatal("never-policy accepted a reboot plan:", got)
	}
	if got := run(Policy{Mode: ModeApprovalRequired}, false, Yes); !strings.HasPrefix(got, "0|") {
		t.Fatal("reboot happened under never:", got)
	}
	if got := run(Policy{Mode: ModeApprovalRequired, Reboot: RebootWithPlan, Windows: win}, false, Yes); !strings.HasPrefix(got, "0|skipped") {
		t.Fatal("with_plan rebooted without plan step:", got)
	}
	if got := run(Policy{Mode: ModeApprovalRequired, Reboot: RebootWithPlan, Windows: win}, true, Yes); got != "1|requested" {
		t.Fatal(got)
	}
	if got := run(Policy{Mode: ModeApprovalRequired, Reboot: RebootWithPlan, Windows: win}, true, No); !strings.HasPrefix(got, "0|skipped") {
		t.Fatal("rebooted a host that does not need it:", got)
	}
	if got := run(Policy{Mode: ModeApprovalRequired, Reboot: RebootAuto, Windows: win}, false, Yes); got != "1|requested" {
		t.Fatal(got)
	}
	if got := run(Policy{Mode: ModeApprovalRequired, Reboot: RebootAuto, Windows: win}, false, Unknown); !strings.HasPrefix(got, "0|") {
		t.Fatal("auto reboot on unknown state:", got)
	}
}

func TestFailedRunNotifiesAndJournals(t *testing.T) {
	f := newFixture(t)
	f.setup(t, approval)
	f.host.applyErr = errors.New("dpkg: error processing package")
	ctx := context.Background()
	p, _ := f.svc.Propose(ctx, "a", "h1", ProposeInput{})
	f.svc.Approve(ctx, "b", p.ID, p.Digest)
	got, err := f.svc.Apply(ctx, p.ID)
	if err == nil || got.State != PlanFailed {
		t.Fatal(got, err)
	}
	op, _ := f.svc.Journal.Get(got.OpID)
	if op.Steps[0].State != opjournal.Failed || op.Steps[1].State != opjournal.Pending {
		t.Fatalf("%+v", op)
	}
	last := f.events[len(f.events)-1]
	if last.Kind != "patch.run_failed" || last.Severity != notify.SeverityError {
		t.Fatalf("%+v", last)
	}
	again, _ := f.svc.Plan(p.ID)
	if again.State != PlanFailed {
		t.Fatal(again.State)
	}
}

func TestNotLeaderCannotApply(t *testing.T) {
	f := newFixture(t)
	f.setup(t, approval)
	ctx := context.Background()
	p, _ := f.svc.Propose(ctx, "a", "h1", ProposeInput{})
	f.svc.Approve(ctx, "b", p.ID, p.Digest)
	f.svc.Term = func() (uint64, error) { return 0, errors.New("not leader") }
	if _, err := f.svc.Apply(ctx, p.ID); err == nil {
		t.Fatal("applied without leadership")
	}
	if f.host.count(CmdApply) != 0 {
		t.Fatal("apply sent")
	}
}

func TestTamperedPlanRefused(t *testing.T) {
	f := newFixture(t)
	f.setup(t, approval)
	ctx := context.Background()
	p, _ := f.svc.Propose(ctx, "a", "h1", ProposeInput{})
	f.svc.Approve(ctx, "b", p.ID, p.Digest)
	stored, ver, _ := f.svc.loadPlan(p.ID)
	stored.Packages = append(stored.Packages, "bash")
	if err := f.svc.put(CollPlans, p.ID, stored, &ver); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Apply(ctx, p.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("tampered plan ran:", err)
	}
}

func TestOverviewAndPackages(t *testing.T) {
	f := newFixture(t)
	f.svc.UpsertGroup(Group{ID: "web", Name: "Web", Policy: approval})
	f.svc.UpsertHost(Host{ID: "h1", Group: "web"})
	f.svc.UpsertHost(Host{ID: "h2"})
	f.svc.Poll(context.Background(), "h1", false)
	ov, err := f.svc.Overview()
	if err != nil || len(ov) != 2 {
		t.Fatal(ov, err)
	}
	if ov[0].SecurityCount != 2 || ov[0].Policy.Mode != ModeApprovalRequired || ov[0].Stale {
		t.Fatalf("%+v", ov[0])
	}
	if !ov[1].Stale || ov[1].RebootRequired != Unknown {
		t.Fatalf("a host without report must be stale/unknown: %+v", ov[1])
	}
	pk, _ := f.svc.Packages()
	if len(pk) != 3 || !pk[0].Security {
		t.Fatalf("%+v", pk)
	}
	if err := f.svc.DeleteGroup("web"); !errors.Is(err, ErrConflict) {
		t.Fatal("deleted a group with hosts:", err)
	}
	f.now = f.now.Add(24 * time.Hour)
	ov, _ = f.svc.Overview()
	if !ov[0].Stale {
		t.Fatal("old report must be stale")
	}
	if _, err := f.svc.UpsertHost(Host{ID: "h3", Group: "nope"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestPollDueAndFailureNotifiedOnce(t *testing.T) {
	f := newFixture(t)
	f.svc.UpsertHost(Host{ID: "h1"})
	f.svc.Agents = failingAgent{}
	ctx := context.Background()
	done, _ := f.svc.PollDue(ctx)
	if len(done) != 1 || len(f.events) != 1 || f.events[0].Kind != "patch.poll_failed" {
		t.Fatal(done, f.events)
	}
	f.now = f.now.Add(7 * time.Hour)
	f.svc.PollDue(ctx)
	if len(f.events) != 1 {
		t.Fatal("repeated failure notified again")
	}
	f.svc.Agents = f.host
	f.now = f.now.Add(7 * time.Hour)
	if done, _ := f.svc.PollDue(ctx); len(done) != 1 {
		t.Fatal(done)
	}
	if done, _ := f.svc.PollDue(ctx); len(done) != 0 {
		t.Fatal("polled again within the interval", done)
	}
}

type failingAgent struct{}

func (failingAgent) Do(context.Context, string, AgentCommand) (AgentResult, error) {
	return AgentResult{}, errors.New("connection refused")
}

func TestIngestRejectsBadReports(t *testing.T) {
	f := newFixture(t)
	r := f.host.report
	r.Manager = "emerge"
	if _, err := f.svc.Ingest(context.Background(), "h1", r); err == nil {
		t.Fatal("unknown manager accepted")
	}
	r = f.host.report
	r.RebootRequired = "maybe"
	if _, err := f.svc.Ingest(context.Background(), "h1", r); err == nil {
		t.Fatal("bad tristate accepted")
	}
	r = f.host.report
	r.HostID = "spoofed"
	f.svc.Ingest(context.Background(), "h1", r)
	if got, err := f.svc.Report("h1"); err != nil || got.HostID != "h1" {
		t.Fatal("payload host ID was trusted:", got.HostID, err)
	}
}

func TestVulnBridge(t *testing.T) {
	f := newFixture(t)
	f.setup(t, nil)
	f.svc.UpsertHost(Host{ID: "h2"})
	r := HostReport{HostID: "h2", OS: OSInfo{ID: "arch"}, Manager: Pacman, RebootRequired: Unknown, Installed: []Package{{Name: "bash", Version: "5.2"}}}
	f.svc.Ingest(context.Background(), "h2", r)
	es, err := VulnInventory{f.svc}.Inventory(context.Background())
	if err != nil || len(es) != 2 {
		t.Fatal(es, err)
	}
	if es[0].Coverage != vuln.CoverageSBOM || es[0].Components[0].Ecosystem != "Debian:12" || es[0].Validate() != nil {
		t.Fatalf("%+v", es[0])
	}
	if es[1].Coverage != vuln.CoverageUnknown || es[1].Validate() != nil {
		t.Fatalf("a distribution without a feed must be unknown, not safe: %+v", es[1])
	}
	for in, want := range map[string]string{"ubuntu/22.04": "Ubuntu:22.04", "alpine/3.20.3": "Alpine:v3.20", "rocky/9.4": "Rocky Linux:9", "debian/12": "Debian:12", "fedora/40": ""} {
		id, ver, _ := strings.Cut(in, "/")
		if got := OSEcosystem(OSInfo{ID: id, VersionID: ver}); got != want {
			t.Fatal(in, got)
		}
	}
	// A real match through the deterministic matcher.
	adv := vuln.Advisory{ID: "CVE-2026-0001", Affect: []vuln.Affected{{Ecosystem: "debian", Package: "openssl", Ranges: []vuln.Range{{Introduced: "0", Fixed: "3.0.11-1~deb12u2"}}}}}
	if fs := vuln.Match([]vuln.Advisory{adv}, es[:1], f.now); len(fs) != 1 {
		t.Fatalf("OS package did not match: %+v", fs)
	}
}

// --- agent side ---

func TestAgentReadOnlyAndFencing(t *testing.T) {
	r := aptRunner()
	r.Responses["apt-get -s -qq install --only-upgrade openssl"] = ok("Inst openssl [1] (2 x)\n")
	r.Responses["env DEBIAN_FRONTEND=noninteractive apt-get -y -qq -o Dpkg::Options::=--force-confold install --only-upgrade openssl"] = ok("")
	r.Responses["systemctl reboot"] = ok("")
	fs := FakeFS{"/usr/bin/apt-get": "", "/etc/os-release": "ID=debian\nVERSION_ID=12\n"}
	a := &AgentService{Collector: Collector{HostID: "h1", Runner: r, FS: fs}}
	c := context.Background()
	if res, err := a.Handle(c, AgentCommand{Kind: CmdReport}); err != nil || res.Report == nil {
		t.Fatal(err)
	}
	if res, err := a.Handle(c, AgentCommand{Kind: CmdSimulate, Packages: []string{"openssl"}}); err != nil || len(res.Simulation.Changes) != 1 {
		t.Fatal(res, err)
	}
	for _, k := range []string{CmdApply, CmdReboot} {
		if _, err := a.Handle(c, AgentCommand{Kind: k, Term: 1, StepID: "s", Packages: []string{"openssl"}}); !errors.Is(err, ErrReadOnly) {
			t.Fatal(k, "ran on a report-only agent:", err)
		}
	}
	if r.Called("install --only-upgrade openssl") && r.Called("DEBIAN_FRONTEND") || r.Called("reboot") {
		t.Fatal("mutating command executed")
	}
	g, _ := fencing.NewGuard(memPersist{})
	a.AllowApply, a.Guard = true, g
	if _, err := a.Handle(c, AgentCommand{Kind: CmdApply, Term: 5, StepID: "s1", Packages: []string{"openssl"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(c, AgentCommand{Kind: CmdApply, Term: 4, StepID: "s2", Packages: []string{"openssl"}}); !errors.Is(err, ErrStale) {
		t.Fatal("stale term accepted:", err)
	}
	n := len(r.Calls)
	if _, err := a.Handle(c, AgentCommand{Kind: CmdApply, Term: 5, StepID: "s1", Packages: []string{"openssl"}}); err != nil || len(r.Calls) != n {
		t.Fatal("replayed step re-ran", err)
	}
	if _, err := a.Handle(c, AgentCommand{Kind: CmdApply, Term: 5, StepID: "s3"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("apply without packages:", err)
	}
	if _, err := a.Handle(c, AgentCommand{Kind: CmdReboot, Term: 5, StepID: "r1"}); err != nil {
		t.Fatal(err)
	}
}

type memPersist struct{}

func (memPersist) Load() (uint64, error) { return 0, nil }
func (memPersist) Save(uint64) error     { return nil }

func TestAgentHTTPRoundTrip(t *testing.T) {
	f := newFixture(t)
	a := &AgentService{Collector: Collector{HostID: "h1", Runner: aptRunner(), FS: FakeFS{"/usr/bin/apt-get": "", "/etc/os-release": "ID=debian\n"}}}
	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	h := HTTPHostAgent{Client: srv.Client(), URL: func(string) (string, bool) { return srv.URL, true }}
	res, err := h.Do(context.Background(), "h1", AgentCommand{Kind: CmdReport})
	if err != nil || res.Report == nil || len(res.Report.Updates) != 3 {
		t.Fatal(res, err)
	}
	if _, err := h.Do(context.Background(), "h1", AgentCommand{Kind: CmdApply, Term: 1, StepID: "x", Packages: []string{"a"}}); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	resp, _ := http.Get(srv.URL + CommandPath)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatal(resp.StatusCode)
	}
	_ = f
}

// --- install ---

type fakeSSH struct {
	out     string
	calls   []string
	stdins  []string
	failRun bool
}

func (f *fakeSSH) Run(_ context.Context, _ provision.Host, _ provision.Credential, cmd, stdin string) (string, error) {
	f.calls = append(f.calls, cmd)
	f.stdins = append(f.stdins, stdin)
	if strings.Contains(cmd, "/etc/os-release") {
		return f.out, nil
	}
	if f.failRun {
		return "", errors.New("exit 1")
	}
	return "", nil
}

const goodPre = "os=debian\narch=aarch64\nsystemd=yes\npriv=sudo\nmanager=apt-get\nagent=absent\ndisk=5000000000\n"

func installReq(mode InstallMode) InstallRequest {
	return InstallRequest{ID: "inst1", Host: provision.Host{ID: "h1", Address: "h1.example.test", User: "admin", Role: "worker"}, Mode: mode,
		BinaryURL: "https://releases.example.test/agent-linux-arm64", BinarySHA256: strings.Repeat("ab", 32), Version: "0.9.0",
		ManagerURL: "https://manager.example.test:7444", CAFingerprint: strings.Repeat("AB", 32), Installation: "inst-1"}
}

func TestInstallPlanAndExecution(t *testing.T) {
	ssh := &fakeSSH{out: goodPre}
	cred := provision.Credential{PrivateKey: "synthetic-key", KnownHosts: "synthetic"}
	req := installReq(InstallReport)
	chk, err := InstallPreflight(context.Background(), ssh, req.Host, cred)
	if err != nil || chk.Manager != APT || chk.Privilege != "sudo" {
		t.Fatal(chk, err)
	}
	plan, err := BuildInstallPlan(req, chk)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sha256sum", "binary checksum mismatch", "IFS= read -r TOKEN", "useradd --system", "--proto '=https'", AgentUnitName} {
		if !strings.Contains(plan.Script, want) {
			t.Fatalf("script lacks %q", want)
		}
	}
	for _, want := range []string{"User=dbcm-patch", "NoNewPrivileges=true", "ProtectSystem=strict", "CapabilityBoundingSet=", "MemoryDenyWriteExecute=true"} {
		if !strings.Contains(plan.Unit, want) {
			t.Fatalf("report unit lacks %q", want)
		}
	}
	if strings.Contains(plan.Unit, "-allow-apply") || strings.Contains(plan.Unit, "User=root") {
		t.Fatal("report unit must not enable apply or run as root")
	}
	if !strings.Contains(UnitFile(installReq(InstallApply)), "-allow-apply") {
		t.Fatal("apply unit lacks -allow-apply")
	}
	if err := ApplyInstall(context.Background(), ssh, plan, "wrongdigest", "tok", cred); !errors.Is(err, ErrInstallDigest) {
		t.Fatal(err)
	}
	before := len(ssh.calls)
	if before != 1 {
		t.Fatal("digest failure must run nothing:", before)
	}
	if err := ApplyInstall(context.Background(), ssh, plan, plan.Digest, "SECRET-TOKEN-1", cred); err != nil {
		t.Fatal(err)
	}
	last := len(ssh.calls) - 1
	if !strings.HasPrefix(ssh.calls[last], "sudo -n sh -c ") || ssh.stdins[last] != "SECRET-TOKEN-1\n" {
		t.Fatal("token must go to stdin of a sudo script")
	}
	if strings.Contains(plan.Script, "SECRET-TOKEN-1") || strings.Contains(ssh.calls[last], "SECRET-TOKEN-1") {
		t.Fatal("token in script or command line")
	}
	if strings.Contains(plan.Script+plan.Unit, "synthetic-key") {
		t.Fatal("credential in plan")
	}
	ssh.failRun = true
	if err := ApplyInstall(context.Background(), ssh, plan, plan.Digest, "t", cred); err == nil || strings.Contains(err.Error(), "exit 1") {
		t.Fatal("install failure must be reported without output:", err)
	}
}

func TestUninstallScriptIsScoped(t *testing.T) {
	s := UninstallScript()
	for _, want := range []string{"systemctl disable --now " + AgentUnitName, "rm -f " + AgentUnitPath + " " + AgentBinaryPath, "rm -rf " + AgentConfigDir + " " + AgentStateDir, "userdel dbcm-patch"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q", want)
		}
	}
	for _, bad := range []string{"rm -rf /\n", "rm -rf /usr", "rm -rf /var/lib\n", "rm -rf /etc\n", "apt", "dnf"} {
		if strings.Contains(s, bad) {
			t.Fatalf("unscoped command %q", bad)
		}
	}
}

func TestInstallRefusals(t *testing.T) {
	cred := provision.Credential{}
	h := installReq(InstallReport).Host
	for name, out := range map[string]string{
		"no systemd":    strings.Replace(goodPre, "systemd=yes", "systemd=no", 1),
		"no privilege":  strings.Replace(goodPre, "priv=sudo", "priv=none", 1),
		"odd arch":      strings.Replace(goodPre, "aarch64", "mips", 1),
		"no manager":    strings.Replace(goodPre, "manager=apt-get", "manager=none", 1),
		"low disk":      strings.Replace(goodPre, "5000000000", "1000", 1),
		"garbled":       "os=debian\nnonsense\n",
		"duplicate key": goodPre + "os=x\n",
	} {
		if _, err := InstallPreflight(context.Background(), &fakeSSH{out: out}, h, cred); !errors.Is(err, ErrInstallPreflight) {
			t.Fatal(name, err)
		}
	}
	if _, err := InstallPreflight(context.Background(), &fakeSSH{out: strings.Replace(goodPre, "agent=absent", "agent=present", 1)}, h, cred); !errors.Is(err, ErrInstallExists) {
		t.Fatal(err)
	}
	bad := map[string]func(*InstallRequest){
		"http url":      func(r *InstallRequest) { r.BinaryURL = "http://x.example.test/a" },
		"quote in url":  func(r *InstallRequest) { r.BinaryURL = "https://x.example.test/a'b" },
		"userinfo":      func(r *InstallRequest) { r.ManagerURL = "https://u:p@x.example.test" },
		"short sha":     func(r *InstallRequest) { r.BinarySHA256 = "abc" },
		"upper sha":     func(r *InstallRequest) { r.BinarySHA256 = strings.Repeat("AB", 32) },
		"bad mode":      func(r *InstallRequest) { r.Mode = "root" },
		"bad version":   func(r *InstallRequest) { r.Version = "1;rm" },
		"bad listen":    func(r *InstallRequest) { r.Listen = "0.0.0.0:7445;x" },
		"bad install":   func(r *InstallRequest) { r.Installation = "a b" },
		"bad host id":   func(r *InstallRequest) { r.Host.ID = "a/b" },
		"bad finger":    func(r *InstallRequest) { r.CAFingerprint = "zz" },
		"query in url":  func(r *InstallRequest) { r.BinaryURL = "https://x.example.test/a?b=c" },
		"newline in id": func(r *InstallRequest) { r.ID = "a\nb" },
	}
	for name, mut := range bad {
		r := installReq(InstallReport)
		mut(&r)
		if _, err := BuildInstallPlan(r, InstallCheck{Systemd: true, Privilege: "root"}); err == nil {
			t.Fatal("accepted:", name)
		}
	}
}
