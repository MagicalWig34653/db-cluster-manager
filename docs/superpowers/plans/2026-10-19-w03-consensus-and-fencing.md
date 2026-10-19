# W03 Consensus and Fencing Prototype Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce reproducible, test-backed evidence for ADR 0003 (replicated manager state, leader-only operations, term-based fencing) without VMs or Docker.

**Architecture:** Small pure-Go packages with clear interfaces: `internal/fencing` (agent-side term guard), `internal/opjournal` (operation journal with ownership by term), `internal/metastore` (replicated-state wrapper over the library chosen in Task 1), and scenario tests combining them with injected network partitions.

**Tech Stack:** Go 1.27 (module `github.com/MagicalWig34653/db-cluster-manager`), standard library `testing`, one Raft library chosen in Task 1.

## Global Constraints

- Code, identifiers, comments and technical docs in English. Every new Go file starts with `// SPDX-License-Identifier: AGPL-3.0-only`.
- License is AGPL-3.0-only: any new dependency needs a license that is compatible; record the license of each in the Task 1 report.
- No Docker, VMs, network access beyond loopback, or writes outside the repository and `t.TempDir()`. No global resource changes.
- Never report unexecuted checks as passing. Run `gofmt -l .`, `go vet ./...`, `go test ./...` and report real output.
- Do not build custom consensus. Three replicas alone are not proof of HA; evidence comes from the scenarios.
- Keep `docs/STATUS.md` and `docs/WORKPLAN.md` honest; only the controller edits them.
- Commit with a message describing the change. Do not add attribution lines to commit messages.
- Test hygiene: no test without an assertion; no sleeps longer than needed — poll with a deadline helper.

---

### Task 1: Evaluate consensus library candidates

**Files:**
- Create: `docs/decisions/0003-evidence-library-evaluation.md`

**Interfaces:**
- Produces: a recommendation of exactly one Go consensus module (import path + exact version) with the minimal API surface needed by Task 4: start a node with an in-memory or loopback transport, apply a log entry on the leader, read the current term, observe leadership changes, and shut a node down.

- [ ] **Step 1:** Using primary sources only (module repository, pkg.go.dev, release notes; use `go list -m -versions <module>` and `go doc`), evaluate: `github.com/hashicorp/raft` (with `raft-boltdb` or its in-memory stores), `go.etcd.io/raft/v3`, and one SQL-replication candidate of your choice if it is a Go module.
- [ ] **Step 2:** For each candidate record: latest stable version and release date, license and AGPL-3.0-only compatibility, direct dependency count (`go mod graph` in a scratch module under `$TMPDIR`), whether the leader term is exposed, whether an in-process test transport exists, and the persistence option.
- [ ] **Step 3:** In a scratch module under `$TMPDIR` (not in the repository), write a 40-line program per top candidate that starts a single node, becomes leader, applies one entry and prints the term. Run it and paste the real output.
- [ ] **Step 4:** Write the recommendation and the evidence into `docs/decisions/0003-evidence-library-evaluation.md` (English, with a comparison table). State explicitly what was not verified.
- [ ] **Step 5:** Run `python3 scripts/check_docs.py`; commit the document.

---

### Task 2: Agent-side term guard (`internal/fencing`)

**Files:**
- Create: `internal/fencing/fencing.go`, `internal/fencing/fencing_test.go`

**Interfaces:**
- Produces:
```go
package fencing

var ErrStaleTerm = errors.New("fencing: stale term")

// Persister stores the highest admitted term durably.
type Persister interface {
	Load() (uint64, error) // returns 0, nil when nothing is stored yet
	Save(term uint64) error
}

func NewFilePersister(path string) Persister // atomic write via temp file + rename + fsync

type Guard struct{ /* unexported */ }

func NewGuard(p Persister) (*Guard, error) // loads the stored term
// Admit accepts a command issued under term. It returns ErrStaleTerm if
// term < highest admitted term. If term > highest it persists the new value
// BEFORE returning nil. term == highest is admitted. term 0 is always rejected.
func (g *Guard) Admit(term uint64) error
func (g *Guard) Highest() uint64
```

