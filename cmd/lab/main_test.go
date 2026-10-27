// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/lab"
)

// fakeRunner records every call and answers the read-only inventory and
// detection commands. limactl list outputs are returned in order; the last
// one repeats.
type fakeRunner struct {
	mu        sync.Mutex
	calls     [][]string
	memsize   string
	sysctlErr error
	limaLists []string
	colima    string // "" with colimaMissing=false means an empty list
	colimaNA  bool   // colima binary missing
	failOn    string // argv prefix (joined by spaces) that fails
	limaIdx   int
}

func newFake() *fakeRunner {
	return &fakeRunner{memsize: "51539607552\n"}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	argv := append([]string{name}, args...)
	f.calls = append(f.calls, argv)
	joined := strings.Join(argv, " ")
	if f.failOn != "" && strings.HasPrefix(joined, f.failOn) {
		return "", fmt.Errorf("fake failure: %s", joined)
	}
	switch joined {
	case "sysctl -n hw.memsize":
		if f.sysctlErr != nil {
			return "", f.sysctlErr
		}
		return f.memsize, nil
	case "limactl list --json":
		if len(f.limaLists) == 0 {
			return "", nil
		}
		i := f.limaIdx
		if i >= len(f.limaLists) {
			i = len(f.limaLists) - 1
		}
		f.limaIdx++
		return f.limaLists[i], nil
	case "colima list --json":
		if f.colimaNA {
			return "", fmt.Errorf("exec colima: %w", exec.ErrNotFound)
		}
		return f.colima, nil
	}
	return "", nil
}

func (f *fakeRunner) joinedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func (f *fakeRunner) hasCallPrefix(prefix string) bool {
	for _, c := range f.joinedCalls() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func limaJSON(names ...string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "{\"name\":%q,\"status\":\"Running\"}\n", n)
	}
	return b.String()
}

func withCPUs(t *testing.T, n int) {
	t.Helper()
	old := cpuCount
	cpuCount = func() int { return n }
	t.Cleanup(func() { cpuCount = old })
}

func manifestPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "lab", "manifest.json")
}

func runLab(t *testing.T, r lab.Runner, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, r)
	return code, stdout.String(), stderr.String()
}

