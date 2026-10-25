// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// ErrJob reports an invalid job invocation or a failed job.
var ErrJob = errors.New("dbbackup: job")

// JobSpec is the parsed job invocation (the argv the agent built with
// agent.BackupJobArgs or agent.RestoreJobArgs).
type JobSpec struct {
	Kind            string // "backup" or "restore"
	Job             string
	Tool            agent.BackupTool
	Mode            agent.RestoreMode
	BackupID        string
	Object          string
	ClusterID       string
	NodeUUID        string
	DBHost          string
	DBOptionFile    string
	BackendConfig   string
	Recipient       string
	DataDir         string
	Database        string
	ApprovalID      string
	SourceClusterID string
	TargetClusterID string
	KeyFile         string
	TargetDatabase  string
}

var jobFlags = map[string]map[string]bool{
	"backup": {"job": true, "tool": true, "backup-id": true, "object": true, "cluster-id": true, "node-uuid": true,
		"db-host": true, "db-option-file": true, "backend-config": true, "recipient": true, "datadir": true, "database": true},
	"restore": {"job": true, "mode": true, "backup-id": true, "object": true, "approval-id": true, "source-cluster-id": true,
		"backend-config": true, "key-file": true, "datadir": true, "target-cluster-id": true, "database": true,
		"target-database": true, "db-host": true, "db-option-file": true},
}

// ParseJobArgs parses the job argv strictly: only the known "--name=value"
// flags of the job kind, each once, all values non-empty, secret files below
// /run/secrets, the datadir fixed.
func ParseJobArgs(args []string) (JobSpec, error) {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrJob, fmt.Sprintf(f, a...)) }
	if len(args) == 0 {
		return JobSpec{}, bad("no job kind")
	}
	known, ok := jobFlags[args[0]]
	if !ok {
		return JobSpec{}, bad("unknown job kind %q", args[0])
	}
	v := map[string]string{}
	for _, a := range args[1:] {
		k, val, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !strings.HasPrefix(a, "--") || !ok || !known[k] {
			return JobSpec{}, bad("unexpected argument %q", a)
		}
		if _, dup := v[k]; dup || val == "" {
			return JobSpec{}, bad("flag --%s repeated or empty", k)
		}
		v[k] = val
	}
	s := JobSpec{Kind: args[0], Job: v["job"], Tool: agent.BackupTool(v["tool"]), Mode: agent.RestoreMode(v["mode"]),
		BackupID: v["backup-id"], Object: v["object"], ClusterID: v["cluster-id"], NodeUUID: v["node-uuid"],
		DBHost: v["db-host"], DBOptionFile: v["db-option-file"], BackendConfig: v["backend-config"], Recipient: v["recipient"],
		DataDir: v["datadir"], Database: v["database"], ApprovalID: v["approval-id"], SourceClusterID: v["source-cluster-id"],
		TargetClusterID: v["target-cluster-id"], KeyFile: v["key-file"], TargetDatabase: v["target-database"]}
	need := func(names ...string) error {
		for _, n := range names {
			if v[n] == "" {
				return bad("--%s required", n)
			}
		}
		return nil
	}
	if err := need("job", "backup-id", "object", "backend-config"); err != nil {
		return JobSpec{}, err
	}
	if !backupIDRe.MatchString(s.BackupID) {
		return JobSpec{}, bad("backup id %q", s.BackupID)
	}
	var err error
	switch {
	case s.Kind == "backup" && s.Tool == agent.ToolMariabackup:
		err = need("cluster-id", "node-uuid", "db-host", "db-option-file", "recipient", "datadir")
		if v["database"] != "" {
			err = bad("physical backup takes no database")
		}
	case s.Kind == "backup" && s.Tool == agent.ToolMariadbDump:
		err = need("cluster-id", "node-uuid", "db-host", "db-option-file", "recipient", "database")
		if v["datadir"] != "" {
			err = bad("logical backup takes no datadir")
		}
	case s.Kind == "restore" && s.Mode == agent.RestorePhysical:
		err = need("approval-id", "source-cluster-id", "key-file", "datadir", "target-cluster-id")
		if s.TargetClusterID == s.SourceClusterID {
			err = bad("target cluster equals the source cluster")
		}
	case s.Kind == "restore" && s.Mode == agent.RestoreLogical:
		err = need("approval-id", "source-cluster-id", "key-file", "database", "target-database", "db-host", "db-option-file")
	default:
		err = bad("tool %q / mode %q not allow-listed for %s", s.Tool, s.Mode, s.Kind)
	}
	if err != nil {
		return JobSpec{}, err
	}
	if s.DataDir != "" && s.DataDir != agent.JobDataDir {
		return JobSpec{}, bad("datadir must be %s", agent.JobDataDir)
	}
	for _, p := range []string{s.DBOptionFile, s.BackendConfig, s.KeyFile} {
		if p != "" && (path.Clean(p) != p || path.Dir(p) != agent.SecretsDir) {
			return JobSpec{}, bad("secret file %q must be directly below %s", p, agent.SecretsDir)
		}
	}
	for _, d := range []string{s.Database, s.TargetDatabase} {
		if d != "" && !schemaNameRe.MatchString(d) {
			return JobSpec{}, bad("database %q", d)
		}
	}
	return s, nil
}

