// SPDX-License-Identifier: AGPL-3.0-only

package galeraprobe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe/fakemysql"
	"github.com/MagicalWig34653/db-cluster-manager/internal/proxy"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

type topo struct {
	slots map[string]string
	all   []string
}

func (tp topo) SlotAddress(_ context.Context, _, slot string) (string, error) {
	a, ok := tp.slots[slot]
	if !ok {
		return "", errors.New("unknown slot")
	}
	return a, nil
}
func (tp topo) ClusterAddresses(context.Context, string) ([]string, error) { return tp.all, nil }

type fixedChecker struct{ ok bool }

func (f fixedChecker) Check(context.Context, string, runner.Step, runner.Condition) (bool, string, error) {
	return f.ok, "fallback", nil
}

func checkerFor(t *testing.T, nodes ...*fakemysql.Server) (*Checker, topo) {
	t.Helper()
	tp := topo{slots: map[string]string{}}
	for i, n := range nodes {
		tp.slots[string(rune('a'+i))] = n.Addr()
		tp.all = append(tp.all, n.Addr())
	}
	return &Checker{Prober: newProber(t, nil), Topology: tp}, tp
}

func node(t *testing.T, vars map[string]string) *fakemysql.Server {
	return fakemysql.New(t, "dbcm_monitor", testPassword, vars)
}

func TestNodeSynced(t *testing.T) {
	base := fakemysql.SyncedVars()
	tests := []struct {
		name string
		vars map[string]string
		size int
		ok   bool
		why  string
	}{
		{"synced size matches", base, 3, true, ""},
		{"size mismatch", base, 4, false, "wsrep_cluster_size"},
		{"donor is not a verified donor", fakemysql.WithVars(base, "wsrep_local_state_comment", "Donor/Desynced"), 3, false, "degraded"},
		{"joined", fakemysql.WithVars(base, "wsrep_local_state_comment", "Joined"), 3, false, "degraded"},
		{"non-primary", fakemysql.WithVars(base, "wsrep_cluster_status", "non-Primary"), 3, false, "unhealthy"},
		{"flow control throttled", fakemysql.WithVars(base, "wsrep_flow_control_paused", "0.9"), 3, false, "degraded"},
		{"unknown (missing vars)", fakemysql.WithVars(base, "wsrep_ready", ""), 3, false, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := checkerFor(t, node(t, tc.vars))
			ok, why, err := k.Check(context.Background(), "op", runner.Step{}, runner.Condition{Kind: runner.CondNodeSynced, Target: "a", ClusterSize: tc.size})
			if err != nil || ok != tc.ok || !strings.Contains(why, tc.why) {
				t.Fatalf("ok=%v why=%q err=%v", ok, why, err)
			}
		})
	}
}

func TestNodeSyncedUnreachableAndBadInput(t *testing.T) {
	n := node(t, fakemysql.SyncedVars())
	k, _ := checkerFor(t, n)
	n.StopListening()
	if ok, why, err := k.Check(context.Background(), "op", runner.Step{}, runner.Condition{Kind: runner.CondNodeSynced, Target: "a", ClusterSize: 3}); ok || err != nil || !strings.Contains(why, "unreachable") {
		t.Fatalf("ok=%v why=%q err=%v", ok, why, err)
	}
	if ok, _, err := k.Check(context.Background(), "op", runner.Step{}, runner.Condition{Kind: runner.CondNodeSynced, Target: "zz", ClusterSize: 3}); ok || err == nil {
		t.Fatalf("unknown slot: ok=%v err=%v", ok, err)
	}
	if ok, _, _ := k.Check(context.Background(), "op", runner.Step{}, runner.Condition{Kind: runner.CondNodeSynced, Target: "a"}); ok {
		t.Fatal("missing cluster size accepted")
	}
}

