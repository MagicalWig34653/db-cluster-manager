// SPDX-License-Identifier: AGPL-3.0-only

package lab

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeRunner records every call and returns canned results.
type fakeRunner struct {
	calls   [][]string
	outputs map[string]string // key: strings.Join(argv, " ")
	failOn  map[string]error  // key: strings.Join(argv, " ")
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	f.calls = append(f.calls, argv)
	key := strings.Join(argv, " ")
	if err, ok := f.failOn[key]; ok {
		return "", err
	}
	return f.outputs[key], nil
}

func testBudget() Budget { return Budget{Host: Host{LogicalCPUs: 14, RAMBytes: 48 << 30}} }

func spec(name string) VMSpec {
	return VMSpec{Name: name, CPUs: 2, RAMBytes: 4 << 30, DiskBytes: 20 << 30}
}

func commands(actions []Action) [][]string {
	out := make([][]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Command)
	}
	return out
}

func TestPlanCreateThreeVMs(t *testing.T) {
	m := &Manifest{Version: 1}
	specs := []VMSpec{spec("dbcm-lab-m1"), spec("dbcm-lab-m2"), spec("dbcm-lab-m3")}
	actions, err := PlanCreate(testBudget(), m, specs)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 6 {
		t.Fatalf("want 6 actions, got %d", len(actions))
	}
	var want [][]string
	for _, n := range []string{"dbcm-lab-m1", "dbcm-lab-m2", "dbcm-lab-m3"} {
		want = append(want,
			[]string{"limactl", "create", "--name=" + n, "--cpus=2", "--memory=4", "--disk=20", "--tty=false", "template:docker"},
			[]string{"limactl", "start", "--tty=false", n})
	}
	if got := commands(actions); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch:\n got %q\nwant %q", got, want)
	}
	for i, a := range actions {
		if a.Describe == "" || strings.Contains(a.Describe, "\n") {
			t.Errorf("action %d: bad Describe %q", i, a.Describe)
		}
		if i%2 == 0 {
			if a.Resource == nil || a.Resource.Kind != "lima-vm" || a.Resource.Name != specs[i/2].Name || a.Remove {
				t.Errorf("create action %d: bad resource %+v remove=%v", i, a.Resource, a.Remove)
			}
			if a.Resource.Spec == nil || *a.Resource.Spec != specs[i/2] {
				t.Errorf("create action %d: spec not recorded: %+v", i, a.Resource.Spec)
			}
		} else if a.Resource != nil {
			t.Errorf("start action %d: unexpected resource %+v", i, a.Resource)
		}
	}
	if len(m.Resources) != 0 {
		t.Fatal("planning must not modify the manifest")
	}
}

func TestPlanCreateFractionalGiB(t *testing.T) {
	s := VMSpec{Name: "dbcm-lab-m1", CPUs: 1, RAMBytes: 1536 << 20, DiskBytes: 10 << 30}
	actions, err := PlanCreate(testBudget(), &Manifest{Version: 1}, []VMSpec{s})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"limactl", "create", "--name=dbcm-lab-m1", "--cpus=1", "--memory=1.5", "--disk=10", "--tty=false", "template:docker"}
	if !reflect.DeepEqual(actions[0].Command, want) {
		t.Fatalf("got %q want %q", actions[0].Command, want)
	}
}

func TestPlanCreateRejects(t *testing.T) {
	owned := &Manifest{Version: 1}
	if err := owned.Add(Resource{Kind: "lima-vm", Name: "dbcm-lab-m1", Spec: &VMSpec{Name: "dbcm-lab-m1", CPUs: 2, RAMBytes: 4 << 30, DiskBytes: 20 << 30}}); err != nil {
		t.Fatal(err)
	}
	big := spec("dbcm-lab-m1")
	big.RAMBytes = 17 << 30
	cases := []struct {
		name  string
		m     *Manifest
		specs []VMSpec
	}{
		{"over ram budget", &Manifest{Version: 1}, []VMSpec{big}},
		{"over cpu budget", &Manifest{Version: 1}, []VMSpec{{Name: "dbcm-lab-m1", CPUs: 8, RAMBytes: 1 << 30, DiskBytes: 1 << 30}}},
		{"over budget with existing VM", owned, []VMSpec{{Name: "dbcm-lab-m2", CPUs: 1, RAMBytes: 13 << 30, DiskBytes: 1 << 30}}},
		{"bad prefix", &Manifest{Version: 1}, []VMSpec{spec("default")}},
		{"prefix not at start", &Manifest{Version: 1}, []VMSpec{spec("x-dbcm-lab-m1")}},
		{"bare prefix", &Manifest{Version: 1}, []VMSpec{spec("dbcm-lab-")}},
		{"shell metacharacters", &Manifest{Version: 1}, []VMSpec{spec("dbcm-lab-m1;rm")}},
		{"path separator", &Manifest{Version: 1}, []VMSpec{spec("dbcm-lab-../m1")}},
		{"duplicate in specs", &Manifest{Version: 1}, []VMSpec{spec("dbcm-lab-m1"), spec("dbcm-lab-m1")}},
		{"already in manifest", owned, []VMSpec{spec("dbcm-lab-m1")}},
		{"empty", &Manifest{Version: 1}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &fakeRunner{}
			actions, err := PlanCreate(testBudget(), c.m, c.specs)
			if err == nil {
				t.Fatalf("expected error, got actions %q", commands(actions))
			}
			if actions != nil {
				t.Fatalf("expected no actions on error, got %q", commands(actions))
			}
			if len(r.calls) != 0 {
				t.Fatalf("runner must not be called, got %q", r.calls)
			}
		})
	}
}

