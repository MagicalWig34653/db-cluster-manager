// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

type okRestic struct{ calls int }

func (o *okRestic) Run(context.Context, restic.Request) (restic.Result, error) {
	o.calls++
	return restic.Result{Stdout: []byte(`{"message_type":"summary","snapshot_id":"abc123"}` + "\n")}, nil
}

// The native agent offers restic as a capability next to patch: same
// endpoint, same allow-list and secret-file rules (ADR 0022), a report of
// runtime=native, and no Docker-object steps.
func TestNativeAgentServesResticCapability(t *testing.T) {
	useFakes(t)
	fake := &okRestic{}
	resticRunner, resticProbe = fake, -1
	t.Cleanup(func() { resticRunner, resticProbe = nil, 0 })
	root, secrets := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(secrets, "pw"), []byte("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- runPatch(ctx, []string{"-insecure-lab", "-listen", "127.0.0.1:0", "-state", t.TempDir(), "-host-id", "h1",
			"-restic-allow-path", root, "-restic-secrets-dir", secrets}, io.Discard, io.Discard, func(a string) { addr <- a })
	}()
	var a string
	select {
	case a = <-addr:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	var got agentrt.Info
	c := &agent.Client{BaseURL: "http://" + a, OnInfo: func(i agentrt.Info, p bool, err error) {
		if err != nil || !p {
			t.Errorf("report: present=%v err=%v", p, err)
		}
		got = i
	}}
	args, _ := json.Marshal(agent.ResticArgs{Backend: restic.BackendLocal, Repository: filepath.Join(root, "repo"), PasswordSecret: "pw", Paths: []string{root}})
	res, err := c.Handle(ctx, agent.Command{Term: 1, OpID: "op", StepID: "b", Kind: agent.KindResticBackup, Args: args})
	if err != nil || res.Status != agent.StatusSucceeded || res.Runtime != agentrt.Native || fake.calls != 1 {
		t.Fatalf("restic on the native agent: %+v err=%v calls=%d", res, err, fake.calls)
	}
	if got.Runtime != agentrt.Native || !got.Has(agentrt.Restic) || !got.Has(agentrt.Patch) || got.Has(agentrt.DockerObjects) {
		t.Fatalf("report %+v", got)
	}
	// A source outside the allow-list is refused exactly as on the container agent.
	bad, _ := json.Marshal(agent.ResticArgs{Backend: restic.BackendLocal, Repository: filepath.Join(root, "repo"), PasswordSecret: "pw", Paths: []string{"/etc"}})
	if res, err := c.Handle(ctx, agent.Command{Term: 1, OpID: "op", StepID: "bad", Kind: agent.KindResticBackup, Args: bad}); err != nil || res.Status != agent.StatusRefused || fake.calls != 1 {
		t.Fatalf("outside source: %+v %v", res, err)
	}
	// Docker object steps are refused by the native agent.
	if _, err := c.Handle(ctx, agent.Command{Term: 1, OpID: "op", StepID: "svc", Kind: agent.KindStopDB, Args: []byte(`{"service_id":"x"}`)}); err == nil {
		t.Fatal("docker step accepted by a native agent")
	}
	// The patch endpoint of the same server reports the same runtime.
	h := patch.HTTPHostAgent{Client: http.DefaultClient, URL: func(string) (string, bool) { return "http://" + a, true }}
	pr, err := h.Do(ctx, "h1", patch.AgentCommand{Kind: patch.CmdReport})
	if err != nil || pr.Runtime != agentrt.Native {
		t.Fatalf("patch report: %+v %v", pr, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNativeAgentWithoutRestic(t *testing.T) {
	useFakes(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan string, 1)
	go func() {
		_ = runPatch(ctx, []string{"-insecure-lab", "-listen", "127.0.0.1:0", "-state", t.TempDir(), "-host-id", "h1"}, io.Discard, io.Discard, func(a string) { addr <- a })
	}()
	a := <-addr
	c := &agent.Client{BaseURL: "http://" + a}
	// No restic roots: the command endpoint does not exist at all.
	if _, err := c.Handle(ctx, agent.Command{Term: 1, OpID: "op", StepID: "b", Kind: agent.KindResticStats, Args: []byte(`{}`)}); err == nil {
		t.Fatal("restic served without the capability")
	}
}
