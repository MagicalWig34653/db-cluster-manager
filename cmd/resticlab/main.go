// SPDX-License-Identifier: AGPL-3.0-only

// Command resticlab is the in-VM harness of lab experiment E11
// (tests/p0/e11-restic.sh, X14). It runs INSIDE a dbcm-lab-* VM as root and
// drives the real agent restic path (agent.Executor with restic.ExecRunner
// and internal/restic) against a real restic binary and real repositories:
// local, SFTP, rest-server and S3 (MinIO). It is a lab harness, not a product
// binary, and refuses to run outside a host whose name starts with
// "lima-dbcm-lab-".
//
// Configuration comes from environment variables so that no secret is in an
// argv: E11_SFTP_REPO, E11_REST_REPO (+ E11_REST_USER/E11_REST_PASS),
// E11_S3_REPO (+ E11_S3_KEY/E11_S3_SECRET) enable the remote backends; a
// private CA is given through SSL_CERT_FILE.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

const (
	allowRoot  = "/srv/e11"
	workDir    = "/var/lib/e11"
	secretsDir = "/var/lib/e11/secrets"
	canaryName = "E11_CANARY_SECRET"
)

var (
	failed   []string
	verified []string
	notRun   []string

	secretValues []string // every synthetic secret; must never leak
	canary       = "canary-" + randHex(8)
)

