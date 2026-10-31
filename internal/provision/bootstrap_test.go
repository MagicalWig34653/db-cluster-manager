// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

var bootHost = Host{ID: "fresh1", Address: "sim.example.test", User: "root"}

func bootEnv() BootstrapEnv {
	return BootstrapEnv{
		Installation: "inst-1", ManagerURL: "https://manager.example.test:8443",
		CAFingerprint: strings.Repeat("AB:", 31) + "AB",
		Release:       AgentRelease{Version: "v0.1.0", BinaryURL: "https://releases.example.test/agent", SHA256: strings.Repeat("a", 64)},
	}
}

func fullProfile() BootstrapProfile {
	return BootstrapProfile{
		ServiceUser: "dbcm", Firewall: true, UnattendedUpgrades: false,
		DedicatedKey: true, DisableRootLogin: true,
		Runtime: "docker", NativeAgent: true, ContainerAgent: true,
		Patch: true, Restic: true, ResticPaths: []string{"/srv/data"},
		Cluster: ClusterChoice{Platform: "swarm", Role: "init"},
	}
}

func inspectAndBuild(t *testing.T, s *simHost, p BootstrapProfile) (BootstrapPlan, BootstrapCheck) {
	t.Helper()
	chk, err := BootstrapInspect(context.Background(), s, bootHost, s.rootCred(), p)
	if err != nil {
		t.Fatal(err)
	}
	pub := ""
	if p.DedicatedKey {
		_, pub, err = GenerateDedicatedKey("dbcm-test")
		if err != nil {
			t.Fatal(err)
		}
	}
	plan, err := BuildBootstrapPlan(BootstrapRequest{ID: "boot1", Host: bootHost, Profile: p}, chk, bootEnv(), simAgents{}, pub)
	if err != nil {
		t.Fatal(err)
	}
	return plan, chk
}

func stepIDs(p BootstrapPlan) []string {
	var ids []string
	for _, s := range p.Steps {
		ids = append(ids, s.ID)
	}
	return ids
}

func newDeps() (BootstrapDeps, *simTokens, *simManager) {
	tok, mgr := &simTokens{}, &simManager{}
	return BootstrapDeps{Tokens: tok, Agents: simAgents{}, Manager: mgr, Journal: opjournal.New()}, tok, mgr
}

func TestSupportMatrix(t *testing.T) {
	for _, c := range []struct{ id, ver, pm string }{
		{"debian", "12", "apt"}, {"ubuntu", "24.04", "apt"}, {"almalinux", "9.4", "dnf"}, {"rocky", "9.3", "dnf"}, {"alpine", "3.20.3", "apk"},
	} {
		got, err := LookupSupport(c.id, c.ver)
		if err != nil || got.PackageManager != c.pm {
			t.Errorf("%s %s: %+v %v", c.id, c.ver, got, err)
		}
	}
	for _, c := range [][2]string{{"fedora", "40"}, {"debian", "9"}, {"ubuntu", "20.04"}, {"almalinux", "8.9"}, {"arch", ""}} {
		if _, err := LookupSupport(c[0], c[1]); !errors.Is(err, ErrUnsupportedOS) {
			t.Errorf("%v accepted: %v", c, err)
		}
	}
	if len(SupportMatrix()) != 5 {
		t.Fatal("matrix size")
	}
}

