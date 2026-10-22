// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/dbadmin"
)

// SQLExecutor runs statements in order on one connection to the database
// unix socket, authenticated as user with password. Implementations must
// not put the password or the statement text into errors; the agent
// redacts known secrets again before it records a message.
type SQLExecutor interface {
	Exec(ctx context.Context, socket, user, password string, statements []string) error
}

// CodeInvalidSecret: a secret file's content fails the template's rules.
const CodeInvalidSecret Code = "invalid-secret"

// DefaultAdminUser is the account exec-sql connects as.
const DefaultAdminUser = "root"

const maxSecretSize = 4 << 10

// placeholderPassword satisfies dbadmin.ValidatePassword; decoding renders
// the template with it so that every template error is an invalid command.
const placeholderPassword = "Placeholder-0000"

func checkExecSQL(a *ExecSQLArgs, volumeBase string) error {
	if err := checkVolumeRoot(a.VolumeRoot, volumeBase); err != nil {
		return err
	}
	if !secretRe.MatchString(a.PasswordSecret) || !secretRe.MatchString(a.AdminSecret) {
		return invalid("password_secret %q and admin_secret %q must be secret names", a.PasswordSecret, a.AdminSecret)
	}
	if a.AdminUser != "" {
		if err := dbadmin.ValidateUserName(a.AdminUser); err != nil {
			return invalid("admin_user: %v", err)
		}
	}
	if _, err := dbadmin.RenderBootstrap(a.params(), dbadmin.NewSecret(placeholderPassword)); err != nil {
		return invalid("sql template: %v", err)
	}
	return nil
}

func (a ExecSQLArgs) params() dbadmin.BootstrapParams {
	return dbadmin.BootstrapParams{Template: a.Template, User: a.User, Host: a.Host, RequireSSL: a.RequireSSL, Logical: a.Logical}
}

// execSQL verifies the node identity, confines the socket to the verified
// volume root, reads both passwords from the secrets directory, renders the
// template and runs it. The recorded result holds only redacted statements.
func (e *Executor) execSQL(ctx context.Context, a ExecSQLArgs) Result {
	if e.cfg.SQL == nil {
		return Result{Status: StatusRefused, Code: CodeNotConfigured, Message: "agent has no SQL executor configured"}
	}
	root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	if err := Verify(root, a.Expected); err != nil {
		return outcome(err)
	}
	// The socket must exist and, with every symlink resolved, stay inside
	// this node's volume: a link to another node's socket would run the
	// statements on the wrong database.
	socket, err := filepath.EvalSymlinks(filepath.Join(root, DefaultSocket))
	if err != nil {
		return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: "database socket: " + err.Error()}
	}
	if !within(root, socket) {
		return Result{Status: StatusRefused, Code: CodeInvalidPath,
			Message: fmt.Sprintf("database socket resolves to %s outside the volume %s", socket, root)}
	}
	adminPW, res, ok := e.readSecret(a.AdminSecret)
	if !ok {
		return res
	}
	pw, res, ok := e.readSecret(a.PasswordSecret)
	if !ok {
		return res
	}
	redact := func(msg string) string {
		for _, s := range []string{adminPW, pw} {
			if s != "" {
				msg = strings.ReplaceAll(msg, s, "***")
			}
		}
		return msg
	}
	stmts, err := dbadmin.RenderBootstrap(a.params(), dbadmin.NewSecret(pw))
	if err != nil {
		return Result{Status: StatusRefused, Code: CodeInvalidSecret, Message: redact(fmt.Sprintf("secret %s: %v", a.PasswordSecret, err))}
	}
	run := make([]string, len(stmts))
	out := ExecSQLOutput{Template: a.Template, Statements: make([]string, len(stmts))}
	for i, s := range stmts {
		run[i], out.Statements[i] = s.Reveal(), s.Redacted
	}
	user := a.AdminUser
	if user == "" {
		user = DefaultAdminUser
	}
	if err := e.cfg.SQL.Exec(ctx, socket, user, adminPW, run); err != nil {
		if ctx.Err() != nil {
			return Result{Status: StatusUnknown, Code: CodeInDoubt, Message: ctx.Err().Error()}
		}
		return Result{Status: StatusFailed, Code: CodeDBUnavailable, Message: redact(err.Error())}
	}
	return succeeded(out)
}

// readSecret reads one secret file from the secrets directory through an
// os.Root (no "..", no symlink out of it), bounded in size, with one
// trailing newline removed.
func (e *Executor) readSecret(name string) (string, Result, bool) {
	fail := func(detail string) (string, Result, bool) {
		return "", Result{Status: StatusRefused, Code: CodeNotConfigured,
			Message: fmt.Sprintf("secret %s is not available to the agent (%s); the agent service must mount it", name, detail)}, false
	}
	r, err := os.OpenRoot(e.cfg.SecretsDir)
	if err != nil {
		return fail("secrets directory unreadable")
	}
	defer r.Close()
	f, err := r.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fail("missing")
		}
		return fail("not readable inside the secrets directory")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return fail("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSecretSize+1))
	if err != nil || len(b) > maxSecretSize {
		return fail("unreadable or too large")
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if s == "" {
		return fail("empty")
	}
	return s, Result{}, true
}
