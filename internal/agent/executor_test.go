// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
)

const testInstallation = "inst-1"

type fixture struct {
	t         *testing.T
	dir       string
	base      string
	statePath string
	termPath  string
	docker    *fakeDocker
	exec      *Executor
}

func (f *fixture) volume(name string) string { return filepath.Join(f.base, name) }

// newFixture builds an executor with these objects:
//
//	svc-1        owned service, mounts volume galera-1
//	vol-1        owned volume, mountpoint galera-1
//	svc-foreign  in the allow-list but labelled for another installation
//	svc-nolabel  in the allow-list without labels
//	svc-unlisted correct label but not in the allow-list
//	vol-1-as-svc ID in allow-list, but it is a volume
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		t:         t,
		dir:       dir,
		base:      filepath.Join(dir, "volumes"),
		statePath: filepath.Join(dir, "state", "agent-state.json"),
		termPath:  filepath.Join(dir, "state", "term"),
	}
	for _, d := range []string{f.base, filepath.Dir(f.statePath), f.volume("galera-1")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	own := map[string]string{LabelInstallation: testInstallation, LabelCluster: "cluster-1"}
	f.docker = newFakeDocker(
		Object{Kind: KindService, ID: "svc-1", Name: "galera-1", Labels: own,
			Mounts: []Mount{{Type: "bind", Source: f.volume("galera-1"), Target: "/dbcm"}}},
		Object{Kind: KindVolume, ID: "vol-1", Labels: own, Mountpoint: f.volume("galera-1")},
		Object{Kind: KindService, ID: "svc-foreign", Labels: map[string]string{LabelInstallation: "other"},
			Mounts: []Mount{{Type: "bind", Source: f.volume("galera-1"), Target: "/dbcm"}}},
		Object{Kind: KindService, ID: "svc-nolabel"},
		Object{Kind: KindService, ID: "svc-unlisted", Labels: own},
		Object{Kind: KindVolume, ID: "vol-1-as-svc", Labels: own},
	)
	f.restart()
	return f
}

// restart builds a new executor and guard from the files on disk, as after
// an agent process restart.
func (f *fixture) restart() {
	f.t.Helper()
	g, err := fencing.NewGuard(fencing.NewFilePersister(f.termPath))
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := NewExecutor(Config{
		InstallationID: testInstallation,
		StatePath:      f.statePath,
		VolumeBase:     f.base,
		AllowedIDs:     []string{"svc-1", "vol-1", "svc-foreign", "svc-nolabel", "vol-1-as-svc"},
		Guard:          g,
		Docker:         f.docker,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec = e
}

func mustArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fixture) do(term uint64, op, step string, kind Kind, args any) (Result, error) {
	f.t.Helper()
	return f.exec.Handle(context.Background(), Command{Term: term, OpID: op, StepID: step, Kind: kind, Args: mustArgs(f.t, args)})
}

func (f *fixture) mustDo(term uint64, op, step string, kind Kind, args any) Result {
	f.t.Helper()
	r, err := f.do(term, op, step, kind, args)
	if err != nil {
		f.t.Fatalf("%s %s/%s: %v", kind, op, step, err)
	}
	return r
}

func wantStatus(t *testing.T, r Result, s Status, c Code) {
	t.Helper()
	if r.Status != s || r.Code != c {
		t.Fatalf("result %+v: want status %s code %q", r, s, c)
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return b
}

func spec(name string) ServiceSpec {
	return ServiceSpec{Name: name, Image: "mariadb:11.4", Replicas: 0}
}

func TestStaleTermRejectedWithoutEffect(t *testing.T) {
	f := newFixture(t)
	f.mustDo(5, "op1", "s1", KindStopDB, ServiceArgs{ServiceID: "svc-1"})
	before := f.docker.Calls()
	stateBefore := readFile(t, f.statePath)

	for _, tc := range []struct {
		kind Kind
		args any
	}{
		{KindCreateService, spec("x")},
		{KindRemoveService, ServiceArgs{ServiceID: "svc-1"}},
		{KindStartDB, StartDBArgs{ServiceID: "svc-1", VolumeRoot: f.volume("galera-1"), Expected: testIdentity()}},
		{KindRemoveVolume, RemoveVolumeArgs{VolumeID: "vol-1", VolumeRoot: f.volume("galera-1"), Expected: testIdentity()}},
		{KindInitIdentity, InitIdentityArgs{VolumeRoot: f.volume("galera-1"), Token: NewIdentityToken{TokenID: "t", Identity: testIdentity()}}},
		{KindInspect, InspectArgs{ObjectKind: KindService, ID: "svc-1"}},
		{KindAllowObjects, AllowObjectsArgs{IDs: []string{"svc-unlisted"}}},
	} {
		_, err := f.do(4, "op-old", "s-"+string(tc.kind), tc.kind, tc.args)
		if !errors.Is(err, fencing.ErrStaleTerm) {
			t.Fatalf("%s at stale term: want ErrStaleTerm, got %v", tc.kind, err)
		}
	}
	// Term 0 is never valid.
	if _, err := f.do(0, "op0", "s", KindStopDB, ServiceArgs{ServiceID: "svc-1"}); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("term 0: want ErrStaleTerm, got %v", err)
	}
	// The same step re-sent by a deposed leader is rejected too, not replayed.
	if _, err := f.do(4, "op1", "s1", KindStopDB, ServiceArgs{ServiceID: "svc-1"}); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("stale replay: want ErrStaleTerm, got %v", err)
	}
	if got := f.docker.Calls(); !reflect.DeepEqual(got, before) {
		t.Fatalf("stale commands reached Docker: %v", got[len(before):])
	}
	if !bytes.Equal(readFile(t, f.statePath), stateBefore) {
		t.Fatal("stale commands changed the state file")
	}
	if _, err := os.Stat(filepath.Join(f.volume("galera-1"), MarkerFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale init-identity wrote a marker")
	}
}

func TestNewerTermFencesOlderLeader(t *testing.T) {
	f := newFixture(t)
	f.mustDo(6, "op-a", "s1", KindStopDB, ServiceArgs{ServiceID: "svc-1"})
	f.mustDo(7, "op-b", "s1", KindStopDB, ServiceArgs{ServiceID: "svc-1"})
	if _, err := f.do(6, "op-a", "s2", KindStopDB, ServiceArgs{ServiceID: "svc-1"}); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("want ErrStaleTerm, got %v", err)
	}
	// The highest term survives a restart.
	f.restart()
	if _, err := f.do(6, "op-a", "s3", KindStopDB, ServiceArgs{ServiceID: "svc-1"}); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("after restart: want ErrStaleTerm, got %v", err)
	}
}

