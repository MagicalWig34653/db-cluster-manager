// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

const (
	applyLine = "env DEBIAN_FRONTEND=noninteractive apt-get -y -qq -o Dpkg::Options::=--force-confold install --only-upgrade openssl"
	rebootCmd = "systemctl reboot"
)

func stateAgent(t *testing.T, dir string, r *FakeRunner) (*AgentService, *AgentState) {
	t.Helper()
	st, err := OpenAgentState(filepath.Join(dir, "patch-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(dir, "term")))
	if err != nil {
		t.Fatal(err)
	}
	fs := FakeFS{"/usr/bin/apt-get": "", "/etc/os-release": "ID=debian\nVERSION_ID=12\n"}
	return &AgentService{Collector: Collector{HostID: "h1", Runner: r, FS: fs}, Guard: g, AllowApply: true, State: st}, st
}

func applyRunner() *FakeRunner {
	r := aptRunner()
	r.Responses["apt-get -s -qq install --only-upgrade openssl"] = ok("Inst openssl [1] (2 x)\n")
	r.Responses[applyLine] = ok("done")
	r.Responses[rebootCmd] = ok("")
	return r
}

func countCalls(r *FakeRunner, substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.Calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

func TestReplaySuppressionSurvivesAgentRestart(t *testing.T) {
	dir := t.TempDir()
	r := applyRunner()
	a, _ := stateAgent(t, dir, r)
	c := context.Background()
	cmd := AgentCommand{Kind: CmdApply, Term: 5, StepID: "op1/apply", Packages: []string{"openssl"}}
	first, err := a.Handle(c, cmd)
	if err != nil || first.Output != "done" {
		t.Fatal(first, err)
	}
	if _, err := a.Handle(c, AgentCommand{Kind: CmdReboot, Term: 5, StepID: "op1-reboot/reboot"}); err != nil {
		t.Fatal(err)
	}
	// Agent restart (for example after the reboot): new process, same state dir.
	r2 := applyRunner()
	b, _ := stateAgent(t, dir, r2)
	again, err := b.Handle(c, cmd)
	if err != nil || again.Output != "done" {
		t.Fatal("replay after restart:", again, err)
	}
	if _, err := b.Handle(c, AgentCommand{Kind: CmdReboot, Term: 5, StepID: "op1-reboot/reboot"}); err != nil {
		t.Fatal(err)
	}
	if countCalls(r2, "install --only-upgrade") != 0 || countCalls(r2, "reboot") != 0 {
		t.Fatalf("mutating command re-ran after restart: %v", r2.Calls)
	}
}

func TestStepInterruptedBeforeResultIsNotRerun(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenAgentState(filepath.Join(dir, "patch-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The agent died after it started the step and before it stored a result.
	if err := st.Begin("op2/apply"); err != nil {
		t.Fatal(err)
	}
	r := applyRunner()
	a, _ := stateAgent(t, dir, r)
	_, err = a.Handle(context.Background(), AgentCommand{Kind: CmdApply, Term: 5, StepID: "op2/apply", Packages: []string{"openssl"}})
	if !errors.Is(err, ErrStepInterrupted) {
		t.Fatal("want ErrStepInterrupted, got", err)
	}
	if countCalls(r, "install --only-upgrade") != 0 {
		t.Fatal("interrupted step was re-run")
	}
}

func TestFailedStepMayBeRetried(t *testing.T) {
	dir := t.TempDir()
	r := applyRunner()
	r.Responses[applyLine] = CmdResult{ExitCode: 100, Stderr: "dpkg lock"}
	a, _ := stateAgent(t, dir, r)
	cmd := AgentCommand{Kind: CmdApply, Term: 5, StepID: "op3/apply", Packages: []string{"openssl"}}
	if _, err := a.Handle(context.Background(), cmd); err == nil {
		t.Fatal("expected failure")
	}
	r.Responses[applyLine] = ok("done")
	if res, err := a.Handle(context.Background(), cmd); err != nil || res.Output != "done" {
		t.Fatal("retry after failure:", res, err)
	}
}

func TestStateFileIsPrivateAndBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patch-state.json")
	st, err := OpenAgentState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func(old int) { maxStoredSteps = old }(maxStoredSteps)
	maxStoredSteps = 20
	for i := 0; i < maxStoredSteps+10; i++ {
		id := "s" + strings.Repeat("x", 3) + string(rune('a'+i%26)) + time.Duration(i).String()
		if err := st.Begin(id); err != nil {
			t.Fatal(err)
		}
		if err := st.Finish(id, AgentResult{Output: "o"}); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v", fi.Mode().Perm())
	}
	if n := st.StepCount(); n != maxStoredSteps {
		t.Fatalf("kept %d steps, want %d", n, maxStoredSteps)
	}
}

func TestCorruptStateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patch-state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAgentState(path); err == nil {
		t.Fatal("corrupt state accepted")
	}
}

func TestDenyListPersistsAndIsConsulted(t *testing.T) {
	dir := t.TempDir()
	a, _ := stateAgent(t, dir, applyRunner())
	c := context.Background()
	cmd := AgentCommand{Kind: CmdDenyList, Term: 5, StepID: "deny-1", DenyRevision: 10, DenySerials: []string{"ab12", "ff"}}
	if _, err := a.Handle(c, cmd); err != nil {
		t.Fatal(err)
	}
	if !a.Revoked(big.NewInt(0xab12)) || !a.Revoked(big.NewInt(0xff)) || a.Revoked(big.NewInt(1)) {
		t.Fatal("deny list not consulted")
	}
	b, _ := stateAgent(t, dir, applyRunner()) // restart
	if !b.Revoked(big.NewInt(0xab12)) {
		t.Fatal("deny list lost over restart")
	}
	// An older revision never shrinks the stored list.
	old := AgentCommand{Kind: CmdDenyList, Term: 5, StepID: "deny-0", DenyRevision: 3, DenySerials: nil}
	res, err := b.Handle(c, old)
	if err != nil || res.Output != "kept revision 10" {
		t.Fatal(res, err)
	}
	if !b.Revoked(big.NewInt(0xff)) {
		t.Fatal("older list replaced newer one")
	}
	// A newer revision replaces it.
	if _, err := b.Handle(c, AgentCommand{Kind: CmdDenyList, Term: 6, StepID: "deny-2", DenyRevision: 11, DenySerials: []string{"1"}}); err != nil {
		t.Fatal(err)
	}
	if b.Revoked(big.NewInt(0xff)) || !b.Revoked(big.NewInt(1)) {
		t.Fatal("list not replaced")
	}
}

func TestDenyListIsFencedValidatedAndWorksOnReportOnlyAgents(t *testing.T) {
	dir := t.TempDir()
	a, _ := stateAgent(t, dir, applyRunner())
	c := context.Background()
	if _, err := a.Handle(c, AgentCommand{Kind: CmdDenyList, Term: 5, StepID: "d", DenyRevision: 1, DenySerials: []string{"1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(c, AgentCommand{Kind: CmdDenyList, Term: 4, StepID: "d2", DenyRevision: 2, DenySerials: []string{"2"}}); !errors.Is(err, ErrStale) {
		t.Fatal("stale term accepted:", err)
	}
	if _, err := a.Handle(c, AgentCommand{Kind: CmdDenyList, Term: 5, StepID: "d3", DenyRevision: 2, DenySerials: []string{"XYZ"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad serial accepted:", err)
	}
	if _, err := a.Handle(c, AgentCommand{Kind: CmdDenyList, StepID: "d4", DenyRevision: 2}); !errors.Is(err, ErrInvalid) {
		t.Fatal("term 0 accepted:", err)
	}
	// Report-only agent (apply off) still takes a deny list: revocation must
	// not depend on the apply switch.
	a.AllowApply = false
	if _, err := a.Handle(c, AgentCommand{Kind: CmdDenyList, Term: 6, StepID: "d5", DenyRevision: 3, DenySerials: []string{"3"}}); err != nil {
		t.Fatal(err)
	}
	if !a.Revoked(big.NewInt(3)) {
		t.Fatal("report-only agent ignored the deny list")
	}
	// Without a guard there is no fencing, so the command is refused.
	bare := &AgentService{Collector: a.Collector}
	if _, err := bare.Handle(c, AgentCommand{Kind: CmdDenyList, Term: 6, StepID: "d6", DenyRevision: 1}); err == nil {
		t.Fatal("unfenced agent took a deny list")
	}
}

// --- manager side ---

type denyHost struct {
	fakeHost
	denies []AgentCommand
	failID string
}

func (d *denyHost) Do(ctx context.Context, id string, c AgentCommand) (AgentResult, error) {
	if c.Kind == CmdDenyList {
		d.mu.Lock()
		defer d.mu.Unlock()
		if id == d.failID {
			return AgentResult{}, errors.New("unreachable")
		}
		d.denies = append(d.denies, c)
		return AgentResult{}, nil
	}
	return d.fakeHost.Do(ctx, id, c)
}

func denyFixture(t *testing.T) (*Service, *denyHost, *DenyList) {
	t.Helper()
	now := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	h := &denyHost{fakeHost: fakeHost{report: HostReport{HostID: "h1", CollectedAt: now, OS: OSInfo{ID: "debian", VersionID: "12"}, Manager: APT}, pending: map[string]string{}}}
	dl := &DenyList{Revision: 5, Serials: []string{"aa"}, Digest: "d1"}
	svc := &Service{KV: &vuln.MemoryKV{}, Journal: opjournal.New(), Agents: h, Guard: AllowAllGuard{},
		Term: func() (uint64, error) { return 9, nil }, Now: func() time.Time { return now }, Notify: func(context.Context, notify.Event) {},
		DenyList: func() (DenyList, error) { return *dl, nil }}
	for _, id := range []string{"h1", "h2"} {
		if _, err := svc.UpsertHost(Host{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	return svc, h, dl
}

func TestDistributeDenyListSendsOnceAndRetriesFailures(t *testing.T) {
	svc, h, dl := denyFixture(t)
	h.failID = "h2"
	c := context.Background()
	err := svc.DistributeDenyList(c)
	if err == nil || !strings.Contains(err.Error(), "h2") {
		t.Fatal("failure of h2 not reported:", err)
	}
	if len(h.denies) != 1 || h.denies[0].Term != 9 || h.denies[0].DenyRevision != 5 || h.denies[0].StepID == "" {
		t.Fatalf("%+v", h.denies)
	}
	h.failID = ""
	if err := svc.DistributeDenyList(c); err != nil || len(h.denies) != 2 {
		t.Fatal("h2 not retried exactly:", err, len(h.denies))
	}
	if err := svc.DistributeDenyList(c); err != nil || len(h.denies) != 2 {
		t.Fatal("unchanged list was resent:", err, len(h.denies))
	}
	dl.Revision, dl.Digest, dl.Serials = 6, "d2", []string{"aa", "bb"}
	if err := svc.DistributeDenyList(c); err != nil || len(h.denies) != 4 {
		t.Fatal("changed list not resent:", err, len(h.denies))
	}
}

func TestDistributeDenyListNeedsLeaderTerm(t *testing.T) {
	svc, h, _ := denyFixture(t)
	svc.Term = func() (uint64, error) { return 0, errors.New("not leader") }
	if err := svc.DistributeDenyList(context.Background()); err == nil || len(h.denies) != 0 {
		t.Fatal("non-leader distributed", err)
	}
	svc.DenyList = nil
	svc.Term = func() (uint64, error) { return 9, nil }
	if err := svc.DistributeDenyList(context.Background()); err != nil || len(h.denies) != 0 {
		t.Fatal("no source must be a no-op:", err)
	}
}
