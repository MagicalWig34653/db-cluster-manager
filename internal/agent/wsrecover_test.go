// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// runnerDocker adds one-off containers to the fake daemon.
type runnerDocker struct {
	*fakeDocker
	running []string
	output  string
	exit    int64
	runs    []ContainerRun
}

func (r *runnerDocker) RunContainer(_ context.Context, c ContainerRun) (ContainerResult, error) {
	r.runs = append(r.runs, c)
	return ContainerResult{ExitCode: r.exit, Output: []byte(r.output)}, nil
}

func (r *runnerDocker) RunningContainersWithVolume(context.Context, string) ([]string, error) {
	return r.running, nil
}

const recoverOut = "2026-10-23  3:27:41 0 [Note] Crash table recovery finished.\n" +
	"2026-10-23  3:27:42 0 [Note] WSREP: Recovered position: 3b2d2499-ce8f-11f1-a8b6-67c327274574:3525\n"

func wsrepFixture(t *testing.T) (*fixture, *runnerDocker, WsrepRecoverArgs) {
	t.Helper()
	f := newFixture(t)
	rd := &runnerDocker{fakeDocker: f.docker, output: recoverOut}
	f.exec.docker = rd
	root := f.volume("galera-1")
	writeMarker(t, root, testIdentity())
	return f, rd, WsrepRecoverArgs{VolumeRoot: root, Volume: "vol-1", Expected: testIdentity(), Image: "mariadb:11.4", UID: 999, GID: 999}
}

func TestWsrepRecover(t *testing.T) {
	f, rd, a := wsrepFixture(t)
	// Volume vol-1 with root .../galera-1: the argument check wants
	// root = base/name.
	if _, err := f.do(1, "op", "bad", KindWsrepRecover, a); err == nil {
		t.Fatal("root/name mismatch accepted")
	}
	f.docker.objects["galera-1"] = Object{Kind: KindVolume, ID: "galera-1", Name: "galera-1", Labels: map[string]string{LabelInstallation: testInstallation}, Mountpoint: f.volume("galera-1")}
	wantStatus(t, f.mustDo(1, "op", "allow", KindAllowObjects, AllowObjectsArgs{IDs: []string{"galera-1"}}), StatusSucceeded, "")
	a.Volume, a.VolumeRoot = "galera-1", f.volume("galera-1")
	r := f.mustDo(1, "op", "w1", KindWsrepRecover, a)
	wantStatus(t, r, StatusSucceeded, "")
	var out WsrepRecoverOutput
	if err := json.Unmarshal(r.Output, &out); err != nil || out.Position == nil || out.Position.Seqno != 3525 {
		t.Fatalf("output %s (%v)", r.Output, err)
	}
	if len(rd.runs) != 1 {
		t.Fatalf("runs %d", len(rd.runs))
	}
	run := rd.runs[0]
	if run.User != "999:999" || run.Entrypoint[0] != "mariadbd" || !slices.Contains(run.Cmd, "--wsrep-recover") ||
		!slices.Contains(run.Cmd, "--datadir=/dbcm/data") || run.Mounts[0].Source != "galera-1" || run.Labels[LabelInstallation] != testInstallation {
		t.Fatalf("run %+v", run)
	}
	for _, c := range run.Cmd {
		if c == "--wsrep-new-cluster" || c == "--wsrep_cluster_address=gcomm://" {
			t.Fatalf("command could bootstrap: %v", run.Cmd)
		}
	}
	// No position in the output: failed, with the log tail.
	rd.output = "2026-10-23 [ERROR] Aborting\n"
	r = f.mustDo(1, "op", "w2", KindWsrepRecover, a)
	wantStatus(t, r, StatusFailed, CodeIO)
	if !strings.Contains(string(r.Output), "Aborting") {
		t.Fatalf("output %s", r.Output)
	}
	// A running container mounts the volume: refused before running.
	rd.running = []string{"abc"}
	n := len(rd.runs)
	wantStatus(t, f.mustDo(1, "op", "w3", KindWsrepRecover, a), StatusRefused, CodeVolumeInUse)
	if len(rd.runs) != n {
		t.Fatal("ran while the volume was in use")
	}
	// Another node's identity: refused.
	rd.running = nil
	b := a
	b.Expected.NodeUUID = "559e29b2-0000-4000-8000-000000000009"
	wantStatus(t, f.mustDo(1, "op", "w4", KindWsrepRecover, b), StatusRefused, CodeIdentityMismatch)
	// Root is never accepted as the database user.
	c := a
	c.UID = 0
	if _, err := f.do(1, "op", "w5", KindWsrepRecover, c); err == nil {
		t.Fatal("uid 0 accepted")
	}
}

func TestWsrepRecoverNeedsAContainerRunner(t *testing.T) {
	f := newFixture(t)
	root := f.volume("galera-1")
	writeMarker(t, root, testIdentity())
	a := WsrepRecoverArgs{VolumeRoot: root, Volume: "galera-1", Expected: testIdentity(), Image: "mariadb:11.4", UID: 999, GID: 999}
	wantStatus(t, f.mustDo(1, "op", "w", KindWsrepRecover, a), StatusRefused, CodeNotConfigured)
}