func TestInvalidCommandDoesNotAdvanceTerm(t *testing.T) {
	f := newFixture(t)
	f.mustDo(3, "op", "s", KindStopDB, ServiceArgs{ServiceID: "svc-1"})
	bad := []Command{
		{Term: 9, OpID: "", StepID: "s", Kind: KindStopDB, Args: json.RawMessage(`{"service_id":"svc-1"}`)},
		{Term: 9, OpID: "o", StepID: "", Kind: KindStopDB, Args: json.RawMessage(`{"service_id":"svc-1"}`)},
		{Term: 9, OpID: "o", StepID: "s", Kind: "format-disk"},
		{Term: 9, OpID: "o", StepID: "s", Kind: KindStopDB, Args: json.RawMessage(`{"service_id":"svc-1","force":true}`)},
		{Term: 9, OpID: "o", StepID: "s", Kind: KindStopDB, Args: json.RawMessage(`{}`)},
		{Term: 9, OpID: "o", StepID: "s", Kind: KindStopDB, Args: json.RawMessage(`not json`)},
		{Term: 9, OpID: "o", StepID: "s", Kind: KindVerifyIdentity, Args: mustArgs(t, VerifyIdentityArgs{VolumeRoot: "/etc", Expected: testIdentity()})},
		{Term: 9, OpID: "o", StepID: "s", Kind: KindVerifyIdentity, Args: mustArgs(t, VerifyIdentityArgs{VolumeRoot: f.base + "/a/../../x", Expected: testIdentity()})},
		{Term: 9, OpID: "o", StepID: "s", Kind: KindVerifyIdentity, Args: mustArgs(t, VerifyIdentityArgs{VolumeRoot: f.base, Expected: testIdentity()})},
		{Term: 9, OpID: "o", StepID: "s", Kind: KindVerifyIdentity, Args: mustArgs(t, VerifyIdentityArgs{VolumeRoot: "rel/path", Expected: testIdentity()})},
	}
	for i, c := range bad {
		if _, err := f.exec.Handle(context.Background(), c); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("case %d: want ErrInvalidCommand, got %v", i, err)
		}
	}
	if h := f.exec.guard.Highest(); h != 3 {
		t.Fatalf("invalid commands advanced the term to %d", h)
	}
}

