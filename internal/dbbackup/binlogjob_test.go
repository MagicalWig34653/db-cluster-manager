// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// fakeBinlog builds a closed binlog file of n events (20 bytes each): the
// first at first, the last at last, the others at first.
func fakeBinlog(first, last time.Time, n int) []byte {
	out := append([]byte(nil), binlogMagic...)
	for i := 0; i < n; i++ {
		ts := first
		if i == n-1 {
			ts = last
		}
		ev := make([]byte, 20)
		binary.LittleEndian.PutUint32(ev[0:4], uint32(ts.Unix()))
		ev[4] = 2
		binary.LittleEndian.PutUint32(ev[5:9], 1)
		binary.LittleEndian.PutUint32(ev[9:13], 20)
		binary.LittleEndian.PutUint32(ev[13:17], uint32(len(out)+20))
		out = append(out, ev...)
	}
	return out
}

func TestScanBinlog(t *testing.T) {
	b := fakeBinlog(t0, t0.Add(time.Minute), 5)
	first, last, err := scanBinlog(bytes.NewReader(b), int64(len(b)))
	if err != nil || !first.Equal(t0) || !last.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%v %v %v", first, last, err)
	}
	for name, c := range map[string]struct {
		data []byte
		size int64
	}{
		"truncated":   {b[:len(b)-7], int64(len(b))},
		"short size":  {b, int64(len(b)) - 20 + 1},
		"not binlog":  {append([]byte("junk"), b[4:]...), int64(len(b))},
		"empty":       {binlogMagic, 4},
		"bad end pos": {append(append([]byte(nil), b[:4+13]...), append([]byte{0, 0, 0, 0}, b[4+17:]...)...), int64(len(b))},
	} {
		if _, _, err := scanBinlog(bytes.NewReader(c.data), c.size); !errors.Is(err, ErrJob) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

type archiveRig struct {
	be    *backup.MemBackend
	key   backup.RecoveryKey
	files fstest.MapFS
	tools *fakeTools
	env   JobEnv
	args  []string
	data  map[string][]byte // binlog file name -> bytes
}

// newArchiveRig runs a physical backup that recorded the binlog position
// dbcm-bin.000002:400 and prepares the node's closed binlog files 2 and 3 and
// the active file 4.
func newArchiveRig(t *testing.T) *archiveRig {
	t.Helper()
	key := testKey(t)
	be := backup.NewMemBackend()
	files := fstest.MapFS{}
	files["tmp/job/lsn/xtrabackup_binlog_info"] = &fstest.MapFile{Data: []byte("dbcm-bin.000002 400 0-1-5\n")}
	tools := newFakeTools(files, payload(t, 1<<10))
	env, _ := jobEnv(t, be, key, tools, files)
	if err := RunJob(context.Background(), physicalJobArgs(t, key), env); err != nil {
		t.Fatal(err)
	}
	r := &archiveRig{be: be, key: key, files: files, tools: tools, env: env, data: map[string][]byte{}}
	r.addFile(2, t0.Add(-10*time.Minute), t0.Add(5*time.Minute))
	r.addFile(3, t0.Add(5*time.Minute), t0.Add(20*time.Minute))
	r.setIndex(2, 4)
	a := agent.RunBackupArgs{JobName: "dbcm-job-b1-arch-1", Tool: agent.ToolBinlogArchive, BackupID: "b1", Object: "prod-b1.xbs.dbcms",
		Expected: agent.Identity{InstallationID: testInst, ClusterID: testCluster, NodeUUID: uuid2}, DBHost: "dbcm-prod-n2", RecipientKey: pubString(t, key)}
	r.args = agent.BackupJobArgs(a, "db-cred", "backend-cfg")
	return r
}

func (r *archiveRig) addFile(n int, first, last time.Time) {
	name := fmt.Sprintf("dbcm-bin.%06d", n)
	b := fakeBinlog(first, last, 25)
	r.data[name] = b
	r.files["dbcm/data/"+name] = &fstest.MapFile{Data: b}
}

// setIndex lists files from..to; the last one is the active file.
func (r *archiveRig) setIndex(from, to int) {
	var sb strings.Builder
	for n := from; n <= to; n++ {
		fmt.Fprintf(&sb, "./dbcm-bin.%06d\n", n)
		if _, ok := r.files[fmt.Sprintf("dbcm/data/dbcm-bin.%06d", n)]; !ok {
			r.files[fmt.Sprintf("dbcm/data/dbcm-bin.%06d", n)] = &fstest.MapFile{Data: []byte("active")}
		}
	}
	r.files["dbcm/data/dbcm-bin.index"] = &fstest.MapFile{Data: []byte(sb.String())}
}

func (r *archiveRig) archives(t *testing.T) []Manifest {
	t.Helper()
	got, err := ListBinlogArchives(context.Background(), r.be, testCluster, "b1")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestArchiveJobArchivesClosedFilesOnly(t *testing.T) {
	r := newArchiveRig(t)
	ctx := context.Background()
	if err := RunJob(ctx, r.args, r.env); err != nil {
		t.Fatal(err)
	}
	if !r.tools.called("mariadb --defaults-extra-file=/run/secrets/db-cred --host=dbcm-prod-n2 --execute=FLUSH BINARY LOGS") {
		t.Fatalf("log not rotated first: %v", r.tools.calls)
	}
	got := r.archives(t)
	if len(got) != 2 {
		t.Fatalf("archived %d files, want the 2 closed ones (the active file must stay)", len(got))
	}
	b := got[0].Binlog
	if b.Sequence != 1 || b.Start.File != "dbcm-bin.000002" || b.End.Offset != uint64(len(r.data["dbcm-bin.000002"])) || b.HistoryID != uint2 ||
		!b.From.Equal(t0.Add(-10*time.Minute)) || !b.Until.Equal(t0.Add(5*time.Minute)) || b.BasePosition.Offset != 400 {
		t.Fatalf("%+v", b)
	}
	b = got[1].Binlog
	if b.Sequence != 2 || b.Start.File != "dbcm-bin.000003" || !b.From.Equal(t0.Add(5*time.Minute)) || !b.Until.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("%+v", b)
	}
	if st := readStatus(t, r.be, "dbcm-job-b1-arch-1"); !st.Succeeded || !st.StreamVerified {
		t.Fatalf("%+v", st)
	}
	// The archive is the file: decrypts to the same bytes and verifies.
	for _, m := range got {
		rd, check, closeFn, err := openBinlog(ctx, r.be, m, r.key)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rd)
		_ = closeFn()
		if err != nil || check() != nil || !bytes.Equal(data, r.data[m.Binlog.Start.File]) {
			t.Fatalf("segment %s differs: %v", m.Object, err)
		}
	}
	c, err := CoverageOf(mustManifest(t, r), got)
	if err != nil || c.Segments != 2 || !c.Until.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("%+v %v", c, err)
	}
}

