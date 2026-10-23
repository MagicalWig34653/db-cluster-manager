// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

func physicalJobArgs(t *testing.T, key backup.RecoveryKey) []string {
	a := agent.RunBackupArgs{JobName: "dbcm-job-b1", Tool: agent.ToolMariabackup, BackupID: "b1", Object: "prod-b1.xbs.dbcms",
		Expected: agent.Identity{InstallationID: testInst, ClusterID: testCluster, NodeUUID: uuid2},
		DBHost:   "dbcm-prod-n2", RecipientKey: pubString(t, key)}
	return agent.BackupJobArgs(a, "db-cred", "backend-cfg")
}

func restoreJobArgs(mode agent.RestoreMode, object string) []string {
	a := agent.RunRestoreArgs{JobName: "dbcm-job-b1-restore", Mode: mode, BackupID: "b1", Object: object, ApprovalID: "appr-1",
		SourceClusterID: testCluster, BackendSecret: "be", KeySecret: "key",
		Expected: agent.Identity{InstallationID: testInst, ClusterID: "cluster-new", NodeUUID: uuid1}}
	if mode == agent.RestoreLogical {
		a.Database, a.TargetDatabase, a.DBHost, a.DBCredentialSecret = "shop", "shop_copy", "dbcm-prod-n1", "db"
	}
	return agent.RestoreJobArgs(a, map[string]string{"be": "backend-cfg", "key": "restore-key", "db": "db-cred"})
}