func TestStepIdempotentAcrossLostReplyAndRestart(t *testing.T) {
	f := newFixture(t)
	first := f.mustDo(2, "op-create", "create", KindCreateService, spec("galera-2"))
	wantStatus(t, first, StatusSucceeded, "")
	if first.Replayed {
		t.Fatal("first execution marked as replayed")
	}
	// Reply lost: the leader re-sends the same step.
	again := f.mustDo(2, "op-create", "create", KindCreateService, spec("galera-2"))
	if !again.Replayed || !bytes.Equal(again.Output, first.Output) || again.Status != first.Status {
		t.Fatalf("replay %+v differs from first %+v", again, first)
	}
	// A new leader with a higher term re-sends it after an agent restart.
	f.restart()
	third := f.mustDo(3, "op-create", "create", KindCreateService, spec("galera-2"))
	if !third.Replayed || !bytes.Equal(third.Output, first.Output) {
		t.Fatalf("replay after restart %+v differs from first %+v", third, first)
	}
	if n := countPrefix(f.docker.Calls(), "create-service"); n != 1 {
		t.Fatalf("CreateService called %d times, want 1", n)
	}
	// Refused and failed outcomes are replayed as well.
	r1 := f.mustDo(3, "op-x", "s", KindRemoveService, ServiceArgs{ServiceID: "svc-unlisted"})
	r2 := f.mustDo(3, "op-x", "s", KindRemoveService, ServiceArgs{ServiceID: "svc-unlisted"})
	wantStatus(t, r1, StatusRefused, CodeNotOwned)
	if !r2.Replayed || r2.Code != CodeNotOwned {
		t.Fatalf("refusal not replayed: %+v", r2)
	}
}

func TestStepReplayIgnoresArgsKeyOrder(t *testing.T) {
	f := newFixture(t)
	c := Command{Term: 1, OpID: "o", StepID: "s", Kind: KindInspect, Args: json.RawMessage(`{"object_kind":"service","id":"svc-1"}`)}
	if _, err := f.exec.Handle(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	c.Args = json.RawMessage(` { "id" : "svc-1", "object_kind" : "service" } `)
	r, err := f.exec.Handle(context.Background(), c)
	if err != nil || !r.Replayed {
		t.Fatalf("reordered args: %+v %v", r, err)
	}
}

func TestStepConflict(t *testing.T) {
	f := newFixture(t)
	f.mustDo(1, "o", "s", KindStopDB, ServiceArgs{ServiceID: "svc-1"})
	if _, err := f.do(1, "o", "s", KindStopDB, ServiceArgs{ServiceID: "svc-foreign"}); !errors.Is(err, ErrStepConflict) {
		t.Fatalf("other args: want ErrStepConflict, got %v", err)
	}
	if _, err := f.do(1, "o", "s", KindRemoveService, ServiceArgs{ServiceID: "svc-1"}); !errors.Is(err, ErrStepConflict) {
		t.Fatalf("other kind: want ErrStepConflict, got %v", err)
	}
	if n := countPrefix(f.docker.Calls(), "remove-service"); n != 0 {
		t.Fatalf("conflicting step executed")
	}
}

// TestCrashAfterEffectIsInDoubt simulates an agent crash after the Docker
// effect but before the outcome was recorded: the state file captured during
// the effect holds only the intent record.
func TestCrashAfterEffectIsInDoubt(t *testing.T) {
	f := newFixture(t)
	crashCopy := filepath.Join(f.dir, "crash-state.json")
	f.docker.onCreate = func() {
		if err := os.WriteFile(crashCopy, readFile(t, f.statePath), 0o600); err != nil {
			t.Error(err)
		}
	}
	f.mustDo(1, "op", "create", KindCreateService, spec("galera-2"))
	f.docker.onCreate = nil
	if err := os.Rename(crashCopy, f.statePath); err != nil {
		t.Fatal(err)
	}
	f.restart()
	r := f.mustDo(2, "op", "create", KindCreateService, spec("galera-2"))
	wantStatus(t, r, StatusUnknown, CodeInDoubt)
	if !r.Replayed {
		t.Fatal("in-doubt result not marked replayed")
	}
	if n := countPrefix(f.docker.Calls(), "create-service"); n != 1 {
		t.Fatalf("in-doubt step re-executed: %d CreateService calls", n)
	}
}

func TestPersistFailureBeforeEffect(t *testing.T) {
	f := newFixture(t)
	f.exec.cfg.StatePath = filepath.Join(f.dir, "missing-dir", "state.json")
	_, err := f.do(1, "o", "s", KindStopDB, ServiceArgs{ServiceID: "svc-1"})
	if !errors.Is(err, ErrPersist) {
		t.Fatalf("want ErrPersist, got %v", err)
	}
	if len(f.docker.Calls()) != 0 {
		t.Fatalf("effect without durable intent: %v", f.docker.Calls())
	}
	// The step was not recorded, so it can run once persistence works.
	f.exec.cfg.StatePath = f.statePath
	wantStatus(t, f.mustDo(1, "o", "s", KindStopDB, ServiceArgs{ServiceID: "svc-1"}), StatusSucceeded, "")
}

func TestPersistFailureAfterEffect(t *testing.T) {
	f := newFixture(t)
	stateDir := filepath.Dir(f.statePath)
	f.docker.onCreate = func() {
		if err := os.Chmod(stateDir, 0o500); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })
	r, err := f.do(1, "o", "s", KindCreateService, spec("galera-2"))
	if !errors.Is(err, ErrPersist) {
		t.Skipf("state directory still writable (running as root?): %v", err)
	}
	if r.Status != StatusSucceeded {
		t.Fatalf("effect result not returned: %+v", r)
	}
	f.docker.onCreate = nil
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The outcome is not durable, so a re-send is in doubt, not re-executed.
	wantStatus(t, f.mustDo(1, "o", "s", KindCreateService, spec("galera-2")), StatusUnknown, CodeInDoubt)
	f.restart()
	wantStatus(t, f.mustDo(1, "o", "s", KindCreateService, spec("galera-2")), StatusUnknown, CodeInDoubt)
	if n := countPrefix(f.docker.Calls(), "create-service"); n != 1 {
		t.Fatalf("CreateService called %d times, want 1", n)
	}
}