func TestQuorum(t *testing.T) {
	good := fakemysql.SyncedVars()
	np := fakemysql.WithVars(good, "wsrep_cluster_status", "non-Primary", "wsrep_ready", "OFF")
	other := fakemysql.WithVars(good, "wsrep_cluster_state_uuid", "uuid-2")
	cond := func(n int) runner.Condition { return runner.Condition{Kind: runner.CondQuorum, ClusterSize: n} }
	run := func(c runner.Condition, vars ...map[string]string) (bool, string) {
		var nodes []*fakemysql.Server
		for _, v := range vars {
			nodes = append(nodes, node(t, v))
		}
		k, _ := checkerFor(t, nodes...)
		ok, why, err := k.Check(context.Background(), "op", runner.Step{}, c)
		if err != nil {
			t.Fatal(err)
		}
		return ok, why
	}
	if ok, why := run(cond(2), good, good, np); !ok {
		t.Fatalf("2 of 3 should hold: %s", why)
	}
	if ok, _ := run(cond(2), good, np, np); ok {
		t.Fatal("1 of 3 accepted as quorum of 2")
	}
	if ok, why := run(cond(2), good, other, good); ok || !strings.Contains(why, "uuid") {
		t.Fatalf("diverging UUIDs accepted: %v %q", ok, why)
	}
	if ok, _ := run(cond(0), good); ok {
		t.Fatal("zero cluster size accepted")
	}
	// An unreachable node does not count.
	dead := node(t, good)
	k, _ := checkerFor(t, node(t, good), dead)
	dead.StopListening()
	if ok, _, _ := k.Check(context.Background(), "op", runner.Step{}, cond(2)); ok {
		t.Fatal("dead node counted toward quorum")
	}
}

func TestOtherConditions(t *testing.T) {
	k, _ := checkerFor(t, node(t, fakemysql.SyncedVars()))
	c := runner.Condition{Kind: runner.CondApproved}
	if ok, _, err := k.Check(context.Background(), "op", runner.Step{}, c); ok || err != nil {
		t.Fatalf("approved without fallback: ok=%v err=%v", ok, err)
	}
	k.Fallback = fixedChecker{ok: true}
	if ok, why, _ := k.Check(context.Background(), "op", runner.Step{}, c); !ok || why != "fallback" {
		t.Fatal("fallback not used")
	}
	if _, _, err := (&Checker{}).Check(context.Background(), "op", runner.Step{}, c); err == nil {
		t.Fatal("misconfigured checker accepted")
	}
}

func TestProxyStatusAdapter(t *testing.T) {
	base := fakemysql.SyncedVars()
	run := func(vars map[string]string) (proxy.GaleraStatus, error) {
		f := node(t, vars)
		return ProxyStatus{Prober: newProber(t, nil)}.GaleraStatus(context.Background(), f.Addr())
	}
	st, err := run(base)
	if err != nil || st.LocalState != 4 || st.ClusterStatus != "Primary" || !st.Ready || st.LocalStateComment != "Synced" {
		t.Fatalf("%+v %v", st, err)
	}
	if s, _ := proxy.SyncFromGalera(st); s.String() != "synced" {
		t.Fatalf("sync = %v", s)
	}
	st, err = run(fakemysql.WithVars(base, "wsrep_local_state", "2", "wsrep_local_state_comment", "Donor/Desynced"))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := proxy.SyncFromGalera(st); s.String() == "synced" {
		t.Fatal("donor routed as synced")
	}
	st, err = run(fakemysql.WithVars(base, "wsrep_connected", "OFF"))
	if err != nil || st.Ready {
		t.Fatalf("disconnected node ready: %+v %v", st, err)
	}
	if _, err = run(fakemysql.WithVars(base, "wsrep_local_state", "")); err == nil {
		t.Fatal("missing wsrep_local_state accepted")
	}
	if _, err = run(fakemysql.WithVars(base, "wsrep_ready", "")); err == nil {
		t.Fatal("incomplete status accepted")
	}
}
