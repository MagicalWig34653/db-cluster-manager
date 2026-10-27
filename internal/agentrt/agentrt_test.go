// SPDX-License-Identifier: AGPL-3.0-only

package agentrt

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestInfoValidateRejectsMismatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		info Info
		ok   bool
	}{
		{"container default", ContainerInfo(false), true},
		{"container with restic", ContainerInfo(true), true},
		{"native patch", NativeInfo(false), true},
		{"native patch and restic", NativeInfo(true), true},
		{"container claims patch", Info{Runtime: Container, Capabilities: []Capability{DockerObjects, Patch}}, false},
		{"container claims host-maintenance", Info{Runtime: Container, Capabilities: []Capability{HostMaintenance}}, false},
		{"native claims docker-objects", Info{Runtime: Native, Capabilities: []Capability{Patch, DockerObjects}}, false},
		{"native claims galera-steps", Info{Runtime: Native, Capabilities: []Capability{GaleraSteps}}, false},
		{"unknown runtime", Info{Runtime: "vm", Capabilities: []Capability{Patch}}, false},
		{"unknown capability", Info{Runtime: Native, Capabilities: []Capability{"root-shell"}}, false},
		{"duplicate capability", Info{Runtime: Native, Capabilities: []Capability{Patch, Patch}}, false},
		{"no capabilities", Info{Runtime: Native}, false},
	} {
		err := tc.info.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrMismatch) {
			t.Errorf("%s: %v does not match ErrMismatch", tc.name, err)
		}
	}
}

func TestHeadersRoundTripAndTamper(t *testing.T) {
	h := http.Header{}
	NativeInfo(true).SetHeaders(h)
	got, present, err := InfoFromHeaders(h)
	if err != nil || !present || got.Runtime != Native || !got.Has(Restic) || !got.Has(Patch) {
		t.Fatalf("round trip: %+v %v %v", got, present, err)
	}
	if _, present, err := InfoFromHeaders(http.Header{}); present || err != nil {
		t.Errorf("absent headers: present=%v err=%v", present, err)
	}
	bad := http.Header{}
	bad.Set(HeaderRuntime, "container")
	bad.Set(HeaderCapabilities, "docker-objects,patch")
	if _, present, err := InfoFromHeaders(bad); !present || !errors.Is(err, ErrMismatch) {
		t.Errorf("mismatched headers: present=%v err=%v", present, err)
	}
	half := http.Header{}
	half.Set(HeaderRuntime, "native")
	if _, present, err := InfoFromHeaders(half); !present || err == nil {
		t.Errorf("runtime without capabilities must be an error: %v %v", present, err)
	}
}

func TestRoutingTable(t *testing.T) {
	for kind, want := range map[string]struct {
		cap Capability
		rts []Runtime
	}{
		"create-service":  {DockerObjects, []Runtime{Container}},
		"remove-volume":   {DockerObjects, []Runtime{Container}},
		"exec-sql":        {GaleraSteps, []Runtime{Container}},
		"collect-health":  {GaleraSteps, []Runtime{Container}},
		"restic-backup":   {Restic, []Runtime{Native, Container}},
		"restic-restore":  {Restic, []Runtime{Native, Container}},
		"patch/apply":     {Patch, []Runtime{Native}},
		"patch/report":    {Patch, []Runtime{Native}},
		"patch/deny-list": {Patch, []Runtime{Native}},
	} {
		req, ok := RequirementFor(kind)
		if !ok || req.Capability != want.cap || len(req.Runtimes) != len(want.rts) {
			t.Errorf("%s: %+v ok=%v, want %v %v", kind, req, ok, want.cap, want.rts)
			continue
		}
		for i := range want.rts {
			if req.Runtimes[i] != want.rts[i] {
				t.Errorf("%s: preference order %v, want %v", kind, req.Runtimes, want.rts)
			}
		}
	}
	if _, ok := RequirementFor("format-disk"); ok {
		t.Error("unmapped kinds must have no requirement (default deny)")
	}
	// Every requirement must be satisfiable by the runtimes it names.
	for kind, req := range table {
		for _, rt := range req.Runtimes {
			if !allowed[rt][req.Capability] {
				t.Errorf("%s: runtime %s can never claim %s", kind, rt, req.Capability)
			}
		}
	}
}

func TestSelectNeverFallsBackSilently(t *testing.T) {
	container := ContainerInfo(true)
	native := NativeInfo(true)
	req, _ := RequirementFor("restic-backup")

	if rt, err := Select(req, map[Runtime]Info{Container: container, Native: native}); err != nil || rt != Native {
		t.Errorf("restic with both: %v %v, want native preferred", rt, err)
	}
	if rt, err := Select(req, map[Runtime]Info{Container: container}); err != nil || rt != Container {
		t.Errorf("restic container only: %v %v", rt, err)
	}
	// A native agent that lacks the restic capability is not used; the
	// container agent is the explicit alternative named by the table.
	if rt, err := Select(req, map[Runtime]Info{Container: container, Native: NativeInfo(false)}); err != nil || rt != Container {
		t.Errorf("restic with a native agent without the capability: %v %v", rt, err)
	}
	patchReq, _ := RequirementFor("patch/apply")
	_, err := Select(patchReq, map[Runtime]Info{Container: container})
	if !errors.Is(err, ErrNoRuntime) || !strings.Contains(err.Error(), "native") || !strings.Contains(err.Error(), "patch") {
		t.Errorf("patch with only a container agent must be refused with a reason: %v", err)
	}
	dockerReq, _ := RequirementFor("create-service")
	_, err = Select(dockerReq, map[Runtime]Info{Native: native})
	if !errors.Is(err, ErrNoRuntime) || !strings.Contains(err.Error(), "container") {
		t.Errorf("docker step with only a native agent must be refused: %v", err)
	}
	if _, err := Select(dockerReq, nil); !errors.Is(err, ErrNoRuntime) || !strings.Contains(err.Error(), "no agent") {
		t.Errorf("no report at all: %v", err)
	}
}
