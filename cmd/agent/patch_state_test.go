// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
)

// startPatchLab runs the lab-transport patch agent on a state directory and
// returns its base URL and a stop function.
func startPatchLab(t *testing.T, dir string, extra ...string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	args := append([]string{"-insecure-lab", "-listen", "127.0.0.1:0", "-state", dir, "-host-id", "h1"}, extra...)
	go func() { done <- runPatch(ctx, args, io.Discard, io.Discard, func(a string) { addr <- a }) }()
	select {
	case a := <-addr:
		return "http://" + a, func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	case err := <-done:
		cancel()
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("not ready")
	}
	return "", nil
}

func TestPatchAgentStateSurvivesRestart(t *testing.T) {
	r := useFakes(t)
	r.Responses["apt-get -s -qq install --only-upgrade openssl"] = patch.CmdResult{Stdout: "Inst openssl [1] (2 x)\n"}
	r.Responses["env DEBIAN_FRONTEND=noninteractive apt-get -y -qq -o Dpkg::Options::=--force-confold install --only-upgrade openssl"] = patch.CmdResult{Stdout: "done"}
	dir := t.TempDir()
	url, stop := startPatchLab(t, dir, "-allow-apply")
	h := patch.HTTPHostAgent{Client: http.DefaultClient, URL: func(string) (string, bool) { return url, true }}
	apply := patch.AgentCommand{Kind: patch.CmdApply, Term: 3, StepID: "op/apply", Packages: []string{"openssl"}}
	if _, err := h.Do(context.Background(), "h1", apply); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Do(context.Background(), "h1", patch.AgentCommand{Kind: patch.CmdDenyList, Term: 3, DenyRevision: 4, DenySerials: []string{"abc"}}); err != nil {
		t.Fatal(err)
	}
	stop()
	before := len(r.Calls)
	url, stop = startPatchLab(t, dir, "-allow-apply")
	defer stop()
	h = patch.HTTPHostAgent{Client: http.DefaultClient, URL: func(string) (string, bool) { return url, true }}
	if _, err := h.Do(context.Background(), "h1", apply); err != nil {
		t.Fatal("replay after restart:", err)
	}
	for _, c := range r.Calls[before:] {
		if strings.Contains(c, "install") {
			t.Fatalf("step re-ran after restart: %q", c)
		}
	}
	st, err := patch.OpenAgentState(filepath.Join(dir, patchStateFileName))
	if err != nil || !st.Revoked(big.NewInt(0xabc)) {
		t.Fatal("deny list not persisted:", err)
	}
}
