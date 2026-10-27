// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// CmdResult is the outcome of a command that ran. A non-zero ExitCode is not
// an error: dnf check-update uses 100 for "updates available".
type CmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner is the only execution boundary of the package managers.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (CmdResult, error)
}

// FS is the read-only file view used for OS detection.
type FS interface {
	ReadFile(path string) ([]byte, error)
	Exists(path string) bool
}

const maxOutput = 8 << 20

type limitBuf struct{ b bytes.Buffer }

func (l *limitBuf) Write(p []byte) (int, error) {
	if room := maxOutput - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

// ExecRunner runs real commands. Only the agent binary uses it; tests use
// FakeRunner. The environment is reduced to a fixed, non-interactive set.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (CmdResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb limitBuf
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "LANG=C", "DEBIAN_FRONTEND=noninteractive"}
	if h := os.Getenv("HOME"); h != "" {
		cmd.Env = append(cmd.Env, "HOME="+h) // dnf keeps a per-user cache below HOME when not root
	}
	err := cmd.Run()
	res := CmdResult{Stdout: out.b.String(), Stderr: errb.b.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

// OSFS reads the real file system below Root ("" means "/").
type OSFS struct{ Root string }

// ReadFile implements FS.
func (f OSFS) ReadFile(p string) ([]byte, error) {
	root := f.Root
	if root == "" {
		root = "/"
	}
	return os.ReadFile(filepath.Join(root, p))
}

// Exists implements FS.
func (f OSFS) Exists(p string) bool {
	root := f.Root
	if root == "" {
		root = "/"
	}
	_, err := os.Stat(filepath.Join(root, p))
	return err == nil
}

// FakeFS is an in-memory FS.
type FakeFS map[string]string

// ReadFile implements FS.
func (f FakeFS) ReadFile(p string) ([]byte, error) {
	v, ok := f[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}

// Exists implements FS.
func (f FakeFS) Exists(p string) bool { _, ok := f[p]; return ok }

// FakeRunner answers commands from a table keyed by the space-joined command
// line. An unknown command fails the call, so a test notices an unexpected
// (for example mutating) command. It records every call.
type FakeRunner struct {
	mu        sync.Mutex
	Responses map[string]CmdResult
	Calls     []string
}

// Run implements Runner.
func (f *FakeRunner) Run(_ context.Context, name string, args ...string) (CmdResult, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, line)
	r, ok := f.Responses[line]
	if !ok {
		return CmdResult{}, fmt.Errorf("fake runner: unexpected command %q", line)
	}
	return r, nil
}

// Called reports whether a recorded command contains substr.
func (f *FakeRunner) Called(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}
