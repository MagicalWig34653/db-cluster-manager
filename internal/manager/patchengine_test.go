// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
)

type fakeRuntime struct{ info EngineRuntimeInfo }

func (f fakeRuntime) Runtime(context.Context, HostRecord) (EngineRuntimeInfo, error) {
	return f.info, nil
}

func setHost(t *testing.T, s *Service, id string, mut func(*HostRecord)) {
	t.Helper()
	h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", id))
	mut(&h.Value)
	if _, err := Put(s.Store(), CollHosts, HostKey("sw1", id), h.Value, IfVersion(h.Version)); err != nil {
		t.Fatal(err)
	}
}

// TestPatchEngineProbeReusesQuorumMathAndFailsClosed: the engine guard of the
// patch module sees the ADR 0019 manager quorum math, refuses without a
// runtime source and refuses a manager whose removal would lose quorum.
func TestPatchEngineProbeReusesQuorumMathAndFailsClosed(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	seed(t, s, 3)
	for _, id := range []string{"h1", "h2", "h3"} {
		setHost(t, s, id, func(h *HostRecord) {
			h.Role, h.EngineVersion, h.State, h.Availability = "manager", "29.8.0", "ready", "drain"
		})
	}
	setHost(t, s, "h1", func(h *HostRecord) { h.Availability = "drain" })
	ctx := context.Background()
	rt := fakeRuntime{EngineRuntimeInfo{DockerInfoOK: true, TasksKnown: true, LiveRestore: patch.Yes}}
	probe := &PatchEngineProbe{Store: s.Store(), Runtime: rt}

	// h2 and h3 are drained too: no majority stays active outside h1.
	st, err := probe.Probe(ctx, "h1")
	if err != nil || st.QuorumSafe != patch.No || patch.EvaluateEngine(st, nil).Allowed {
		t.Fatalf("%+v %v", st, err)
	}
	for _, id := range []string{"h2", "h3"} {
		setHost(t, s, id, func(h *HostRecord) { h.Availability = "active" })
	}
	st, err = probe.Probe(ctx, "h1")
	if err != nil || st.QuorumSafe != patch.Yes || st.Version != "29.8.0" || !patch.EvaluateEngine(st, nil).Allowed {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := (&PatchEngineProbe{Store: s.Store()}).Probe(ctx, "h1"); err == nil {
		t.Fatal("probe without a runtime source answered")
	}
	if _, err := probe.Probe(ctx, "nope"); err == nil {
		t.Fatal("unknown host answered")
	}
	// The same helper backs the engine update preview.
	if !managerQuorumSafe([]HostRecord{{NodeID: "a", Role: "manager", State: "ready", Availability: "active"}, {NodeID: "b", Role: "manager", State: "ready", Availability: "active"}, {NodeID: "c", Role: "manager", State: "ready", Availability: "active"}}, "a") {
		t.Fatal("3 managers, one leaves: quorum is safe")
	}
}