- [ ] **Step 1:** Write failing tests: admits increasing terms; admits equal term; rejects lower term with `errors.Is(err, ErrStaleTerm)`; rejects term 0; persists the new term before returning (use a Persister fake that records order and can fail — when `Save` fails, `Admit` returns that error and `Highest()` is unchanged); a new `Guard` over the same file reloads the stored term (restart survives); concurrent `Admit` calls from 50 goroutines with random terms end with `Highest()` equal to the maximum term (run with `-race`).
- [ ] **Step 2:** Run `go test -race ./internal/fencing/` and confirm the failures are for the missing implementation.
- [ ] **Step 3:** Implement with a `sync.Mutex`; file format is the decimal term plus newline; corrupt file content makes `NewGuard` return an error (add a test).
- [ ] **Step 4:** Run `go test -race ./internal/fencing/` and `go vet ./...`; all pass.
- [ ] **Step 5:** Commit.

---

### Task 3: Operation journal (`internal/opjournal`)

**Files:**
- Create: `internal/opjournal/journal.go`, `internal/opjournal/journal_test.go`

**Interfaces:**
- Consumes: nothing from other tasks (pure in-memory state machine; Task 4 replicates its commands).
- Produces:
```go
package opjournal

type State string
const (
	Pending   State = "pending"
	Running   State = "running"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Unknown   State = "unknown" // completion could not be observed
	Aborted   State = "aborted"
)

var ErrNotOwner = errors.New("opjournal: term does not own operation")
var ErrExists = errors.New("opjournal: operation exists")
var ErrNotFound = errors.New("opjournal: operation not found")

type Step struct {
	ID       string
	State    State
	Attempts int
}
type Operation struct {
	ID    string
	Kind  string
	Owner uint64 // term of the controller that currently owns the operation
	Steps []Step
}

type Journal struct{ /* unexported */ }

func New() *Journal
func (j *Journal) Begin(id, kind string, stepIDs []string, term uint64) error // ErrExists if id used
// StartStep moves a Pending/Unknown/Failed step to Running and increments Attempts.
// Returns ErrNotOwner if term != Owner. Steps must start in order: a step may only start
// when all earlier steps are Succeeded.
func (j *Journal) StartStep(opID, stepID string, term uint64) error
// FinishStep sets Succeeded/Failed/Unknown on a Running step; ErrNotOwner if term != Owner.
func (j *Journal) FinishStep(opID, stepID string, result State, term uint64) error
// Adopt hands the operation to a newer term (newTerm > Owner, else ErrNotOwner)
// and turns every Running step into Unknown. Idempotent for the same newTerm.
func (j *Journal) Adopt(opID string, newTerm uint64) error
func (j *Journal) Abort(opID string, term uint64) error // only at a safe boundary: no step Running
func (j *Journal) Get(opID string) (Operation, error)   // returns a deep copy
func (j *Journal) Unfinished() []Operation              // not all steps Succeeded and not Aborted, sorted by ID
```

- [ ] **Step 1:** Write failing table-driven tests for: Begin/duplicate ID; ordered step execution (starting step 2 before step 1 succeeded is an error); `ErrNotOwner` for a stale term on StartStep/FinishStep/Abort; Adopt turns Running into Unknown, bumps Owner, rejects lower or equal-and-different owner terms, is idempotent for the same new term; a step in `Unknown` can be restarted by the new owner (Attempts increments); Abort refused while a step is Running; `Unfinished` ordering; `Get` returns a copy (mutating it does not change the journal).
- [ ] **Step 2:** Run `go test -race ./internal/opjournal/` and confirm failure for missing implementation.
- [ ] **Step 3:** Implement with a `sync.Mutex` and a map.
- [ ] **Step 4:** Run `go test -race ./internal/opjournal/`, `go vet ./...`; all pass.
- [ ] **Step 5:** Commit.

---

### Task 4: Replicated metadata store (`internal/metastore`)

**Files:**
- Create: `internal/metastore/store.go`, `internal/metastore/cluster_test_helpers_test.go`, `internal/metastore/store_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: the library recommended by Task 1 (read `docs/decisions/0003-evidence-library-evaluation.md`; if it recommends a library, use exactly that version); `opjournal.Journal` and its method signatures from Task 3.
- Produces:
```go
package metastore

type Command struct {
	Kind   string   // "begin", "start", "finish", "adopt", "abort"
	OpID   string
	OpKind string   // only for "begin"
	Steps  []string // only for "begin"
	StepID string   // for "start", "finish"
	Result opjournal.State // only for "finish"
	Term   uint64   // fencing term the issuer believes it holds
}

