// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// Lab E4 failcreate: a create whose idempotent exec-sql step kept failing
// could not be aborted; abort answered 202, but the operation stayed
// running through its retry backoff for more than 300 s, so the cluster's
// deletion was refused as busy.

// execSQLOp is a step whose agent command always fails with a retryable
// error, followed by a destructive step that must never be sent.
func execSQLOp(id string) Operation {
	return Operation{ID: id, Kind: "test", Locks: []string{"cluster/c1"}, Steps: []Step{
		{ID: "05-exec-sql-n1", Target: "h1", Command: agent.KindExecSQL, Args: inspectArgs("a"), MaxRetries: 30},
		{ID: "06-create", Target: "h1", Command: agent.KindStopDB, Args: svcArgs("b")},
	}}
}

// realRunner sleeps for real with a long backoff, like production after a
// few retries (30 s); the test fails by timing out if the abort does not
// interrupt the wait.
func (h *harness) realRunner(log *slog.Logger) *Runner {
	n := h.meta.node("m1")
	r, err := New(Config{Leader: n, Journal: n, Agents: h.agents, Checker: h.checker, Now: h.clock.Now,
		StepTimeout: time.Hour, BackoffBase: time.Hour, BackoffMax: time.Hour, Log: log})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func runAsync(r *Runner, op Operation) <-chan View {
	done := make(chan View, 1)
	go func() {
		v, _ := r.Run(context.Background(), op)
		done <- v
	}()
	return done
}

func waitView(t *testing.T, r *Runner, opID, what string, cond func(View) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, err := r.Snapshot(opID); err == nil && cond(v) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func awaitDone(t *testing.T, done <-chan View) View {
	t.Helper()
	select {
	case v := <-done:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("the operation did not end promptly after the abort")
		return View{}
	}
}

// syncBuf is a concurrency-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAbortInterruptsIdempotentRetryBackoff(t *testing.T) {
	h := newHarness(t, "h1")
	h.agents["h1"].kindFault = map[agent.Kind]fault{agent.KindExecSQL: failNotRunning}
	logs := &syncBuf{}
	r := h.realRunner(slog.New(slog.NewTextHandler(logs, nil)))
	done := runAsync(r, execSQLOp("op1"))
	waitView(t, r, "op1", "a retry wait", func(v View) bool {
		for _, e := range v.Timeline {
			if e.Type == EventRetry {
				return true
			}
		}
		return false
	})
	if err := r.Abort("op1"); err != nil {
		t.Fatal(err)
	}
	v := awaitDone(t, done)
	if v.Status != StatusAborted {
		t.Fatalf("status %s (%s)", v.Status, v.Reason)
	}
	js := journalStates(t, h.meta.node("m1"), "op1")
	if s := js["05-exec-sql-n1"]; s.State != opjournal.Failed {
		t.Errorf("exec-sql step %s; the observed failure must stay recorded", s.State)
	}
	for _, c := range h.agents["h1"].Received() {
		if c.Kind == agent.KindStopDB {
			t.Fatal("step after the abort was sent")
		}
	}
	if n := len(h.agents["h1"].Received()); n != 1 {
		t.Errorf("%d attempts; the abort must interrupt the first backoff", n)
	}
	// The locks are released: the same cluster can be operated on again.
	h.agents["h1"].kindFault = nil
	if v2, err := r.Run(context.Background(), threeStepOpOn("op2", "h1")); err != nil || v2.Status != StatusSucceeded {
		t.Fatalf("after abort: %v %s", err, v2.Status)
	}
	// The retry reason names the cause and is logged with step and attempt.
	var retry string
	for _, e := range v.Timeline {
		if e.Type == EventRetry {
			retry = e.Detail
		}
	}
	if !strings.Contains(retry, "dependency not running") || !strings.Contains(retry, "mysqld.sock does not exist") {
		t.Errorf("retry detail %q", retry)
	}
	out := logs.String()
	for _, want := range []string{"runner: step retry", "step=05-exec-sql-n1", "retry=1", "db-not-running", "runner: operation aborted"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func threeStepOpOn(id, host string) Operation {
	op := threeStepOp(id)
	for i := range op.Steps {
		op.Steps[i].Target = host
	}
	return op
}

// TestAbortCancelsInFlightIdempotentCommand: an abort while the idempotent
// command is in flight cancels it; its step is journaled Unknown (never
// Succeeded) and the abort reason says so.
func TestAbortCancelsInFlightIdempotentCommand(t *testing.T) {
	h := newHarness(t, "h1")
	entered, release := h.agents["h1"].block("05-exec-sql-n1")
	defer release()
	r := h.realRunner(nil)
	done := runAsync(r, execSQLOp("op1"))
	<-entered
	if err := r.Abort("op1"); err != nil {
		t.Fatal(err)
	}
	v := awaitDone(t, done)
	if v.Status != StatusAborted {
		t.Fatalf("status %s (%s)", v.Status, v.Reason)
	}
	if s := journalStates(t, h.meta.node("m1"), "op1")["05-exec-sql-n1"]; s.State != opjournal.Unknown {
		t.Errorf("in-flight step %s; its outcome is unknown", s.State)
	}
	if !strings.Contains(v.Reason, "outcome unknown") || !strings.Contains(v.Reason, "05-exec-sql-n1") {
		t.Errorf("reason %q", v.Reason)
	}
	for _, c := range h.agents["h1"].Received() {
		if c.Kind == agent.KindStopDB {
			t.Fatal("step after the abort was sent")
		}
	}
}

// TestAbortDuringInDoubtRetryKeepsUnknown: an abort during the backoff
// after an unanswered idempotent attempt aborts with the step Unknown.
func TestAbortDuringInDoubtRetryKeepsUnknown(t *testing.T) {
	h := newHarness(t, "h1")
	h.agents["h1"].addFault("05-exec-sql-n1", crashInDoubt)
	r := h.realRunner(nil)
	done := runAsync(r, execSQLOp("op1"))
	waitView(t, r, "op1", "a retry wait", func(v View) bool {
		return len(v.Timeline) > 0 && v.Timeline[len(v.Timeline)-1].Type == EventRetry
	})
	if err := r.Abort("op1"); err != nil {
		t.Fatal(err)
	}
	v := awaitDone(t, done)
	if v.Status != StatusAborted {
		t.Fatalf("status %s (%s)", v.Status, v.Reason)
	}
	if s := journalStates(t, h.meta.node("m1"), "op1")["05-exec-sql-n1"]; s.State != opjournal.Unknown {
		t.Errorf("step %s", s.State)
	}
	if !strings.Contains(v.Reason, "outcome unknown") {
		t.Errorf("reason %q", v.Reason)
	}
}

func TestRedactMasksCredentials(t *testing.T) {
	for in, leak := range map[string]string{
		"ALTER USER 'm'@'%' IDENTIFIED BY 'S3cret-pw' failed": "S3cret-pw",
		"connect: password=hunter2 refused":                   "hunter2",
		"dsn root:pw123@unix(/run/mysqld.sock) refused":       "pw123",
		`token: "abc.def" expired`:                            "abc.def",
	} {
		if out := Redact(in); strings.Contains(out, leak) || !strings.Contains(out, "***") {
			t.Errorf("Redact(%q) = %q", in, out)
		}
	}
	if s := Redact("database socket /v/run/mysqld.sock does not exist"); s != "database socket /v/run/mysqld.sock does not exist" {
		t.Errorf("changed a harmless text: %q", s)
	}
	if s := Redact(strings.Repeat("x", 5000)); len(s) > maxDetail+3 {
		t.Errorf("not bounded: %d", len(s))
	}
}

// TestRetryExhaustionReportsAgentError: without an abort the bounded
// retries end in Failed with the agent's message as the reason.
func TestRetryExhaustionReportsAgentError(t *testing.T) {
	h := newHarness(t, "h1")
	h.agents["h1"].kindFault = map[agent.Kind]fault{agent.KindExecSQL: failNotRunning}
	op := execSQLOp("op1")
	op.Steps[0].MaxRetries = 2
	v, err := h.runner("m1").Run(context.Background(), op)
	if err != nil || v.Status != StatusFailed {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	if !strings.Contains(v.Reason, string(agent.CodeDBNotRunning)) {
		t.Errorf("reason %q", v.Reason)
	}
	if n := len(h.agents["h1"].Received()); n != 3 {
		t.Errorf("%d attempts", n)
	}
}