func say(f string, a ...any) {
	fmt.Printf("%s "+f+"\n", append([]any{time.Now().UTC().Format("15:04:05Z")}, a...)...)
}
func section(s string) { fmt.Printf("\n## %s\n\n", s) }
func ok(what string)   { verified = append(verified, what); say("VERIFIED: %s", what) }
func check(cond bool, what string, detail ...any) bool {
	if cond {
		ok(what)
		return true
	}
	failed = append(failed, what)
	say("FAILED: %s %v", what, detail)
	return false
}
func unverified(what string) { notRun = append(notRun, what); say("NOT VERIFIED: %s", what) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newSecret(prefix string) string {
	v := prefix + randHex(12)
	secretValues = append(secretValues, v)
	return v
}

// leak reports every synthetic secret value found in data.
func leak(data string) []string {
	var found []string
	for _, s := range secretValues {
		if s != "" && strings.Contains(data, s) {
			found = append(found, "<redacted:"+s[:4]+">")
		}
	}
	return found
}

// ---------------------------------------------------------------- observer

// observer polls /proc for restic children while the test runs. For every
// child it checks argv (no secret value), environment (the unrelated canary
// must not be inherited; the password must be passed as a file, never as
// RESTIC_PASSWORD) and records what it saw.
type observer struct {
	mu          sync.Mutex
	seen        map[int]bool
	argvLeaks   []string
	envCanary   int
	envPassword int
	envPwFile   int
	sample      string
	stop        chan struct{}
	done        chan struct{}
}

func startObserver() *observer {
	o := &observer{seen: map[int]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(o.done)
		for {
			select {
			case <-o.stop:
				return
			default:
			}
			o.scan()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return o
}

func (o *observer) scan() {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		var pid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid == os.Getpid() {
			continue
		}
		cmd, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(cmd) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")
		// Every process of the VM is checked for secret values in argv, not
		// only restic: a helper (ssh) must not receive them either.
		if l := leak(string(cmd)); len(l) > 0 {
			o.mu.Lock()
			o.argvLeaks = append(o.argvLeaks, fmt.Sprintf("pid %d %s: %v", pid, filepath.Base(args[0]), l))
			o.mu.Unlock()
		}
		if filepath.Base(args[0]) != "restic" {
			continue
		}
		env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		o.mu.Lock()
		if !o.seen[pid] {
			o.seen[pid] = true
			if o.sample == "" {
				o.sample = strings.Join(args, " ")
			}
		}
		for _, kv := range strings.Split(string(env), "\x00") {
			switch {
			case strings.HasPrefix(kv, canaryName+"="):
				o.envCanary++
			case strings.HasPrefix(kv, "RESTIC_PASSWORD="):
				o.envPassword++
			case strings.HasPrefix(kv, "RESTIC_PASSWORD_FILE="):
				o.envPwFile++
			}
		}
		o.mu.Unlock()
	}
}

func (o *observer) finish() {
	close(o.stop)
	<-o.done
}

// ---------------------------------------------------------------- runner

// countingRunner wraps the production ExecRunner, counting child starts and
// recording argv and the environment variable NAMES (never values).
type countingRunner struct {
	inner restic.ExecRunner
	calls atomic.Int64
	mu    sync.Mutex
	argvs [][]string
	names map[string]bool
}

func (c *countingRunner) Run(ctx context.Context, r restic.Request) (restic.Result, error) {
	c.calls.Add(1)
	c.mu.Lock()
	c.argvs = append(c.argvs, append([]string(nil), r.Args...))
	for k := range r.Env {
		c.names[k] = true
	}
	c.mu.Unlock()
	return c.inner.Run(ctx, r)
}

// ---------------------------------------------------------------- harness

type nopDocker struct{ agent.DockerAPI }

type harness struct {
	exec   *agent.Executor
	runner *countingRunner
	term   uint64
	step   int
	logs   strings.Builder // everything an operator or the manager would see
}

func (h *harness) do(kind agent.Kind, a agent.ResticArgs) agent.Result {
	h.step++
	args, _ := json.Marshal(a)
	r, err := h.exec.Handle(context.Background(), agent.Command{Term: h.term, OpID: "e11", StepID: fmt.Sprintf("s%03d", h.step), Kind: kind, Args: args})
	return h.record(kind, r, err)
}

func (h *harness) doCtx(ctx context.Context, kind agent.Kind, a agent.ResticArgs) agent.Result {
	h.step++
	args, _ := json.Marshal(a)
	r, err := h.exec.Handle(ctx, agent.Command{Term: h.term, OpID: "e11", StepID: fmt.Sprintf("s%03d", h.step), Kind: kind, Args: args})
	return h.record(kind, r, err)
}

func (h *harness) record(kind agent.Kind, r agent.Result, err error) agent.Result {
	if err != nil {
		say("   %s: handler error: %v", kind, err)
		fmt.Fprintf(&h.logs, "%v\n", err)
		return agent.Result{Status: agent.StatusFailed, Message: err.Error()}
	}
	out, _ := json.Marshal(r)
	fmt.Fprintf(&h.logs, "%s\n", out)
	msg := r.Message
	if len(msg) > 160 {
		msg = msg[:160] + "..."
	}
	say("   %s -> %s %s %q", kind, r.Status, r.Code, msg)
	return r
}

// raw runs the real restic CLI directly (lab verification and the steps the
// product does not automate: init, unlock, list locks).
func raw(env map[string]string, args ...string) (string, error) {
	cmd := exec.Command("restic", args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, canaryName+"=") { // operator tool calls must not carry the canary either
			cmd.Env = append(cmd.Env, kv)
		}
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

type backend struct {
	name     string
	kind     restic.Backend
	repo     string
	pwSecret string
	credName string
	creds    map[string]string
}

func (b backend) env() map[string]string {
	e := map[string]string{"RESTIC_PASSWORD_FILE": filepath.Join(secretsDir, b.pwSecret)}
	for k, v := range b.creds {
		e[k] = v
	}
	return e
}

func (b backend) args() agent.ResticArgs {
	return agent.ResticArgs{Backend: b.kind, Repository: b.repo, PasswordSecret: b.pwSecret, CredentialSecret: b.credName}
}

func writeSecret(name, content string) {
	if err := os.WriteFile(filepath.Join(secretsDir, name), []byte(content), 0o600); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	say("FATAL: %v", err)
	summary()
	os.Exit(2)
}

func main() {
	host, _ := os.Hostname()
	if !strings.HasPrefix(host, "lima-dbcm-lab-") {
		fmt.Fprintln(os.Stderr, "resticlab: refusing to run outside a dbcm-lab VM (hostname", host, ")")
		os.Exit(2)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "resticlab: run as root (the agent runs as root)")
		os.Exit(2)
	}
	_ = os.Setenv(canaryName, canary)
	for _, d := range []string{allowRoot, workDir, secretsDir, workDir + "/volumes", workDir + "/state", "/var/lib/e11-repos"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			fatal(err)
		}
	}
	if out, err := raw(nil, "version"); err != nil {
		fatal(fmt.Errorf("restic missing: %v %s", err, out))
	} else {
		say("%s", strings.TrimSpace(out))
	}
	obs := startObserver()
	g, err := fencing.NewGuard(fencing.NewFilePersister(workDir + "/state/term"))
	if err != nil {
		fatal(err)
	}
	cr := &countingRunner{names: map[string]bool{}}
	ex, err := agent.NewExecutor(agent.Config{InstallationID: "e11", StatePath: workDir + "/state/agent-state.json",
		VolumeBase: workDir + "/volumes", Guard: g, Docker: nopDocker{}, SecretsDir: secretsDir,
		ResticAllowedPaths: []string{allowRoot}, ResticRunner: cr})
	if err != nil {
		fatal(err)
	}
	h := &harness{exec: ex, runner: cr, term: 1}

	section("Source data")
	makeSource()

	section("Backends")
	var backends []backend
	localPW := newSecret("pw-local-")
	writeSecret("restic-local", localPW)
	backends = append(backends, backend{name: "local", kind: restic.BackendLocal, repo: "/var/lib/e11-repos/local", pwSecret: "restic-local"})
	if r := os.Getenv("E11_SFTP_REPO"); r != "" {
		writeSecret("restic-sftp", newSecret("pw-sftp-"))
		backends = append(backends, backend{name: "sftp", kind: restic.BackendSFTP, repo: r, pwSecret: "restic-sftp"})
	} else {
		unverified("sftp backend (E11_SFTP_REPO unset)")
	}
	if r := os.Getenv("E11_REST_REPO"); r != "" {
		writeSecret("restic-rest", newSecret("pw-rest-"))
		cred := map[string]string{"RESTIC_REST_USERNAME": os.Getenv("E11_REST_USER"), "RESTIC_REST_PASSWORD": os.Getenv("E11_REST_PASS")}
		secretValues = append(secretValues, cred["RESTIC_REST_PASSWORD"])
		b, _ := json.Marshal(cred)
		writeSecret("restic-rest-cred", string(b))
		backends = append(backends, backend{name: "rest-server", kind: restic.BackendREST, repo: r, pwSecret: "restic-rest", credName: "restic-rest-cred", creds: cred})
	} else {
		unverified("rest-server backend (E11_REST_REPO unset)")
	}
	if r := os.Getenv("E11_S3_REPO"); r != "" {
		writeSecret("restic-s3", newSecret("pw-s3-"))
		cred := map[string]string{"AWS_ACCESS_KEY_ID": os.Getenv("E11_S3_KEY"), "AWS_SECRET_ACCESS_KEY": os.Getenv("E11_S3_SECRET"), "AWS_DEFAULT_REGION": "us-east-1"}
		secretValues = append(secretValues, cred["AWS_SECRET_ACCESS_KEY"])
		b, _ := json.Marshal(cred)
		writeSecret("restic-s3-cred", string(b))
		backends = append(backends, backend{name: "s3", kind: restic.BackendS3, repo: r, pwSecret: "restic-s3", credName: "restic-s3-cred", creds: cred})
	} else {
		unverified("s3 backend (E11_S3_REPO unset)")
	}
	say("synthetic secrets generated: %d values (never printed)", len(secretValues))

	for _, b := range backends {
		runBackend(h, b)
	}
	lb := backends[0]
	section("Wrong password")
	wrongPassword(h, lb)
	section("Interrupted backup, stale lock")
	interrupted(h, lb)
	section("Allow-list refusals")
	allowList(h, lb)
	if len(backends) > 1 {
		section("Backend credential errors")
		badCredentials(h, backends[1:])
	}

	obs.finish()
	section("Secret hygiene")
	hygiene(h, cr, obs)
	summary()
	if len(failed) > 0 {
		os.Exit(1)
	}
}

func summary() {
	section("Summary")
	say("verified checks: %d", len(verified))
	for _, f := range failed {
		say("FAILED: %s", f)
	}
	for _, n := range notRun {
		say("NOT VERIFIED: %s", n)
	}
}

// ---------------------------------------------------------------- data

func writeRandom(path string, n int) {
	f, err := os.Create(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	if _, err := io.CopyN(f, rand.Reader, int64(n)); err != nil {
		fatal(err)
	}
}

func makeSource() {
	src := allowRoot + "/src"
	for _, d := range []string{src + "/docs", src + "/secrets", src + "/home/.ssh", src + "/cache", src + "/big"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		writeRandom(fmt.Sprintf("%s/docs/file%02d.bin", src, i), 1<<20+i*4096)
	}
	_ = os.WriteFile(src+"/docs/readme.txt", []byte("e11 restic lab source\n"), 0o644)
	// Files the agent must keep out of a backup by its built-in excludes.
	_ = os.WriteFile(src+"/secrets/token.txt", []byte("excluded-by-default-secret-dir"), 0o600)
	_ = os.WriteFile(src+"/home/.ssh/id_test", []byte("excluded-by-default-ssh-dir"), 0o600)
	_ = os.WriteFile(src+"/home/keep.txt", []byte("kept"), 0o644)
	_ = os.WriteFile(src+"/cache/skip.bin", []byte("excluded-by-request"), 0o644)
	_ = os.Symlink("/etc", src+"/docs/link-to-etc") // a symlink in the data is stored as a link
	writeRandom(src+"/big/big.bin", 1<<30)
	say("source tree created below %s (12 x ~1 MiB, 1 GiB big file, excluded dirs, symlink)", src)
}

func hashTree(root string, skip func(rel string) bool) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			out[rel] = "link:" + t
		case d.Type().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			h := sha256.New()
			_, _ = io.Copy(h, f)
			f.Close()
			out[rel] = hex.EncodeToString(h.Sum(nil))
		}
		return nil
	})
	return out
}

