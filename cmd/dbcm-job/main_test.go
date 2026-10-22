// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tool writes an executable shell script named like an allow-listed tool.
func tool(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerPipesStdinAndStdout(t *testing.T) {
	dir := t.TempDir()
	tool(t, dir, "mbstream", "exec /bin/cat")
	var out bytes.Buffer
	r := execRunner{dirs: []string{dir}}
	if err := r.Run(context.Background(), []string{"mbstream", "-x"}, strings.NewReader("payload"), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "payload" {
		t.Fatalf("out %q", out.String())
	}
}

func TestRunnerRefusesUnlistedOrPathTools(t *testing.T) {
	dir := t.TempDir()
	tool(t, dir, "sh", "true")
	r := execRunner{dirs: []string{dir}}
	for _, argv := range [][]string{{"sh", "-c", "id"}, {dir + "/sh"}, {"../mariadb"}, {}, {"mariadb"}} {
		if err := r.Run(context.Background(), argv, nil, io.Discard); err == nil {
			t.Errorf("%v accepted", argv)
		}
	}
	// A non-executable file is not a tool.
	if err := os.WriteFile(filepath.Join(dir, "mariadb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Output(context.Background(), []string{"mariadb", "--version"}); err == nil {
		t.Error("non-executable tool ran")
	}
}

func TestRunnerNonZeroExitIsError(t *testing.T) {
	dir := t.TempDir()
	tool(t, dir, "mariadb-dump", "echo partial; exit 3")
	err := execRunner{dirs: []string{dir}}.Run(context.Background(), []string{"mariadb-dump"}, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v", err)
	}
}

type failingReader struct{ sent bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		return copy(p, "partial"), nil
	}
	return 0, errors.New("decryption failed")
}

// A read error on stdin kills the tool and is reported, even though the
// tool itself would have exited cleanly on EOF.
func TestRunnerInputErrorKillsTool(t *testing.T) {
	dir := t.TempDir()
	tool(t, dir, "mariadb", "/bin/cat >/dev/null; exec /bin/sleep 30")
	start := time.Now()
	err := execRunner{dirs: []string{dir}}.Run(context.Background(), []string{"mariadb"}, &failingReader{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "decryption failed") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("tool was not killed")
	}
}

func TestOutputCombines(t *testing.T) {
	dir := t.TempDir()
	tool(t, dir, "mariabackup", "echo out; echo err >&2")
	b, err := execRunner{dirs: []string{dir}}.Output(context.Background(), []string{"mariabackup", "--version"})
	if err != nil || !strings.Contains(string(b), "out") || !strings.Contains(string(b), "err") {
		t.Fatalf("output %q %v", b, err)
	}
}

func TestRunRejectsBadArgs(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"drop-everything"}, &stderr); code != 1 || !strings.Contains(stderr.String(), "unknown job kind") {
		t.Fatalf("code %d stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"-version"}, &stderr); code != 0 || !strings.HasPrefix(stderr.String(), "dbcm-job ") {
		t.Fatalf("version: %d %q", code, stderr.String())
	}
}
