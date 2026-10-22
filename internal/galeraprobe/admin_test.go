// SPDX-License-Identifier: AGPL-3.0-only

package galeraprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe/fakemysql"
)

const adminPassword = "Root-pw_for.tests1"

func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ga")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "mysqld.sock")
}

func drain(f *fakemysql.Server) []string {
	var out []string
	for {
		select {
		case q := <-f.QueryLog:
			out = append(out, q)
		default:
			return out
		}
	}
}

func TestAdminExecRunsStatementsInOrder(t *testing.T) {
	sock := shortSocket(t)
	srv := fakemysql.NewUnix(t, sock, "root", adminPassword, nil)
	stmts := []string{"CREATE USER IF NOT EXISTS 'm'@'localhost' IDENTIFIED BY 'Mon-pw_12345'", "REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'm'@'localhost'"}
	if err := (Admin{}).Exec(context.Background(), sock, "root", adminPassword, stmts); err != nil {
		t.Fatal(err)
	}
	got := slices.DeleteFunc(drain(srv), func(q string) bool { return !strings.Contains(q, "'m'@") })
	if !slices.Equal(got, stmts) {
		t.Fatalf("queries %q", got)
	}
	if srv.Accepted.Load() != 1 {
		t.Errorf("%d connections, want one for all statements", srv.Accepted.Load())
	}
}

func TestAdminExecErrorsAreRedacted(t *testing.T) {
	sock := shortSocket(t)
	srv := fakemysql.NewUnix(t, sock, "root", adminPassword, nil, fakemysql.WithFailOn("GRANT"))
	err := (Admin{}).Exec(context.Background(), sock, "root", adminPassword,
		[]string{"SELECT 1", "GRANT RELOAD ON *.* TO 'b'@'%' /* " + adminPassword + " */", "SELECT 2"})
	if err == nil {
		t.Fatal("failing statement succeeded")
	}
	if strings.Contains(err.Error(), adminPassword) {
		t.Fatalf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "statement 2 of 3") {
		t.Errorf("error does not name the statement position: %v", err)
	}
	for _, q := range drain(srv) {
		if q == "SELECT 2" {
			t.Error("statements after a failure were executed")
		}
	}
	// Wrong admin password: access denied, redacted.
	err = (Admin{}).Exec(context.Background(), sock, "root", "Wrong-pw_123456", []string{"SELECT 1"})
	if !errors.Is(err, ErrAccessDenied) || strings.Contains(err.Error(), "Wrong-pw_123456") {
		t.Fatalf("auth failure: %v", err)
	}
	if err := (Admin{}).Exec(context.Background(), sock, "root", adminPassword, nil); err == nil {
		t.Error("empty statement list accepted")
	}
}