func TestCancelledContextBeforeExecution(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.exec.Handle(ctx, Command{Term: 1, OpID: "o", StepID: "s", Kind: KindStopDB, Args: mustArgs(t, ServiceArgs{ServiceID: "svc-1"})})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if len(f.docker.Calls()) != 0 {
		t.Fatal("cancelled command reached Docker")
	}
}

func TestDockerErrorsRecorded(t *testing.T) {
	f := newFixture(t)
	f.docker.failNext = errors.New("daemon says no")
	wantStatus(t, f.mustDo(1, "o", "s1", KindStopDB, ServiceArgs{ServiceID: "svc-1"}), StatusFailed, CodeDocker)
	f.docker.failNext = fmt.Errorf("timeout: %w", ErrOutcomeUnknown)
	wantStatus(t, f.mustDo(1, "o", "s2", KindStopDB, ServiceArgs{ServiceID: "svc-1"}), StatusUnknown, CodeInDoubt)
	f.docker.failNext = context.DeadlineExceeded
	wantStatus(t, f.mustDo(1, "o", "s3", KindStopDB, ServiceArgs{ServiceID: "svc-1"}), StatusUnknown, CodeInDoubt)
	// Replays do not retry Docker.
	n := len(f.docker.Calls())
	wantStatus(t, f.mustDo(1, "o", "s1", KindStopDB, ServiceArgs{ServiceID: "svc-1"}), StatusFailed, CodeDocker)
	if len(f.docker.Calls()) != n {
		t.Fatal("replay of failed step called Docker")
	}
}

