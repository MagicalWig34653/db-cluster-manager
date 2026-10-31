// SPDX-License-Identifier: AGPL-3.0-only

// Command dbcm-job is the backup and restore job helper (ADR 0009). The
// agent starts it as the command of a one-shot Swarm service built from
// deploy/Dockerfile.jobs, with the argv from agent.BackupJobArgs or
// agent.RestoreJobArgs:
//
//	dbcm-job backup --job=... --tool=mariabackup ... (see internal/dbbackup.ParseJobArgs)
//	dbcm-job restore --job=... --mode=physical ...
//	dbcm-job -version
//
// It only wires the side effects of dbbackup.RunJob: the allow-listed
// tools run without a shell from fixed system directories, files are read
// from the container's file system, scratch space is a private temporary
// directory. The exit status is 0 on success and 1 on failure; the job's
// status report is written to the backend by RunJob in both cases.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

// toolDirs are the only directories tools are resolved in (the mariadb
// image installs them in /usr/bin); $PATH is ignored.
var toolDirs = []string{"/usr/bin", "/usr/local/bin"}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-version" || args[0] == "--version") {
		_, _ = fmt.Fprintln(stderr, "dbcm-job", version.Version, version.Commit)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tmp, err := os.MkdirTemp("", "dbcm-job-")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "dbcm-job: scratch directory:", err)
		return 1
	}
	defer os.RemoveAll(tmp)
	env := dbbackup.JobEnv{
		Runner:  execRunner{dirs: toolDirs},
		Files:   os.DirFS("/"),
		Remove:  os.Remove,
		TempDir: tmp,
		WriteFile: func(name string, r io.Reader) error {
			// Only below the private scratch directory.
			if rel, err := filepath.Rel(tmp, name); err != nil || !filepath.IsLocal(rel) {
				return fmt.Errorf("dbcm-job: %s is outside the scratch directory", name)
			}
			if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, r); err != nil {
				_ = f.Close()
				return err
			}
			return f.Close()
		},
	}
	if err := dbbackup.RunJob(ctx, args, env); err != nil {
		_, _ = fmt.Fprintln(stderr, "dbcm-job:", err)
		return 1
	}
	return 0
}

// execRunner implements dbbackup.ToolRunner with os/exec.
type execRunner struct {
	dirs []string
}

// resolve finds argv0 (a bare allow-listed name) in the fixed directories.
func (r execRunner) resolve(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("dbcm-job: empty command")
	}
	name := argv[0]
	if !slices.Contains(dbbackup.AllowedTools, name) || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("dbcm-job: tool %q is not allow-listed", name)
	}
	for _, d := range r.dirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("dbcm-job: tool %s not found in %v", name, r.dirs)
}

// Run executes the tool. Stdin is copied by the runner (not handed to
// os/exec) so that a read error, for example a failed decryption or a
// truncated download, kills the tool instead of looking like a clean EOF.
func (r execRunner) Run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	bin, err := r.resolve(argv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	cmd.Env = []string{"PATH=" + strings.Join(r.dirs, ":"), "LANG=C.UTF-8", "TZ=UTC"}
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	var in io.WriteCloser
	if stdin != nil {
		if in, err = cmd.StdinPipe(); err != nil {
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("dbcm-job: start %s: %w", argv[0], err)
	}
	copyErr := make(chan error, 1)
	if in != nil {
		go func() {
			_, err := io.Copy(in, stdin)
			_ = in.Close()
			copyErr <- err // buffered: recorded before the kill below
			if err != nil {
				cancel() // kill the tool: its input is incomplete
			}
		}()
	} else {
		copyErr <- nil
	}
	waitErr := cmd.Wait()
	var cErr error
	if waitErr == nil {
		cErr = <-copyErr // the tool consumed its input; wait for the copy's verdict
	} else {
		select {
		case cErr = <-copyErr:
		default: // the tool failed on its own; do not wait for slow input
		}
	}
	if cErr != nil && !errors.Is(cErr, syscall.EPIPE) {
		return fmt.Errorf("dbcm-job: %s input: %w", argv[0], cErr)
	}
	if waitErr != nil {
		return fmt.Errorf("dbcm-job: %s: %w", argv[0], waitErr)
	}
	return nil
}

// Output runs the tool and returns combined stdout and stderr.
func (r execRunner) Output(ctx context.Context, argv []string) ([]byte, error) {
	bin, err := r.resolve(argv)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	cmd.Env = []string{"PATH=" + strings.Join(r.dirs, ":"), "LANG=C.UTF-8", "TZ=UTC"}
	return cmd.CombinedOutput()
}
