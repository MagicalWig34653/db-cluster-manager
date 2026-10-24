// SPDX-License-Identifier: AGPL-3.0-only

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Cmd is one restricted command: an absolute binary and arguments, run
// directly (never through a shell), with stdin, a minimal environment, a
// fresh empty working directory, a timeout and output caps.
type Cmd struct {
	Path  string
	Args  []string
	Stdin []byte
	// Env is the complete environment (KEY=VALUE); nothing is inherited.
	Env       []string
	Timeout   time.Duration
	MaxStdout int
	MaxStderr int
}

// CmdResult is the captured output.
type CmdResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner errors.
var (
	ErrCommandRejected = errors.New("ai: command rejected")
	ErrOutputTooLarge  = errors.New("ai: command output exceeds the cap")
	ErrCommandTimeout  = errors.New("ai: command timed out")
	ErrCommandFailed   = errors.New("ai: command failed")
)

// CommandRunner runs a restricted command. ExecRunner is the real one;
// tests use FakeRunner.
type CommandRunner interface {
	Run(ctx context.Context, c Cmd) (CmdResult, error)
}

// CheckCmd enforces the restrictions that do not depend on the runner.
func CheckCmd(c Cmd) error {
	switch {
	case !strings.HasPrefix(c.Path, "/") || strings.Contains(c.Path, ".."):
		return fmt.Errorf("%w: binary must be an absolute path", ErrCommandRejected)
	case c.Timeout <= 0 || c.Timeout > 10*time.Minute:
		return fmt.Errorf("%w: timeout must be between 0 and 10 minutes", ErrCommandRejected)
	case c.MaxStdout <= 0 || c.MaxStderr <= 0 || c.MaxStdout > 8<<20 || c.MaxStderr > 8<<20:
		return fmt.Errorf("%w: output caps must be set (at most 8 MiB)", ErrCommandRejected)
	}
	for _, a := range append([]string{c.Path}, c.Args...) {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("%w: NUL in argument", ErrCommandRejected)
		}
	}
	for _, e := range c.Env {
		if !strings.Contains(e, "=") || strings.ContainsRune(e, 0) {
			return fmt.Errorf("%w: malformed environment entry", ErrCommandRejected)
		}
	}
	return nil
}

// capWriter keeps at most n bytes and records an overflow.
type capWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	n        int
	overflow bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.n - w.buf.Len(); room < len(p) {
		w.overflow = true
		if room > 0 {
			w.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

// ExecRunner runs commands with os/exec in a fresh temporary directory
// that is removed afterwards. The process is killed at the timeout and the
// output beyond the caps is discarded and reported as ErrOutputTooLarge.
type ExecRunner struct {
	// TempRoot is where working directories are created; empty selects
	// the OS temp directory.
	TempRoot string
}

// Run implements CommandRunner.
func (r ExecRunner) Run(ctx context.Context, c Cmd) (CmdResult, error) {
	if err := CheckCmd(c); err != nil {
		return CmdResult{}, err
	}
	dir, err := os.MkdirTemp(r.TempRoot, "dbcm-ai-")
	if err != nil {
		return CmdResult{}, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = dir
	cmd.Env = append([]string{}, c.Env...)
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Stdin = bytes.NewReader(c.Stdin)
	so, se := &capWriter{n: c.MaxStdout}, &capWriter{n: c.MaxStderr}
	cmd.Stdout, cmd.Stderr = so, se
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	res := CmdResult{Stdout: so.buf.Bytes(), Stderr: se.buf.Bytes(), ExitCode: cmd.ProcessState.ExitCode()}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return res, ErrCommandTimeout
	case so.overflow || se.overflow:
		return res, ErrOutputTooLarge
	case err != nil:
		return res, fmt.Errorf("%w: exit code %d", ErrCommandFailed, res.ExitCode)
	}
	return res, nil
}

// FakeRunner records commands and answers with a scripted result (a fake).
type FakeRunner struct {
	mu     sync.Mutex
	Calls  []Cmd
	Result CmdResult
	Err    error
}

// Run implements CommandRunner.
func (f *FakeRunner) Run(_ context.Context, c Cmd) (CmdResult, error) {
	if err := CheckCmd(c); err != nil {
		return CmdResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, c)
	return f.Result, f.Err
}

// cliProvider runs the Claude Code or Codex CLI non-interactively with the
// prompt on stdin. The arguments disable tools (Claude Code: --tools "",
// --strict-mcp-config) or select the read-only sandbox (Codex). They were
// taken from the --help output of the locally installed CLIs; no real
// login was used and no CLI was run by the tests.
type cliProvider struct {
	cfg    ProviderConfig
	runner CommandRunner
}

// MaxCLIOutput caps the CLI's stdout.
const MaxCLIOutput = 1 << 20

func (p *cliProvider) env() []string {
	env := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "NO_COLOR=1"}
	if p.cfg.Home != "" {
		env = append(env, "HOME="+p.cfg.Home)
	}
	return env
}

// args returns the argument list for a model.
func (p *cliProvider) args(r Request) []string {
	switch p.cfg.Kind {
	case KindClaudeCodeCLI:
		return []string{"-p", "--output-format", "json", "--model", r.Model, "--tools", "",
			"--strict-mcp-config", "--no-session-persistence", "--system-prompt", r.System}
	default: // Codex
		return []string{"exec", "--model", r.Model, "--sandbox", "read-only", "--skip-git-repo-check",
			"--ephemeral", "--color", "never", "-"}
	}
}

// Complete implements Provider.
func (p *cliProvider) Complete(ctx context.Context, r Request) (Response, error) {
	stdin := r.User
	if p.cfg.Kind == KindCodexCLI {
		// Codex has no separate system prompt flag here; the fixed
		// instructions precede the data block.
		stdin = r.System + "\n\n" + r.User
	}
	res, err := p.runner.Run(ctx, Cmd{
		Path: p.cfg.Binary, Args: p.args(r), Stdin: []byte(stdin), Env: p.env(),
		Timeout: p.cfg.Timeout(), MaxStdout: MaxCLIOutput, MaxStderr: 64 << 10,
	})
	if err != nil {
		// stderr is not forwarded: it may contain account details.
		return Response{}, err
	}
	if p.cfg.Kind == KindClaudeCodeCLI {
		var out struct {
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
			Usage   struct {
				In  int `json:"input_tokens"`
				Out int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(res.Stdout, &out) != nil || out.IsError {
			return Response{}, ErrResponseMalformed
		}
		return Response{Text: out.Result, Model: r.Model, InputTokens: out.Usage.In, OutputTokens: out.Usage.Out}, nil
	}
	return Response{Text: string(res.Stdout), Model: r.Model}, nil
}

// Models implements Provider: the CLIs have no model listing endpoint, so
// the admin's list is returned.
func (p *cliProvider) Models(context.Context) ([]string, error) {
	return append([]string(nil), p.cfg.Models...), nil
}