func TestProfileValidation(t *testing.T) {
	bad := []func(*BootstrapProfile){
		func(p *BootstrapProfile) { p.ServiceUser = "root" },
		func(p *BootstrapProfile) { p.ServiceUser = "x;rm -rf /" },
		func(p *BootstrapProfile) { p.Runtime = "podman" },
		func(p *BootstrapProfile) { p.Runtime = "none" },                                                // swarm needs docker
		func(p *BootstrapProfile) { p.DedicatedKey = false },                                            // root login needs the dedicated key
		func(p *BootstrapProfile) { p.NativeAgent = false },                                             // patch/restic need the native agent
		func(p *BootstrapProfile) { p.Cluster.Platform = "none" },                                       // container agent needs swarm
		func(p *BootstrapProfile) { p.Cluster = ClusterChoice{Platform: "swarm", Role: "join-worker"} }, // join needs an address
		func(p *BootstrapProfile) { p.Cluster.Platform = "mesos" },
	}
	for i, mutate := range bad {
		p := fullProfile()
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
	if err := fullProfile().Validate(); err != nil {
		t.Fatal(err)
	}
}

// Fresh Debian host: preflight, plan, execution of the main phase and of the
// separately approved lockdown phase.
func TestFreshDebianPlanAndRun(t *testing.T) {
	s := newSimHost("debian", "12.5")
	p := fullProfile()
	plan, ded := planWithKey(t, s, p)
	chk := plan.Check
	if plan.Blocked || chk.OS != "debian" || plan.OS.PackageManager != "apt" {
		t.Fatalf("%+v %+v", plan.Findings, chk)
	}
	want := []string{"base-packages", "service-user", "sshd-audit", "firewall", "runtime-docker", "restic", "agent-native", "cluster-swarm", "agent-container",
		"lockdown-install-key", "lockdown-verify-key", "lockdown-store-key", "lockdown-disable-root", "lockdown-confirm"}
	if got := stepIDs(plan); !slices.Equal(got, want) {
		t.Fatalf("steps %v", got)
	}
	if plan.Digest == "" || plan.LockdownDigest == "" || plan.Digest == plan.LockdownDigest {
		t.Fatal("digests")
	}
	fw := plan.Step("firewall")
	if i, j := strings.Index(fw.Script, "allow 22/tcp"), strings.Index(fw.Script, "ufw --force enable"); i < 0 || j < 0 || i > j {
		t.Fatalf("ssh port must be allowed before the firewall is enabled:\n%s", fw.Script)
	}
	docker := plan.Step("runtime-docker")
	if !strings.Contains(docker.Script, DockerKeyFingerprint) || !strings.Contains(docker.Script, "https://download.docker.com/linux/debian") ||
		!strings.Contains(docker.Script, "fingerprint mismatch") {
		t.Fatal("docker repository key is not pinned by fingerprint")
	}
	for _, st := range plan.Steps {
		if st.ManagerAction != "" {
			if st.Script != "" || st.RunAs != "manager" {
				t.Errorf("manager step %s must not carry a script", st.ID)
			}
			continue
		}
		if st.ScriptDigest == "" || st.Script == "" || !strings.Contains(st.Script, "# dbcm-bootstrap boot1 "+st.ID+" run") ||
			!strings.Contains(st.Check, "# dbcm-bootstrap boot1 "+st.ID+" check") && st.Mutating {
			t.Errorf("step %s is not a marked, digested script", st.ID)
		}
		if st.Mutating && st.Cleanup == "" && st.ManagerAction == "" {
			t.Errorf("step %s has no cleanup plan", st.ID)
		}
	}
	for _, id := range []string{"lockdown-install-key", "lockdown-verify-key", "lockdown-store-key", "lockdown-disable-root", "lockdown-confirm"} {
		if plan.Step(id).Gate != "lockdown" {
			t.Errorf("%s is not gated", id)
		}
	}
	// A changed script changes the digest.
	again := plan
	again.Steps = slices.Clone(plan.Steps)
	again.Steps[0].Script += "echo extra\n"
	if again.ComputeDigest() == plan.Digest {
		t.Fatal("digest does not cover scripts")
	}
	cleanup := CleanupScript(plan)
	if !strings.Contains(cleanup, "sshd-drop-in") && !strings.Contains(cleanup, "00-dbcm-lockdown") {
		t.Fatal("cleanup plan misses the lockdown drop-in")
	}
	for _, bad := range []string{"docker system prune", "rm -rf /\n", "rm -rf /*", "--volumes", "apt-get purge", "dnf remove docker"} {
		if strings.Contains(cleanup, bad) {
			t.Errorf("cleanup contains global destructive command %q", bad)
		}
	}
	// All generated scripts parse.
	if sh, err := exec.LookPath("sh"); err == nil {
		for _, st := range plan.Steps {
			for name, body := range map[string]string{"script": st.Script, "check": st.Check, "cleanup": st.Cleanup} {
				if body == "" {
					continue
				}
				cmd := exec.Command(sh, "-n")
				cmd.Stdin = strings.NewReader(body)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("step %s %s: sh -n: %v %s", st.ID, name, err, out)
				}
			}
		}
	}

	deps, tok, mgr := newDeps()
	deps.Runner = s
	secrets := BootstrapSecrets{Root: s.rootCred(), JoinToken: ""}
	prog, err := ExecuteBootstrap(context.Background(), deps, plan, secrets, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !prog.MainDone || prog.LockdownDone {
		t.Fatalf("%+v", prog)
	}
	if !s.docker || s.swarm != "active" || !s.svcUser || !s.firewall || !s.agent || !s.restic || len(mgr.deploys) != 1 {
		t.Fatalf("host state %+v deploys=%v", s, mgr.deploys)
	}
	if s.rootDisabled || s.dedPub != "" {
		t.Fatal("lockdown ran without approval")
	}
	// Secrets: enrollment token only on stdin, never in a command line.
	if len(tok.issued) != 1 {
		t.Fatalf("tokens %v", tok.issued)
	}
	for _, c := range s.calls {
		for _, secret := range []string{s.rootKey, "enroll-token-SECRET"} {
			if strings.Contains(c.Cmd, secret) {
				t.Fatalf("secret in command of step %s", c.Step)
			}
		}
	}
	if !slices.ContainsFunc(s.calls, func(c simCall) bool {
		return c.Step == "agent-native" && c.Kind == "run" && strings.HasPrefix(c.Stdin, "enroll-token-SECRET")
	}) {
		t.Fatal("token was not delivered on stdin")
	}
	if j := mustOp(t, deps.Journal); j.Steps[0].State != opjournal.Succeeded {
		t.Fatalf("journal %+v", j)
	}

	// Lockdown phase.
	secrets.Dedicated = ded
	prog, err = ExecuteBootstrap(context.Background(), deps, plan, secrets, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if !prog.LockdownDone || !s.rootDisabled || s.revertArmed || len(mgr.stored) != 1 {
		t.Fatalf("lockdown %+v armed=%v stored=%d", prog, s.revertArmed, len(mgr.stored))
	}
	// The key was stored before root login was disabled.
	storeAt, disableAt := -1, -1
	for i, c := range s.calls {
		if c.Step == "lockdown-disable-root" && c.Kind == "run" {
			disableAt = i
		}
	}
	for i, c := range s.calls {
		if c.Step == "lockdown-verify-key" && c.User == "dbcm" {
			storeAt = i
		}
	}
	if storeAt < 0 || disableAt < 0 || storeAt > disableAt {
		t.Fatalf("verify %d disable %d", storeAt, disableAt)
	}
}

func mustOp(t *testing.T, j *opjournal.Journal) opjournal.Operation {
	t.Helper()
	op, err := j.Get("bootstrap/boot1")
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func planWithKey(t *testing.T, s *simHost, p BootstrapProfile) (BootstrapPlan, Credential) {
	t.Helper()
	chk, err := BootstrapInspect(context.Background(), s, bootHost, s.rootCred(), p)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := GenerateDedicatedKey("dbcm-test")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildBootstrapPlan(BootstrapRequest{ID: "boot1", Host: bootHost, Profile: p}, chk, bootEnv(), simAgents{}, pub)
	if err != nil {
		t.Fatal(err)
	}
	return plan, Credential{PrivateKey: priv, KnownHosts: s.knownHosts()}
}

func TestFreshAlmaLinuxUsesDnfAndFirewalld(t *testing.T) {
	s := newSimHost("almalinux", "9.4")
	p := fullProfile()
	p.NativeAgent, p.ContainerAgent, p.Patch, p.Restic, p.ResticPaths = false, false, false, false, nil
	p.DedicatedKey, p.DisableRootLogin = false, false
	p.UnattendedUpgrades = true
	p.Cluster = ClusterChoice{Platform: "none"}
	p.Runtime = "docker"
	plan, _ := inspectAndBuild(t, s, p)
	if plan.Blocked {
		t.Fatalf("%+v", plan.Findings)
	}
	d := plan.Step("runtime-docker").Script
	if !strings.Contains(d, "dnf") || !strings.Contains(d, "download.docker.com/linux/centos") || !strings.Contains(d, DockerRPMKeyFingerprint) || strings.Contains(d, DockerKeyFingerprint) || strings.Contains(d, "apt-get") {
		t.Fatalf("docker script:\n%s", d)
	}
	if fw := plan.Step("firewall").Script; !strings.Contains(fw, "firewall-cmd") || strings.Contains(fw, "ufw") {
		t.Fatal("firewalld expected")
	}
	if ua := plan.Step("unattended-upgrades").Script; !strings.Contains(ua, "dnf-automatic") || !strings.Contains(ua, "security") {
		t.Fatal("dnf-automatic expected")
	}
	deps, _, _ := newDeps()
	deps.Runner = s
	if _, err := ExecuteBootstrap(context.Background(), deps, plan, BootstrapSecrets{Root: s.rootCred()}, 1, false); err != nil {
		t.Fatal(err)
	}
	if !s.docker || !s.firewall || !s.autoUpdates {
		t.Fatalf("%+v", s)
	}
}

func TestAlpineLimitsAreBlockers(t *testing.T) {
	s := newSimHost("alpine", "3.20.3")
	p := fullProfile()
	plan, _ := inspectAndBuild(t, s, p)
	if !plan.Blocked {
		t.Fatal("native agent, firewall and unattended-upgrades must be refused on Alpine")
	}
	codes := map[string]bool{}
	for _, f := range plan.Findings {
		if f.Severity == "block" {
			codes[f.Code] = true
		}
	}
	if !codes["native-agent-unsupported-os"] || !codes["firewall-unsupported-os"] {
		t.Fatalf("%v", codes)
	}
	// A runtime-only profile is fine and uses the distribution package.
	p = BootstrapProfile{ServiceUser: "dbcm", Runtime: "docker", Cluster: ClusterChoice{Platform: "none"}}
	plan, _ = inspectAndBuild(t, s, p)
	if plan.Blocked || !strings.Contains(plan.Step("runtime-docker").Script, "apk add") {
		t.Fatalf("%+v", plan.Findings)
	}
}

func TestPartiallyConfiguredHost(t *testing.T) {
	s := newSimHost("ubuntu", "24.04")
	s.docker, s.svcUser, s.sudoers, s.basePkgs, s.chrony, s.timeSynced = true, true, true, true, true, true
	p := fullProfile()
	p.NativeAgent, p.ContainerAgent, p.Patch, p.Restic, p.ResticPaths, p.Firewall = false, false, false, false, nil, false
	p.DedicatedKey, p.DisableRootLogin = false, false
	plan, chk := inspectAndBuild(t, s, p)
	if chk.Docker == "" || chk.Docker == "none" || !chk.ServiceUserExists {
		t.Fatalf("%+v", chk)
	}
	if plan.Step("runtime-docker").Expect != "done" || plan.Step("service-user").Expect != "done" {
		t.Fatal("plan does not show what is already satisfied")
	}
	warned := false
	for _, f := range plan.Findings {
		warned = warned || f.Code == "docker-existing"
	}
	if !warned {
		t.Fatal("existing docker engine must be reported")
	}
	deps, _, _ := newDeps()
	deps.Runner = s
	if _, err := ExecuteBootstrap(context.Background(), deps, plan, BootstrapSecrets{Root: s.rootCred()}, 1, false); err != nil {
		t.Fatal(err)
	}
	if s.stepCalls("runtime-docker", "run") != 0 || s.stepCalls("service-user", "run") != 0 || s.stepCalls("base-packages", "run") != 0 {
		t.Fatal("satisfied steps must not run their scripts again")
	}
	if s.swarm != "active" || s.stepCalls("cluster-swarm", "run") != 1 {
		t.Fatal("remaining steps must still run")
	}
	// Every step is journaled as succeeded.
	for _, st := range mustOp(t, deps.Journal).Steps {
		if st.State != opjournal.Succeeded {
			t.Fatalf("%+v", st)
		}
	}
}

func TestExistingStateBlocksThePlan(t *testing.T) {
	check := func(name string, mutate func(*simHost), code string, wantErr error) {
		t.Helper()
		s := newSimHost("debian", "12")
		mutate(s)
		p := fullProfile()
		chk, err := BootstrapInspect(context.Background(), s, bootHost, s.rootCred(), p)
		if wantErr != nil {
			if !errors.Is(err, wantErr) {
				t.Fatalf("%s: %v", name, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, pub, _ := GenerateDedicatedKey("x")
		plan, err := BuildBootstrapPlan(BootstrapRequest{ID: "b", Host: bootHost, Profile: p}, chk, bootEnv(), simAgents{}, pub)
		if err != nil || !plan.Blocked {
			t.Fatalf("%s: blocked=%v err=%v", name, plan.Blocked, err)
		}
		if !slices.ContainsFunc(plan.Findings, func(f BootstrapFinding) bool { return f.Code == code && f.Severity == "block" }) {
			t.Fatalf("%s: %+v", name, plan.Findings)
		}
	}
	check("foreign swarm", func(s *simHost) { s.docker, s.swarm = true, "active" }, "", ErrExistingCluster)
	check("foreign k3s", func(s *simHost) { s.k3s = true }, "", ErrExistingCluster)
	check("foreign agent", func(s *simHost) { s.agent = true }, "agent-present", nil)
	check("busy port", func(s *simHost) { s.busy = []string{"2377"} }, "ports-busy", nil)
	check("small disk", func(s *simHost) { s.disk = 2 << 30 }, "disk-low", nil)
	check("small memory", func(s *simHost) { s.mem = 256 << 20 }, "memory-low", nil)
	check("unsupported os", func(s *simHost) { s.osID = "fedora" }, "os-unsupported", nil)
	// Own earlier agent (marker present) is accepted.
	s := newSimHost("debian", "12")
	s.agent, s.agentMarker = true, true
	plan, _ := inspectAndBuild(t, s, fullProfile())
	if plan.Blocked || plan.Step("agent-native").Expect != "done" {
		t.Fatalf("%+v", plan.Findings)
	}
}

func TestOtherSoftwareIsAWarningNotABlocker(t *testing.T) {
	s := newSimHost("debian", "12")
	s.other = []string{"nginx", "podman"}
	plan, _ := inspectAndBuild(t, s, fullProfile())
	if plan.Blocked || !slices.ContainsFunc(plan.Findings, func(f BootstrapFinding) bool { return f.Code == "software-existing" && f.Severity == "warn" }) {
		t.Fatalf("%+v", plan.Findings)
	}
}

func TestFailureMidRunThenResumeAndFencing(t *testing.T) {
	s := newSimHost("debian", "12")
	p := fullProfile()
	p.DedicatedKey, p.DisableRootLogin = false, false
	plan, _ := inspectAndBuild(t, s, p)
	s.failOnce["runtime-docker"] = true
	deps, _, _ := newDeps()
	deps.Runner = s
	secrets := BootstrapSecrets{Root: s.rootCred()}
	prog, err := ExecuteBootstrap(context.Background(), deps, plan, secrets, 1, false)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("err = %v", err)
	}
	if prog.MainDone || prog.FailedStep != "runtime-docker" {
		t.Fatalf("%+v", prog)
	}
	op := mustOp(t, deps.Journal)
	states := map[string]opjournal.State{}
	for _, st := range op.Steps {
		states[st.ID] = st.State
	}
	if states["base-packages"] != opjournal.Succeeded || states["runtime-docker"] != opjournal.Unknown || states["cluster-swarm"] != opjournal.Pending {
		t.Fatalf("%v", states)
	}
	if s.swarm == "active" {
		t.Fatal("later steps ran after a failed step")
	}
	baseRuns := s.stepCalls("base-packages", "run")

	// A stale controller (lower term) is fenced out.
	if _, err := ExecuteBootstrap(context.Background(), deps, plan, secrets, 0, false); !errors.Is(err, opjournal.ErrNotOwner) {
		t.Fatalf("stale term: %v", err)
	}
	// Resume under a newer term: the unknown step is re-checked, re-run, the
	// finished steps are not touched again.
	prog, err = ExecuteBootstrap(context.Background(), deps, plan, secrets, 2, false)
	if err != nil || !prog.MainDone {
		t.Fatalf("resume: %v %+v", err, prog)
	}
	if s.stepCalls("base-packages", "run") != baseRuns || s.stepCalls("runtime-docker", "run") != 2 || !s.docker || s.swarm != "active" {
		t.Fatalf("base=%d docker=%d", s.stepCalls("base-packages", "run"), s.stepCalls("runtime-docker", "run"))
	}
	// Running a completed plan again changes nothing.
	n := s.mutatingCalls()
	if _, err := ExecuteBootstrap(context.Background(), deps, plan, secrets, 2, false); err != nil || s.mutatingCalls() != n {
		t.Fatalf("not idempotent: %v", err)
	}
}

func TestExecuteRefusesTamperedAndBlockedPlans(t *testing.T) {
	s := newSimHost("debian", "12")
	plan, _ := inspectAndBuild(t, s, fullProfile())
	deps, _, _ := newDeps()
	deps.Runner = s
	tampered := plan
	tampered.Steps = slices.Clone(plan.Steps)
	tampered.Steps[1].Script += "\necho pwned\n"
	if _, err := ExecuteBootstrap(context.Background(), deps, tampered, BootstrapSecrets{Root: s.rootCred()}, 1, false); !errors.Is(err, ErrDigest) {
		t.Fatalf("%v", err)
	}
	blocked := plan
	blocked.Blocked = true
	blocked.Digest = blocked.ComputeDigest()
	if _, err := ExecuteBootstrap(context.Background(), deps, blocked, BootstrapSecrets{Root: s.rootCred()}, 1, false); !errors.Is(err, ErrBootstrapBlocked) {
		t.Fatalf("%v", err)
	}
	if s.mutatingCalls() != 0 {
		t.Fatal("a refused plan touched the host")
	}
}

func TestPlanDoesNotContainSecrets(t *testing.T) {
	s := newSimHost("debian", "12")
	plan, _ := inspectAndBuild(t, s, fullProfile())
	b, _ := json.Marshal(plan)
	for _, secret := range []string{s.rootKey, "enroll-token", "PRIVATE KEY"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("plan contains %q", secret)
		}
	}
}

func TestLockdownRevertsWhenTheDedicatedKeyFails(t *testing.T) {
	s := newSimHost("debian", "12")
	p := fullProfile()
	p.NativeAgent, p.ContainerAgent, p.Patch, p.Restic, p.ResticPaths, p.Firewall = false, false, false, false, nil, false
	p.Cluster = ClusterChoice{Platform: "none"}
	plan, ded := planWithKey(t, s, p)
	deps, _, mgr := newDeps()
	deps.Runner = s
	secrets := BootstrapSecrets{Root: s.rootCred(), Dedicated: ded}
	if _, err := ExecuteBootstrap(context.Background(), deps, plan, secrets, 1, false); err != nil {
		t.Fatal(err)
	}
	// After root login is disabled the dedicated key stops working.
	hook := &breakAfterDisable{simHost: s}
	deps.Runner = hook
	prog, err := ExecuteBootstrap(context.Background(), deps, plan, secrets, 1, true)
	if err == nil || prog.LockdownDone {
		t.Fatalf("lockdown must fail: %v %+v", err, prog)
	}
	if prog.FailedStep != "lockdown-confirm" || !s.revertArmed {
		t.Fatalf("step=%s armed=%v", prog.FailedStep, s.revertArmed)
	}
	if len(mgr.stored) != 1 {
		t.Fatal("dedicated key must be stored before root login is disabled")
	}
	s.fireRevert() // the dead-man switch on the host
	if !s.rootLogin {
		t.Fatal("the operator is locked out")
	}
	// Root credentials still work afterwards.
	if _, err := BootstrapInspect(context.Background(), s, bootHost, s.rootCred(), p); err != nil {
		t.Fatal(err)
	}
}

// breakAfterDisable makes the dedicated key fail once root login is disabled.
type breakAfterDisable struct{ *simHost }

func (b *breakAfterDisable) Run(ctx context.Context, h Host, c Credential, cmd, stdin string) (string, error) {
	out, err := b.simHost.Run(ctx, h, c, cmd, stdin)
	if err == nil && strings.Contains(cmd, "lockdown-disable-root run") {
		b.mu.Lock()
		b.dedBroken = true
		b.mu.Unlock()
	}
	return out, err
}

func TestDedicatedKeyStoreFailureStopsBeforeRootIsDisabled(t *testing.T) {
	s := newSimHost("debian", "12")
	p := fullProfile()
	p.NativeAgent, p.ContainerAgent, p.Patch, p.Restic, p.ResticPaths, p.Firewall = false, false, false, false, nil, false
	p.Cluster = ClusterChoice{Platform: "none"}
	plan, ded := planWithKey(t, s, p)
	deps, _, mgr := newDeps()
	deps.Runner = s
	mgr.storeErr = errors.New("sealed store unavailable")
	secrets := BootstrapSecrets{Root: s.rootCred(), Dedicated: ded}
	if _, err := ExecuteBootstrap(context.Background(), deps, plan, secrets, 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ExecuteBootstrap(context.Background(), deps, plan, secrets, 1, true); err == nil {
		t.Fatal("expected failure")
	}
	if s.rootDisabled || !s.rootLogin {
		t.Fatal("root login was disabled without a stored dedicated key")
	}
}
