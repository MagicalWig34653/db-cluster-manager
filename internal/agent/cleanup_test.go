// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRemoveClusterServiceOnlyOwnedClusterServices(t *testing.T) {
	f := newFixture(t)
	rm := func(step, name, cluster string) Result {
		return f.mustDo(1, "op", step, KindRemoveClusterService, RemoveClusterServiceArgs{ServiceName: name, ClusterID: cluster})
	}
	// Wrong cluster label: refused, nothing removed.
	wantStatus(t, rm("s1", "galera-1", "cluster-2"), StatusRefused, CodeNotOwned)
	// Correct label but not in the allow-list.
	f.docker.objects["svc-unlisted"] = Object{Kind: KindService, ID: "svc-unlisted", Name: "galera-x",
		Labels: map[string]string{LabelInstallation: testInstallation, LabelCluster: "cluster-1"}}
	wantStatus(t, rm("s2", "galera-x", "cluster-1"), StatusRefused, CodeNotOwned)
	// A job is not a database service.
	f.docker.objects["svc-job"] = Object{Kind: KindService, ID: "svc-job", Name: "dbcm-job-b1",
		Labels: map[string]string{LabelInstallation: testInstallation, LabelCluster: "cluster-1", LabelJob: JobBackup}}
	f.exec.allow("svc-job")
	wantStatus(t, rm("s3", "dbcm-job-b1", "cluster-1"), StatusRefused, CodeNotOwned)
	if n := countPrefix(f.docker.Mutations(), "remove-service"); n != 0 {
		t.Fatalf("refused removals reached Docker: %d", n)
	}
	wantStatus(t, rm("s4", "galera-1", "cluster-1"), StatusSucceeded, "")
	if _, ok := f.docker.objects["svc-1"]; ok || f.exec.owner.Allowed("svc-1") {
		t.Fatal("service not removed or still allow-listed")
	}
	// Removing again (or a service that was never created) is success.
	r := rm("s5", "galera-1", "cluster-1")
	wantStatus(t, r, StatusSucceeded, "")
	if string(r.Output) != `{"removed":"absent"}` {
		t.Fatalf("output %s", r.Output)
	}
}

func TestRemoveSecretOwnedOnly(t *testing.T) {
	f := jobFixture(t)
	wantStatus(t, f.mustDo(1, "op", "s1", KindRemoveSecret, RemoveSecretArgs{Secret: "foreign"}), StatusRefused, CodeNotOwned)
	f.docker.objects["sec-unlisted"] = Object{Kind: KindSecret, ID: "sec-unlisted", Name: "unlisted",
		Labels: map[string]string{LabelInstallation: testInstallation}}
	wantStatus(t, f.mustDo(1, "op", "s2", KindRemoveSecret, RemoveSecretArgs{Secret: "unlisted"}), StatusRefused, CodeNotOwned)
	f.docker.failNext = fmt.Errorf("busy: %w", ErrConflict)
	wantStatus(t, f.mustDo(1, "op", "s3", KindRemoveSecret, RemoveSecretArgs{Secret: "restore-key"}), StatusFailed, CodeSecretInUse)
	wantStatus(t, f.mustDo(1, "op", "s4", KindRemoveSecret, RemoveSecretArgs{Secret: "restore-key"}), StatusSucceeded, "")
	if _, ok := f.docker.objects["sec-key"]; ok || f.exec.owner.Allowed("sec-key") {
		t.Fatal("secret not removed or still allow-listed")
	}
	wantStatus(t, f.mustDo(1, "op", "s5", KindRemoveSecret, RemoveSecretArgs{Secret: "restore-key"}), StatusSucceeded, "")
	if _, err := f.do(1, "op", "s6", KindRemoveSecret, RemoveSecretArgs{Secret: "../x"}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("bad name: %v", err)
	}
}