func sameTree(a, b map[string]string) (bool, string) {
	if len(a) != len(b) {
		return false, fmt.Sprintf("entry count %d vs %d", len(a), len(b))
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if a[k] != b[k] {
			return false, "differs at " + k
		}
	}
	return true, ""
}

// ---------------------------------------------------------------- per backend

var snapID = regexp.MustCompile(`"snapshot_id":"([0-9a-f]+)"`)

// srcSkip returns the skip function for the tree below allowRoot/src/<top>:
// entries the agent excludes by default or by request.
func srcSkip(top string) func(string) bool {
	return func(rel string) bool {
		r := top + "/" + rel
		return r == "docs/cache" || r == "home/.ssh" || strings.HasPrefix(r, "home/.ssh/")
	}
}

func runBackend(h *harness, b backend) {
	section("Backend " + b.name)
	say("repository %s", b.repo)
	// Initialization is not automated by the product (guide); do it as the operator would.
	out, err := raw(b.env(), "-r", b.repo, "init")
	if !check(err == nil, b.name+": restic init (operator step, not automated)", out) {
		return
	}
	src := allowRoot + "/src"
	paths := []string{src + "/docs", src + "/home"}
	a := b.args()
	a.Paths, a.Excludes = paths, []string{src + "/cache"}
	r := h.do(agent.KindResticBackup, a)
	if !check(r.Status == agent.StatusSucceeded, b.name+": backup #1 through the agent executor", r.Message) {
		return
	}
	m := snapID.FindStringSubmatch(string(r.Output))
	check(m != nil, b.name+": backup result carries a snapshot id")

	// Change the source and take a second snapshot.
	writeRandom(src+"/docs/added.bin", 2<<20)
	_ = os.WriteFile(src+"/docs/readme.txt", []byte("e11 restic lab source, changed\n"), 0o644)
	r = h.do(agent.KindResticBackup, a)
	check(r.Status == agent.StatusSucceeded, b.name+": backup #2 (incremental) succeeds", r.Message)

	lsOut, _ := raw(b.env(), "-r", b.repo, "snapshots", "--json")
	var snaps []restic.Snapshot
	_ = json.Unmarshal([]byte(lsOut), &snaps)
	check(len(snaps) == 2, b.name+": raw restic snapshots shows 2 snapshots", len(snaps))

	r = h.do(agent.KindResticSnapshots, b.args())
	var viaAgent []restic.Snapshot
	_ = json.Unmarshal(r.Output, &viaAgent)
	check(r.Status == agent.StatusSucceeded && len(viaAgent) == 2, b.name+": snapshots through the agent lists 2", r.Message, len(viaAgent))

	// Excludes: neither the built-in ones (.ssh, secrets) nor the requested one is stored.
	latest := viaAgent[len(viaAgent)-1].ID
	lsAll, _ := raw(b.env(), "-r", b.repo, "ls", latest)
	check(strings.Contains(lsAll, "keep.txt") && !strings.Contains(lsAll, "id_test") && !strings.Contains(lsAll, "skip.bin"),
		b.name+": built-in (.ssh) and requested exclusions are not in the snapshot")

	// Restore the latest snapshot to a NEW path with approval.
	target := allowRoot + "/restore-" + b.name
	ra := b.args()
	ra.Snapshot, ra.Target = latest, target
	r = h.do(agent.KindResticRestore, ra)
	check(r.Status == agent.StatusRefused, b.name+": restore without approval is refused", r.Status, r.Code)
	if _, err := os.Stat(target); err == nil {
		check(false, b.name+": refused restore created the target")
	}
	ra.Approved = true
	r = h.do(agent.KindResticRestore, ra)
	if check(r.Status == agent.StatusSucceeded, b.name+": approved restore into a new path succeeds", r.Message) {
		for _, d := range []string{"docs", "home"} {
			want := hashTree(src+"/"+d, srcSkip(d))
			got := hashTree(target+src+"/"+d, srcSkip(d))
			same, why := sameTree(want, got)
			check(same, b.name+": restored "+d+" is byte-identical to the source (sha256, links)", why)
		}
	}
	r = h.do(agent.KindResticRestore, ra)
	check(r.Status == agent.StatusRefused, b.name+": restore onto the now existing target is refused", r.Status, r.Code)

	// Retention with prune.
	fa := b.args()
	fa.Retention = restic.Retention{KeepLast: 1, Prune: true}
	r = h.do(agent.KindResticForget, fa)
	check(r.Status == agent.StatusSucceeded, b.name+": forget --keep-last 1 --prune succeeds", r.Message)
	lsOut, _ = raw(b.env(), "-r", b.repo, "snapshots", "--json")
	snaps = nil
	_ = json.Unmarshal([]byte(lsOut), &snaps)
	check(len(snaps) == 1, b.name+": one snapshot remains after forget --prune", len(snaps))

	ca := b.args()
	ca.ReadDataSubset = "25%"
	r = h.do(agent.KindResticCheck, ca)
	check(r.Status == agent.StatusSucceeded, b.name+": check --read-data-subset 25% succeeds", r.Message)
	ca.ReadDataSubset = "100%"
	r = h.do(agent.KindResticCheck, ca)
	check(r.Status == agent.StatusSucceeded, b.name+": check --read-data-subset 100% succeeds", r.Message)

	r = h.do(agent.KindResticStats, b.args())
	var st restic.StatsResult
	_ = json.Unmarshal(r.Output, &st)
	check(r.Status == agent.StatusSucceeded && st.TotalFileCount > 0, b.name+": stats reports files", r.Message, st)

	// A restore of the remaining snapshot must still equal the source (prune did not eat data).
	snap := snaps[0].ID
	target2 := allowRoot + "/restore2-" + b.name
	ra.Snapshot, ra.Target = snap, target2
	r = h.do(agent.KindResticRestore, ra)
	same, why := sameTree(hashTree(src+"/docs", srcSkip("docs")), hashTree(target2+src+"/docs", srcSkip("docs")))
	check(r.Status == agent.StatusSucceeded && same, b.name+": restore after prune is identical to the source", r.Message, why)
}