// ToolRunner runs one allow-listed tool. There is no shell: argv[0] is the
// binary. Implementations must kill the process and return an error when
// stdin returns an error, and must return an error for a non-zero exit.
type ToolRunner interface {
	Run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error
	// Output returns combined stdout and stderr (for --version).
	Output(ctx context.Context, argv []string) ([]byte, error)
}

// AllowedTools are the only binaries a job runs.
var AllowedTools = []string{"mariabackup", "mbstream", "mariadb-dump", "mariadb"}

// JobEnv supplies the job's side effects; tests use fakes.
type JobEnv struct {
	Runner ToolRunner
	// Files reads secret and checkpoint files (absolute paths without the
	// leading slash, as fs.FS requires); Remove deletes a file.
	Files  fs.FS
	Remove func(path string) error
	// Backend opens the backend; default BackendConfig.Open.
	Backend func(BackendConfig) (backup.Backend, error)
	Now     func() time.Time
	// TempDir is scratch space for mariabackup's --target-dir and
	// --extra-lsndir.
	TempDir string
	// Catalog optionally captures schema metadata from the source.
	Catalog func(ctx context.Context, s JobSpec) (*Catalog, error)
}

func (e JobEnv) run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	if len(argv) == 0 || !slices.Contains(AllowedTools, argv[0]) {
		return fmt.Errorf("%w: tool %v is not allow-listed", ErrJob, argv)
	}
	return e.Runner.Run(ctx, argv, stdin, stdout)
}

func (e JobEnv) read(p string) ([]byte, error) {
	return fs.ReadFile(e.Files, strings.TrimPrefix(path.Clean(p), "/"))
}

// lsnFilePrefixes are the names mariabackup gives its --extra-lsndir
// files: MariaDB 11.x writes mariadb_backup_<name> (lab E4, 11.4.13: the
// job failed looking for xtrabackup_checkpoints), earlier versions
// xtrabackup_<name>.
var lsnFilePrefixes = []string{"mariadb_backup_", "xtrabackup_"}

