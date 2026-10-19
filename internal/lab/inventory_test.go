// SPDX-License-Identifier: AGPL-3.0-only

package lab

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"testing"
)

// Samples follow the formats observed with limactl 2.2.0 and colima 0.10.3
// (one JSON object per line); nested fields are shortened.
const limaListNDJSON = `{"name":"thermoctl-e2e-basestation","hostname":"lima-thermoctl-e2e-basestation","status":"Stopped","dir":"/Users/u/.lima/thermoctl-e2e-basestation","vmType":"vz","arch":"aarch64","cpus":2,"memory":3221225472,"disk":8589934592,"network":[{"vzNAT":true,"interface":"lima0"}],"config":{"images":[{"location":"https://example.invalid/a.qcow2","arch":"aarch64"}]}}
{"name":"thermoctl-test-vm","status":"Stopped","dir":"/Users/u/.lima/thermoctl-test-vm","cpus":2,"memory":2147483648}
{"name":"dbcm-lab-m1","status":"Running","cpus":2}
`

const colimaListNDJSON = `{"name":"default","status":"Running","arch":"aarch64","cpus":6,"memory":8589934592,"disk":64424509440,"runtime":"docker"}
`

func TestSnapshotParsesNDJSON(t *testing.T) {
	r := &fakeRunner{outputs: map[string]string{
		"limactl list --json": limaListNDJSON,
		"colima list --json":  colimaListNDJSON,
	}}
	got, err := Snapshot(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"colima:default", "lima:dbcm-lab-m1", "lima:thermoctl-e2e-basestation", "lima:thermoctl-test-vm"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	wantCalls := [][]string{{"limactl", "list", "--json"}, {"colima", "list", "--json"}}
	if !reflect.DeepEqual(r.calls, wantCalls) {
		t.Fatalf("calls %q want %q", r.calls, wantCalls)
	}
}

func TestSnapshotParsesArrayAndEmpty(t *testing.T) {
	r := &fakeRunner{outputs: map[string]string{
		"limactl list --json": "\n",
		"colima list --json":  `[{"name":"default","status":"Stopped"},{"name":"work","status":"Running"}]`,
	}}
	got, err := Snapshot(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"colima:default", "colima:work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSnapshotRejectsMalformed(t *testing.T) {
	for _, out := range []string{
		`{"name":"a"`,
		`{"status":"Running"}`,
		`{"name":""}`,
		`not json`,
		`"string"`,
	} {
		r := &fakeRunner{outputs: map[string]string{"limactl list --json": out}}
		if _, err := Snapshot(context.Background(), r); err == nil {
			t.Errorf("%q: expected error", out)
		}
	}
}

func TestSnapshotRunnerErrors(t *testing.T) {
	limaErr := errors.New("lima broken")
	r := &fakeRunner{failOn: map[string]error{"limactl list --json": limaErr}}
	if _, err := Snapshot(context.Background(), r); !errors.Is(err, limaErr) {
		t.Fatalf("want lima error, got %v", err)
	}
	colimaErr := errors.New("colima broken")
	r = &fakeRunner{failOn: map[string]error{"colima list --json": colimaErr}}
	if _, err := Snapshot(context.Background(), r); !errors.Is(err, colimaErr) {
		t.Fatalf("want colima error, got %v", err)
	}
}

func TestSnapshotToleratesMissingColima(t *testing.T) {
	notFound := fmt.Errorf("lab: colima list --json: %w", &exec.Error{Name: "colima", Err: exec.ErrNotFound})
	r := &fakeRunner{
		outputs: map[string]string{"limactl list --json": `{"name":"thermoctl-test-vm"}`},
		failOn:  map[string]error{"colima list --json": notFound},
	}
	got, err := Snapshot(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lima:thermoctl-test-vm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestUnrelated(t *testing.T) {
	m := &Manifest{Version: 1}
	if err := m.Add(Resource{Kind: "lima-vm", Name: "dbcm-lab-m1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(Resource{Kind: "colima-profile", Name: "dbcm-lab-p1"}); err != nil {
		t.Fatal(err)
	}
	// A hand-inserted foreign entry must not hide an unrelated resource.
	m.Resources = append(m.Resources, Resource{Kind: "lima-vm", Name: "thermoctl-test-vm"}, Resource{Kind: "colima-profile", Name: "default"})
	snap := []string{"colima:default", "colima:dbcm-lab-p1", "lima:dbcm-lab-m1", "lima:dbcm-lab-m2", "lima:thermoctl-test-vm"}
	got := Unrelated(snap, m)
	want := []string{"colima:default", "lima:dbcm-lab-m2", "lima:thermoctl-test-vm"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := Unrelated(snap, nil); !reflect.DeepEqual(got, snap) {
		t.Fatalf("nil manifest: got %q", got)
	}
}

func TestDiff(t *testing.T) {
	before := []string{"colima:default", "lima:dbcm-lab-m1", "lima:thermoctl-test-vm"}
	after := []string{"lima:dbcm-lab-m2", "colima:default", "lima:dbcm-lab-m1"}
	added, removed := Diff(before, after)
	if want := []string{"lima:dbcm-lab-m2"}; !reflect.DeepEqual(added, want) {
		t.Errorf("added %q want %q", added, want)
	}
	if want := []string{"lima:thermoctl-test-vm"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %q want %q", removed, want)
	}
	added, removed = Diff(before, before)
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("identical snapshots: added %q removed %q", added, removed)
	}
}