func TestOwnershipGuardRefusesUnrelatedObjects(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		id         string
		kind       Kind
		wantNoCall bool // refused from the allow-list alone, without inspect
	}{
		{"svc-unlisted", KindRemoveService, true},
		{"svc-unlisted", KindStopDB, true},
		{"unknown-id", KindRemoveService, true},
		{"svc-foreign", KindRemoveService, false},
		{"svc-nolabel", KindStopDB, false},
		{"vol-1-as-svc", KindRemoveService, false},
	}
	for i, tc := range cases {
		before := len(f.docker.Calls())
		r := f.mustDo(1, "op", fmt.Sprintf("s%d", i), tc.kind, ServiceArgs{ServiceID: tc.id})
		wantStatus(t, r, StatusRefused, CodeNotOwned)
		calls := f.docker.Calls()[before:]
		if tc.wantNoCall && len(calls) != 0 {
			t.Fatalf("%s: unlisted object caused Docker calls %v", tc.id, calls)
		}
	}
	r := f.mustDo(1, "op", "inspect-foreign", KindInspect, InspectArgs{ObjectKind: KindService, ID: "svc-foreign"})
	wantStatus(t, r, StatusRefused, CodeNotOwned)
	r = f.mustDo(1, "op", "start-foreign", KindStartDB, StartDBArgs{ServiceID: "svc-foreign", VolumeRoot: f.volume("galera-1"), Expected: testIdentity()})
	wantStatus(t, r, StatusRefused, CodeNotOwned)
	if m := f.docker.Mutations(); len(m) != 0 {
		t.Fatalf("unrelated objects were modified: %v", m)
	}
	// Owned object passes.
	wantStatus(t, f.mustDo(1, "op", "ok", KindInspect, InspectArgs{ObjectKind: KindService, ID: "svc-1"}), StatusSucceeded, "")
}

func TestAllowObjectsStillRequiresLabel(t *testing.T) {
	f := newFixture(t)
	wantStatus(t, f.mustDo(1, "op", "s1", KindRemoveService, ServiceArgs{ServiceID: "svc-unlisted"}), StatusRefused, CodeNotOwned)
	f.mustDo(1, "op", "allow", KindAllowObjects, AllowObjectsArgs{IDs: []string{"svc-unlisted", "svc-other-unlisted"}})
	f.restart() // allow-list additions are durable
	wantStatus(t, f.mustDo(1, "op", "s2", KindStopDB, ServiceArgs{ServiceID: "svc-unlisted"}), StatusSucceeded, "")
	// A label from another installation is still refused even when allowed.
	f.docker.objects["svc-other-unlisted"] = Object{Kind: KindService, ID: "svc-other-unlisted", Labels: map[string]string{LabelInstallation: "other"}}
	wantStatus(t, f.mustDo(1, "op", "s3", KindStopDB, ServiceArgs{ServiceID: "svc-other-unlisted"}), StatusRefused, CodeNotOwned)
}

func TestListOwnedFiltersUnrelated(t *testing.T) {
	f := newFixture(t)
	r := f.mustDo(1, "op", "list", KindListOwned, ListOwnedArgs{ObjectKind: KindService})
	wantStatus(t, r, StatusSucceeded, "")
	var objs []Object
	if err := json.Unmarshal(r.Output, &objs); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, o := range objs {
		ids = append(ids, o.ID)
	}
	if !reflect.DeepEqual(ids, []string{"svc-1"}) {
		t.Fatalf("owned services %v, want [svc-1]", ids)
	}
}