// ---------------------------------------------------------------- negative cases

func wrongPassword(h *harness, b backend) {
	writeSecret("restic-wrong", newSecret("pw-wrong-"))
	a := b.args()
	a.PasswordSecret = "restic-wrong"
	a.Paths = []string{allowRoot + "/src/docs"}
	r := h.do(agent.KindResticBackup, a)
	check(r.Status == agent.StatusFailed, "wrong password: backup fails", r.Status)
	check(strings.Contains(r.Message, "authentication"), "wrong password: classified as authentication failure", r.Message)
	r = h.do(agent.KindResticSnapshots, a)
	check(r.Status == agent.StatusFailed && strings.Contains(r.Message, "authentication"), "wrong password: snapshots classified as authentication failure", r.Message)
	// A missing password secret is refused before any restic start.
	n := h.runner.calls.Load()
	a.PasswordSecret = "restic-does-not-exist"
	r = h.do(agent.KindResticSnapshots, a)
	check(r.Status == agent.StatusRefused && h.runner.calls.Load() == n, "missing password secret: refused without starting restic", r.Status)
}

func countLocks(b backend) int {
	out, err := raw(b.env(), "-r", b.repo, "list", "locks", "--no-lock")
	if err != nil {
		return -1
	}
	return len(strings.Fields(out))
}