func payload(t *testing.T, n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func readStatus(t *testing.T, be backup.Backend, key string) JobStatus {
	t.Helper()
	rc, err := be.Get(context.Background(), StatusName(key))
	if err != nil {
		t.Fatalf("status %s: %v", key, err)
	}
	defer rc.Close()
	var st JobStatus
	if err := json.NewDecoder(rc).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestParseJobArgsMatchesAgentArgv(t *testing.T) {
	key := testKey(t)
	s, err := ParseJobArgs(physicalJobArgs(t, key))
	if err != nil {
		t.Fatal(err)
	}
	if s.Tool != agent.ToolMariabackup || s.DataDir != agent.JobDataDir || s.DBOptionFile != "/run/secrets/db-cred" || s.NodeUUID != uuid2 {
		t.Fatalf("%+v", s)
	}
	if _, err := ParseJobArgs(restoreJobArgs(agent.RestorePhysical, "o")); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJobArgs(restoreJobArgs(agent.RestoreLogical, "o")); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]string{
		"unknown kind":   {"shell"},
		"unknown flag":   append(physicalJobArgs(t, key), "--exec=sh"),
		"positional":     append(physicalJobArgs(t, key), "rm"),
		"repeated":       append(physicalJobArgs(t, key), "--tool=mariabackup"),
		"tool":           replaceArg(physicalJobArgs(t, key), "--tool=", "--tool=xtrabackup"),
		"datadir":        replaceArg(physicalJobArgs(t, key), "--datadir=", "--datadir=/var/lib/mysql"),
		"secret outside": replaceArg(physicalJobArgs(t, key), "--db-option-file=", "--db-option-file=/etc/shadow"),
		"secret nested":  replaceArg(physicalJobArgs(t, key), "--db-option-file=", "--db-option-file=/run/secrets/../x"),
		"same cluster":   replaceArg(restoreJobArgs(agent.RestorePhysical, "o"), "--target-cluster-id=", "--target-cluster-id="+testCluster),
		"no approval":    dropArg(restoreJobArgs(agent.RestorePhysical, "o"), "--approval-id="),
	}
	for name, args := range bad {
		if _, err := ParseJobArgs(args); !errors.Is(err, ErrJob) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func replaceArg(args []string, prefix, with string) []string {
	out := slices.Clone(args)
	for i, a := range out {
		if strings.HasPrefix(a, prefix) {
			out[i] = with
		}
	}
	return out
}

func dropArg(args []string, prefix string) []string {
	return slices.DeleteFunc(slices.Clone(args), func(a string) bool { return strings.HasPrefix(a, prefix) })
}

// runPhysicalBackup runs a backup job and returns the backend and payload.
func runPhysicalBackup(t *testing.T, key backup.RecoveryKey) (*backup.MemBackend, []byte, *fakeTools) {
	t.Helper()
	be := backup.NewMemBackend()
	files := fstest.MapFS{}
	data := payload(t, 3<<20+123)
	tools := newFakeTools(files, data)
	env, _ := jobEnv(t, be, key, tools, files)
	if err := RunJob(context.Background(), physicalJobArgs(t, key), env); err != nil {
		t.Fatal(err)
	}
	return be, data, tools
}

func TestBackupJobStreamsEncryptedWithManifest(t *testing.T) {
	key := testKey(t)
	be, data, tools := runPhysicalBackup(t, key)
	ctx := context.Background()
	m, err := readManifest(ctx, be, "prod-b1.xbs.dbcms")
	if err != nil {
		t.Fatal(err)
	}
	if m.Checkpoints.ToLSN != 4711 || m.Galera == nil || m.Galera.Seqno != 1234 || m.Tool.Version != "11.4.3-MariaDB" ||
		m.SourceNodeUUID != uuid2 || m.ClusterID != testCluster || len(m.Stream.Chunks) != 4 {
		t.Fatalf("manifest %+v", m)
	}
	rc, _ := be.Get(ctx, m.Object)
	raw, _ := io.ReadAll(rc)
	if bytes.Contains(raw, data[:64]) {
		t.Fatal("stream is not encrypted")
	}
	sr, err := backup.OpenStream(bytes.NewReader(raw), key)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := io.ReadAll(sr)
	if err != nil || !bytes.Equal(pt, data) {
		t.Fatalf("decrypted payload differs: %v", err)
	}
	if err := backup.CompareSummary(sr.Summary(), m.Stream); err != nil {
		t.Fatal(err)
	}
	if st := readStatus(t, be, m.Object); !st.Succeeded {
		t.Fatalf("status %+v", st)
	}
	if !tools.called("mariabackup --defaults-extra-file=/run/secrets/db-cred --host=dbcm-prod-n2 --backup --stream=xbstream --galera-info") {
		t.Fatalf("calls %v", tools.calls)
	}
	// The gate sees a complete backup.
	c := &Checker{Backend: be}
	ok, why, err := c.Check(ctx, "op", runner.Step{}, runner.Condition{Kind: CondBackupComplete, Target: m.Object})
	if !ok || err != nil {
		t.Fatalf("gate: %s %v", why, err)
	}
}

func TestBackupJobToolFailureCommitsNothing(t *testing.T) {
	key := testKey(t)
	be := backup.NewMemBackend()
	files := fstest.MapFS{}
	tools := newFakeTools(files, payload(t, 1<<20))
	tools.failTool, tools.failAt = "mariabackup", 1000
	env, _ := jobEnv(t, be, key, tools, files)
	if err := RunJob(context.Background(), physicalJobArgs(t, key), env); err == nil {
		t.Fatal("failed tool reported success")
	}
	ctx := context.Background()
	for _, name := range []string{"prod-b1.xbs.dbcms", ManifestName("prod-b1.xbs.dbcms")} {
		if _, err := be.Get(ctx, name); !errors.Is(err, backup.ErrNotFound) {
			t.Fatalf("%s committed: %v", name, err)
		}
	}
	st := readStatus(t, be, "prod-b1.xbs.dbcms")
	if st.Succeeded || !strings.Contains(st.Error, "exit status 1") {
		t.Fatalf("status %+v", st)
	}
	c := &Checker{Backend: be}
	ok, why, _ := c.Check(ctx, "op", runner.Step{}, runner.Condition{Kind: CondBackupComplete, Target: "prod-b1.xbs.dbcms"})
	if ok || !strings.Contains(why, "failed") {
		t.Fatalf("gate: %v %s", ok, why)
	}
}

func TestBackupJobWithoutGaleraInfoWarns(t *testing.T) {
	key := testKey(t)
	be := backup.NewMemBackend()
	files := fstest.MapFS{}
	tools := newFakeTools(files, payload(t, 100))
	tools.noGalera = true
	env, _ := jobEnv(t, be, key, tools, files)
	if err := RunJob(context.Background(), physicalJobArgs(t, key), env); err != nil {
		t.Fatal(err)
	}
	m, _ := readManifest(context.Background(), be, "prod-b1.xbs.dbcms")
	if m.Galera != nil || len(m.Warnings) != 1 {
		t.Fatalf("%+v", m)
	}
}

// TestBackupJobMariaDB11FileNames: MariaDB 11.4.13 writes
// mariadb_backup_checkpoints and mariadb_backup_galera_info (lab E4: the
// job failed after a complete upload looking for xtrabackup_checkpoints).
func TestBackupJobMariaDB11FileNames(t *testing.T) {
	key := testKey(t)
	be := backup.NewMemBackend()
	files := fstest.MapFS{}
	tools := newFakeTools(files, payload(t, 100))
	tools.lsnPrefix = "mariadb_backup_"
	env, _ := jobEnv(t, be, key, tools, files)
	if err := RunJob(context.Background(), physicalJobArgs(t, key), env); err != nil {
		t.Fatal(err)
	}
	m, _ := readManifest(context.Background(), be, "prod-b1.xbs.dbcms")
	if m.Checkpoints == nil || m.Checkpoints.ToLSN != 4711 || m.Galera == nil || len(m.Warnings) != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestLogicalBackupJob(t *testing.T) {
	key := testKey(t)
	be := backup.NewMemBackend()
	files := fstest.MapFS{}
	tools := newFakeTools(files, []byte("CREATE TABLE t (id int);\n"))
	tools.version = "mariadb-dump  Ver 10.19 Distrib 11.4.3-MariaDB, for Linux (x86_64)"
	env, _ := jobEnv(t, be, key, tools, files)
	a := agent.RunBackupArgs{JobName: "dbcm-job-b1-shop", Tool: agent.ToolMariadbDump, BackupID: "b1", Object: "prod-b1-db-shop.sql.dbcms",
		Expected: agent.Identity{InstallationID: testInst, ClusterID: testCluster, NodeUUID: uuid2}, Database: "shop",
		DBHost: "dbcm-prod-n2", RecipientKey: pubString(t, key)}
	if err := RunJob(context.Background(), agent.BackupJobArgs(a, "db-cred", "backend-cfg"), env); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(context.Background(), be, a.Object)
	if err != nil || m.Method != MethodLogical || m.Database != "shop" || m.Checkpoints != nil {
		t.Fatalf("%+v %v", m, err)
	}
	if !tools.called("mariadb-dump --defaults-extra-file=/run/secrets/db-cred --host=dbcm-prod-n2 --single-transaction") ||
		tools.called("mariadb-dump --databases") {
		t.Fatalf("calls %v", tools.calls)
	}
}

func TestPhysicalRestoreJobRoundTrip(t *testing.T) {
	key := testKey(t)
	be, data, _ := runPhysicalBackup(t, key)
	files := fstest.MapFS{}
	tools := newFakeTools(files, nil)
	env, removed := jobEnv(t, be, key, tools, files)
	args := restoreJobArgs(agent.RestorePhysical, "prod-b1.xbs.dbcms")
	if err := RunJob(context.Background(), args, env); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tools.stdin["mbstream"], data) {
		t.Fatal("mbstream did not receive the exact backup stream")
	}
	if !tools.called("mbstream -x -C /dbcm/data") || !tools.called("mariabackup --prepare --target-dir=/dbcm/data") {
		t.Fatalf("calls %v", tools.calls)
	}
	for _, f := range galeraStateFiles {
		if !slices.Contains(*removed, "/dbcm/data/"+f) {
			t.Fatalf("galera state %s not removed: %v", f, *removed)
		}
	}
	st := readStatus(t, be, "dbcm-job-b1-restore")
	if !st.Succeeded || !st.StreamVerified {
		t.Fatalf("status %+v", st)
	}
	c := &Checker{Backend: be}
	if ok, why, _ := c.Check(context.Background(), "op", runner.Step{}, runner.Condition{Kind: CondRestoreComplete, Target: "dbcm-job-b1-restore"}); !ok {
		t.Fatal(why)
	}
}

// TestRestoreJobAcceptsFormat1Manifest: a backup whose manifest has the
// older plaintext chunk hashes (format 1) still restores and is verified
// against the stream; the new backup writes format 2 without them.
func TestRestoreJobAcceptsFormat1Manifest(t *testing.T) {
	key := testKey(t)
	ctx := context.Background()
	obj := "prod-b1.xbs.dbcms"
	be, _, _ := runPhysicalBackup(t, key)
	m, err := readManifest(ctx, be, obj)
	if err != nil || m.Format != ManifestFormat || m.Stream.PlainSHA256 != "" || m.Stream.Chunks[0].SHA256 != "" {
		t.Fatalf("new manifest %+v %v", m, err)
	}
	rc, err := be.Get(ctx, obj)
	if err != nil {
		t.Fatal(err)
	}
	sr, err := backup.OpenStream(rc, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, sr); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	v1 := sr.Summary()
	v1.PlainMAC = ""
	for i := range v1.Chunks {
		v1.Chunks[i].MAC = ""
	}
	m.Format, m.Stream = ManifestFormatV1, v1
	putManifest(t, be, m)
	files := fstest.MapFS{}
	env, _ := jobEnv(t, be, key, newFakeTools(files, nil), files)
	if err := RunJob(ctx, restoreJobArgs(agent.RestorePhysical, obj), env); err != nil {
		t.Fatalf("format 1 restore: %v", err)
	}
	m.Stream.Chunks[1].SHA256 = strings.Repeat("0", 64)
	putManifest(t, be, m)
	files = fstest.MapFS{}
	env, _ = jobEnv(t, be, key, newFakeTools(files, nil), files)
	if err := RunJob(ctx, restoreJobArgs(agent.RestorePhysical, obj), env); !errors.Is(err, backup.ErrIntegrity) {
		t.Fatalf("tampered format 1 manifest: %v", err)
	}
}

func putManifest(t *testing.T, be backup.Backend, m Manifest) {
	t.Helper()
	data, _ := json.Marshal(m)
	if err := be.Put(context.Background(), ManifestName(m.Object), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreJobRefusals(t *testing.T) {
	key := testKey(t)
	ctx := context.Background()
	obj := "prod-b1.xbs.dbcms"
	run := func(t *testing.T, be backup.Backend, k backup.RecoveryKey, args []string) (error, *fakeTools) {
		files := fstest.MapFS{}
		tools := newFakeTools(files, nil)
		env, _ := jobEnv(t, be, k, tools, files)
		return RunJob(ctx, args, env), tools
	}

	t.Run("manifest digests tampered", func(t *testing.T) {
		be, _, _ := runPhysicalBackup(t, key)
		m, _ := readManifest(ctx, be, obj)
		m.Stream.Chunks[1].MAC = strings.Repeat("0", 64)
		putManifest(t, be, m)
		err, tools := run(t, be, key, restoreJobArgs(agent.RestorePhysical, obj))
		if !errors.Is(err, backup.ErrIntegrity) || tools.called("mariabackup --prepare") {
			t.Fatalf("got %v", err)
		}
		if st := readStatus(t, be, "dbcm-job-b1-restore"); st.Succeeded || st.StreamVerified {
			t.Fatalf("status %+v", st)
		}
	})
	t.Run("backup of another cluster", func(t *testing.T) {
		be, _, _ := runPhysicalBackup(t, key)
		args := replaceArg(restoreJobArgs(agent.RestorePhysical, obj), "--source-cluster-id=", "--source-cluster-id=cluster-other")
		if err, _ := run(t, be, key, args); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("wrong recovery key", func(t *testing.T) {
		be, _, _ := runPhysicalBackup(t, key)
		if err, tools := run(t, be, testKey(t), restoreJobArgs(agent.RestorePhysical, obj)); !errors.Is(err, backup.ErrDecrypt) || tools.called("mbstream") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("stream truncated", func(t *testing.T) {
		be, _, _ := runPhysicalBackup(t, key)
		rc, _ := be.Get(ctx, obj)
		raw, _ := io.ReadAll(rc)
		_ = be.Put(ctx, obj, bytes.NewReader(raw[:len(raw)-100]))
		if err, tools := run(t, be, key, restoreJobArgs(agent.RestorePhysical, obj)); !errors.Is(err, backup.ErrCorrupt) || tools.called("mariabackup --prepare") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("logical mode for physical backup", func(t *testing.T) {
		be, _, _ := runPhysicalBackup(t, key)
		if err, _ := run(t, be, key, restoreJobArgs(agent.RestoreLogical, obj)); !errors.Is(err, ErrJob) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no manifest", func(t *testing.T) {
		if err, _ := run(t, backup.NewMemBackend(), key, restoreJobArgs(agent.RestorePhysical, obj)); !errors.Is(err, ErrInvalidManifest) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestLogicalRestoreJobCreatesTargetThenLoads(t *testing.T) {
	key := testKey(t)
	be := backup.NewMemBackend()
	files := fstest.MapFS{}
	dump := []byte("CREATE TABLE t (id int);\nINSERT INTO t VALUES (1);\n")
	tools := newFakeTools(files, dump)
	env, _ := jobEnv(t, be, key, tools, files)
	a := agent.RunBackupArgs{JobName: "dbcm-job-b1-shop", Tool: agent.ToolMariadbDump, BackupID: "b1", Object: "obj-shop",
		Expected: agent.Identity{InstallationID: testInst, ClusterID: testCluster, NodeUUID: uuid2}, Database: "shop",
		DBHost: "dbcm-prod-n2", RecipientKey: pubString(t, key)}
	if err := RunJob(context.Background(), agent.BackupJobArgs(a, "db-cred", "backend-cfg"), env); err != nil {
		t.Fatal(err)
	}
	if err := RunJob(context.Background(), restoreJobArgs(agent.RestoreLogical, "obj-shop"), env); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tools.stdin["mariadb"], dump) {
		t.Fatalf("loaded %q", tools.stdin["mariadb"])
	}
	if !tools.called("mariadb --defaults-extra-file=/run/secrets/db-cred --host=dbcm-prod-n1 --execute=CREATE DATABASE IF NOT EXISTS `shop_copy`") ||
		!tools.called("mariadb --defaults-extra-file=/run/secrets/db-cred --host=dbcm-prod-n1 --default-character-set=utf8mb4 --batch --database=shop_copy") {
		t.Fatalf("calls %v", tools.calls)
	}
}

func TestJobEnvRefusesUnlistedTool(t *testing.T) {
	env := JobEnv{Runner: newFakeTools(fstest.MapFS{}, nil)}
	if err := env.run(context.Background(), []string{"sh", "-c", "id"}, nil, io.Discard); !errors.Is(err, ErrJob) {
		t.Fatalf("got %v", err)
	}
}

func TestParseBackendConfig(t *testing.T) {
	good := map[string]string{
		"s3":   `{"type":"s3","s3":{"region":"eu","bucket":"b","access_key":"a","secret_key":"s"}}`,
		"sftp": `{"type":"sftp","sftp":{"addr":"h:22","user":"u","dir":"/b","host_key_fingerprint":"SHA256:x","password":"p"}}`,
		"dir":  `{"type":"dir","dir":"` + t.TempDir() + `"}`,
	}
	for name, js := range good {
		c, err := ParseBackendConfig([]byte(js))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := c.Open(); err != nil {
			t.Fatalf("%s open: %v", name, err)
		}
	}
	for _, js := range []string{`{"type":"s3"}`, `{"type":"dir","dir":"/x","s3":{}}`, `{"type":"ftp"}`, `{"type":"dir","dir":"/x","insecure":true}`} {
		if _, err := ParseBackendConfig([]byte(js)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: %v", js, err)
		}
	}
	// SFTP without host key verification is refused by the backend.
	c, _ := ParseBackendConfig([]byte(`{"type":"sftp","sftp":{"addr":"h:22","user":"u","dir":"/b","password":"p"}}`))
	if _, err := c.Open(); err == nil {
		t.Fatal("sftp without host key accepted")
	}
}
