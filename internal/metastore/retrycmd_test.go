// SPDX-License-Identifier: AGPL-3.0-only

package metastore

import (
	"encoding/json"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// TestJournalCommandRetryAndAgentStep: "start" records the agent step ID,
// "finish" with Retry marks the retry as due, and commands without the new
// fields encode exactly as before.
func TestJournalCommandRetryAndAgentStep(t *testing.T) {
	j := opjournal.New()
	for _, c := range []Command{
		{Kind: "begin", OpID: "op1", OpKind: "test", Steps: []string{"rm"}, Term: 1},
		{Kind: "start", OpID: "op1", StepID: "rm", AgentStep: "rm~a2", Term: 1},
		{Kind: "finish", OpID: "op1", StepID: "rm", Result: opjournal.Failed, Retry: true, Term: 1},
	} {
		if err := applyTo(j, c); err != nil {
			t.Fatalf("%s: %v", c.Kind, err)
		}
	}
	op, _ := j.Get("op1")
	if s := op.Steps[0]; !s.Retry || s.AgentStep != "rm~a2" || s.State != opjournal.Failed {
		t.Fatalf("step %+v", s)
	}
	b, err := json.Marshal(Command{Kind: "finish", OpID: "op1", StepID: "rm", Result: opjournal.Failed, Term: 1})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"Kind":"finish","OpID":"op1","OpKind":"","Steps":null,"StepID":"rm","Result":"failed","Term":1}`; string(b) != want {
		t.Fatalf("encoding changed:\n got %s\nwant %s", b, want)
	}
}