func interrupted(h *harness, b backend) {
	// Take a clean baseline snapshot of the big data in a new repo area: use
	// the local repo but a path the other snapshots do not contain.
	before, _ := raw(b.env(), "-r", b.repo, "snapshots", "--json")
	var beforeSnaps []restic.Snapshot
	_ = json.Unmarshal([]byte(before), &beforeSnaps)
	check(countLocks(b) == 0, "interrupted: no lock before the test", countLocks(b))

	a := b.args()
	a.Paths = []string{allowRoot + "/src/big"}
	// Kill the backup 1.5 s after its restic child appeared (not a fixed
	// timeout: a fast disk finished a 400 MiB backup before 2.5 s).
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if resticBackupRunning(allowRoot + "/src/big") {
				time.Sleep(1500 * time.Millisecond)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	r := h.doCtx(ctx, agent.KindResticBackup, a)
	cancel()
	check(r.Status == agent.StatusFailed, "interrupted: backup killed mid-run reports failure, not success", r.Status, r.Message)
	time.Sleep(500 * time.Millisecond)
	mid, _ := raw(b.env(), "-r", b.repo, "snapshots", "--json")
	var midSnaps []restic.Snapshot
	_ = json.Unmarshal([]byte(mid), &midSnaps)
	check(len(midSnaps) == len(beforeSnaps), "interrupted: the killed backup created no snapshot", len(midSnaps), len(beforeSnaps))
	locks := countLocks(b)
	check(locks >= 1, "stale lock: the killed restic left a lock in the repository", locks)

	// The exclusive operations must report the lock, and the wrapper must not unlock it.
	fa := b.args()
	fa.Retention = restic.Retention{KeepLast: 5, Prune: true}
	r = h.do(agent.KindResticForget, fa)
	check(r.Status == agent.StatusFailed && strings.Contains(r.Message, "locked"), "stale lock: forget --prune reports the repository as locked", r.Status, r.Message)
	check(countLocks(b) >= 1, "stale lock: the wrapper did not remove the lock")
	for _, rec := range h.runner.argvs {
		for _, arg := range rec {
			if arg == "unlock" {
				check(false, "stale lock: the wrapper never runs unlock")
			}
		}
	}
	ok("stale lock: the wrapper never runs unlock (argv audit of all child starts)")

	// Operator reconciles the stale lock (owner process is dead); the retried backup completes.
	if out, err := raw(b.env(), "-r", b.repo, "unlock"); !check(err == nil, "stale lock: operator unlock after confirming the owner is dead", out) {
		return
	}
	r = h.do(agent.KindResticBackup, a)
	check(r.Status == agent.StatusSucceeded, "interrupted: the retried backup completes", r.Message)
	ca := b.args()
	ca.ReadDataSubset = "100%"
	r = h.do(agent.KindResticCheck, ca)
	check(r.Status == agent.StatusSucceeded, "interrupted: repository check passes after interrupted+retried backup", r.Message)
	// Restore the big snapshot to a new path and compare.
	snapsOut, _ := raw(b.env(), "-r", b.repo, "snapshots", "--json", "--path", allowRoot+"/src/big")
	var bs []restic.Snapshot
	_ = json.Unmarshal([]byte(snapsOut), &bs)
	if check(len(bs) == 1, "interrupted: exactly one snapshot of the big path", len(bs)) {
		ra := b.args()
		ra.Snapshot, ra.Target, ra.Approved = bs[0].ID, allowRoot+"/restore-big", true
		r = h.do(agent.KindResticRestore, ra)
		want := hashTree(allowRoot+"/src/big", nil)
		got := hashTree(allowRoot+"/restore-big"+allowRoot+"/src/big", nil)
		same, why := sameTree(want, got)
		check(r.Status == agent.StatusSucceeded && same, "interrupted: restored 1 GiB file is byte-identical", r.Message, why)
	}
}

// resticBackupRunning reports whether a restic backup child for path runs.
func resticBackupRunning(path string) bool {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		cmd, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")
		if len(args) > 3 && filepath.Base(args[0]) == "restic" && strings.Contains(string(cmd), "backup") && args[len(args)-1] == path {
			return true
		}
	}
	return false
}

