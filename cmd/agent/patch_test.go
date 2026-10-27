// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
)

// Fakes only: the runner and the file view are replaced, so no package
// manager of this machine is ever executed.
func useFakes(t *testing.T) *patch.FakeRunner {
	r := &patch.FakeRunner{Responses: map[string]patch.CmdResult{
		"uname -rm": {Stdout: "6.1.0-18-arm64 aarch64\n"},
		`dpkg-query -W -f=${Package}\t${Version}\t${Architecture}\n`: {Stdout: "openssl\t3.0.11-1~deb12u1\tarm64\n"},
		"apt list --upgradable": {Stdout: "Listing...\nopenssl/stable-security 3.0.11-1~deb12u2 arm64 [upgradable from: 3.0.11-1~deb12u1]\n"},
	}}
	oldR, oldF := patchRunner, patchFS
	patchRunner, patchFS = r, patch.FakeFS{"/usr/bin/apt-get": "", "/etc/os-release": "ID=debian\nVERSION_ID=12\n"}
	t.Cleanup(func() { patchRunner, patchFS = oldR, oldF })
	return r
}

func TestPatchOnceIsReadOnly(t *testing.T) {
	r := useFakes(t)
	var out bytes.Buffer
	if err := runPatch(context.Background(), []string{"-once", "-host-id", "h1"}, &out, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	var rep patch.HostReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Manager != patch.APT || len(rep.SecurityUpdates()) != 1 || rep.HostID != "h1" {
		t.Fatalf("%+v", rep)
	}
	for _, c := range r.Calls {
		if strings.Contains(c, "install") || strings.Contains(c, "reboot") || strings.Contains(c, "upgrade") && !strings.Contains(c, "list") {
			t.Fatalf("mutating command %q", c)
		}
	}
}

func TestParsePatchFlags(t *testing.T) {
	good := []string{"-insecure-lab", "-listen", "127.0.0.1:7445", "-state", "/var/lib/p"}
	if _, err := parsePatchFlags(good); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no listen":       {"-state", "/var/lib/p"},
		"relative state":  {"-insecure-lab", "-listen", "127.0.0.1:1", "-state", "x"},
		"non-loopback":    {"-insecure-lab", "-listen", "0.0.0.0:7445", "-state", "/var/lib/p"},
		"mtls w/o fields": {"-listen", "0.0.0.0:7445", "-state", "/var/lib/p"},
		"http manager":    {"-listen", "0.0.0.0:7445", "-state", "/var/lib/p", "-host-id", "h", "-installation", "i", "-manager-url", "http://m:1", "-ca-fingerprint", strings.Repeat("ab", 32)},
		"stray":           append(append([]string{}, good...), "x"),
	} {
		if _, err := parsePatchFlags(args); err == nil {
			t.Fatal("accepted:", name)
		}
	}
	o, _ := parsePatchFlags(good)
	if o.allowApply {
		t.Fatal("apply must be off by default")
	}
}

func TestPatchLabServerServesReadOnly(t *testing.T) {
	useFakes(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- runPatch(ctx, []string{"-insecure-lab", "-listen", "127.0.0.1:0", "-state", t.TempDir(), "-host-id", "h1"}, io.Discard, io.Discard, func(a string) { addr <- a })
	}()
	var a string
	select {
	case a = <-addr:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	h := patch.HTTPHostAgent{Client: http.DefaultClient, URL: func(string) (string, bool) { return "http://" + a, true }}
	res, err := h.Do(ctx, "h1", patch.AgentCommand{Kind: patch.CmdReport})
	if err != nil || res.Report == nil || res.Report.Manager != patch.APT {
		t.Fatal(res, err)
	}
	if _, err := h.Do(ctx, "h1", patch.AgentCommand{Kind: patch.CmdApply, Term: 1, StepID: "s", Packages: []string{"openssl"}}); err == nil {
		t.Fatal("apply must be refused without -allow-apply")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
