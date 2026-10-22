// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	testNodeID  = "swarm-node-a"
	testPubKey  = "dbcmpk1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	uuidRestore = "11111111-2222-4333-8444-555555555555"
)

// jobFixture extends newFixture with a node ID, owned secrets, an owned
// network and a second, unmounted volume vol-2 (galera-2).
func jobFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	own := map[string]string{LabelInstallation: testInstallation}
	for _, o := range []Object{
		{Kind: KindSecret, ID: "sec-db", Name: "db-cred", Labels: own},
		{Kind: KindSecret, ID: "sec-be", Name: "backend-cfg", Labels: own},
		{Kind: KindSecret, ID: "sec-key", Name: "restore-key", Labels: own},
		{Kind: KindSecret, ID: "sec-foreign", Name: "foreign", Labels: map[string]string{LabelInstallation: "other"}},
		{Kind: KindNetwork, ID: "net-1", Name: "cl-net", Labels: own},
		{Kind: KindVolume, ID: "vol-2", Labels: own, Mountpoint: f.volume("galera-2")},
	} {
		f.docker.objects[o.ID] = o
	}
	if err := os.MkdirAll(f.volume("galera-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.restart()
	f.exec.cfg.NodeID = testNodeID
	for _, id := range []string{"sec-db", "sec-be", "sec-key", "sec-foreign", "net-1", "vol-2"} {
		f.exec.allow(id)
	}
	return f
}

func physicalBackupArgs(f *fixture) RunBackupArgs {
	return RunBackupArgs{
		JobName: "dbcm-job-b1", Image: "dbcm/job:1", Tool: ToolMariabackup, BackupID: "b1", Object: "c1-b1.xbs",
		Expected: testIdentity(), SourceVolume: "vol-1", SourceVolumeRoot: f.volume("galera-1"),
		DBHost: "galera-n2", Network: "cl-net", DBCredentialSecret: "sec-db", BackendSecret: "sec-be",
		RecipientKey: testPubKey, Resources: JobResources{NanoCPUs: 5e8, MemoryBytes: 256 << 20},
	}
}

func initVolume(t *testing.T, root string, id Identity) {
	t.Helper()
	if err := Initialize(root, NewIdentityToken{TokenID: "t-" + id.NodeUUID, Identity: id}); err != nil {
		t.Fatal(err)
	}
}

func createdSpec(t *testing.T, f *fixture, r Result) ServiceSpec {
	t.Helper()
	wantStatus(t, r, StatusSucceeded, "")
	for id, s := range f.docker.specs {
		if strings.Contains(string(r.Output), id) {
			return s
		}
	}
	t.Fatalf("no spec for %s", r.Output)
	return ServiceSpec{}
}

func TestRunBackupPhysicalLaunchesPinnedReadOnlyJob(t *testing.T) {
	f := jobFixture(t)
	initVolume(t, f.volume("galera-1"), testIdentity())
	spec := createdSpec(t, f, f.mustDo(1, "op", "s1", KindRunBackup, physicalBackupArgs(f)))

	if !slices.Equal(spec.Constraints, []string{"node.id==" + testNodeID}) {
		t.Fatalf("constraints %v", spec.Constraints)
	}
	if spec.Replicas != 1 || spec.RestartPolicy == nil || spec.RestartPolicy.Condition != "none" {
		t.Fatalf("not a one-shot job: %+v", spec)
	}
	if len(spec.Mounts) != 1 || !spec.Mounts[0].ReadOnly || spec.Mounts[0].Source != "vol-1" || spec.Mounts[0].Target != JobVolumeTarget {
		t.Fatalf("mounts %+v", spec.Mounts)
	}
	if !slices.Equal(spec.Command, []string{JobCommand}) {
		t.Fatalf("command %v", spec.Command)
	}
	for _, want := range []string{"backup", "--tool=mariabackup", "--datadir=/dbcm/data", "--db-option-file=/run/secrets/db-cred",
		"--backend-config=/run/secrets/backend-cfg", "--recipient=" + testPubKey, "--node-uuid=" + uuidA} {
		if !slices.Contains(spec.Args, want) {
			t.Fatalf("args %v lack %s", spec.Args, want)
		}
	}
	if !slices.Equal(spec.Secrets, []string{"sec-db", "sec-be"}) || !slices.Equal(spec.Networks, []string{"net-1"}) {
		t.Fatalf("refs %v %v", spec.Secrets, spec.Networks)
	}
	if spec.Labels[LabelJob] != JobBackup || spec.Labels[LabelJobID] != "b1" || spec.Labels[LabelInstallation] != testInstallation {
		t.Fatalf("labels %v", spec.Labels)
	}
	if spec.Resources == nil || spec.Resources.Limits.MemoryBytes != 256<<20 {
		t.Fatalf("resources %+v", spec.Resources)
	}
}

func TestRunBackupLogicalHasNoMount(t *testing.T) {
	f := jobFixture(t)
	a := physicalBackupArgs(f)
	a.Tool, a.SourceVolume, a.SourceVolumeRoot, a.Database, a.JobName = ToolMariadbDump, "", "", "shop", "dbcm-job-b1-shop"
	spec := createdSpec(t, f, f.mustDo(1, "op", "s1", KindRunBackup, a))
	if len(spec.Mounts) != 0 || !slices.Contains(spec.Args, "--database=shop") {
		t.Fatalf("%+v", spec)
	}
}

func TestRunBackupRefusals(t *testing.T) {
	f := jobFixture(t)
	initVolume(t, f.volume("galera-1"), testIdentity())
	step := 0
	run := func(a RunBackupArgs) Result {
		step++
		return f.mustDo(1, "op", "r"+string(rune('a'+step)), KindRunBackup, a)
	}
	a := physicalBackupArgs(f)
	a.Expected.NodeUUID = uuidRestore
	wantStatus(t, run(a), StatusRefused, CodeIdentityMismatch)

	a = physicalBackupArgs(f)
	a.SourceVolume = "vol-2" // owned, but bound elsewhere
	wantStatus(t, run(a), StatusRefused, CodeVolumeMismatch)

	a = physicalBackupArgs(f)
	a.BackendSecret = "sec-foreign"
	wantStatus(t, run(a), StatusRefused, CodeNotOwned)

	f.exec.cfg.NodeID = ""
	wantStatus(t, run(physicalBackupArgs(f)), StatusRefused, CodeNodeNotConfigured)
	f.exec.cfg.NodeID = testNodeID

	if n := countPrefix(f.docker.Mutations(), "create-service"); n != 0 {
		t.Fatalf("a refused job created %d services", n)
	}

	invalidCases := map[string]func(*RunBackupArgs){
		"tool":      func(a *RunBackupArgs) { a.Tool = "sh" },
		"job name":  func(a *RunBackupArgs) { a.JobName = "galera-1" },
		"resources": func(a *RunBackupArgs) { a.Resources = JobResources{} },
		"recipient": func(a *RunBackupArgs) { a.RecipientKey = "secret" },
		"object":    func(a *RunBackupArgs) { a.Object = "../x" },
		"volume":    func(a *RunBackupArgs) { a.SourceVolumeRoot = "/etc" },
		"identity":  func(a *RunBackupArgs) { a.Expected = Identity{} },
		"logical db": func(a *RunBackupArgs) {
			a.Tool, a.SourceVolume, a.SourceVolumeRoot, a.Database = ToolMariadbDump, "", "", "mysql"
		},
		"db option": func(a *RunBackupArgs) { a.DBCredentialSecret = "../../etc/passwd" },
	}
	for name, mut := range invalidCases {
		a := physicalBackupArgs(f)
		mut(&a)
		if _, err := f.do(1, "op", "inv-"+strings.ReplaceAll(name, " ", "-"), KindRunBackup, a); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func restoreArgs(f *fixture) RunRestoreArgs {
	return RunRestoreArgs{
		JobName: "dbcm-job-r1", Image: "dbcm/job:1", Mode: RestorePhysical, BackupID: "b1", Object: "c1-b1.xbs",
		ApprovalID: "appr-1", SourceClusterID: "cluster-1",
		TargetVolume: "vol-2", TargetVolumeRoot: f.volume("galera-2"),
		Expected:      Identity{InstallationID: testInstallation, ClusterID: "restore-1", NodeUUID: uuidRestore},
		BackendSecret: "sec-be", KeySecret: "sec-key", Resources: JobResources{NanoCPUs: 5e8, MemoryBytes: 256 << 20},
	}
}

func TestRunRestorePhysicalIntoNewVolume(t *testing.T) {
	f := jobFixture(t)
	a := restoreArgs(f)
	initVolume(t, f.volume("galera-2"), a.Expected)
	spec := createdSpec(t, f, f.mustDo(1, "op", "s1", KindRunRestore, a))
	if len(spec.Mounts) != 1 || spec.Mounts[0].ReadOnly || spec.Mounts[0].Source != "vol-2" {
		t.Fatalf("mounts %+v", spec.Mounts)
	}
	for _, want := range []string{"restore", "--mode=physical", "--approval-id=appr-1", "--key-file=/run/secrets/restore-key",
		"--source-cluster-id=cluster-1", "--target-cluster-id=restore-1"} {
		if !slices.Contains(spec.Args, want) {
			t.Fatalf("args %v lack %s", spec.Args, want)
		}
	}
	if len(spec.Networks) != 0 {
		t.Fatalf("a physical restore job joins no network: %v", spec.Networks)
	}
	if spec.Labels[LabelJob] != JobRestore {
		t.Fatalf("labels %v", spec.Labels)
	}
}

func TestRunRestoreSafetyRefusals(t *testing.T) {
	f := jobFixture(t)

	// The production node's volume (mounted by svc-1) is refused even when
	// its marker carries the restore identity.
	prod := restoreArgs(f)
	prod.TargetVolume, prod.TargetVolumeRoot = "vol-1", f.volume("galera-1")
	initVolume(t, f.volume("galera-1"), prod.Expected)
	wantStatus(t, f.mustDo(1, "op", "prod", KindRunRestore, prod), StatusRefused, CodeVolumeInUse)

	// A target identity of the source cluster is refused (no restore into
	// the live membership).
	same := restoreArgs(f)
	same.Expected.ClusterID = same.SourceClusterID
	wantStatus(t, f.mustDo(1, "op", "same", KindRunRestore, same), StatusRefused, CodeSameCluster)

	// Wrong identity on the target volume.
	a := restoreArgs(f)
	initVolume(t, f.volume("galera-2"), a.Expected)
	other := a
	other.Expected.NodeUUID = uuidA
	wantStatus(t, f.mustDo(1, "op", "ident", KindRunRestore, other), StatusRefused, CodeIdentityMismatch)

	// Existing data in the datadir.
	writeRaw(t, filepath.Join(f.volume("galera-2"), "data", "ibdata1"), "x")
	wantStatus(t, f.mustDo(1, "op", "data", KindRunRestore, a), StatusRefused, CodeUnknownData)

	if n := countPrefix(f.docker.Mutations(), "create-service"); n != 0 {
		t.Fatalf("refused restores created %d services", n)
	}

	// No approval, no restore.
	noAppr := restoreArgs(f)
	noAppr.ApprovalID = ""
	if _, err := f.do(1, "op", "appr", KindRunRestore, noAppr); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("got %v", err)
	}
	bad := restoreArgs(f)
	bad.Mode = "rsync"
	if _, err := f.do(1, "op", "mode", KindRunRestore, bad); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("got %v", err)
	}
	logical := restoreArgs(f)
	logical.Mode, logical.TargetVolume, logical.TargetVolumeRoot = RestoreLogical, "", ""
	logical.Database, logical.TargetDatabase, logical.DBHost, logical.Network, logical.DBCredentialSecret = "shop", "mysql", "galera-n1", "cl-net", "sec-db"
	if _, err := f.do(1, "op", "sysdb", KindRunRestore, logical); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("restore into mysql schema: %v", err)
	}
}

