// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const testPW = "Xy7-secret_pw.42"

func sqlOf(st []BootstrapStatement) []string {
	out := make([]string, len(st))
	for i, s := range st {
		out[i] = s.Reveal()
	}
	return out
}

func redactedOf(st []BootstrapStatement) []string {
	out := make([]string, len(st))
	for i, s := range st {
		out[i] = s.Redacted
	}
	return out
}

func TestRenderBootstrapMonitor(t *testing.T) {
	st, err := RenderBootstrap(BootstrapParams{Template: TemplateCreateMonitorUser, User: "dbcm_monitor", Host: "localhost"}, NewSecret(testPW))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CREATE USER IF NOT EXISTS 'dbcm_monitor'@'localhost' IDENTIFIED BY '" + testPW + "'",
		"ALTER USER 'dbcm_monitor'@'localhost' IDENTIFIED BY '" + testPW + "' REQUIRE NONE",
		"REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'dbcm_monitor'@'localhost'",
	}
	if got := sqlOf(st); !slices.Equal(got, want) {
		t.Fatalf("sql =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range redactedOf(st) {
		if strings.Contains(r, testPW) {
			t.Errorf("redacted form leaks the password: %s", r)
		}
	}
	if !strings.Contains(st[0].Redacted, "IDENTIFIED BY '***'") {
		t.Errorf("redacted = %s", st[0].Redacted)
	}
	// No GRANT: SHOW GLOBAL STATUS needs no privilege (USAGE only, ADR 0008).
	for _, s := range sqlOf(st) {
		if strings.HasPrefix(s, "GRANT") {
			t.Errorf("monitor account got a grant: %s", s)
		}
	}
	// Formatting a statement never reveals the password.
	for _, s := range st {
		if f := fmt.Sprintf("%v %+v %s %#v", s, s, s, s); strings.Contains(f, testPW) {
			t.Errorf("formatted statement leaks the password: %s", f)
		}
	}
}

func TestRenderBootstrapMonitorRequireSSL(t *testing.T) {
	st, err := RenderBootstrap(BootstrapParams{Template: TemplateCreateMonitorUser, User: "dbcm_monitor", Host: "%", RequireSSL: true}, NewSecret(testPW))
	if err != nil {
		t.Fatal(err)
	}
	if got := st[1].Redacted; got != "ALTER USER 'dbcm_monitor'@'%' IDENTIFIED BY '***' REQUIRE SSL" {
		t.Errorf("alter = %s", got)
	}
}

func TestRenderBootstrapBackup(t *testing.T) {
	st, err := RenderBootstrap(BootstrapParams{Template: TemplateCreateBackupUser, User: "dbcm_backup", Host: "%"}, NewSecret(testPW))
	if err != nil {
		t.Fatal(err)
	}
	got := redactedOf(st)
	want := []string{
		"CREATE USER IF NOT EXISTS 'dbcm_backup'@'%' IDENTIFIED BY '***'",
		"ALTER USER 'dbcm_backup'@'%' IDENTIFIED BY '***' REQUIRE NONE",
		"REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'dbcm_backup'@'%'",
		"GRANT RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR ON *.* TO 'dbcm_backup'@'%'",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sql =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	st, err = RenderBootstrap(BootstrapParams{Template: TemplateCreateBackupUser, User: "dbcm_backup", Host: "%", Logical: true}, NewSecret(testPW))
	if err != nil {
		t.Fatal(err)
	}
	if g := st[3].Redacted; g != "GRANT RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR, SELECT, SHOW VIEW, TRIGGER, EVENT, SHOW CREATE ROUTINE ON *.* TO 'dbcm_backup'@'%'" {
		t.Errorf("logical grant = %s", g)
	}
}

func TestRenderBootstrapRootPassword(t *testing.T) {
	st, err := RenderBootstrap(BootstrapParams{Template: TemplateSetRootPassword, User: "root", Host: "localhost"}, NewSecret(testPW))
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].Redacted != "ALTER USER 'root'@'localhost' IDENTIFIED BY '***'" || !strings.Contains(st[0].Reveal(), testPW) {
		t.Fatalf("root password = %+v", redactedOf(st))
	}
	// Default user and host.
	st, err = RenderBootstrap(BootstrapParams{Template: TemplateSetRootPassword}, NewSecret(testPW))
	if err != nil || st[0].Redacted != "ALTER USER 'root'@'localhost' IDENTIFIED BY '***'" {
		t.Fatalf("defaults: %v %v", err, redactedOf(st))
	}
}

func TestRenderBootstrapRefusals(t *testing.T) {
	cases := map[string]struct {
		p  BootstrapParams
		pw string
	}{
		"unknown template":      {BootstrapParams{Template: "drop-database", User: "x", Host: "%"}, testPW},
		"raw sql as template":   {BootstrapParams{Template: "SELECT 1", User: "x", Host: "%"}, testPW},
		"empty template":        {BootstrapParams{User: "x", Host: "%"}, testPW},
		"injection in user":     {BootstrapParams{Template: TemplateCreateMonitorUser, User: "a'@'%' IDENTIFIED BY 'x'; --", Host: "%"}, testPW},
		"injection in host":     {BootstrapParams{Template: TemplateCreateMonitorUser, User: "m", Host: "%'; DROP USER root; --"}, testPW},
		"empty user":            {BootstrapParams{Template: TemplateCreateBackupUser, Host: "%"}, testPW},
		"protected user":        {BootstrapParams{Template: TemplateCreateBackupUser, User: "root", Host: "%"}, testPW},
		"mysql socket user":     {BootstrapParams{Template: TemplateCreateMonitorUser, User: "mysql", Host: "localhost"}, testPW},
		"root password for x":   {BootstrapParams{Template: TemplateSetRootPassword, User: "admin", Host: "localhost"}, testPW},
		"root password host":    {BootstrapParams{Template: TemplateSetRootPassword, User: "root", Host: "10.0.0.1"}, testPW},
		"quote in password":     {BootstrapParams{Template: TemplateCreateMonitorUser, User: "m", Host: "%"}, "abc'defgh"},
		"backslash in password": {BootstrapParams{Template: TemplateCreateMonitorUser, User: "m", Host: "%"}, `abc\defgh`},
		"newline in password":   {BootstrapParams{Template: TemplateCreateMonitorUser, User: "m", Host: "%"}, "abcdefgh\n"},
		"short password":        {BootstrapParams{Template: TemplateCreateMonitorUser, User: "m", Host: "%"}, "short"},
		"logical on monitor":    {BootstrapParams{Template: TemplateCreateMonitorUser, User: "m", Host: "%", Logical: true}, testPW},
		"ssl on root password":  {BootstrapParams{Template: TemplateSetRootPassword, RequireSSL: true}, testPW},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			st, err := RenderBootstrap(c.p, NewSecret(c.pw))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, statements %v", err, redactedOf(st))
			}
			if c.pw != "" && strings.Contains(err.Error(), c.pw) {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

func TestBootstrapTemplatesAllowList(t *testing.T) {
	want := []string{TemplateCreateBackupUser, TemplateCreateMonitorUser, TemplateSetRootPassword}
	if got := BootstrapTemplates(); !slices.Equal(got, want) {
		t.Errorf("templates %v, want %v", got, want)
	}
}