func commandLines(out, prefix string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

func TestPlanDefaults(t *testing.T) {
	withCPUs(t, 14)
	f := newFake()
	code, out, errOut := runLab(t, f, "plan", "-manifest", manifestPath(t))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	lines := commandLines(out, "limactl ")
	if len(lines) != 6 {
		t.Fatalf("want 6 command lines, got %d:\n%s", len(lines), out)
	}
	want := "limactl create --name=dbcm-lab-m1 --cpus=2 --memory=4 --disk=20 --tty=false template:docker"
	if lines[0] != want {
		t.Fatalf("first command %q, want %q", lines[0], want)
	}
	if lines[5] != "limactl start --tty=false dbcm-lab-m3" {
		t.Fatalf("last command %q", lines[5])
	}
	if got := f.joinedCalls(); len(got) != 1 || got[0] != "sysctl -n hw.memsize" {
		t.Fatalf("plan must only detect the host, calls %q", got)
	}
}

func TestPlanTemplates(t *testing.T) {
	withCPUs(t, 14)
	f := newFake()
	code, out, errOut := runLab(t, f, "plan", "-manifest", manifestPath(t), "-vms", "2", "-templates", "debian-12,almalinux-9")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	lines := commandLines(out, "limactl ")
	if len(lines) != 4 || !strings.HasSuffix(lines[0], "--tty=false template:debian-12") || !strings.HasSuffix(lines[2], "--tty=false template:almalinux-9") {
		t.Fatalf("unexpected commands:\n%s", out)
	}
	for _, bad := range []string{"debian-12", "docker,evil", "docker,../x", "docker,https://x/y.yaml", "docker,", "template:docker,docker"} {
		f := newFake()
		code, _, _ := runLab(t, f, "plan", "-manifest", manifestPath(t), "-vms", "2", "-templates", bad)
		if code != 2 {
			t.Errorf("-templates %q: exit %d, want 2", bad, code)
		}
		if f.hasCallPrefix("limactl") {
			t.Errorf("-templates %q caused limactl calls", bad)
		}
	}
}

func TestPlanUserV2Network(t *testing.T) {
	withCPUs(t, 14)
	f := newFake()
	code, out, errOut := runLab(t, f, "plan", "-manifest", manifestPath(t), "-network", "lima:user-v2")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	lines := commandLines(out, "limactl ")
	want := "limactl create --name=dbcm-lab-m1 --cpus=2 --memory=4 --disk=20 --tty=false --network=lima:user-v2 template:docker"
	if len(lines) != 6 || lines[0] != want || lines[1] != "limactl start --tty=false dbcm-lab-m1" {
		t.Fatalf("unexpected commands:\n%s", out)
	}
	for _, bad := range []string{"lima:shared", "vzNAT", "user-v2", "lima:user-v2,lima:shared"} {
		f := newFake()
		code, _, _ := runLab(t, f, "plan", "-manifest", manifestPath(t), "-network", bad)
		if code != 2 {
			t.Errorf("-network %q: exit %d, want 2", bad, code)
		}
		if f.hasCallPrefix("limactl") {
			t.Errorf("-network %q caused limactl calls %q", bad, f.joinedCalls())
		}
	}
}

func TestPlanRAMBudgetExceeded(t *testing.T) {
	withCPUs(t, 14)
	f := newFake()
	code, _, errOut := runLab(t, f, "plan", "-manifest", manifestPath(t), "-vms", "3", "-ram", "8")
	if code == 0 {
		t.Fatal("24 GiB RAM must be rejected")
	}
	if !strings.Contains(errOut, "ram") {
		t.Fatalf("stderr %q lacks \"ram\"", errOut)
	}
}

func TestPlanCPUBudgetFromHost(t *testing.T) {
	withCPUs(t, 8) // limit 4 CPUs < 6 requested
	code, _, errOut := runLab(t, newFake(), "plan", "-manifest", manifestPath(t))
	if code == 0 || !strings.Contains(errOut, "cpu") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestPlanRejectsInvalidFlags(t *testing.T) {
	withCPUs(t, 14)
	for _, args := range [][]string{
		{"-vms", "0"}, {"-cpus", "-1"}, {"-ram", "0"}, {"-disk", "-5"}, {"extra"}, {"-unknown"},
	} {
		f := newFake()
		code, _, _ := runLab(t, f, append([]string{"plan", "-manifest", manifestPath(t)}, args...)...)
		if code == 0 {
			t.Errorf("args %q accepted", args)
		}
		if f.hasCallPrefix("limactl") {
			t.Errorf("args %q caused limactl calls %q", args, f.joinedCalls())
		}
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}} {
		code, _, errOut := runLab(t, newFake(), args...)
		if code != 2 || !strings.Contains(errOut, "usage") {
			t.Errorf("args %q: exit %d stderr %q", args, code, errOut)
		}
	}
}

func TestApplyWithoutConfirmationIsDryRun(t *testing.T) {
	withCPUs(t, 14)
	f := newFake()
	path := manifestPath(t)
	code, out, errOut := runLab(t, f, "apply", "-manifest", path)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if f.hasCallPrefix("limactl create") || f.hasCallPrefix("limactl start") {
		t.Fatalf("dry run made mutating calls %q", f.joinedCalls())
	}
	if !strings.Contains(out, "--apply") {
		t.Fatalf("dry run must explain --apply, got:\n%s", out)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run must not write the manifest: %v", err)
	}
}

func TestApplyExecutesAndShowsDiff(t *testing.T) {
	withCPUs(t, 14)
	f := newFake()
	f.colimaNA = true
	f.limaLists = []string{
		limaJSON("default", "other"),
		limaJSON("default", "other", "dbcm-lab-m1"),
	}
	path := manifestPath(t)
	code, out, errOut := runLab(t, f, "apply", "-manifest", path, "-vms", "1", "--apply")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q\n%s", code, errOut, out)
	}
	for _, want := range []string{"pre-snapshot", "lima:default", "lima:other", "added: lima:dbcm-lab-m1", "removed: (none)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !f.hasCallPrefix("limactl create --name=dbcm-lab-m1") || !f.hasCallPrefix("limactl start --tty=false dbcm-lab-m1") {
		t.Fatalf("calls %q", f.joinedCalls())
	}
	m, err := lab.LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Find("lima-vm", "dbcm-lab-m1"); !ok {
		t.Fatalf("manifest lacks VM: %+v", m)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "pre-apply-snapshot.json")); err != nil {
		t.Fatalf("pre-snapshot not recorded: %v", err)
	}
}