func allowList(h *harness, b backend) {
	if err := os.Symlink("/etc", allowRoot+"/src/escape"); err != nil {
		fatal(err)
	}
	_ = os.MkdirAll("/srv/e11x", 0o755) // sibling sharing the allow-root prefix
	_ = os.MkdirAll("/srv/outside", 0o755)
	cases := map[string]string{
		"/etc":                        "system directory",
		"/srv":                        "parent of the allowed root",
		"/srv/e11x":                   "sibling with the root as string prefix",
		"/srv/e11/../outside":         "unclean path with ..",
		"/srv/e11/src/escape":         "symlink inside the root pointing to /etc",
		"/srv/e11/src/escape/passwd":  "path through such a symlink",
		secretsDir:                    "the agent's own secret directory",
		"/run/secrets":                "swarm secret mount",
		"/var/run/docker.sock":        "docker socket",
		"/":                           "filesystem root",
		"relative/path":               "relative path",
		allowRoot + "/does-not-exist": "missing path",
	}
	n := h.runner.calls.Load()
	for p, why := range cases {
		a := b.args()
		a.Paths = []string{p}
		r := h.do(agent.KindResticBackup, a)
		check(r.Status == agent.StatusRefused && r.Code == agent.CodeInvalidPath, "allow-list: backup of "+p+" refused ("+why+")", r.Status, r.Code)
	}
	a := b.args()
	a.Paths = []string{allowRoot + "/src/docs"}
	a.Excludes = []string{"/etc/passwd"}
	r := h.do(agent.KindResticBackup, a)
	check(r.Status == agent.StatusRefused, "allow-list: exclusion outside the root refused", r.Status)
	check(h.runner.calls.Load() == n, "allow-list: no refused request started a restic child", h.runner.calls.Load()-n)

	// Restore targets.
	snapsOut, _ := raw(b.env(), "-r", b.repo, "snapshots", "--json")
	var s []restic.Snapshot
	_ = json.Unmarshal([]byte(snapsOut), &s)
	if len(s) == 0 {
		check(false, "allow-list: no snapshot available for restore-target tests")
		return
	}
	n = h.runner.calls.Load()
	for _, t := range []string{"/srv/outside/new", "/tmp/e11-new", "/srv/e11/../outside/new2", "/srv/e11/src/escape/new", allowRoot, allowRoot + "/src/docs", secretsDir + "/new"} {
		ra := b.args()
		ra.Snapshot, ra.Target, ra.Approved = s[0].ID, t, true
		r := h.do(agent.KindResticRestore, ra)
		check(r.Status == agent.StatusRefused, "allow-list: restore target "+t+" refused", r.Status, r.Code)
		if _, err := os.Lstat(t + "/etc"); err == nil && t != allowRoot {
			check(false, "allow-list: restore wrote below "+t)
		}
	}
	check(h.runner.calls.Load() == n, "allow-list: no refused restore started a restic child")
	if _, err := os.Stat("/srv/outside/new"); err == nil {
		check(false, "allow-list: refused restore created /srv/outside/new")
	}
	// A hostile snapshot argument must not reach restic.
	for _, snap := range []string{"--help", "abc;touch /tmp/pwned", "../../x", ""} {
		ra := b.args()
		ra.Snapshot, ra.Target, ra.Approved = snap, allowRoot+"/restore-bad", true
		r := h.do(agent.KindResticRestore, ra)
		check(r.Status != agent.StatusSucceeded, fmt.Sprintf("allow-list: snapshot argument %q rejected", snap), r.Status)
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		check(false, "snapshot argument injection executed a command")
	}
}