// foreignNames are names that the cleanup planner must never act on.
var foreignNames = []string{
	"default",
	"colima",
	"thermoctl-test-vm",
	"thermoctl-e2e-basestation",
	"x-dbcm-lab-m1",
	"my-dbcm-lab-vm",
	"",
	" ",
	"dbcm-lab-",
	"dbcm-lab",
	"DBCM-LAB-m1",
	" dbcm-lab-m1",
	"dbcm-lab-m1 ",
	"dbcm-lab-m1\n",
	"dbcm-lab-a/b",
	"dbcm-lab-../../etc",
	"dbcm-lab-..",
	"dbcm-lab-a\\b",
	"dbcm-lab-m1;rm -rf ~",
	"dbcm-lab-$(whoami)",
	"dbcm-lab-`id`",
	"dbcm-lab-m1|cat",
	"dbcm-lab-m1&",
	"dbcm-lab-*",
	"dbcm-lab-m1 default",
	"--force",
	"-dbcm-lab-m1",
	"dbcm-lab-m1\x00default",
}

func TestPlanCleanupOnlyOwnedNames(t *testing.T) {
	m := &Manifest{Version: 1}
	if err := m.Add(Resource{Kind: "lima-vm", Name: "dbcm-lab-m1"}); err != nil {
		t.Fatal(err)
	}
	// Hand-insert foreign resources, bypassing Add's prefix check.
	for _, n := range foreignNames {
		for _, k := range []string{"lima-vm", "directory", "colima-profile", "docker-context"} {
			m.Resources = append(m.Resources, Resource{Kind: k, Name: n})
		}
	}
	actions := PlanCleanup(m)
	want := [][]string{
		{"limactl", "stop", "dbcm-lab-m1"},
		{"limactl", "delete", "--force", "dbcm-lab-m1"},
	}
	if got := commands(actions); !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup argv mismatch:\n got %q\nwant %q", got, want)
	}
	for _, a := range actions {
		if operand := a.Command[len(a.Command)-1]; !IsLabName(operand) {
			t.Fatalf("non-lab operand %q in %q", operand, a.Command)
		}
	}
	for _, n := range foreignNames {
		if IsLabName(n) {
			t.Fatalf("IsLabName accepts foreign name %q", n)
		}
	}
	if actions[0].Resource != nil {
		t.Errorf("stop action must not change the manifest: %+v", actions[0].Resource)
	}
	if r := actions[1].Resource; r == nil || r.Kind != "lima-vm" || r.Name != "dbcm-lab-m1" || !actions[1].Remove {
		t.Errorf("delete action must remove the resource: %+v remove=%v", r, actions[1].Remove)
	}
}

func TestPlanCleanupOnlyForeign(t *testing.T) {
	m := &Manifest{Version: 1}
	for _, n := range foreignNames {
		m.Resources = append(m.Resources, Resource{Kind: "lima-vm", Name: n}, Resource{Kind: "directory", Name: n})
	}
	if actions := PlanCleanup(m); len(actions) != 0 {
		t.Fatalf("expected no actions, got %q", commands(actions))
	}
	if actions := PlanCleanup(nil); len(actions) != 0 {
		t.Fatalf("expected no actions for nil manifest, got %q", commands(actions))
	}
}

