// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

const (
	testInst    = "inst-1"
	testCluster = "cluster-prod"
	uuid1       = "00000000-0000-4000-8000-000000000001"
	uuid2       = "00000000-0000-4000-8000-000000000002"
	uuid3       = "00000000-0000-4000-8000-000000000003"
	gib         = int64(1) << 30
)

var t0 = time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)

func healthy(size int) galera.NodeHealth {
	return galera.NodeHealth{State: galera.HealthHealthy, ClusterSize: size, ClusterStatus: "Primary", LocalState: "Synced", Ready: true, Connected: true}
}

func threeNodes() []NodeState {
	free := galera.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}
	return []NodeState{
		{Slot: "n1", NodeUUID: uuid1, HostID: "h1", Volume: "dbcm-prod-n1-00000000", Health: healthy(3), Writer: true, HostFree: free},
		{Slot: "n2", NodeUUID: uuid2, HostID: "h2", Volume: "dbcm-prod-n2-00000000", Health: healthy(3), HostFree: free},
		{Slot: "n3", NodeUUID: uuid3, HostID: "h3", Volume: "dbcm-prod-n3-00000000", Health: healthy(3), HostFree: free},
	}
}

func testKey(t *testing.T) backup.RecoveryKey {
	t.Helper()
	k, err := backup.GenerateRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pubString(t *testing.T, k backup.RecoveryKey) string {
	t.Helper()
	p, err := k.StreamPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return p.String()
}

// fakeTools simulates the allow-listed tools. mariabackup --backup writes
// payload to stdout and the checkpoint files into files; mbstream and
// mariadb record what they read.
type fakeTools struct {
	mu       sync.Mutex
	files    fstest.MapFS
	payload  []byte
	failTool string // tool name that exits non-zero
	failAt   int    // fail after writing this many bytes (with failTool)
	noGalera bool
	// lsnPrefix names the --extra-lsndir files ("" = xtrabackup_, the
	// pre-11 names; MariaDB 11.x writes mariadb_backup_).
	lsnPrefix string
	calls     [][]string
	stdin     map[string][]byte
	binlogOut []byte // what mariadb-binlog prints
	// probeFailures is how many "SELECT 1" probes fail before the instance answers.
	probeFailures int
	version       string
}

func newFakeTools(files fstest.MapFS, payload []byte) *fakeTools {
	return &fakeTools{files: files, payload: payload, stdin: map[string][]byte{}, version: "mariabackup based on MariaDB server 11.4.3-MariaDB Linux (x86_64)"}
}

func (f *fakeTools) Output(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	return []byte(f.version), nil
}

func (f *fakeTools) Run(_ context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	fail := f.failTool == argv[0]
	f.mu.Unlock()
	switch argv[0] {
	case "mariabackup", "mariadb-dump":
		if len(argv) > 1 && argv[1] == "--prepare" {
			return nil
		}
		if fail {
			_, _ = stdout.Write(f.payload[:f.failAt])
			return errors.New("exit status 1")
		}
		if _, err := io.Copy(stdout, bytes.NewReader(f.payload)); err != nil {
			return err
		}
		if argv[0] == "mariabackup" {
			var lsn string
			for _, a := range argv {
				if v, ok := strings.CutPrefix(a, "--extra-lsndir="); ok {
					lsn = strings.TrimPrefix(v, "/")
				}
			}
			pre := f.lsnPrefix
			if pre == "" {
				pre = "xtrabackup_"
			}
			f.mu.Lock()
			f.files[lsn+"/"+pre+"checkpoints"] = &fstest.MapFile{Data: []byte("backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 4711\nlast_lsn = 4720\n")}
			if !f.noGalera {
				f.files[lsn+"/"+pre+"galera_info"] = &fstest.MapFile{Data: []byte("6f2b1a3c-9d8e-11ef-8a1b-0242ac120002:1234\n")}
			}
			f.mu.Unlock()
		}
		return nil
	case "mariadb-binlog":
		if fail {
			return errors.New("exit status 3")
		}
		_, err := stdout.Write(f.binlogOut)
		return err
	case "mbstream", "mariadb":
		if stdin == nil {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.probeFailures > 0 && slices.Contains(argv, "--execute=SELECT 1") {
				f.probeFailures--
				return errors.New("can't connect")
			}
			return nil
		}
		data, err := io.ReadAll(stdin)
		f.mu.Lock()
		f.stdin[argv[0]] = data
		f.mu.Unlock()
		if err != nil {
			return err
		}
		if fail {
			return errors.New("exit status 2")
		}
		return nil
	}
	return fmt.Errorf("fake: unexpected tool %s", argv[0])
}

func (f *fakeTools) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

// jobEnv returns an environment whose secret files hold the backend config
// (a MemBackend shared through the returned pointer), the DB option file
// and the recovery key.
func jobEnv(t *testing.T, be backup.Backend, key backup.RecoveryKey, tools *fakeTools, files fstest.MapFS) (JobEnv, *[]string) {
	t.Helper()
	files["run/secrets/backend-cfg"] = &fstest.MapFile{Data: []byte(`{"type":"dir","dir":"/unused"}`)}
	files["run/secrets/db-cred"] = &fstest.MapFile{Data: []byte("[client]\nuser=\"backup\"\npassword=\"x\"\n")}
	files["run/secrets/restore-key"] = &fstest.MapFile{Data: []byte(key.String())}
	var removed []string
	return JobEnv{
		Runner:  tools,
		Files:   files,
		Remove:  func(p string) error { removed = append(removed, p); return nil },
		Backend: func(BackendConfig) (backup.Backend, error) { return be, nil },
		Now:     func() time.Time { return t0 },
		TempDir: "/tmp/job",
	}, &removed
}

// nopDocker is a DockerAPI that knows no objects; with it the agent
// executor exercises command decoding and guards without effects.
type nopDocker struct{}

func (nopDocker) CreateService(context.Context, agent.ServiceSpec) (string, error) {
	return "", errors.New("nop")
}
func (nopDocker) RemoveService(context.Context, string) error          { return agent.ErrObjectNotFound }
func (nopDocker) ScaleService(context.Context, string, uint64) error   { return agent.ErrObjectNotFound }
func (nopDocker) RemoveVolume(context.Context, string) error           { return agent.ErrObjectNotFound }
func (nopDocker) RemoveSecret(context.Context, string) error           { return agent.ErrObjectNotFound }
func (nopDocker) CreateVolume(context.Context, agent.VolumeSpec) error { return errors.New("nop") }
func (nopDocker) UpdateService(context.Context, string, agent.ServiceSpec) error {
	return agent.ErrObjectNotFound
}
func (nopDocker) Inspect(context.Context, agent.ObjectKind, string) (agent.Object, error) {
	return agent.Object{}, agent.ErrObjectNotFound
}
func (nopDocker) ListByLabel(context.Context, agent.ObjectKind, string, string) ([]agent.Object, error) {
	return nil, nil
}

// requireAgentAccepts sends every command step to a real agent executor and
// fails if the agent rejects its shape (ErrInvalidCommand). Guards may
// refuse (nothing exists), which is fine here.
func requireAgentAccepts(t *testing.T, op runner.Operation, volumeBase string) {
	t.Helper()
	dir := t.TempDir()
	g, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(dir, "term")))
	if err != nil {
		t.Fatal(err)
	}
	e, err := agent.NewExecutor(agent.Config{InstallationID: testInst, StatePath: filepath.Join(dir, "state.json"),
		VolumeBase: volumeBase, Guard: g, Docker: nopDocker{}, NodeID: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range op.Steps {
		if s.Command == "" {
			continue
		}
		_, err := e.Handle(context.Background(), agent.Command{Term: 1, OpID: op.ID, StepID: s.ID, Kind: s.Command, Args: s.Args})
		if errors.Is(err, agent.ErrInvalidCommand) {
			t.Fatalf("agent rejects step %s (%s): %v", s.ID, s.Command, err)
		}
	}
}

func condKinds(s runner.Step) []string {
	var out []string
	for _, c := range s.Preconditions {
		out = append(out, c.Kind)
	}
	return out
}

func (nopDocker) CreateConfig(context.Context, agent.ConfigSpec) (string, error) {
	return "", agent.ErrObjectNotFound
}
func (nopDocker) RemoveConfig(context.Context, string) error { return agent.ErrObjectNotFound }
