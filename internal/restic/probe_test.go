// SPDX-License-Identifier: AGPL-3.0-only

package restic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type ctxRunner struct {
	calls []string
	block bool // block until the context is done, like restic retrying a 403
}

func (r *ctxRunner) Run(ctx context.Context, req Request) (Result, error) {
	r.calls = append(r.calls, req.Args[2])
	if r.block {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	return Result{Stdout: []byte(`[]`)}, nil
}

func passwordFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(p, []byte("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Lab E11: with wrong S3 credentials restic retried for more than ten minutes
// and the agent step never ended. The probe bounds the repository open.
func TestProbeBoundsUnreachableRepository(t *testing.T) {
	p := passwordFile(t)
	r := &ctxRunner{block: true}
	c, err := New(Config{Backend: BackendLocal, Repository: "/var/backups/restic", PasswordFile: p, ProbeTimeout: 30 * time.Millisecond}, r)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Snapshots(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > 5*time.Second || len(r.calls) != 1 || r.calls[0] != "cat" {
		t.Fatalf("probe not bounded or real command started: %v", r.calls)
	}
	// With a responsive repository the probe runs once, then the command.
	r = &ctxRunner{}
	c, _ = New(Config{Backend: BackendLocal, Repository: "/var/backups/restic", PasswordFile: p, ProbeTimeout: time.Second}, r)
	if _, err := c.Snapshots(context.Background()); err != nil || strings.Join(r.calls, ",") != "cat,snapshots" {
		t.Fatalf("calls=%v err=%v", r.calls, err)
	}
}

// Lab E11: a killed backup reported only "command failed".
func TestCancelledCommandIsReportedAsInterrupted(t *testing.T) {
	r := &ctxRunner{block: true}
	c, _ := New(Config{Backend: BackendLocal, Repository: "/var/backups/restic", PasswordFile: passwordFile(t)}, r)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Backup(ctx, []string{"/srv/data"}, nil)
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("err=%v", err)
	}
}