type Node struct{ /* unexported */ }

// Config.Transport is an interface from this package so tests can inject partitions.
func NewNode(cfg Config) (*Node, error)
func (n *Node) Close() error
func (n *Node) IsLeader() bool
func (n *Node) Term() uint64                    // current consensus term
func (n *Node) Apply(cmd Command) error         // leader only; ErrNotLeader otherwise; returns the journal error of applying the command deterministically
func (n *Node) Journal() *opjournal.Journal     // read access to the replicated state machine
var ErrNotLeader = errors.New("metastore: not leader")
```
The replicated state machine applies each committed `Command` to an `opjournal.Journal`. The consensus term of the leader at commit time is the fencing term.

- [ ] **Step 1:** Write a test helper that builds a 3-node cluster over an in-memory transport with a switchable partition matrix (`Isolate(nodeID)`, `Heal()`), and a deadline-based `waitForLeader`. Write failing tests: a 3-node cluster elects exactly one leader; `Apply` of begin/start/finish on the leader is visible in all three journals within a deadline; `Apply` on a follower returns `ErrNotLeader`; after the leader is stopped, a new leader is elected among the remaining two and keeps the committed state.
- [ ] **Step 2:** Run `go test -race ./internal/metastore/` and confirm failure.
- [ ] **Step 3:** Implement the wrapper around the chosen library. Keep persistence in-memory for the prototype but isolate it behind a small unexported interface so a disk-backed store can replace it. Document in the package comment that this is a prototype.
- [ ] **Step 4:** Run `go test -race -count=3 ./internal/metastore/` (flakiness check), `go vet ./...`; all pass.
- [ ] **Step 5:** Commit (include `go.mod` and `go.sum`).

---

### Task 5: Failure scenarios with a simulated agent

**Files:**
- Create: `internal/metastore/scenarios_test.go`

**Interfaces:**
- Consumes: `metastore.Node`, `Command`, `ErrNotLeader`, the test helpers from Task 4; `fencing.Guard.Admit`; `opjournal` states.
- A test-local `simAgent` wraps a `fencing.Guard` and a counter of executed commands: `func (a *simAgent) Execute(term uint64, opID, stepID string) error` calls `Admit(term)`; on success it records the execution and returns nil.

- [ ] **Step 1:** Write the scenario tests, each asserting the stated outcome:
  1. **Leader loss mid-operation:** leader begins an operation with 3 steps, finishes step 1, starts step 2 and is stopped before finishing it. New leader calls `adopt`; step 2 is `Unknown`, never `Succeeded`; the new leader restarts step 2 (Attempts 2) and completes all steps; the simulated agent executed step 1 exactly once.
  2. **Partitioned old leader:** isolate the leader; the majority elects a new leader with a higher term and sends a command to the agent (admitted). The old leader, still believing it leads, sends a command with its old term: the agent returns `ErrStaleTerm`, and a `metastore.Apply` on the isolated old leader does not commit (use a bounded wait and assert the journal on the majority side is unchanged).
  3. **Majority loss:** stop two of three nodes; the survivor's `Apply` does not commit within the deadline (assert the journal is unchanged) and reads of the existing journal still work.
  4. **Heal:** after healing scenario 2, the old leader steps down and converges to the same journal as the others (compare `Unfinished()` of all nodes).
- [ ] **Step 2:** Run `go test -race -count=3 ./internal/metastore/ -run Scenario`; fix only test or wrapper defects, not by weakening assertions.
- [ ] **Step 3:** Commit.

---

### Task 6: Record evidence and update ADR 0003

**Files:**
- Modify: `docs/decisions/0003-manager-metadata-ha-and-fencing.md`

- [ ] **Step 1:** Run `go test -race -count=3 ./...` and capture the summary line per package.
- [ ] **Step 2:** In ADR 0003 add a section "Prototype evidence (in-process, no VMs)" listing each scenario, the test name, the observed result, the library and version chosen, and clearly marked limitations (in-memory persistence, in-process transport, no real network, no agent mTLS, no secrets). Change the status line to "Proposed; prototype evidence in-process only".
- [ ] **Step 3:** Run `python3 scripts/check_docs.py`; commit.