const uint2 = uuid2

func mustManifest(t *testing.T, r *archiveRig) Manifest {
	t.Helper()
	m, err := readManifest(context.Background(), r.be, "prod-b1.xbs.dbcms")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestArchiveJobIsIncremental(t *testing.T) {
	r := newArchiveRig(t)
	ctx := context.Background()
	if err := RunJob(ctx, r.args, r.env); err != nil {
		t.Fatal(err)
	}
	before := r.archives(t)
	// Nothing new: succeeds without writing.
	if err := RunJob(ctx, r.args, r.env); err != nil || len(r.archives(t)) != 2 {
		t.Fatalf("idle run: %v", err)
	}
	// File 4 was active and is closed now; 5 is the active one.
	r.addFile(4, t0.Add(20*time.Minute), t0.Add(40*time.Minute))
	r.setIndex(2, 5)
	if err := RunJob(ctx, r.args, r.env); err != nil {
		t.Fatal(err)
	}
	got := r.archives(t)
	if len(got) != 3 || got[2].Binlog.Sequence != 3 || !got[2].Binlog.From.Equal(before[1].Binlog.Until) || !got[2].Binlog.Until.Equal(t0.Add(40*time.Minute)) {
		t.Fatalf("%d archives, last %+v", len(got), got[len(got)-1].Binlog)
	}
	if _, err := ResolvePITR(mustManifest(t, r), got, t0.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveJobRefusesAGap(t *testing.T) {
	r := newArchiveRig(t)
	ctx := context.Background()
	// The node purged file 2 before it was archived.
	r.setIndex(3, 4)
	err := RunJob(ctx, r.args, r.env)
	if !errors.Is(err, ErrJob) || !strings.Contains(err.Error(), "dbcm-bin.000002 is needed next") {
		t.Fatalf("%v", err)
	}
	if len(r.archives(t)) != 0 {
		t.Fatal("archived across a gap")
	}
	if st := readStatus(t, r.be, "dbcm-job-b1-arch-1"); st.Succeeded || !strings.Contains(st.Error, "take a new full backup") {
		t.Fatalf("%+v", st)
	}
}

func TestArchiveJobRefusesInconsistentInput(t *testing.T) {
	ctx := context.Background()
	t.Run("truncated file", func(t *testing.T) {
		r := newArchiveRig(t)
		b := r.data["dbcm-bin.000003"]
		r.files["dbcm/data/dbcm-bin.000003"] = &fstest.MapFile{Data: b[:len(b)-5]}
		if err := RunJob(ctx, r.args, r.env); !errors.Is(err, ErrJob) {
			t.Fatalf("%v", err)
		}
		if got := r.archives(t); len(got) != 1 {
			t.Fatalf("file 2 is intact and stays archived, got %d", len(got))
		}
	})
	t.Run("other node", func(t *testing.T) {
		r := newArchiveRig(t)
		args := replaceArg(r.args, "--node-uuid=", "--node-uuid="+uuid3)
		if err := RunJob(ctx, args, r.env); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("backup without binlog position", func(t *testing.T) {
		be, _, tools := runPhysicalBackup(t, testKey(t))
		_ = tools
		key := testKey(t)
		env, _ := jobEnv(t, be, key, newFakeTools(fstest.MapFS{}, nil), fstest.MapFS{})
		a := agent.RunBackupArgs{JobName: "dbcm-job-b1-arch-1", Tool: agent.ToolBinlogArchive, BackupID: "b1", Object: "prod-b1.xbs.dbcms",
			Expected: agent.Identity{InstallationID: testInst, ClusterID: testCluster, NodeUUID: uuid2}, DBHost: "dbcm-prod-n2", RecipientKey: pubString(t, key)}
		err := RunJob(ctx, agent.BackupJobArgs(a, "db-cred", "backend-cfg"), env)
		if !errors.Is(err, ErrJob) || !strings.Contains(err.Error(), "binary log was off") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("broken chain", func(t *testing.T) {
		r := newArchiveRig(t)
		if err := RunJob(ctx, r.args, r.env); err != nil {
			t.Fatal(err)
		}
		_ = r.be.Delete(ctx, ManifestName(r.archives(t)[0].Object))
		err := RunJob(ctx, r.args, r.env)
		if !errors.Is(err, ErrJob) || !strings.Contains(err.Error(), "archive of backup b1 is broken") {
			t.Fatalf("%v", err)
		}
	})
}

// replayRig archives the rig's two files and prepares a replay job.
type replayRig struct {
	*archiveRig
	written map[string][]byte
	args    func(segments []string, stop string) []string
}

func newReplayRig(t *testing.T) *replayRig {
	t.Helper()
	r := &replayRig{archiveRig: newArchiveRig(t), written: map[string][]byte{}}
	if err := RunJob(context.Background(), r.archiveRig.args, r.env); err != nil {
		t.Fatal(err)
	}
	r.tools.binlogOut = []byte("/* replayed statements */;\n")
	r.env.WriteFile = func(name string, rd io.Reader) error {
		b, err := io.ReadAll(rd)
		r.written[name] = b
		return err
	}
	r.env.Sleep = func(context.Context, time.Duration) error { return nil }
	r.args = func(segments []string, stop string) []string {
		a := agent.RunRestoreArgs{JobName: "dbcm-job-b1-pitr-s1", Mode: agent.RestoreBinlog, BackupID: "b1", Object: "prod-b1.xbs.dbcms",
			ApprovalID: "appr-1", SourceClusterID: testCluster, BackendSecret: "be", KeySecret: "key",
			DBHost: "dbcm-inspect-s1", DBCredentialSecret: "db", Segments: segments, StartOffset: 400, StopDatetime: stop}
		return agent.RestoreJobArgs(a, map[string]string{"be": "backend-cfg", "key": "restore-key", "db": "db-cred"})
	}
	return r
}

func (r *replayRig) objects(t *testing.T) []string {
	var out []string
	for _, m := range r.archives(t) {
		out = append(out, m.Object)
	}
	return out
}

func TestReplayJobAppliesTheChain(t *testing.T) {
	r := newReplayRig(t)
	ctx := context.Background()
	r.tools.probeFailures = 3 // the instance needs a moment
	at := t0.Add(10 * time.Minute)
	if err := RunJob(ctx, r.args(r.objects(t), ReplayStopDatetime(at)), r.env); err != nil {
		t.Fatal(err)
	}
	var binlogCall, dropCall []string
	probes := 0
	for _, c := range r.tools.calls {
		switch {
		case c[0] == "mariadb-binlog":
			binlogCall = c
		case c[0] == "mariadb" && strings.Contains(strings.Join(c, " "), "DROP USER"):
			dropCall = c
		case c[0] == "mariadb" && strings.Contains(strings.Join(c, " "), "SELECT 1"):
			probes++
		}
	}
	wantBinlog := "mariadb-binlog --start-position=400 --stop-datetime=2026-10-22 01:10:01 /tmp/job/binlog/dbcm-bin.000002 /tmp/job/binlog/dbcm-bin.000003"
	if got := strings.Join(binlogCall, " "); got != wantBinlog {
		t.Fatalf("mariadb-binlog call %q", got)
	}
	if probes != 4 {
		t.Fatalf("%d readiness probes, want 3 failures and 1 success", probes)
	}
	if string(r.tools.stdin["mariadb"]) != string(r.tools.binlogOut) {
		t.Fatalf("client received %q", r.tools.stdin["mariadb"])
	}
	if len(dropCall) == 0 || !strings.HasSuffix(dropCall[len(dropCall)-1], "DROP USER IF EXISTS 'dbcm_replay'@'%'") {
		t.Fatalf("replay account not dropped: %v", dropCall)
	}
	for name, want := range r.data {
		if got, ok := r.written["/tmp/job/binlog/"+name]; ok && !bytes.Equal(got, want) {
			t.Fatalf("%s was written differently", name)
		}
	}
	if len(r.written) != 2 {
		t.Fatalf("wrote %d files", len(r.written))
	}
	if st := readStatus(t, r.be, "dbcm-job-b1-pitr-s1"); !st.Succeeded || !st.StreamVerified {
		t.Fatalf("%+v", st)
	}
}

// The job replays in the order of the chain, whatever order it was given.
func TestReplayJobOrdersByTheChain(t *testing.T) {
	r := newReplayRig(t)
	o := r.objects(t)
	if err := RunJob(context.Background(), r.args([]string{o[1], o[0]}, ReplayStopDatetime(t0.Add(10*time.Minute))), r.env); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.tools.calls {
		if c[0] == "mariadb-binlog" && !strings.HasSuffix(strings.Join(c, " "), "dbcm-bin.000002 /tmp/job/binlog/dbcm-bin.000003") {
			t.Fatalf("%v", c)
		}
	}
}

func TestReplayJobRefusesWhatThePlanCouldNotProduce(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(*replayRig) []string{
		"after the archive": func(r *replayRig) []string {
			return r.args(r.objects(t), ReplayStopDatetime(t0.Add(25*time.Minute)))
		},
		"skipped first file": func(r *replayRig) []string {
			return r.args(r.objects(t)[1:], ReplayStopDatetime(t0.Add(10*time.Minute)))
		},
		"another start": func(r *replayRig) []string {
			return replaceArg(r.args(r.objects(t), ReplayStopDatetime(t0.Add(10*time.Minute))), "--start-offset=", "--start-offset=401")
		},
		"foreign object": func(r *replayRig) []string {
			return r.args([]string{"prod-b1.xbs.dbcms"}, ReplayStopDatetime(t0.Add(10*time.Minute)))
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r := newReplayRig(t)
			before := len(r.tools.calls)
			if err := RunJob(ctx, mk(r), r.env); err == nil {
				t.Fatal("accepted")
			}
			for _, c := range r.tools.calls[before:] {
				if c[0] == "mariadb-binlog" || len(r.tools.stdin["mariadb"]) > 0 {
					t.Fatalf("a tool ran before the check: %v", c)
				}
			}
			if st := readStatus(t, r.be, "dbcm-job-b1-pitr-s1"); st.Succeeded {
				t.Fatal("success reported")
			}
		})
	}
	t.Run("production host", func(t *testing.T) {
		r := newReplayRig(t)
		args := replaceArg(r.args(r.objects(t), "2026-10-22 01:10:01"), "--db-host=", "--db-host=dbcm-prod-n1")
		if _, err := ParseJobArgs(args); !errors.Is(err, ErrJob) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("tampered segment", func(t *testing.T) {
		r := newReplayRig(t)
		m := r.archives(t)[1]
		raw, _ := r.be.Get(ctx, m.Object)
		b, _ := io.ReadAll(raw)
		_ = raw.Close()
		b[len(b)-1] ^= 1
		_ = r.be.Put(ctx, m.Object, bytes.NewReader(b))
		if err := RunJob(ctx, r.args(r.objects(t), ReplayStopDatetime(t0.Add(10*time.Minute))), r.env); err == nil {
			t.Fatal("tampered segment replayed")
		}
		if len(r.tools.stdin["mariadb"]) > 0 {
			t.Fatal("client received data from a tampered segment")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		r := newReplayRig(t)
		r.files["run/secrets/restore-key"] = &fstest.MapFile{Data: []byte(testKey(t).String())}
		if err := RunJob(ctx, r.args(r.objects(t), ReplayStopDatetime(t0.Add(10*time.Minute))), r.env); err == nil {
			t.Fatal("replayed with the wrong key")
		}
	})
}

func TestReplayJobFailureLeavesTheAccountAndReports(t *testing.T) {
	r := newReplayRig(t)
	r.tools.failTool = "mariadb-binlog"
	err := RunJob(context.Background(), r.args(r.objects(t), ReplayStopDatetime(t0.Add(10*time.Minute))), r.env)
	if err == nil || !strings.Contains(err.Error(), "mariadb-binlog") {
		t.Fatalf("%v", err)
	}
	if r.tools.called("mariadb --defaults-extra-file=/run/secrets/db-cred --host=dbcm-inspect-s1 --execute=DROP USER") {
		t.Fatal("account dropped after a failed replay")
	}
	if st := readStatus(t, r.be, "dbcm-job-b1-pitr-s1"); st.Succeeded || !strings.Contains(st.Error, "mariadb-binlog") {
		t.Fatalf("%+v", st)
	}
}

func TestReplayStopDatetimeIncludesTheTargetSecond(t *testing.T) {
	for at, want := range map[time.Time]string{
		time.Date(2026, 10, 22, 1, 10, 0, 0, time.UTC):                 "2026-10-22 01:10:01",
		time.Date(2026, 10, 22, 1, 10, 0, 900_000_000, time.UTC):       "2026-10-22 01:10:01",
		time.Date(2026, 10, 22, 23, 59, 59, 0, time.UTC):               "2026-10-23 00:00:00",
		time.Date(2026, 10, 22, 3, 0, 0, 0, time.FixedZone("x", 7200)): "2026-10-22 01:00:01",
	} {
		if got := ReplayStopDatetime(at); got != want {
			t.Fatalf("%v: %s, want %s", at, got, want)
		}
	}
}
