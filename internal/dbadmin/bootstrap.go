// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
)

// Bootstrap statement templates (ADR 0011). The manager never sends SQL to
// an agent: it names one of these templates with structured arguments, the
// agent reads the password from a secret file and renders the statements
// here. Every name passes the dbadmin allowlists and the password passes
// ValidatePassword (no quote, backslash or control character), so the
// rendered text contains no input that could end a literal.
const (
	TemplateCreateMonitorUser = "create-monitor-user"
	TemplateCreateBackupUser  = "create-backup-user"
	TemplateSetRootPassword   = "set-root-password"
)

// BootstrapTemplates returns the allow-listed template names, sorted.
func BootstrapTemplates() []string {
	return []string{TemplateCreateBackupUser, TemplateCreateMonitorUser, TemplateSetRootPassword}
}

// Privileges of the backup account. Physical: what the MariaDB
// documentation ("mariadb-backup Overview", Authentication and Privileges)
// lists for MariaDB 10.5 and later. Logical: the read privileges
// mariadb-dump needs with --single-transaction --routines --triggers
// --events (SHOW CREATE ROUTINE exists since MariaDB 11.3).
const (
	backupPhysicalPrivileges = "RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR"
	backupLogicalPrivileges  = "SELECT, SHOW VIEW, TRIGGER, EVENT, SHOW CREATE ROUTINE"
)

// BootstrapParams are the structured arguments of a template.
type BootstrapParams struct {
	Template   string
	User       string // set-root-password: "root" (default)
	Host       string // set-root-password: "localhost" (default) or "%"
	RequireSSL bool   // create templates only
	Logical    bool   // create-backup-user only
}

// BootstrapStatement is one rendered statement. Reveal returns the text with the
// password; Redacted the same text with the password replaced by ***.
// Formatting a BootstrapStatement prints only the redacted form.
type BootstrapStatement struct {
	sql      string
	Redacted string
}

// Reveal returns the executable statement, password included.
func (s BootstrapStatement) Reveal() string { return s.sql }

func (s BootstrapStatement) String() string { return s.Redacted }
func (s BootstrapStatement) GoString() string {
	return fmt.Sprintf("dbadmin.BootstrapStatement(%q)", s.Redacted)
}
func (s BootstrapStatement) LogValue() slog.Value         { return slog.StringValue(s.Redacted) }
func (s BootstrapStatement) MarshalText() ([]byte, error) { return []byte(s.Redacted), nil }
func (s BootstrapStatement) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprintf(f, "%q", s.Redacted)
		return
	}
	_, _ = io.WriteString(f, s.Redacted)
}

// stmt builds a statement from parts; a Secret part is the password.
func bstmt(parts ...any) BootstrapStatement {
	var full, red strings.Builder
	for _, p := range parts {
		switch v := p.(type) {
		case Secret:
			full.WriteString(quoteStringRaw(v.Reveal()))
			red.WriteString("'***'")
		case string:
			full.WriteString(v)
			red.WriteString(v)
		}
	}
	return BootstrapStatement{sql: full.String(), Redacted: red.String()}
}

// RenderBootstrap renders the statements of an allow-listed template. The
// create templates are idempotent: CREATE USER IF NOT EXISTS, then ALTER
// USER sets the password and TLS requirement, REVOKE ALL removes any
// privilege a pre-existing account had, and only the template's privileges
// are granted again. Errors never contain the password.
func RenderBootstrap(p BootstrapParams, password Secret) ([]BootstrapStatement, error) {
	if !slices.Contains(BootstrapTemplates(), p.Template) {
		return nil, invalid("template", "%s is not an allow-listed bootstrap template", safeQuote(p.Template))
	}
	if err := ValidatePassword(password.Reveal()); err != nil {
		return nil, err
	}
	if p.Template == TemplateSetRootPassword {
		user, host := p.User, p.Host
		if user == "" {
			user = "root"
		}
		if host == "" {
			host = "localhost"
		}
		switch {
		case user != "root":
			return nil, invalid("user", "set-root-password only changes root, not %s", safeQuote(user))
		case host != "localhost" && host != "%":
			return nil, invalid("host", "set-root-password host must be localhost or %%, not %s", safeQuote(host))
		case p.RequireSSL || p.Logical:
			return nil, invalid("template options", "set-root-password takes no require_ssl or logical")
		}
		acct, err := QuoteAccount(user, host)
		if err != nil {
			return nil, err
		}
		return []BootstrapStatement{bstmt("ALTER USER ", acct, " IDENTIFIED BY ", password)}, nil
	}
	if IsProtectedUser(p.User) {
		return nil, invalid("user", "%s is a system account", safeQuote(p.User))
	}
	acct, err := QuoteAccount(p.User, p.Host)
	if err != nil {
		return nil, err
	}
	if p.Logical && p.Template != TemplateCreateBackupUser {
		return nil, invalid("template options", "logical applies to create-backup-user only")
	}
	tls := " REQUIRE NONE"
	if p.RequireSSL {
		tls = " REQUIRE SSL"
	}
	out := []BootstrapStatement{
		bstmt("CREATE USER IF NOT EXISTS ", acct, " IDENTIFIED BY ", password),
		bstmt("ALTER USER ", acct, " IDENTIFIED BY ", password, tls),
		bstmt("REVOKE ALL PRIVILEGES, GRANT OPTION FROM ", acct),
	}
	if p.Template == TemplateCreateBackupUser {
		privs := backupPhysicalPrivileges
		if p.Logical {
			privs += ", " + backupLogicalPrivileges
		}
		out = append(out, bstmt("GRANT ", privs, " ON *.* TO ", acct))
	}
	return out, nil
}
