// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
)

const (
	adminPW   = "Root-pw_for.tests1"
	monitorPW = "Mon-pw_for.tests22"
)

type sqlCall struct {
	socket, user, password string
	statements             []string
}

type fakeSQL struct {
	mu    sync.Mutex
	calls []sqlCall
	err   error
}

func (s *fakeSQL) Exec(_ context.Context, socket, user, password string, statements []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sqlCall{socket, user, password, statements})
	return s.err
}

// withSQL rebuilds the executor with an SQL port and a secrets directory.
func (f *fixture) withSQL(s SQLExecutor) string {
	f.t.Helper()
	secrets := filepath.Join(f.dir, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		f.t.Fatal(err)
	}
	writeRaw(f.t, filepath.Join(secrets, "root-pw"), adminPW+"\n")
	writeRaw(f.t, filepath.Join(secrets, "monitor-pw"), monitorPW)
	g, err := fencing.NewGuard(fencing.NewFilePersister(f.termPath))
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := NewExecutor(Config{InstallationID: testInstallation, StatePath: f.statePath, VolumeBase: f.base,
		AllowedIDs: []string{"svc-1", "vol-1"}, Guard: g, Docker: f.docker, SQL: s, SecretsDir: secrets})
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec = e
	return secrets
}

// sqlVolume prepares a verified volume with a socket file in run/.
func sqlVolume(t *testing.T, f *fixture) string {
	t.Helper()
	root := f.volume("galera-1")
	writeMarker(t, root, testIdentity())
	writeRaw(t, filepath.Join(root, DefaultSocket), "")
	return root
}

func monitorArgs(root string) ExecSQLArgs {
	return ExecSQLArgs{VolumeRoot: root, Expected: testIdentity(), Template: "create-monitor-user",
		User: "dbcm_monitor", Host: "localhost", PasswordSecret: "monitor-pw", AdminSecret: "root-pw"}
}

func TestExecSQLCreateMonitorUser(t *testing.T) {
	f := newFixture(t)
	db := &fakeSQL{}
	f.withSQL(db)
	root := sqlVolume(t, f)

	r := f.mustDo(1, "op", "mon", KindExecSQL, monitorArgs(root))
	wantStatus(t, r, StatusSucceeded, "")
	if len(db.calls) != 1 {
		t.Fatalf("calls %+v", db.calls)
	}
	c := db.calls[0]
	if c.user != "root" || c.password != adminPW || !strings.HasSuffix(c.socket, "/galera-1/run/mysqld.sock") {
		t.Errorf("connection socket=%s user=%s password-ok=%v", c.socket, c.user, c.password == adminPW)
	}
	if len(c.statements) != 3 || !strings.Contains(c.statements[0], "IDENTIFIED BY '"+monitorPW+"'") ||
		!strings.HasPrefix(c.statements[0], "CREATE USER IF NOT EXISTS 'dbcm_monitor'@'localhost'") {
		t.Errorf("statements %q", c.statements)
	}
	var out ExecSQLOutput
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out.Template != "create-monitor-user" || len(out.Statements) != 3 || !strings.Contains(out.Statements[0], "'***'") {
		t.Errorf("output %+v", out)
	}
	// Neither password appears in the result or in the persisted record.
	state := string(readFile(t, f.statePath))
	for _, pw := range []string{adminPW, monitorPW} {
		if strings.Contains(string(r.Output), pw) || strings.Contains(r.Message, pw) || strings.Contains(state, pw) {
			t.Errorf("password leaked into result or state")
		}
	}
	// Recorded like every effect: a re-sent step replays without a second call.
	r = f.mustDo(1, "op", "mon", KindExecSQL, monitorArgs(root))
	if !r.Replayed || len(db.calls) != 1 {
		t.Fatalf("replay %+v calls %d", r, len(db.calls))
	}
}

func TestExecSQLRedactsDriverErrors(t *testing.T) {
	f := newFixture(t)
	db := &fakeSQL{err: errors.New("Error 1045: Access denied for root using password " + adminPW + " near '" + monitorPW + "'")}
	f.withSQL(db)
	root := sqlVolume(t, f)
	r := f.mustDo(1, "op", "mon", KindExecSQL, monitorArgs(root))
	wantStatus(t, r, StatusFailed, CodeDBUnavailable)
	state := string(readFile(t, f.statePath))
	for _, pw := range []string{adminPW, monitorPW} {
		if strings.Contains(r.Message, pw) || strings.Contains(state, pw) {
			t.Fatalf("password in message %q", r.Message)
		}
	}
	if !strings.Contains(r.Message, "Access denied") {
		t.Errorf("message lost the cause: %q", r.Message)
	}
}