func badCredentials(h *harness, remote []backend) {
	for _, b := range remote {
		if b.credName == "" {
			continue
		}
		bad := map[string]string{}
		for k, v := range b.creds {
			bad[k] = v
			if strings.Contains(k, "SECRET") || strings.Contains(k, "PASSWORD") {
				bad[k] = newSecret("wrongcred-")
			}
		}
		j, _ := json.Marshal(bad)
		writeSecret(b.credName+"-bad", string(j))
		a := b.args()
		a.CredentialSecret = b.credName + "-bad"
		r := h.do(agent.KindResticSnapshots, a)
		check(r.Status == agent.StatusFailed, b.name+": wrong backend credentials make snapshots fail", r.Status, r.Message)
		say("   classification for wrong %s credentials: %q", b.name, r.Message)
		check(len(leak(r.Message)) == 0, b.name+": the failure message carries no secret value")
	}
}

// ---------------------------------------------------------------- hygiene

func hygiene(h *harness, cr *countingRunner, o *observer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	say("observed %d distinct restic child processes via /proc; sample argv: %s", len(o.seen), o.sample)
	check(len(o.seen) > 5, "hygiene: restic children were observed in /proc", len(o.seen))
	check(len(o.argvLeaks) == 0, "hygiene: no secret value in the argv of ANY process during the run", o.argvLeaks)
	check(o.envCanary == 0, "hygiene: unrelated canary variable of the agent process is not inherited by restic", o.envCanary)
	check(o.envPassword == 0, "hygiene: password never passed as RESTIC_PASSWORD", o.envPassword)
	check(o.envPwFile > 0, "hygiene: password passed as RESTIC_PASSWORD_FILE", o.envPwFile)
	bad := 0
	for _, argv := range cr.argvs {
		if len(leak(strings.Join(argv, " "))) > 0 {
			bad++
		}
	}
	check(bad == 0, "hygiene: no secret in the argv recorded for all child starts", bad)
	cr.mu.Lock()
	var names []string
	for k := range cr.names {
		names = append(names, k)
	}
	cr.mu.Unlock()
	sort.Strings(names)
	say("environment variable NAMES handed to restic children: %s", strings.Join(names, ","))
	check(len(leak(h.logs.String())) == 0, "hygiene: no secret value in any result, message or error the manager would receive")
	// Persisted state and everything below the work dir except the secrets directory.
	found := 0
	_ = filepath.WalkDir(workDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || strings.HasPrefix(p, secretsDir) {
			return nil
		}
		b, _ := os.ReadFile(p)
		if len(leak(string(b))) > 0 {
			found++
			say("   secret value found in %s", p)
		}
		return nil
	})
	check(found == 0, "hygiene: no secret value in the agent state or work directory outside the secret files", found)
	// Secret files keep mode 0600 and are not part of any snapshot.
	st, _ := os.Stat(filepath.Join(secretsDir, "restic-local"))
	check(st != nil && st.Mode().Perm() == 0o600, "hygiene: password file mode is 0600")
	b := backend{kind: restic.BackendLocal, repo: "/var/lib/e11-repos/local", pwSecret: "restic-local"}
	all, _ := raw(b.env(), "-r", b.repo, "ls", "latest", "--recursive")
	check(!strings.Contains(all, "restic-local") && !strings.Contains(all, "e11/secrets"), "hygiene: no secret file path in any local snapshot listing")
}