// readLSN reads the --extra-lsndir file name under either prefix.
func (e JobEnv) readLSN(name string) ([]byte, error) {
	var first error
	for _, p := range lsnFilePrefixes {
		raw, err := e.read(path.Join(e.TempDir, "lsn", p+name))
		if err == nil {
			return raw, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, first
}

// BackupArgv is the tool invocation of a backup job. The option file
// (credentials) is always the first argument, as the tools require.
func BackupArgv(s JobSpec, tempDir string) []string {
	if s.Tool == agent.ToolMariabackup {
		return []string{"mariabackup", "--defaults-extra-file=" + s.DBOptionFile, "--host=" + s.DBHost,
			"--backup", "--stream=xbstream", "--galera-info",
			"--datadir=" + s.DataDir, "--target-dir=" + path.Join(tempDir, "target"), "--extra-lsndir=" + path.Join(tempDir, "lsn")}
	}
	// One database without --databases: the dump has no CREATE DATABASE or
	// USE, so a selective restore can load it under another name.
	return []string{"mariadb-dump", "--defaults-extra-file=" + s.DBOptionFile, "--host=" + s.DBHost,
		"--single-transaction", "--skip-lock-tables", "--hex-blob", "--default-character-set=utf8mb4", "--quick",
		"--routines", "--triggers", "--events", "--", s.Database}
}

// RunJob runs one job and always tries to write its status report.
func RunJob(ctx context.Context, args []string, env JobEnv) error {
	s, err := ParseJobArgs(args)
	if err != nil {
		return err
	}
	if env.Runner == nil || env.Files == nil {
		return fmt.Errorf("%w: runner and files required", ErrJob)
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Backend == nil {
		env.Backend = BackendConfig.Open
	}
	raw, err := env.read(s.BackendConfig)
	if err != nil {
		return fmt.Errorf("%w: backend config: %v", ErrJob, err)
	}
	cfg, err := ParseBackendConfig(raw)
	if err != nil {
		return err
	}
	be, err := env.Backend(cfg)
	if err != nil {
		return err
	}
	st := JobStatus{Job: s.Job, BackupID: s.BackupID}
	key := s.Job
	if s.Kind == "backup" {
		key = s.Object
		_, err = runBackupJob(ctx, s, env, be)
	} else {
		st.StreamVerified, err = runRestoreJob(ctx, s, env, be)
	}
	st.Finished, st.Succeeded = env.Now().UTC(), err == nil
	if err != nil {
		st.Error = err.Error()
	}
	data, _ := json.Marshal(st)
	// The status is written even when ctx was cancelled.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if perr := be.Put(wctx, StatusName(key), bytes.NewReader(data)); perr != nil && err == nil {
		err = fmt.Errorf("%w: write status: %v", ErrJob, perr)
	}
	return err
}

func runBackupJob(ctx context.Context, s JobSpec, env JobEnv, be backup.Backend) (Manifest, error) {
	pub, err := backup.ParseStreamPublicKey(s.Recipient)
	if err != nil {
		return Manifest{}, err
	}
	out, err := env.Runner.Output(ctx, []string{string(s.Tool), "--version"})
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %s --version: %v", ErrJob, s.Tool, err)
	}
	version, err := ParseToolVersion(string(out))
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Format: ManifestFormat, BackupID: s.BackupID, ClusterID: s.ClusterID, SourceNodeUUID: s.NodeUUID,
		Object: s.Object, Started: env.Now().UTC(), Tool: ToolInfo{Name: string(s.Tool), Version: version}, Method: MethodPhysical}
	if s.Tool == agent.ToolMariadbDump {
		m.Method, m.Database = MethodLogical, s.Database
	}
	if env.Catalog != nil {
		if m.Catalog, err = env.Catalog(ctx, s); err != nil {
			return Manifest{}, fmt.Errorf("%w: catalog: %v", ErrJob, err)
		}
	}
	meta := backup.StreamMeta{Kind: string(m.Method), BackupID: s.BackupID, ClusterID: s.ClusterID,
		SourceNodeUUID: s.NodeUUID, Database: s.Database, Created: m.Started}
	pr, pw := io.Pipe()
	type produced struct {
		sum backup.StreamSummary
		err error
	}
	done := make(chan produced, 1)
	// The physical stream is scanned for the Galera info file on its way to
	// the sealing writer (MariaDB 11.4 does not copy it to --extra-lsndir).
	var tap *galeraInfoTap
	if m.Method == MethodPhysical {
		tap = newGaleraInfoTap()
	}
	go func() {
		var p produced
		sw, err := backup.NewSealWriter(pw, meta, pub)
		if err == nil {
			var out io.Writer = sw
			if tap != nil {
				out = io.MultiWriter(sw, tap)
			}
			err = env.run(ctx, BackupArgv(s, env.TempDir), nil, out)
			if err == nil {
				err = sw.Close()
				p.sum = sw.Summary()
			}
		}
		// A tool failure reaches PutStream as a read error, so the backend
		// never commits a partial object.
		_ = pw.CloseWithError(err)
		p.err = err
		done <- p
	}()
	res, err := backup.PutStream(ctx, be, s.Object, pr)
	_ = pr.CloseWithError(errors.New("upload ended"))
	p := <-done
	if err != nil {
		return Manifest{}, err
	}
	if p.err != nil {
		return Manifest{}, p.err
	}
	sum := p.sum
	if res.SHA256 != sum.CipherSHA256 || res.Size != sum.CipherSize {
		return Manifest{}, fmt.Errorf("%w: stored digest differs from the sealed stream", ErrJob)
	}
	m.Stream, m.Stored, m.Finished = sum, res, env.Now().UTC()
	if m.Method == MethodPhysical {
		raw, err := env.readLSN("checkpoints")
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: checkpoints: %v", ErrJob, err)
		}
		cp, err := ParseCheckpoints(string(raw))
		if err != nil {
			return Manifest{}, err
		}
		m.Checkpoints = &cp
		if raw, err := env.readLSN("binlog_info"); err == nil {
			pos, err := ParseBinlogInfo(string(raw))
			if err != nil {
				return Manifest{}, err
			}
			m.BinlogBase = &pos
		}
		if raw, err := env.readLSN("galera_info"); err == nil {
			gp, err := ParseGaleraInfo(string(raw))
			if err != nil {
				return Manifest{}, err
			}
			gp.Source = GaleraFromLSNDir
			m.Galera = &gp
		} else if raw, ok := tap.Content(); ok {
			if gp, err := ParseGaleraInfo(raw); err == nil {
				gp.Source = GaleraFromStream
				m.Galera = &gp
			} else {
				m.Warnings = append(m.Warnings, "galera_info in the stream is not parseable; Galera position not recorded")
			}
		} else {
			m.Warnings = append(m.Warnings, "galera_info found neither in --extra-lsndir nor in the stream; Galera position not recorded")
		}
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	// The manifest is written last: its presence marks a complete backup.
	if err := be.Put(ctx, ManifestName(s.Object), bytes.NewReader(data)); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// trackReader records the first error of the stream reader so a tool that
// ignores a stdin error cannot turn a corrupt stream into success.
type trackReader struct {
	r   io.Reader
	err error
}

func (t *trackReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF && t.err == nil {
		t.err = err
	}
	return n, err
}

// galeraStateFiles are removed after a physical restore so the node starts a
// new Galera history and never resumes the source cluster's state.
var galeraStateFiles = []string{"grastate.dat", "gvwstate.dat", "galera.cache"}

func runRestoreJob(ctx context.Context, s JobSpec, env JobEnv, be backup.Backend) (bool, error) {
	raw, err := env.read(s.KeyFile)
	if err != nil {
		return false, fmt.Errorf("%w: key file: %v", ErrJob, err)
	}
	key, err := backup.ParseRecoveryKey(string(raw))
	if err != nil {
		return false, err
	}
	m, err := readManifest(ctx, be, s.Object)
	if err != nil {
		return false, err
	}
	want := MethodPhysical
	if s.Mode == agent.RestoreLogical {
		want = MethodLogical
	}
	switch {
	case m.BackupID != s.BackupID || m.Object != s.Object:
		return false, fmt.Errorf("%w: manifest is for backup %s object %s", ErrIdentityMismatch, m.BackupID, m.Object)
	case m.ClusterID != s.SourceClusterID:
		return false, fmt.Errorf("%w: backup from %s, restore expects %s", ErrIdentityMismatch, m.ClusterID, s.SourceClusterID)
	case m.Method != want:
		return false, fmt.Errorf("%w: %s backup cannot be restored in %s mode", ErrJob, m.Method, s.Mode)
	case want == MethodLogical && m.Database != s.Database:
		return false, fmt.Errorf("%w: export holds %s, not %s", ErrJob, m.Database, s.Database)
	}
	rc, err := be.Get(ctx, s.Object)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	sr, err := backup.OpenStream(rc, key)
	if err != nil {
		return false, err
	}
	if meta := sr.Meta(); meta.BackupID != m.BackupID || meta.ClusterID != m.ClusterID || meta.Kind != string(m.Method) {
		return false, fmt.Errorf("%w: stream header does not match the manifest", ErrIdentityMismatch)
	}
	tr := &trackReader{r: sr}
	if want == MethodPhysical {
		err = env.run(ctx, []string{"mbstream", "-x", "-C", s.DataDir}, tr, io.Discard)
	} else {
		base := []string{"mariadb", "--defaults-extra-file=" + s.DBOptionFile, "--host=" + s.DBHost}
		if err = env.run(ctx, append(slices.Clone(base), "--execute=CREATE DATABASE IF NOT EXISTS `"+s.TargetDatabase+"`"), nil, io.Discard); err == nil {
			err = env.run(ctx, append(base, "--default-character-set=utf8mb4", "--batch", "--database="+s.TargetDatabase), tr, io.Discard)
		}
	}
	if err == nil {
		err = tr.err
	}
	if err == nil {
		// The whole stream, including the final chunk, must have been read.
		if _, rerr := io.ReadFull(sr, make([]byte, 1)); rerr != io.EOF {
			err = fmt.Errorf("%w: the tool stopped before the end of the stream (%v)", ErrJob, rerr)
		}
	}
	if err != nil {
		return false, err
	}
	if err := backup.CompareSummary(sr.Summary(), m.Stream); err != nil {
		return false, err
	}
	if want == MethodPhysical {
		if err := env.run(ctx, []string{"mariabackup", "--prepare", "--target-dir=" + s.DataDir}, nil, io.Discard); err != nil {
			return true, err
		}
		for _, f := range galeraStateFiles {
			if err := env.Remove(path.Join(s.DataDir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return true, fmt.Errorf("%w: remove %s: %v", ErrJob, f, err)
			}
		}
	}
	return true, nil
}

func readManifest(ctx context.Context, be backup.Backend, object string) (Manifest, error) {
	rc, err := be.Get(ctx, ManifestName(object))
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest of %s: %v", ErrInvalidManifest, object, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxMetaObject))
	if err != nil {
		return Manifest{}, err
	}
	return DecodeManifest(data)
}