func TestPlanCleanupDirectoriesAndDedup(t *testing.T) {
	m := &Manifest{Version: 1}
	for _, r := range []Resource{
		{Kind: "lima-vm", Name: "dbcm-lab-m1"},
		{Kind: "directory", Name: "dbcm-lab-cache"},
		{Kind: "lima-vm", Name: "dbcm-lab-m2"},
	} {
		if err := m.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	m.Resources = append(m.Resources, Resource{Kind: "lima-vm", Name: "dbcm-lab-m1"}) // hand-inserted duplicate
	want := [][]string{
		{"limactl", "stop", "dbcm-lab-m1"},
		{"limactl", "delete", "--force", "dbcm-lab-m1"},
		{"limactl", "stop", "dbcm-lab-m2"},
		{"limactl", "delete", "--force", "dbcm-lab-m2"},
		{"rm", "-rf", "--", LabDir + "/dbcm-lab-cache"},
	}
	actions := PlanCleanup(m)
	if got := commands(actions); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	last := actions[len(actions)-1]
	if last.Resource == nil || last.Resource.Kind != "directory" || !last.Remove {
		t.Fatalf("directory action must remove the resource: %+v", last)
	}
}

func TestExecuteCreateStopsAtFirstError(t *testing.T) {
	m := &Manifest{Version: 1}
	actions, err := PlanCreate(testBudget(), m, []VMSpec{spec("dbcm-lab-m1"), spec("dbcm-lab-m2"), spec("dbcm-lab-m3")})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	r := &fakeRunner{failOn: map[string]error{"limactl start --tty=false dbcm-lab-m2": boom}}
	saves := 0
	err = Execute(context.Background(), r, m, actions, func() error { saves++; return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if len(r.calls) != 4 {
		t.Fatalf("want 4 calls (stop after failing start of m2), got %q", r.calls)
	}
	// m1 created+started, m2 created (start failed): both exist and are recorded.
	for _, n := range []string{"dbcm-lab-m1", "dbcm-lab-m2"} {
		res, ok := m.Find("lima-vm", n)
		if !ok {
			t.Fatalf("%s must be recorded", n)
		}
		if res.Created.IsZero() {
			t.Errorf("%s: Created not set", n)
		}
	}
	if _, ok := m.Find("lima-vm", "dbcm-lab-m3"); ok {
		t.Fatal("dbcm-lab-m3 must not be recorded")
	}
	if saves != 3 {
		t.Fatalf("want 3 saves (one per successful action), got %d", saves)
	}
	if actions[0].Resource.Created.IsZero() == false {
		t.Fatal("Execute must not mutate the planned action's resource")
	}
}

func TestExecuteFailedCreateNotRecorded(t *testing.T) {
	m := &Manifest{Version: 1}
	actions, err := PlanCreate(testBudget(), m, []VMSpec{spec("dbcm-lab-m1")})
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{failOn: map[string]error{strings.Join(actions[0].Command, " "): errors.New("create failed")}}
	if err := Execute(context.Background(), r, m, actions, func() error { return nil }); err == nil {
		t.Fatal("expected error")
	}
	if len(m.Resources) != 0 || len(r.calls) != 1 {
		t.Fatalf("manifest %+v calls %q", m.Resources, r.calls)
	}
}

func TestExecuteCleanupKeepsManifestInSync(t *testing.T) {
	m := &Manifest{Version: 1}
	for _, n := range []string{"dbcm-lab-m1", "dbcm-lab-m2"} {
		if err := m.Add(Resource{Kind: "lima-vm", Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	m.Resources = append(m.Resources, Resource{Kind: "lima-vm", Name: "default"})
	actions := PlanCleanup(m)
	r := &fakeRunner{failOn: map[string]error{"limactl delete --force dbcm-lab-m2": errors.New("busy")}}
	saves := 0
	if err := Execute(context.Background(), r, m, actions, func() error { saves++; return nil }); err == nil {
		t.Fatal("expected error")
	}
	if _, ok := m.Find("lima-vm", "dbcm-lab-m1"); ok {
		t.Fatal("dbcm-lab-m1 was deleted and must be removed from the manifest")
	}
	if _, ok := m.Find("lima-vm", "dbcm-lab-m2"); !ok {
		t.Fatal("dbcm-lab-m2 delete failed; it must stay recorded")
	}
	if _, ok := m.Find("lima-vm", "default"); !ok {
		t.Fatal("foreign entry must not be touched")
	}
	if saves != 3 {
		t.Fatalf("want 3 saves, got %d", saves)
	}
}

func TestExecuteSaveErrorStops(t *testing.T) {
	m := &Manifest{Version: 1}
	actions, err := PlanCreate(testBudget(), m, []VMSpec{spec("dbcm-lab-m1")})
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{}
	saveErr := errors.New("disk full")
	if err := Execute(context.Background(), r, m, actions, func() error { return saveErr }); !errors.Is(err, saveErr) {
		t.Fatalf("want save error, got %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("must stop after failed save, calls %q", r.calls)
	}
}

func TestExecuteRejectsUnsafeActions(t *testing.T) {
	cases := []Action{
		{Describe: "empty"},
		{Describe: "foreign add", Command: []string{"true"}, Resource: &Resource{Kind: "lima-vm", Name: "default"}},
		{Describe: "foreign remove", Command: []string{"true"}, Resource: &Resource{Kind: "lima-vm", Name: "thermoctl-test-vm"}, Remove: true},
	}
	for _, a := range cases {
		m := &Manifest{Version: 1}
		r := &fakeRunner{}
		if err := Execute(context.Background(), r, m, []Action{a}, func() error { return nil }); err == nil {
			t.Errorf("%s: expected error", a.Describe)
		}
		if len(r.calls) != 0 {
			t.Errorf("%s: runner must not be called, got %q", a.Describe, r.calls)
		}
	}
}

func TestExecuteCancelledContext(t *testing.T) {
	m := &Manifest{Version: 1}
	actions, err := PlanCreate(testBudget(), m, []VMSpec{spec("dbcm-lab-m1")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &fakeRunner{}
	if err := Execute(ctx, r, m, actions, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("runner must not be called, got %q", r.calls)
	}
}