func TestRunRestoreLogical(t *testing.T) {
	f := jobFixture(t)
	a := restoreArgs(f)
	a.Mode, a.TargetVolume, a.TargetVolumeRoot = RestoreLogical, "", ""
	a.Database, a.TargetDatabase, a.DBHost, a.Network, a.DBCredentialSecret = "shop", "shop_restored", "galera-n1", "cl-net", "sec-db"
	spec := createdSpec(t, f, f.mustDo(1, "op", "s1", KindRunRestore, a))
	if len(spec.Mounts) != 0 || !slices.Contains(spec.Args, "--target-database=shop_restored") ||
		!slices.Contains(spec.Args, "--db-option-file=/run/secrets/db-cred") {
		t.Fatalf("%+v", spec)
	}
}

func TestJobLabelReservedAndJobsImmutable(t *testing.T) {
	f := jobFixture(t)
	s := spec("dbcm-job-fake")
	s.Labels = map[string]string{LabelJob: JobBackup}
	if _, err := f.do(1, "op", "c1", KindCreateService, s); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("create-service with job label: %v", err)
	}
	upd := spec("galera-1")
	upd.Labels = map[string]string{LabelJob: JobRestore}
	if _, err := f.do(1, "op", "u1", KindUpdateService, UpdateServiceArgs{ServiceName: "galera-1", Spec: upd}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("update-service adding job label: %v", err)
	}
	initVolume(t, f.volume("galera-1"), testIdentity())
	createdSpec(t, f, f.mustDo(1, "op", "b", KindRunBackup, physicalBackupArgs(f)))
	js := spec("dbcm-job-b1")
	js.Replicas = 1
	js.Constraints = []string{"node.id==" + testNodeID}
	r := f.mustDo(1, "op", "u2", KindUpdateService, UpdateServiceArgs{ServiceName: "dbcm-job-b1", Spec: js})
	wantStatus(t, r, StatusRefused, CodeImmutable)
}

