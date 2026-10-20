// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

// proc is one metad process under test.
type proc struct {
	id, raft, http, data string
	cmd                  *exec.Cmd
	log                  *bytes.Buffer
}

type stateResp struct {
	ID         string           `json:"id"`
	Leader     bool             `json:"leader"`
	Term       uint64           `json:"term"`
	Unfinished []map[string]any `json:"unfinished"`
}

func buildMetad(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "metad")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func (p *proc) start(t *testing.T, bin, peers string) {
	t.Helper()
	p.log = &bytes.Buffer{}
	p.cmd = exec.Command(bin, "-id", p.id, "-bind", p.raft, "-data", p.data, "-http", p.http, "-peers", peers, "-bootstrap")
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
}

// stop sends SIGTERM and waits for a clean exit.
func (p *proc) stop(t *testing.T) {
	t.Helper()
	if p.cmd == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("%s exited with %v\n%s", p.id, err, lastLines(p.log.String(), 20))
		}
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
		t.Errorf("%s did not stop on SIGTERM", p.id)
	}
	p.cmd = nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var client = &http.Client{Timeout: 5 * time.Second}

func (p *proc) state(barrier bool) (stateResp, error) {
	url := "http://" + p.http + "/state"
	if barrier {
		url += "?barrier=1"
	}
	resp, err := client.Get(url)
	if err != nil {
		return stateResp{}, err
	}
	defer resp.Body.Close()
	var s stateResp
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("status %d", resp.StatusCode)
	}
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

func (p *proc) apply(cmd metastore.Command) (int, map[string]any, error) {
	body, _ := json.Marshal(cmd)
	resp, err := client.Post("http://"+p.http+"/apply", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	err = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, err
}

func waitLeader(t *testing.T, ps []*proc) (*proc, stateResp) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var leaders []*proc
		var ls stateResp
		for _, p := range ps {
			if s, err := p.state(false); err == nil && s.Leader {
				leaders, ls = append(leaders, p), s
			}
		}
		if len(leaders) == 1 {
			return leaders[0], ls
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range ps {
		t.Logf("%s log:\n%s", p.id, lastLines(p.log.String(), 20))
	}
	t.Fatal("no single leader among metad processes")
	return nil, stateResp{}
}

// TestThreeProcessesRestartFromDisk runs three metad processes on loopback,
// commits through the HTTP harness, stops all of them with SIGTERM, starts
// them again and checks the journal and term.
func TestThreeProcessesRestartFromDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs metad processes")
	}
	bin := buildMetad(t)
	var ps []*proc
	var peers []string
	for i := 1; i <= 3; i++ {
		p := &proc{id: fmt.Sprintf("m%d", i), raft: freeAddr(t), http: freeAddr(t), data: t.TempDir()}
		ps = append(ps, p)
		peers = append(peers, p.id+"="+p.raft)
	}
	peerFlag := strings.Join(peers, ",")
	t.Cleanup(func() {
		for _, p := range ps {
			if p.cmd != nil {
				_ = p.cmd.Process.Kill()
				_ = p.cmd.Wait()
			}
		}
	})
	for _, p := range ps {
		p.start(t, bin, peerFlag)
	}

	leader, ls := waitLeader(t, ps)
	code, out, err := leader.apply(metastore.Command{Kind: "begin", OpID: "op1", OpKind: "failover", Steps: []string{"a", "b"}, Term: ls.Term})
	if err != nil || code != http.StatusOK {
		t.Fatalf("apply begin on leader: %d %v %v", code, out, err)
	}
	code, out, err = leader.apply(metastore.Command{Kind: "start", OpID: "op1", StepID: "a", Term: ls.Term})
	if err != nil || code != http.StatusOK {
		t.Fatalf("apply start on leader: %d %v %v", code, out, err)
	}
	for _, p := range ps {
		if p == leader {
			continue
		}
		code, out, err := p.apply(metastore.Command{Kind: "abort", OpID: "op1", Term: ls.Term})
		if err != nil || code != http.StatusServiceUnavailable || out["code"] != "not_leader" {
			t.Fatalf("apply on follower %s: %d %v %v", p.id, code, out, err)
		}
	}

	termBefore := ls.Term
	for _, p := range ps {
		p.stop(t)
	}
	if t.Failed() {
		t.FailNow()
	}
	for _, p := range ps {
		if _, err := os.Stat(filepath.Join(p.data, "raft.db")); err != nil {
			t.Fatal(err)
		}
		p.start(t, bin, peerFlag)
	}

	leader, ls = waitLeader(t, ps)
	if ls.Term < termBefore {
		t.Fatalf("term after restart %d below %d", ls.Term, termBefore)
	}
	s, err := leader.state(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Unfinished) != 1 || s.Unfinished[0]["ID"] != "op1" {
		t.Fatalf("unfinished after restart: %+v", s.Unfinished)
	}
	steps, _ := s.Unfinished[0]["Steps"].([]any)
	if len(steps) != 2 || steps[0].(map[string]any)["State"] != "running" {
		t.Fatalf("steps after restart: %+v", steps)
	}
	// The new term adopts the operation through the harness.
	code, out, err = leader.apply(metastore.Command{Kind: "adopt", OpID: "op1", Term: s.Term})
	if err != nil || code != http.StatusOK {
		t.Fatalf("adopt after restart: %d %v %v", code, out, err)
	}
	for _, p := range ps {
		p.stop(t)
	}
}