func TestExecSQLRefusesNonTemplates(t *testing.T) {
	f := newFixture(t)
	db := &fakeSQL{}
	f.withSQL(db)
	root := sqlVolume(t, f)
	bad := map[string]func(*ExecSQLArgs){
		"unknown template":   func(a *ExecSQLArgs) { a.Template = "drop-user" },
		"sql as template":    func(a *ExecSQLArgs) { a.Template = "DROP DATABASE shop" },
		"empty template":     func(a *ExecSQLArgs) { a.Template = "" },
		"injected user":      func(a *ExecSQLArgs) { a.User = "x'@'%'; DROP USER root; --" },
		"injected host":      func(a *ExecSQLArgs) { a.Host = "%' OR 1=1" },
		"protected user":     func(a *ExecSQLArgs) { a.User = "root" },
		"secret path":        func(a *ExecSQLArgs) { a.PasswordSecret = "../root-pw" },
		"absolute secret":    func(a *ExecSQLArgs) { a.AdminSecret = "/etc/shadow" },
		"no admin secret":    func(a *ExecSQLArgs) { a.AdminSecret = "" },
		"bad admin user":     func(a *ExecSQLArgs) { a.AdminUser = "root'--" },
		"volume outside":     func(a *ExecSQLArgs) { a.VolumeRoot = f.dir },
		"logical on monitor": func(a *ExecSQLArgs) { a.Logical = true },
	}
	for name, mutate := range bad {
		a := monitorArgs(root)
		mutate(&a)
		if _, err := f.do(1, "op", "bad-"+name, KindExecSQL, a); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Raw SQL or an inline password in the arguments is not even decodable.
	for name, raw := range map[string]string{
		"sql field":      `{"volume_root":"` + root + `","template":"create-monitor-user","user":"m","host":"%","password_secret":"monitor-pw","admin_secret":"root-pw","sql":"DROP DATABASE x"}`,
		"password field": `{"volume_root":"` + root + `","template":"create-monitor-user","user":"m","host":"%","password_secret":"monitor-pw","admin_secret":"root-pw","password":"x"}`,
	} {
		_, err := f.exec.Handle(context.Background(), Command{Term: 1, OpID: "op", StepID: "raw-" + name, Kind: KindExecSQL, Args: json.RawMessage(raw)})
		if !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(db.calls) != 0 {
		t.Fatalf("SQL executed for an invalid command: %+v", db.calls)
	}
}

func TestExecSQLGuards(t *testing.T) {
	f := newFixture(t)
	root := sqlVolume(t, f)
	// Without a configured SQL port the command is refused.
	wantStatus(t, f.mustDo(1, "op", "nocfg", KindExecSQL, monitorArgs(root)), StatusRefused, CodeNotConfigured)

	db := &fakeSQL{}
	secrets := f.withSQL(db)
	// Identity mismatch: refused before any connection.
	a := monitorArgs(root)
	a.Expected.NodeUUID = uuidB
	wantStatus(t, f.mustDo(1, "op", "foreign", KindExecSQL, a), StatusRefused, CodeIdentityMismatch)
	// Missing secret file.
	a = monitorArgs(root)
	a.PasswordSecret = "absent"
	wantStatus(t, f.mustDo(1, "op", "nosecret", KindExecSQL, a), StatusRefused, CodeNotConfigured)
	// A secret that is a symlink out of the secrets directory is refused.
	outside := filepath.Join(f.dir, "outside-pw")
	writeRaw(t, outside, "Outside-pw_12345")
	if err := os.Symlink(outside, filepath.Join(secrets, "link-pw")); err != nil {
		t.Fatal(err)
	}
	a = monitorArgs(root)
	a.PasswordSecret = "link-pw"
	wantStatus(t, f.mustDo(1, "op", "link", KindExecSQL, a), StatusRefused, CodeNotConfigured)
	// A password that fails the template's rules is refused without echo.
	writeRaw(t, filepath.Join(secrets, "quote-pw"), "abc'defghij")
	a = monitorArgs(root)
	a.PasswordSecret = "quote-pw"
	r := f.mustDo(1, "op", "quote", KindExecSQL, a)
	wantStatus(t, r, StatusRefused, CodeInvalidSecret)
	if strings.Contains(r.Message, "abc'defghij") {
		t.Errorf("message echoes the secret: %q", r.Message)
	}
	// Socket missing (database not up) or redirected out of the volume.
	if err := os.Remove(filepath.Join(root, DefaultSocket)); err != nil {
		t.Fatal(err)
	}
	// A missing socket is classified as a database that is not running
	// (lab E4 failcreate: the node's task never started).
	r = f.mustDo(1, "op", "nosock", KindExecSQL, monitorArgs(root))
	wantStatus(t, r, StatusFailed, CodeDBNotRunning)
	if !strings.Contains(r.Message, "not running") || !strings.Contains(r.Message, DefaultSocket) {
		t.Errorf("message %q", r.Message)
	}
	other := f.volume("galera-2")
	writeRaw(t, filepath.Join(other, DefaultSocket), "")
	if err := os.Symlink(filepath.Join(other, DefaultSocket), filepath.Join(root, DefaultSocket)); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.mustDo(1, "op", "othersock", KindExecSQL, monitorArgs(root)), StatusRefused, CodeInvalidPath)
	if len(db.calls) != 0 {
		t.Fatalf("connected despite a refused guard: %d calls", len(db.calls))
	}
	// Fencing applies.
	if _, err := f.do(0, "op", "stale", KindExecSQL, monitorArgs(root)); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("stale term: %v", err)
	}
}
