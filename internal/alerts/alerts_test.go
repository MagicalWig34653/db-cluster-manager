// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

type env struct {
	t   *testing.T
	now time.Time
	st  *metrics.Store
	ev  *Evaluator
}

func newEnv(t *testing.T, rules []Rule, auth metrics.Authorizer) *env {
	t.Helper()
	e := &env{t: t, now: t0}
	o := metrics.DefaultOptions(t.TempDir())
	o.Now = func() time.Time { return e.now }
	o.SyncWAL = false
	st, err := metrics.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st
	if auth == nil {
		auth = metrics.AllowAll
	}
	e.ev, err = New(Config{Rules: rules, Querier: st, Authorizer: auth, Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) put(name string, v float64, kv ...string) {
	e.t.Helper()
	if _, err := e.st.Append([]metrics.Entry{{Labels: metrics.MustLabels(name, kv...), Sample: metrics.Sample{T: e.now.UnixMilli(), V: v}}}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) eval() []notify.Event {
	e.t.Helper()
	ev, err := e.ev.Evaluate(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

func (e *env) advance(d time.Duration) { e.now = e.now.Add(d) }

func kinds(es []notify.Event) []string {
	var k []string
	for _, e := range es {
		k = append(k, e.Kind)
	}
	return k
}

func state(t *testing.T, e *env, rule string) State {
	t.Helper()
	for _, i := range e.ev.Instances() {
		if i.Rule == rule {
			return i.State
		}
	}
	t.Fatalf("no instance for %s", rule)
	return ""
}

var lbl = []string{"swarm", "s", "cluster", "c", "node", "n1"}

func TestThresholdForDurationFiringAndResolve(t *testing.T) {
	r := Rule{ID: "r", Summary: "size low", Selector: collect.MGaleraSizeDeficit, Cmp: Above, Threshold: 0, For: time.Minute, Severity: notify.SeverityError}
	e := newEnv(t, []Rule{r}, nil)
	e.put(collect.MGaleraSizeDeficit, 1, lbl...)
	if ev := e.eval(); len(ev) != 0 || state(t, e, "r") != StatePending {
		t.Fatalf("first: %v %s", kinds(ev), state(t, e, "r"))
	}
	for range 4 {
		e.advance(15 * time.Second)
		e.put(collect.MGaleraSizeDeficit, 1, lbl...)
		if ev := e.eval(); len(ev) != 0 && e.now.Sub(t0) < time.Minute {
			t.Fatalf("fired too early at %v", e.now.Sub(t0))
		}
	}
	if state(t, e, "r") != StateFiring {
		t.Fatalf("state %s", state(t, e, "r"))
	}
	e.advance(15 * time.Second)
	e.put(collect.MGaleraSizeDeficit, 0, lbl...)
	ev := e.eval()
	if len(ev) != 1 || ev[0].Kind != KindResolved || ev[0].Severity != notify.SeverityInfo {
		t.Fatalf("%v", ev)
	}
	if state(t, e, "r") != StateOK {
		t.Fatal("state")
	}
}

func TestFiringEventContent(t *testing.T) {
	r := Rule{ID: "r", Summary: "size low", Selector: collect.MGaleraSizeDeficit, Cmp: Above, Threshold: 0, Severity: notify.SeverityCritical}
	e := newEnv(t, []Rule{r}, nil)
	e.put(collect.MGaleraSizeDeficit, 2, lbl...)
	ev := e.eval()
	if len(ev) != 1 {
		t.Fatalf("%v", ev)
	}
	x := ev[0]
	if x.Kind != KindFiring || x.Severity != notify.SeverityCritical || x.Resource != "swarm/s/cluster/c/node/n1" ||
		x.Details["cluster"] != "c" || x.Details["rule"] != "r" || x.Details["value"] != "2" || !strings.Contains(x.Message, "size low") {
		t.Fatalf("%+v", x)
	}
	// Events carry no secrets by construction: only whitelisted label keys.
	if _, ok := x.Details["__name__"]; ok {
		t.Fatal("unexpected detail")
	}
}

func TestMissingDataIsUnknownNeverOK(t *testing.T) {
	r := Rule{ID: "r", Summary: "x", Selector: collect.MGaleraPrimary, Cmp: Below, Threshold: 1, For: 30 * time.Second, Severity: notify.SeverityCritical}
	e := newEnv(t, []Rule{r}, nil)
	// Never any data: rule-level unknown instance, and no ok.
	if ev := e.eval(); len(ev) != 0 || state(t, e, "r") != StateUnknown {
		t.Fatalf("%v %s", kinds(ev), state(t, e, "r"))
	}
	e.advance(time.Minute)
	ev := e.eval()
	if len(ev) != 1 || ev[0].Kind != KindUnknown || ev[0].Severity != notify.SeverityUnknown {
		t.Fatalf("%+v", ev)
	}
	if again := e.eval(); len(again) != 0 {
		t.Fatal("unknown must be notified once")
	}
	// Data arrives, healthy: ok. Then data stops: unknown, not ok.
	e.put(collect.MGaleraPrimary, 1, lbl...)
	e.eval()
	if state(t, e, "r") != StateOK {
		t.Fatalf("want ok, got %s", state(t, e, "r"))
	}
	for _, i := range e.ev.Instances() {
		if i.Series == noSeries {
			t.Fatal("rule-level instance must be replaced by real series")
		}
	}
	e.advance(10 * time.Minute)
	e.eval()
	e.advance(time.Minute)
	ev = e.eval()
	if state(t, e, "r") != StateUnknown || len(ev) != 1 || ev[0].Kind != KindUnknown {
		t.Fatalf("stale data: %s %v", state(t, e, "r"), kinds(ev))
	}
}

func TestFiringStaysFiringOnMissingDataAndRepeatsAfterCooldown(t *testing.T) {
	r := Rule{ID: "r", Summary: "x", Selector: collect.MHostDiskRatio, Cmp: Above, Threshold: 0.9, Severity: notify.SeverityError, Cooldown: time.Hour}
	e := newEnv(t, []Rule{r}, nil)
	e.put(collect.MHostDiskRatio, 0.95, "swarm", "s", "host", "h")
	if ev := e.eval(); len(ev) != 1 {
		t.Fatal("fire")
	}
	e.advance(10 * time.Minute) // data older than the window
	if ev := e.eval(); len(ev) != 0 || state(t, e, "r") != StateFiring {
		t.Fatalf("must stay firing on missing data: %s", state(t, e, "r"))
	}
	e.advance(time.Hour)
	if ev := e.eval(); len(ev) != 1 || !strings.Contains(ev[0].Message, "still firing") {
		t.Fatalf("repeat: %v", ev)
	}
}

func TestCooldownZeroDoesNotRepeat(t *testing.T) {
	r := Rule{ID: "r", Summary: "x", Selector: "m", Cmp: Above, Threshold: 0, Severity: notify.SeverityWarning}
	e := newEnv(t, []Rule{r}, nil)
	for range 5 {
		e.put("m", 1)
		e.eval()
		e.advance(15 * time.Second)
	}
	e.put("m", 1)
	if ev := e.eval(); len(ev) != 0 {
		t.Fatal("repeated without cooldown")
	}
}

func TestAbsenceRule(t *testing.T) {
	r := Rule{ID: "abs", Summary: "no heartbeat", Selector: collect.MTargetUp, Absent: true, For: 30 * time.Second, Severity: notify.SeverityWarning}
	e := newEnv(t, []Rule{r}, nil)
	e.put(collect.MTargetUp, 1, lbl...)
	if ev := e.eval(); len(ev) != 0 || state(t, e, "abs") != StateOK {
		t.Fatalf("%s", state(t, e, "abs"))
	}
	e.advance(5 * time.Minute) // data gone
	if ev := e.eval(); len(ev) != 0 || state(t, e, "abs") != StatePending {
		t.Fatalf("pending expected, got %s", state(t, e, "abs"))
	}
	e.advance(time.Minute)
	if ev := e.eval(); len(ev) != 1 || ev[0].Kind != KindFiring {
		t.Fatalf("%v", ev)
	}
	e.put(collect.MTargetUp, 1, lbl...)
	if ev := e.eval(); len(ev) != 1 || ev[0].Kind != KindResolved {
		t.Fatalf("%v", ev)
	}
}

type failQuerier struct{}

func (failQuerier) Query(context.Context, metrics.Query) (*metrics.Result, error) {
	return nil, errors.New("store down")
}

func TestQueryFailureIsUnknownNotOK(t *testing.T) {
	r := Rule{ID: "r", Summary: "x", Selector: "m", Cmp: Above, Threshold: 0, Severity: notify.SeverityWarning}
	e := newEnv(t, []Rule{r}, nil)
	e.put("m", 0)
	e.eval()
	if state(t, e, "r") != StateOK {
		t.Fatal("setup")
	}
	e.ev.cfg.Querier = failQuerier{}
	e.advance(time.Second)
	ev, err := e.ev.Evaluate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("err %v", err)
	}
	if state(t, e, "r") != StateUnknown || len(ev) != 1 || ev[0].Kind != KindUnknown {
		t.Fatalf("%s %v", state(t, e, "r"), kinds(ev))
	}
	// An absence rule must not fire merely because the store failed.
	ra := Rule{ID: "a", Summary: "x", Selector: "m", Absent: true, Severity: notify.SeverityWarning}
	e2 := newEnv(t, []Rule{ra}, nil)
	e2.put("m", 1)
	e2.eval()
	e2.ev.cfg.Querier = failQuerier{}
	e2.advance(time.Second)
	ev, _ = e2.ev.Evaluate(context.Background())
	for _, x := range ev {
		if x.Kind == KindFiring {
			t.Fatal("absence must not fire on a query error")
		}
	}
}

func TestEvaluationUsesPermissionFilter(t *testing.T) {
	r := Rule{ID: "r", Summary: "x", Selector: collect.MGaleraPrimary, Cmp: Below, Threshold: 1, Severity: notify.SeverityCritical}
	onlyS1 := func(l metrics.Labels) bool { return l.Get("swarm") == "s1" }
	e := newEnv(t, []Rule{r}, onlyS1)
	e.put(collect.MGaleraPrimary, 0, "swarm", "s1", "cluster", "c", "node", "n")
	e.put(collect.MGaleraPrimary, 0, "swarm", "s2", "cluster", "c", "node", "n")
	ev := e.eval()
	if len(ev) != 1 || ev[0].Details["swarm"] != "s1" {
		t.Fatalf("hidden series must not be evaluated: %+v", ev)
	}
	for _, i := range e.ev.Instances() {
		if strings.Contains(i.Series, "s2") {
			t.Fatal("instance for a series the identity cannot read")
		}
	}
}

func TestPerSeriesInstances(t *testing.T) {
	r := Rule{ID: "r", Summary: "x", Selector: collect.MGaleraPrimary, Cmp: Below, Threshold: 1, Severity: notify.SeverityCritical}
	e := newEnv(t, []Rule{r}, nil)
	e.put(collect.MGaleraPrimary, 0, "swarm", "s", "cluster", "c", "node", "n1")
	e.put(collect.MGaleraPrimary, 1, "swarm", "s", "cluster", "c", "node", "n2")
	ev := e.eval()
	if len(ev) != 1 || ev[0].Details["node"] != "n1" {
		t.Fatalf("%+v", ev)
	}
}

func TestValidationAndDefaults(t *testing.T) {
	for _, r := range DefaultRules() {
		if err := r.Validate(); err != nil {
			t.Errorf("%v", err)
		}
		if _, ok := collect.Lookup(strings.SplitN(r.Selector, "{", 2)[0]); !ok {
			t.Errorf("rule %s selects %s which is not in the catalog", r.ID, r.Selector)
		}
	}
	bad := []Rule{
		{ID: "", Summary: "x"},
		{ID: "a", Summary: "x", Selector: "m{", Cmp: Above, Severity: notify.SeverityInfo},
		{ID: "a", Summary: "x", Selector: "m", Cmp: "~", Severity: notify.SeverityInfo},
		{ID: "a", Summary: "x", Selector: "m", Cmp: Above, Severity: notify.SeverityUnknown},
		{ID: "a", Summary: "x", Selector: "m", Cmp: Above, Severity: notify.SeverityInfo, Func: "quantile"},
		{ID: "a", Summary: "x", Selector: "m", Cmp: Above, Severity: notify.SeverityInfo, For: -1},
	}
	for i, r := range bad {
		if r.Validate() == nil {
			t.Errorf("bad rule %d accepted", i)
		}
	}
	if _, err := New(Config{Rules: []Rule{DefaultRules()[0], DefaultRules()[0]}, Querier: failQuerier{}, Authorizer: metrics.AllowAll}); err == nil {
		t.Fatal("duplicate ids")
	}
	if _, err := New(Config{Querier: failQuerier{}}); err == nil {
		t.Fatal("authorizer required")
	}
	if got := Without(DefaultRules(), "target-down"); len(got) != len(DefaultRules())-1 {
		t.Fatal("Without")
	}
}

func TestDefaultGaleraRules(t *testing.T) {
	e := newEnv(t, DefaultRules(), nil)
	c := []string{"swarm", "s", "cluster", "c", "node", "n1"}
	e.put(collect.MGaleraSizeDeficit, 1, c...)
	e.put(collect.MGaleraPrimary, 0, c...)
	e.put(collect.MGaleraFlowControlPaused, 0.5, c...)
	e.put(collect.MHostDiskRatio, 0.95, "swarm", "s", "host", "h1")
	e.put(collect.MBackupAgeRatio, 1.5, "swarm", "s", "cluster", "c")
	e.put(collect.MCertExpirySeconds, 2*24*3600, "swarm", "s", "cluster", "c")
	got := map[string]bool{}
	fire := func() {
		for _, ev := range e.eval() {
			if ev.Kind == KindFiring {
				got[ev.Details["rule"]] = true
			}
		}
	}
	fire()
	for range 24 { // 6 minutes at 15s, covers the longest For
		e.advance(15 * time.Second)
		for _, kv := range []struct {
			n string
			v float64
			l []string
		}{
			{collect.MGaleraSizeDeficit, 1, c}, {collect.MGaleraPrimary, 0, c}, {collect.MGaleraFlowControlPaused, 0.5, c},
			{collect.MHostDiskRatio, 0.95, []string{"swarm", "s", "host", "h1"}},
			{collect.MBackupAgeRatio, 1.5, []string{"swarm", "s", "cluster", "c"}},
			{collect.MCertExpirySeconds, 2 * 24 * 3600, []string{"swarm", "s", "cluster", "c"}},
		} {
			e.put(kv.n, kv.v, kv.l...)
		}
		fire()
	}
	for _, id := range []string{"galera-cluster-size-low", "galera-not-primary", "galera-flow-control-high", "disk-almost-full",
		"backup-too-old", "certificate-expiry-soon", "certificate-expiry-imminent"} {
		if !got[id] {
			t.Errorf("rule %s did not fire", id)
		}
	}
	if got["proxy-no-writer"] || got["target-down"] {
		t.Error("rules without data must not fire; they are unknown")
	}
}

func TestNewestSampleIsVisibleImmediately(t *testing.T) {
	r := Rule{ID: "r", Summary: "x", Selector: "m", Cmp: Above, Threshold: 0, Severity: notify.SeverityWarning}
	e := newEnv(t, []Rule{r}, nil)
	e.advance(7*time.Second + 300*time.Millisecond) // not on a 15s boundary
	e.put("m", 1)
	if ev := e.eval(); len(ev) != 1 || ev[0].Kind != KindFiring {
		t.Fatalf("a sample between step boundaries must be evaluated at once: %v", kinds(ev))
	}
}