func TestRemoveJob(t *testing.T) {
	f := jobFixture(t)
	initVolume(t, f.volume("galera-1"), testIdentity())
	createdSpec(t, f, f.mustDo(1, "op", "b", KindRunBackup, physicalBackupArgs(f)))
	// A plain owned service with a job-like name is not removed.
	plain := spec("dbcm-job-plain")
	wantStatus(t, f.mustDo(1, "op", "c", KindCreateService, plain), StatusSucceeded, "")
	wantStatus(t, f.mustDo(1, "op", "rm-plain", KindRemoveJob, RemoveJobArgs{JobName: "dbcm-job-plain"}), StatusRefused, CodeNotJob)

	wantStatus(t, f.mustDo(1, "op", "rm", KindRemoveJob, RemoveJobArgs{JobName: "dbcm-job-b1"}), StatusSucceeded, "")
	if countPrefix(f.docker.Mutations(), "remove-service") != 1 {
		t.Fatalf("mutations %v", f.docker.Mutations())
	}
	wantStatus(t, f.mustDo(1, "op", "rm-again", KindRemoveJob, RemoveJobArgs{JobName: "dbcm-job-b1"}), StatusSucceeded, "")
	if _, err := f.do(1, "op", "rm-bad", KindRemoveJob, RemoveJobArgs{JobName: "galera-1"}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("got %v", err)
	}
}
