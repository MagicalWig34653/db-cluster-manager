// SPDX-License-Identifier: AGPL-3.0-only

package lab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelperProcess is not a real test: it is the child process used by the
// ExecRunner tests, so that no real external tool is ever executed.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("DBCM_LAB_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	switch args[0] {
	case "echo":
		fmt.Fprint(os.Stdout, strings.Join(args[1:], "|"))
		fmt.Fprint(os.Stderr, "ignored-on-success")
		os.Exit(0)
	case "fail":
		fmt.Fprint(os.Stdout, "partial")
		fmt.Fprint(os.Stderr, "something went wrong")
		os.Exit(3)
	}
	os.Exit(2)
}

func helperArgs(args ...string) []string {
	return append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)
}

func TestExecRunnerCapturesStdout(t *testing.T) {
	t.Setenv("DBCM_LAB_HELPER", "1")
	out, err := NewExecRunner().Run(context.Background(), os.Args[0], helperArgs("echo", "a b", "$(x);c")...)
	if err != nil {
		t.Fatal(err)
	}
	if out != "a b|$(x);c" {
		t.Fatalf("argv must be passed verbatim without a shell, got %q", out)
	}
}

func TestExecRunnerIncludesStderrInError(t *testing.T) {
	t.Setenv("DBCM_LAB_HELPER", "1")
	_, err := NewExecRunner().Run(context.Background(), os.Args[0], helperArgs("fail")...)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Fatalf("stderr missing from error: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("exit error not wrapped: %v", err)
	}
}

func TestExecRunnerNotFound(t *testing.T) {
	_, err := NewExecRunner().Run(context.Background(), "dbcm-lab-definitely-missing-tool")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("want exec.ErrNotFound, got %v", err)
	}
}