// TestRemoveVolumeAfterFailedCreate: a deletion after a failed create must
// cope with a volume that was never created or never got its marker, but
// still refuses data of unknown identity and volumes it does not own.
func TestRemoveVolumeAfterFailedCreate(t *testing.T) {
	f := newFixture(t)
	root := f.volume("galera-1")
	args := func(vol, path string) RemoveVolumeArgs {
		return RemoveVolumeArgs{VolumeID: vol, VolumeRoot: path, Expected: testIdentity(), MissingOK: true, AllowUnmarked: true}
	}
	// Never created: success without Docker removal.
	r := f.mustDo(1, "op", "s1", KindRemoveVolume, args("vol-never", f.volume("galera-never")))
	wantStatus(t, r, StatusSucceeded, "")
	// Exists but not owned: refused even with MissingOK.
	f.docker.objects["vol-foreign"] = Object{Kind: KindVolume, ID: "vol-foreign", Mountpoint: f.volume("galera-f")}
	wantStatus(t, f.mustDo(1, "op", "s2", KindRemoveVolume, args("vol-foreign", f.volume("galera-f"))), StatusRefused, CodeNotOwned)
	// Unknown data without marker: refused.
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, filepath.Join(root, "data", "ibdata1"), "x")
	wantStatus(t, f.mustDo(1, "op", "s3", KindRemoveVolume, args("vol-1", root)), StatusRefused, CodeUnknownData)
	if n := countPrefix(f.docker.Calls(), "remove-volume"); n != 0 {
		t.Fatalf("refused removals reached Docker: %d", n)
	}
	// Empty, unmarked (only directories): removed.
	if err := os.Remove(filepath.Join(root, "data", "ibdata1")); err != nil {
		t.Fatal(err)
	}
	// Docker still sees a container on it first: failed without effect, with
	// a code the runner may retry on.
	f.docker.failNext = fmt.Errorf("in use: %w", ErrConflict)
	wantStatus(t, f.mustDo(1, "op", "s4", KindRemoveVolume, args("vol-1", root)), StatusFailed, CodeVolumeInUse)
	wantStatus(t, f.mustDo(1, "op", "s5", KindRemoveVolume, args("vol-1", root)), StatusSucceeded, "")
	if f.exec.owner.Allowed("vol-1") {
		t.Fatal("removed volume still allow-listed")
	}
	// Without AllowUnmarked an unmarked volume is still refused.
	f.docker.objects["vol-3"] = Object{Kind: KindVolume, ID: "vol-3", Labels: map[string]string{LabelInstallation: testInstallation},
		Mountpoint: f.volume("galera-3")}
	_ = os.MkdirAll(f.volume("galera-3"), 0o755)
	f.exec.allow("vol-3")
	strict := args("vol-3", f.volume("galera-3"))
	strict.AllowUnmarked = false
	wantStatus(t, f.mustDo(1, "op", "s6", KindRemoveVolume, strict), StatusRefused, CodeEmptyVolume)
}

func TestCheckRestoreTargetAndDelegatedRestore(t *testing.T) {
	f := jobFixture(t)
	a := restoreArgs(f)
	check := CheckRestoreTargetArgs{TargetVolume: a.TargetVolume, TargetVolumeRoot: a.TargetVolumeRoot, Expected: a.Expected}
	// No marker yet: refused.
	wantStatus(t, f.mustDo(1, "op", "c1", KindCheckRestoreTarget, check), StatusRefused, CodeEmptyVolume)
	initVolume(t, f.volume("galera-2"), a.Expected)
	wantStatus(t, f.mustDo(1, "op", "c2", KindCheckRestoreTarget, check), StatusSucceeded, "")
	writeRaw(t, filepath.Join(f.volume("galera-2"), "data", "ibdata1"), "x")
	wantStatus(t, f.mustDo(1, "op", "c3", KindCheckRestoreTarget, check), StatusRefused, CodeUnknownData)
	if n := len(f.docker.Mutations()); n != 0 {
		t.Fatalf("check changed something: %v", f.docker.Mutations())
	}

	// Delegated: the manager-node agent pins the job to the worker and does
	// not look at local files (they live on the worker).
	d := restoreArgs(f)
	d.NodeID, d.TargetVolume, d.TargetVolumeRoot = "worker-node-7", "vol-remote", f.volume("galera-remote")
	spec := createdSpec(t, f, f.mustDo(1, "op", "r1", KindRunRestore, d))
	if !slices.Equal(spec.Constraints, []string{"node.id==worker-node-7"}) || len(spec.Mounts) != 1 || spec.Mounts[0].Source != "vol-remote" {
		t.Fatalf("spec %+v", spec)
	}
	// It still refuses a target that a database service mounts.
	prod := restoreArgs(f)
	prod.NodeID, prod.TargetVolume, prod.TargetVolumeRoot, prod.JobName = "worker-node-7", "vol-1", f.volume("galera-1"), "dbcm-job-r2"
	wantStatus(t, f.mustDo(1, "op", "r2", KindRunRestore, prod), StatusRefused, CodeVolumeInUse)
	bad := restoreArgs(f)
	bad.NodeID = "a=b"
	if _, err := f.do(1, "op", "r3", KindRunRestore, bad); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("bad node id: %v", err)
	}
}