func TestCreateServiceLabelsAndOwnership(t *testing.T) {
	f := newFixture(t)
	bad := spec("evil")
	bad.Labels = map[string]string{LabelInstallation: "other"}
	wantStatus(t, f.mustDo(1, "op", "bad", KindCreateService, bad), StatusRefused, CodeNotOwned)
	if n := countPrefix(f.docker.Calls(), "create-service"); n != 0 {
		t.Fatal("foreign-labelled service created")
	}
	s := spec("galera-2")
	s.Labels = map[string]string{LabelCluster: "cluster-1"}
	r := f.mustDo(1, "op", "create", KindCreateService, s)
	wantStatus(t, r, StatusSucceeded, "")
	var out struct{ ID string }
	if err := json.Unmarshal(r.Output, &out); err != nil || out.ID == "" {
		t.Fatalf("output %s: %v", r.Output, err)
	}
	got := f.docker.objects[out.ID].Labels
	want := map[string]string{LabelInstallation: testInstallation, LabelCluster: "cluster-1", LabelOpID: "op", LabelStepID: "create"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
	// The created service is in the allow-list, also after a restart, and
	// leaves it when removed.
	f.restart()
	wantStatus(t, f.mustDo(1, "op", "rm", KindRemoveService, ServiceArgs{ServiceID: out.ID}), StatusSucceeded, "")
	f.restart()
	if f.exec.owner.Allowed(out.ID) {
		t.Fatal("removed service still in allow-list")
	}
}

func TestStartDBIdentityLifecycle(t *testing.T) {
	f := newFixture(t)
	root := f.volume("galera-1")
	id := testIdentity()
	start := func(step string, exp Identity) Result {
		return f.mustDo(1, "op", step, KindStartDB, StartDBArgs{ServiceID: "svc-1", VolumeRoot: root, Expected: exp})
	}
	scaled := func() int { return countPrefix(f.docker.Calls(), "scale-service svc-1 1") }

	// Empty volume, no token: refused (marker-guard exit 67).
	_ = os.MkdirAll(filepath.Join(root, "data"), 0o755)
	wantStatus(t, start("s1", id), StatusRefused, CodeEmptyVolume)

	// Explicit new identity, then start.
	tok := NewIdentityToken{TokenID: "tok-1", Identity: id}
	wantStatus(t, f.mustDo(1, "op", "init", KindInitIdentity, InitIdentityArgs{VolumeRoot: root, Token: tok}), StatusSucceeded, "")
	wantStatus(t, start("s2", id), StatusSucceeded, "")
	if scaled() != 1 {
		t.Fatalf("scale calls %d, want 1", scaled())
	}

	// The token is single-use: the volume is lost again, the old token must
	// not re-create the same identity.
	if err := os.Remove(filepath.Join(root, MarkerFileName)); err != nil {
		t.Fatal(err)
	}
	f.restart()
	wantStatus(t, f.mustDo(1, "op", "init-again", KindInitIdentity, InitIdentityArgs{VolumeRoot: root, Token: tok}), StatusRefused, CodeTokenConsumed)
	wantStatus(t, start("s3", id), StatusRefused, CodeEmptyVolume)

	// Data without marker (exit 66).
	writeRaw(t, filepath.Join(root, "data", "ibdata1"), "x")
	wantStatus(t, start("s4", id), StatusRefused, CodeUnknownData)
	tok2 := NewIdentityToken{TokenID: "tok-2", Identity: Identity{InstallationID: testInstallation, ClusterID: "cluster-1", NodeUUID: uuidB}}
	wantStatus(t, f.mustDo(1, "op", "init-data", KindInitIdentity, InitIdentityArgs{VolumeRoot: root, Token: tok2}), StatusRefused, CodeUnknownData)

	// Marker of another node / another installation (exit 65).
	other := id
	other.NodeUUID = uuidB
	writeMarker(t, root, other)
	wantStatus(t, start("s5", id), StatusRefused, CodeIdentityMismatch)
	foreign := id
	foreign.InstallationID = "other"
	writeMarker(t, root, foreign)
	wantStatus(t, start("s6", id), StatusRefused, CodeForeignMarker)
	writeRaw(t, filepath.Join(root, MarkerFileName), "{")
	wantStatus(t, start("s7", id), StatusRefused, CodeMarkerCorrupt)

	// Expected identity not configured (exit 64).
	writeMarker(t, root, id)
	wantStatus(t, start("s8", Identity{}), StatusRefused, CodeIdentityNotConfigured)

	if scaled() != 1 {
		t.Fatalf("refused starts scaled the service: %d scale calls", scaled())
	}
	wantStatus(t, start("s9", id), StatusSucceeded, "")
}

func TestStartDBRequiresServiceMount(t *testing.T) {
	f := newFixture(t)
	other := f.volume("galera-2")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, other, testIdentity())
	r := f.mustDo(1, "op", "s", KindStartDB, StartDBArgs{ServiceID: "svc-1", VolumeRoot: other, Expected: testIdentity()})
	wantStatus(t, r, StatusRefused, CodeVolumeMismatch)
	if len(f.docker.Mutations()) != 0 {
		t.Fatal("service scaled despite volume mismatch")
	}
}