func TestApplyRefusesWithoutHostCapacity(t *testing.T) {
	withCPUs(t, 14)
	for name, f := range map[string]*fakeRunner{
		"sysctl fails": {sysctlErr: errors.New("no sysctl")},
		"bad memsize":  {memsize: "garbage"},
		"zero memsize": {memsize: "0"},
	} {
		code, _, errOut := runLab(t, f, "apply", "-manifest", manifestPath(t), "--apply")
		if code == 0 {
			t.Errorf("%s: apply must refuse", name)
		}
		if !strings.Contains(errOut, "host capacity") {
			t.Errorf("%s: stderr %q", name, errOut)
		}
		if f.hasCallPrefix("limactl") {
			t.Errorf("%s: calls %q", name, f.joinedCalls())
		}
	}
	withCPUs(t, 0)
	f := newFake()
	if code, _, _ := runLab(t, f, "apply", "-manifest", manifestPath(t), "--apply"); code == 0 || f.hasCallPrefix("limactl") {
		t.Fatalf("zero CPUs: exit %d calls %q", code, f.joinedCalls())
	}
}

func TestApplyFailsOnUnexpectedDiff(t *testing.T) {
	withCPUs(t, 14)
	f := newFake()
	f.limaLists = []string{
		limaJSON("default"),
		limaJSON("dbcm-lab-m1", "surprise"), // default vanished, foreign VM appeared
	}
	code, out, errOut := runLab(t, f, "apply", "-manifest", manifestPath(t), "-vms", "1", "--apply")
	if code == 0 {
		t.Fatalf("unexpected diff must fail:\n%s", out)
	}
	for _, want := range []string{"lima:default", "lima:surprise"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q: %q", want, errOut)
		}
	}
}

func TestApplyExecuteFailureStillDiffs(t *testing.T) {
	withCPUs(t, 14)
	f := newFake()
	f.failOn = "limactl start"
	f.limaLists = []string{limaJSON("default"), limaJSON("default", "dbcm-lab-m1")}
	path := manifestPath(t)
	code, out, _ := runLab(t, f, "apply", "-manifest", path, "-vms", "1", "--apply")
	if code == 0 {
		t.Fatal("failed start must fail apply")
	}
	if !strings.Contains(out, "added: lima:dbcm-lab-m1") {
		t.Fatalf("post-snapshot diff missing:\n%s", out)
	}
	m, err := lab.LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Find("lima-vm", "dbcm-lab-m1"); !ok {
		t.Fatal("created VM must stay recorded for cleanup")
	}
}

func seedManifest(t *testing.T, path string, res ...lab.Resource) {
	t.Helper()
	m := &lab.Manifest{Version: 1}
	m.Resources = append(m.Resources, res...) // bypass Add to allow foreign names
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
}

var mutatingPrefixes = []string{"limactl create", "limactl start", "limactl stop", "limactl delete", "rm ", "colima start", "colima stop", "colima delete", "docker"}

func TestCleanupPreviewMakesNoMutatingCall(t *testing.T) {
	path := manifestPath(t)
	seedManifest(t, path,
		lab.Resource{Kind: "lima-vm", Name: "dbcm-lab-m1"},
		lab.Resource{Kind: "lima-vm", Name: "default"},
	)
	f := newFake()
	code, out, errOut := runLab(t, f, "cleanup", "-manifest", path)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	for _, p := range mutatingPrefixes {
		if f.hasCallPrefix(p) {
			t.Fatalf("preview made mutating call %q", f.joinedCalls())
		}
	}
	if !strings.Contains(out, "limactl delete --force dbcm-lab-m1") || strings.Contains(out, " default") {
		t.Fatalf("preview output:\n%s", out)
	}
	if !strings.Contains(out, "--apply") {
		t.Fatalf("preview must explain --apply:\n%s", out)
	}
}

