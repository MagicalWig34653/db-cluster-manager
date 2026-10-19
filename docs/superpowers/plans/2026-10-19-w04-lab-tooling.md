# W04 Lab Tooling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go command `lab` that creates, inventories and removes dedicated Lima/Colima test VMs within the confirmed resource budget, touching only resources recorded in an ownership manifest.

**Architecture:** `internal/lab` holds pure logic (budget, manifest, planning, cleanup preview, unrelated-inventory diff) behind a `Runner` interface that executes external commands; tests use a fake Runner. `cmd/lab` is a thin CLI. Real VM creation is explicit (`apply`); everything else is read-only or dry-run by default.

**Tech Stack:** Go 1.27 standard library only; external tools `limactl` (v2.x) and `colima` invoked only through `Runner`.

## Global Constraints

- Budget from docs/TESTING.md, verbatim: combined project test workloads at most 16 GB RAM, at most 50% of host CPU capacity (detect actual core count; do not assume a chip's core count), approximately 100 GB SSD including VM disks, images and caches.
- Existing Colima profiles, Lima VMs, Docker contexts and unrelated workloads must remain untouched. No global prune, no deleting all instances, no host restart, no Docker upgrade.
- All lab VMs use the name prefix `dbcm-lab-`; the manifest (`.lab/manifest.json`, git-ignored) records every owned resource. Cleanup acts only on manifest entries whose name still starts with `dbcm-lab-`.
- Commands are dry-run unless `--apply` is given. Docker commands always use an explicit `--context`.
- Go files start with `// SPDX-License-Identifier: AGPL-3.0-only`; English identifiers/comments/docs; standard library only; TDD; no tests without assertions; `gofmt -l .`, `go vet ./...`, `go test -race ./...` must pass; commit messages without attribution lines.
- Never report unexecuted checks as passing. Tests must not call real `limactl`/`colima`/`docker`.

---

### Task 1: Budget and host capacity (`internal/lab/budget.go`)

**Files:** Create `internal/lab/budget.go`, `internal/lab/budget_test.go`

**Interfaces:**
- Produces:
```go
package lab

const (
	MaxRAMBytes  uint64 = 16 << 30
	MaxDiskBytes uint64 = 100 << 30
)

type Host struct {
	LogicalCPUs int    // detected core count
	RAMBytes    uint64 // physical memory
}

// MaxCPUs returns floor(LogicalCPUs/2), at least 1.
func (h Host) MaxCPUs() int

type VMSpec struct {
	Name      string
	CPUs      int
	RAMBytes  uint64
	DiskBytes uint64
}

type Budget struct{ Host Host }

// Check validates that the sum over specs fits MaxCPUs, MaxRAMBytes and MaxDiskBytes
// and that each spec has positive values. Error text names the exceeded resource and both numbers.
func (b Budget) Check(specs []VMSpec) error

// DetectHost returns Host using runtime.NumCPU() and, on darwin, `sysctl -n hw.memsize` through the Runner
// (Runner is defined in Task 3; until then accept an `func(name string, args ...string) (string, error)` parameter).
func DetectHost(run func(name string, args ...string) (string, error)) (Host, error)
```

- [ ] **Step 1:** Write failing tests: MaxCPUs for 14 → 7, 1 → 1, 2 → 1, 3 → 1; Check passes for 3 VMs of 2 CPU / 4 GiB / 20 GiB on a 14-CPU host; Check fails (error mentions "cpu", "ram" or "disk" respectively with both numbers) when each limit is exceeded by one unit; zero-value spec rejected; DetectHost parses a fake sysctl output "51539607552\n" and returns error on non-numeric output.
- [ ] **Step 2:** Run `go test ./internal/lab/` and confirm failure for the missing implementation.
- [ ] **Step 3:** Implement. **Step 4:** Run tests with `-race`, `go vet ./...`. **Step 5:** Commit.

---

### Task 2: Ownership manifest (`internal/lab/manifest.go`)

**Files:** Create `internal/lab/manifest.go`, `internal/lab/manifest_test.go`

**Interfaces:**
- Produces:
```go
package lab

const NamePrefix = "dbcm-lab-"

type Resource struct {
	Kind    string    `json:"kind"`    // "lima-vm", "colima-profile", "docker-context", "directory"
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Spec    *VMSpec   `json:"spec,omitempty"`
}

type Manifest struct {
	Version   int        `json:"version"` // 1
	Resources []Resource `json:"resources"`
}

// LoadManifest returns an empty Manifest when the file does not exist; an error for invalid JSON or unknown version.
func LoadManifest(path string) (*Manifest, error)
// Save writes atomically (temp file + rename), creating parent directories (0o700).
func (m *Manifest) Save(path string) error
// Add rejects names without NamePrefix (error) and duplicates of the same kind+name (error).
func (m *Manifest) Add(r Resource) error
func (m *Manifest) Remove(kind, name string)
func (m *Manifest) Find(kind, name string) (Resource, bool)
```

- [ ] **Step 1:** Failing tests: missing file → empty manifest; save/load round trip preserves resources including Spec; Add rejects `default` and `thermoctl-test-vm`; Add rejects duplicates; Remove; invalid JSON and `"version": 2` rejected; saved file mode is 0o600.
- [ ] **Step 2–5:** run failing, implement, pass with `-race`, commit.

---

### Task 3: Runner, planner and unrelated-inventory diff (`internal/lab/plan.go`, `internal/lab/runner.go`, `internal/lab/inventory.go`)

**Files:** Create `internal/lab/runner.go`, `internal/lab/plan.go`, `internal/lab/inventory.go` and matching `_test.go` files.

**Interfaces:**
- Consumes: `VMSpec`, `Budget`, `Manifest`, `Resource`, `NamePrefix` from Tasks 1–2.
- Produces:
```go
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}
func NewExecRunner() Runner // os/exec, captures stdout, includes stderr in the error

type Action struct {
	Describe string   // human readable, one line
	Command  []string // exact argv, e.g. {"limactl","start","--name=dbcm-lab-m1", ...}
	Resource *Resource // resource recorded in the manifest after success (nil for none)
}

// PlanCreate validates the specs against the budget (and that names have NamePrefix and are not
// already in the manifest) and returns one Action per VM: `limactl create` with --name, --cpus, --memory (GiB),
// --disk (GiB), --tty=false, template docker, followed by `limactl start --tty=false <name>`.
func PlanCreate(b Budget, m *Manifest, specs []VMSpec) ([]Action, error)

// PlanCleanup returns, for each manifest resource of kind "lima-vm", `limactl stop <name>` and
// `limactl delete --force <name>` (only names with NamePrefix) and removal of "directory" resources
// under the lab directory. It never emits a command for a name that does not start with NamePrefix.
func PlanCleanup(m *Manifest) []Action

// Execute runs actions in order, stops at the first error, and after each successful action updates
// the manifest (Add for create actions, Remove for delete actions) and calls save.
func Execute(ctx context.Context, r Runner, m *Manifest, actions []Action, save func() error) error

// Snapshot lists Lima instances and Colima profiles (`limactl list --json`, `colima list --json`)
// as a sorted []string of "kind:name:status-independent-config" entries ("lima:NAME", "colima:NAME").
func Snapshot(ctx context.Context, r Runner) ([]string, error)
// Unrelated returns snapshot entries that are not owned by the manifest.
func Unrelated(snapshot []string, m *Manifest) []string
// Diff reports entries added or removed between two snapshots.
func Diff(before, after []string) (added, removed []string)
```

- [ ] **Step 1:** Failing tests with a fake Runner recording calls: PlanCreate for 3 valid specs returns 6 actions with exact argv; PlanCreate rejects over-budget specs, bad prefix and duplicate names without any Runner call; PlanCleanup on a manifest containing both `dbcm-lab-m1` and (hand-inserted, bypassing Add) `default` emits commands only for `dbcm-lab-m1`; Execute stops at first error and leaves the manifest in sync with what succeeded; Snapshot parses sample `limactl list --json` (newline-delimited JSON objects with `name`) and `colima list --json` output; Unrelated excludes owned names; Diff added/removed.
- [ ] **Step 2–5:** run failing, implement, pass with `-race`, commit.

---

### Task 4: `lab` command (`cmd/lab/main.go`) and documentation

**Files:** Create `cmd/lab/main.go`, `cmd/lab/main_test.go`; Modify `Makefile` (build already covers `./cmd/...`), `.gitignore` (add `.lab/` if not present — it is), `docs/TESTING.md` (add a "Lab commands" section), `AGENTS.md` (Commands list).

**Interfaces:**
- Consumes: everything from Tasks 1–3.
- CLI: `lab status` (host capacity, budget, manifest, unrelated resources — read-only), `lab plan -vms N -cpus C -ram GiB -disk GiB` (prints the create actions; dry-run), `lab apply ...same flags` (executes after printing and recording a pre-snapshot, then prints the diff showing only owned additions), `lab cleanup` (preview only) and `lab cleanup --apply` (executes PlanCleanup, then compares a post-snapshot to the pre-existing unrelated list and fails loudly if any unrelated entry disappeared). Manifest path default `.lab/manifest.json`, flag `-manifest`. VM names are `dbcm-lab-m1`.. `dbcm-lab-mN`.
- Structure `main` as `run(args []string, stdout, stderr io.Writer, r lab.Runner) int` so tests call it with a fake Runner.

- [ ] **Step 1:** Failing tests via `run`: `plan` with defaults (3 VMs, 2 CPU, 4 GiB RAM, 20 GiB disk) prints 6 command lines and exits 0 without Runner calls other than host detection; `plan -vms 3 -ram 8` (24 GiB total) exits non-zero with message containing "ram"; `apply` without prior plan flags still dry-runs unless `--apply` is present (check no `limactl create` call is made); `cleanup` without `--apply` makes no mutating call; `cleanup --apply` removes only `dbcm-lab-` entries and returns non-zero when the post-snapshot lost an unrelated entry (fake Runner returns a shrunken list).
- [ ] **Step 2–4:** run failing, implement, pass with `-race`; run `make check` equivalent (`gofmt -l .`, `go vet ./...`) and `go test -race ./...`.
- [ ] **Step 5:** Document the commands in docs/TESTING.md and AGENTS.md exactly as implemented, stating that no VM has been created by the tests. Run `python3 scripts/check_docs.py`. Commit.
- [ ] **Step 6 (read-only smoke, no mutation):** build and run `./bin/lab status` and `./bin/lab plan` on the real host; paste output into the report. Do NOT run `apply` or `cleanup --apply`.