func TestRemoveVolumeRequiresIdentity(t *testing.T) {
	f := newFixture(t)
	root := f.volume("galera-1")
	rm := func(step string, vol, path string, exp Identity) Result {
		return f.mustDo(1, "op", step, KindRemoveVolume, RemoveVolumeArgs{VolumeID: vol, VolumeRoot: path, Expected: exp})
	}
	wantStatus(t, rm("s1", "vol-1", root, testIdentity()), StatusRefused, CodeEmptyVolume)
	writeRaw(t, filepath.Join(root, "data", "ibdata1"), "x")
	wantStatus(t, rm("s2", "vol-1", root, testIdentity()), StatusRefused, CodeUnknownData)
	foreign := testIdentity()
	foreign.ClusterID = "other"
	writeMarker(t, root, foreign)
	wantStatus(t, rm("s3", "vol-1", root, testIdentity()), StatusRefused, CodeForeignMarker)
	writeMarker(t, root, testIdentity())
	// Path that is not the volume's mountpoint.
	other := f.volume("galera-9")
	_ = os.MkdirAll(other, 0o755)
	writeMarker(t, other, testIdentity())
	wantStatus(t, rm("s4", "vol-1", other, testIdentity()), StatusRefused, CodeVolumeMismatch)
	// Volume ID in the allow-list but labelled for nobody.
	f.docker.objects["vol-1-as-svc"] = Object{Kind: KindVolume, ID: "vol-1-as-svc", Mountpoint: root}
	wantStatus(t, rm("s5", "vol-1-as-svc", root, testIdentity()), StatusRefused, CodeNotOwned)
	if n := countPrefix(f.docker.Calls(), "remove-volume"); n != 0 {
		t.Fatalf("refused removals reached Docker: %d", n)
	}
	wantStatus(t, rm("s6", "vol-1", root, testIdentity()), StatusSucceeded, "")
	if n := countPrefix(f.docker.Calls(), "remove-volume"); n != 1 {
		t.Fatalf("RemoveVolume calls %d, want 1", n)
	}
}

func TestVolumeRootSymlinkEscapeRefused(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := f.volume("escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	tok := NewIdentityToken{TokenID: "tok", Identity: testIdentity()}
	r := f.mustDo(1, "op", "s", KindInitIdentity, InitIdentityArgs{VolumeRoot: link, Token: tok})
	wantStatus(t, r, StatusRefused, CodeInvalidPath)
	if _, err := os.Stat(filepath.Join(outside, MarkerFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker written outside the volume base")
	}
	// A refused init does not consume the token.
	_ = os.MkdirAll(f.volume("galera-3"), 0o755)
	wantStatus(t, f.mustDo(1, "op", "s2", KindInitIdentity, InitIdentityArgs{VolumeRoot: f.volume("galera-3"), Token: tok}), StatusSucceeded, "")
}

func TestVerifyIdentityCommand(t *testing.T) {
	f := newFixture(t)
	root := f.volume("galera-1")
	writeMarker(t, root, testIdentity())
	wantStatus(t, f.mustDo(1, "op", "v1", KindVerifyIdentity, VerifyIdentityArgs{VolumeRoot: root, Expected: testIdentity()}), StatusSucceeded, "")
	other := testIdentity()
	other.NodeUUID = uuidB
	wantStatus(t, f.mustDo(1, "op", "v2", KindVerifyIdentity, VerifyIdentityArgs{VolumeRoot: root, Expected: other}), StatusRefused, CodeIdentityMismatch)
	if len(f.docker.Calls()) != 0 {
		t.Fatal("verify-identity called Docker")
	}
}

func TestConcurrentHandle(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			term := uint64(1 + i%4)
			_, err := f.do(term, "op", fmt.Sprintf("s%d", i%8), KindInspect, InspectArgs{ObjectKind: KindService, ID: "svc-1"})
			if err != nil && !errors.Is(err, fencing.ErrStaleTerm) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	f.restart()
	for i := range 8 {
		r, err := f.do(4, "op", fmt.Sprintf("s%d", i), KindInspect, InspectArgs{ObjectKind: KindService, ID: "svc-1"})
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != StatusSucceeded {
			t.Fatalf("step s%d: %+v", i, r)
		}
	}
}

func TestNewExecutorValidation(t *testing.T) {
	g, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(t.TempDir(), "term")))
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	good := Config{InstallationID: "i", StatePath: filepath.Join(t.TempDir(), "s.json"), VolumeBase: base, Guard: g, Docker: newFakeDocker()}
	if _, err := NewExecutor(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for name, mut := range map[string]func(*Config){
		"no installation": func(c *Config) { c.InstallationID = "" },
		"no state path":   func(c *Config) { c.StatePath = "" },
		"no guard":        func(c *Config) { c.Guard = nil },
		"no docker":       func(c *Config) { c.Docker = nil },
		"relative base":   func(c *Config) { c.VolumeBase = "volumes" },
		"missing base":    func(c *Config) { c.VolumeBase = filepath.Join(base, "absent") },
	} {
		c := good
		mut(&c)
		if _, err := NewExecutor(c); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
	corrupt := filepath.Join(t.TempDir(), "s.json")
	writeRaw(t, corrupt, "{")
	c := good
	c.StatePath = corrupt
	if _, err := NewExecutor(c); err == nil {
		t.Fatal("corrupt state file accepted")
	}
}

func countPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}