func TestCleanupApplyRemovesOnlyLabEntries(t *testing.T) {
	withCPUs(t, 14)
	path := manifestPath(t)
	seedManifest(t, path,
		lab.Resource{Kind: "lima-vm", Name: "dbcm-lab-m1"},
		lab.Resource{Kind: "lima-vm", Name: "default"}, // hand-edited foreign entry
	)
	f := newFake()
	f.colima = `{"name":"default"}`
	f.limaLists = []string{limaJSON("default", "dbcm-lab-m1"), limaJSON("default")}
	code, out, errOut := runLab(t, f, "cleanup", "-manifest", path, "--apply")
	if code != 0 {
		t.Fatalf("exit %d stderr %q\n%s", code, errOut, out)
	}
	for _, c := range f.joinedCalls() {
		for _, p := range mutatingPrefixes {
			if strings.HasPrefix(c, p) && !strings.HasSuffix(c, " dbcm-lab-m1") {
				t.Fatalf("mutating call on non-lab resource: %q", c)
			}
		}
	}
	if !f.hasCallPrefix("limactl stop --force dbcm-lab-m1") || !f.hasCallPrefix("limactl delete --force dbcm-lab-m1") {
		t.Fatalf("calls %q", f.joinedCalls())
	}
	for _, want := range []string{"pre-snapshot", "removed: lima:dbcm-lab-m1", "unrelated resources intact"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	m, err := lab.LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Find("lima-vm", "dbcm-lab-m1"); ok {
		t.Fatal("deleted VM still recorded")
	}
}

func TestCleanupApplyFailsWhenUnrelatedEntryDisappears(t *testing.T) {
	withCPUs(t, 14)
	path := manifestPath(t)
	seedManifest(t, path, lab.Resource{Kind: "lima-vm", Name: "dbcm-lab-m1"})
	f := newFake()
	f.colima = `{"name":"work"}`
	f.limaLists = []string{limaJSON("default", "dbcm-lab-m1"), limaJSON()} // shrunken: default lost
	code, _, errOut := runLab(t, f, "cleanup", "-manifest", path, "--apply")
	if code == 0 {
		t.Fatal("lost unrelated entry must fail cleanup")
	}
	if !strings.Contains(errOut, "lima:default") {
		t.Fatalf("stderr must name the lost entry: %q", errOut)
	}
}

func TestCleanupApplyRefusesWithoutHostCapacity(t *testing.T) {
	path := manifestPath(t)
	seedManifest(t, path, lab.Resource{Kind: "lima-vm", Name: "dbcm-lab-m1"})
	f := &fakeRunner{sysctlErr: errors.New("no sysctl")}
	code, _, errOut := runLab(t, f, "cleanup", "-manifest", path, "--apply")
	if code == 0 || !strings.Contains(errOut, "host capacity") {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	if f.hasCallPrefix("limactl") {
		t.Fatalf("calls %q", f.joinedCalls())
	}
}

func TestCleanupApplyFailedDeleteIsFatal(t *testing.T) {
	withCPUs(t, 14)
	path := manifestPath(t)
	seedManifest(t, path, lab.Resource{Kind: "lima-vm", Name: "dbcm-lab-m1"})
	f := newFake()
	f.failOn = "limactl delete"
	f.limaLists = []string{limaJSON("dbcm-lab-m1")}
	if code, _, _ := runLab(t, f, "cleanup", "-manifest", path, "--apply"); code == 0 {
		t.Fatal("failed delete must fail cleanup")
	}
	m, err := lab.LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Find("lima-vm", "dbcm-lab-m1"); !ok {
		t.Fatal("VM must stay recorded after failed delete")
	}
}

func TestStatusReadOnly(t *testing.T) {
	withCPUs(t, 14)
	path := manifestPath(t)
	seedManifest(t, path, lab.Resource{Kind: "lima-vm", Name: "dbcm-lab-m1"})
	f := newFake()
	f.colimaNA = true
	f.limaLists = []string{limaJSON("default", "dbcm-lab-m1")}
	code, out, errOut := runLab(t, f, "status", "-manifest", path)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	for _, want := range []string{"logical CPUs: 14", "lab CPU limit: 7", "lima-vm dbcm-lab-m1", "unrelated", "lima:default"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	for _, p := range mutatingPrefixes {
		if f.hasCallPrefix(p) {
			t.Fatalf("status made mutating call %q", f.joinedCalls())
		}
	}
}
