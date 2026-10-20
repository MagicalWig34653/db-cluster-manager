// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalid is wrapped by every input validation failure.
var ErrInvalid = errors.New("dbadmin: invalid input")

// ValidationError describes a rejected input.
type ValidationError struct {
	Kind   string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("dbadmin: invalid %s: %s", e.Kind, e.Reason)
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

func invalid(kind, format string, a ...any) error {
	return &ValidationError{Kind: kind, Reason: fmt.Sprintf(format, a...)}
}

// The allowlists are intentionally stricter than MariaDB. Names must not start
// with '-' so that they can never be mistaken for command-line options.
var (
	reDBName   = regexp.MustCompile(`^[A-Za-z0-9_$][A-Za-z0-9_$-]{0,63}$`)
	reUserName = regexp.MustCompile(`^[A-Za-z0-9_$][A-Za-z0-9_.$-]{0,79}$`)
	reHost     = regexp.MustCompile(`^[A-Za-z0-9_%:][A-Za-z0-9_.%:-]{0,254}$`)
)

func validate(kind string, re *regexp.Regexp, s string) error {
	if s == "" {
		return invalid(kind, "must not be empty")
	}
	if len(s) > 255 {
		return invalid(kind, "too long (%d bytes)", len(s))
	}
	if !re.MatchString(s) {
		return invalid(kind, "%s contains characters outside the allowlist or has a bad length", safeQuote(s))
	}
	return nil
}

// safeQuote renders untrusted input for error messages.
func safeQuote(s string) string {
	if len(s) > 32 {
		s = s[:32] + "..."
	}
	return fmt.Sprintf("%q", s)
}

// ValidateDatabaseName checks a database (or table) name.
func ValidateDatabaseName(s string) error { return validate("database name", reDBName, s) }

// ValidateTableName checks a table name.
func ValidateTableName(s string) error { return validate("table name", reDBName, s) }

// ValidateUserName checks an account user name. The anonymous user is rejected.
func ValidateUserName(s string) error { return validate("user name", reUserName, s) }

// ValidateHost checks the host part of an account.
func ValidateHost(s string) error { return validate("host", reHost, s) }

// quoteIdentRaw quotes an already validated identifier with backticks.
func quoteIdentRaw(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// QuoteIdent validates and backtick-quotes a database or table name.
func QuoteIdent(s string) (string, error) {
	if err := ValidateDatabaseName(s); err != nil {
		return "", err
	}
	return quoteIdentRaw(s), nil
}

// quoteStringRaw quotes a validated value as a single-quoted SQL string.
func quoteStringRaw(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "'" + s + "'"
}

// QuoteAccount validates and renders 'user'@'host'.
func QuoteAccount(user, host string) (string, error) {
	if err := ValidateUserName(user); err != nil {
		return "", err
	}
	if err := ValidateHost(host); err != nil {
		return "", err
	}
	return quoteStringRaw(user) + "@" + quoteStringRaw(host), nil
}

// quoteGrantDB quotes a database name for GRANT/REVOKE. In these statements
// '_' and '%' are pattern characters unless escaped with a backslash.
func quoteGrantDB(db string) (string, error) {
	if err := ValidateDatabaseName(db); err != nil {
		return "", err
	}
	r := strings.NewReplacer("_", `\_`, "%", `\%`)
	return quoteIdentRaw(r.Replace(db)), nil
}

var (
	protectedDatabases = map[string]bool{
		"mysql": true, "information_schema": true, "performance_schema": true, "sys": true,
	}
	protectedUsers = map[string]bool{
		"root": true, "mariadb.sys": true, "mysql": true,
	}
)

// ErrProtected is returned when an operation targets a system object.
var ErrProtected = errors.New("dbadmin: protected system object")
