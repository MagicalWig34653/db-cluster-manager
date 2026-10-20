// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Scope is the level a grant applies to.
type Scope string

const (
	ScopeGlobal   Scope = "global"
	ScopeDatabase Scope = "database"
	ScopeTable    Scope = "table"
	ScopeRoutine  Scope = "routine"
	ScopeProxy    Scope = "proxy"
	ScopeRole     Scope = "role"
)

// PrivilegeEntry is one privilege, optionally limited to columns.
type PrivilegeEntry struct {
	Name    string
	Columns []string
}

// Grant is one parsed line of SHOW GRANTS.
type Grant struct {
	Scope      Scope
	Database   string // unescaped; "*" for global
	Object     string // table/routine name, "*" for all, or proxied account
	ObjectType string // "", TABLE, FUNCTION, PROCEDURE
	Privileges []PrivilegeEntry
	Roles      []string
	// GrantOption is true for WITH GRANT OPTION (or WITH ADMIN OPTION for roles).
	GrantOption bool
	// DatabasePattern is true when the database name contains unescaped
	// '_' or '%' wildcards and may therefore match several databases.
	DatabasePattern bool
	Raw             string
}

// HasAll reports ALL [PRIVILEGES].
func (g Grant) HasAll() bool {
	for _, p := range g.Privileges {
		if p.Name == "ALL PRIVILEGES" || p.Name == "ALL" {
			return true
		}
	}
	return false
}

// Limits are per-account resource limits; 0 means unlimited.
type Limits struct {
	MaxQueriesPerHour     uint32
	MaxUpdatesPerHour     uint32
	MaxConnectionsPerHour uint32
	MaxUserConnections    uint32
}

// AccountGrants is the parsed result of SHOW GRANTS FOR an account.
type AccountGrants struct {
	User, Host  string
	Grants      []Grant
	Limits      Limits
	RequireSSL  bool
	HasPassword bool
	AuthPlugin  string
}

// DatabaseGrant returns the exact (non-pattern) database-level grant for db.
func (a *AccountGrants) DatabaseGrant(db string) (Grant, bool) {
	for _, g := range a.Grants {
		if g.Scope == ScopeDatabase && g.Database == db && !g.DatabasePattern {
			return g, true
		}
	}
	return Grant{}, false
}

type scanner struct {
	s string
	i int
}

func (c *scanner) space() {
	for c.i < len(c.s) && (c.s[c.i] == ' ' || c.s[c.i] == '\t' || c.s[c.i] == '\n' || c.s[c.i] == '\r') {
		c.i++
	}
}

func (c *scanner) eof() bool { c.space(); return c.i >= len(c.s) }

func isWordByte(b byte) bool {
	return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// keyword consumes kw (case-insensitive) when it is the next whole word.
func (c *scanner) keyword(kw string) bool {
	c.space()
	end := c.i + len(kw)
	if end > len(c.s) || !strings.EqualFold(c.s[c.i:end], kw) {
		return false
	}
	if end < len(c.s) && isWordByte(c.s[end]) {
		return false
	}
	c.i = end
	return true
}

func (c *scanner) char(b byte) bool {
	c.space()
	if c.i < len(c.s) && c.s[c.i] == b {
		c.i++
		return true
	}
	return false
}

// quoted reads '...', `...` or "..." with doubled-quote and backslash escapes
// (backslashes only inside ' and ", never inside backticks).
func (c *scanner) quoted() (string, error) {
	c.space()
	if c.i >= len(c.s) {
		return "", fmt.Errorf("unexpected end")
	}
	q := c.s[c.i]
	if q != '\'' && q != '`' && q != '"' {
		return "", fmt.Errorf("expected quote at %d", c.i)
	}
	c.i++
	var b strings.Builder
	for c.i < len(c.s) {
		ch := c.s[c.i]
		switch {
		case ch == q && c.i+1 < len(c.s) && c.s[c.i+1] == q:
			b.WriteByte(q)
			c.i += 2
		case ch == q:
			c.i++
			return b.String(), nil
		case ch == '\\' && q != '`' && c.i+1 < len(c.s):
			b.WriteByte(c.s[c.i+1])
			c.i += 2
		default:
			b.WriteByte(ch)
			c.i++
		}
	}
	return "", fmt.Errorf("unterminated quote")
}

// namePart reads a backtick-quoted identifier, a bare word or '*'.
func (c *scanner) namePart() (string, error) {
	c.space()
	if c.i >= len(c.s) {
		return "", fmt.Errorf("unexpected end")
	}
	switch c.s[c.i] {
	case '*':
		c.i++
		return "*", nil
	case '`':
		return c.quoted()
	}
	start := c.i
	for c.i < len(c.s) && (isWordByte(c.s[c.i]) || c.s[c.i] == '-') {
		c.i++
	}
	if start == c.i {
		return "", fmt.Errorf("expected identifier at %d", c.i)
	}
	return c.s[start:c.i], nil
}

func (c *scanner) account() (user, host string, err error) {
	if user, err = c.quoted(); err != nil {
		return
	}
	if !c.char('@') {
		return "", "", fmt.Errorf("expected @")
	}
	host, err = c.quoted()
	return
}

// splitTop splits s at sep outside quotes and parentheses.
func splitTop(s string, sep byte) []string {
	var parts []string
	depth, start := 0, 0
	var q byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case q != 0:
			if ch == q {
				q = 0
			}
		case ch == '`' || ch == '\'' || ch == '"':
			q = ch
		case ch == '(':
			depth++
		case ch == ')':
			depth--
		case ch == sep && depth == 0:
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}

// findTop returns the index of word (surrounded by spaces) outside quotes and
// parentheses, or -1.
func findTop(s, word string) int {
	depth := 0
	var q byte
	needle := " " + word + " "
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case q != 0:
			if ch == q {
				q = 0
			}
		case ch == '`' || ch == '\'' || ch == '"':
			q = ch
		case ch == '(':
			depth++
		case ch == ')':
			depth--
		case depth == 0 && len(s)-i >= len(needle) && strings.EqualFold(s[i:i+len(needle)], needle):
			return i
		}
	}
	return -1
}

func parsePrivilegeList(s string) ([]PrivilegeEntry, error) {
	var out []PrivilegeEntry
	for _, item := range splitTop(s, ',') {
		if item == "" {
			return nil, fmt.Errorf("empty privilege")
		}
		e := PrivilegeEntry{}
		if p := strings.IndexByte(item, '('); p >= 0 {
			if !strings.HasSuffix(item, ")") {
				return nil, fmt.Errorf("bad column list")
			}
			e.Name = item[:p]
			for _, col := range splitTop(item[p+1:len(item)-1], ',') {
				cs := &scanner{s: col}
				name, err := cs.namePart()
				if err != nil {
					return nil, err
				}
				e.Columns = append(e.Columns, name)
			}
		} else {
			e.Name = item
		}
		e.Name = strings.Join(strings.Fields(strings.ToUpper(e.Name)), " ")
		out = append(out, e)
	}
	return out, nil
}

var (
	reQuoted   = regexp.MustCompile("'(?:[^'\\\\]|\\\\.|'')*'|`(?:[^`]|``)*`")
	reMaxQ     = regexp.MustCompile(`(?i)\bMAX_QUERIES_PER_HOUR (\d+)`)
	reMaxU     = regexp.MustCompile(`(?i)\bMAX_UPDATES_PER_HOUR (\d+)`)
	reMaxC     = regexp.MustCompile(`(?i)\bMAX_CONNECTIONS_PER_HOUR (\d+)`)
	reMaxUC    = regexp.MustCompile(`(?i)\bMAX_USER_CONNECTIONS (\d+)`)
	reGrantOpt = regexp.MustCompile(`(?i)\b(?:GRANT|ADMIN) OPTION\b`)
	reRequire  = regexp.MustCompile(`(?i)\bREQUIRE (?:SSL|X509|ISSUER|SUBJECT|CIPHER)\b`)
	reVia      = regexp.MustCompile(`(?i)\bIDENTIFIED VIA ([A-Za-z0-9_]+)`)
)

// ParseGrant parses a single SHOW GRANTS line.
func ParseGrant(line string) (Grant, error) {
	g := Grant{Raw: line}
	line = strings.TrimSpace(line)
	if len(line) < 6 || !strings.EqualFold(line[:6], "GRANT ") {
		return g, fmt.Errorf("not a GRANT statement: %s", safeQuote(line))
	}
	s := " " + line[6:]
	var tail string
	if idx := findTop(s, "ON"); idx >= 0 {
		privs, err := parsePrivilegeList(strings.TrimSpace(s[:idx]))
		if err != nil {
			return g, fmt.Errorf("privileges: %w", err)
		}
		g.Privileges = privs
		c := &scanner{s: s, i: idx + len(" ON ")}
		for _, kw := range []string{"TABLE", "FUNCTION", "PROCEDURE"} {
			if c.keyword(kw) {
				g.ObjectType = kw
				break
			}
		}
		c.space()
		if c.i < len(c.s) && (c.s[c.i] == '\'' || c.s[c.i] == '"') {
			u, h, err := c.account()
			if err != nil {
				return g, fmt.Errorf("proxy: %w", err)
			}
			g.Scope, g.Object = ScopeProxy, u+"@"+h
		} else {
			db, err := c.namePart()
			if err != nil {
				return g, fmt.Errorf("database: %w", err)
			}
			if !c.char('.') {
				return g, fmt.Errorf("expected '.' after database")
			}
			obj, err := c.namePart()
			if err != nil {
				return g, fmt.Errorf("object: %w", err)
			}
			g.Database, g.Object = unescapeDB(db, &g), obj
			switch {
			case db == "*" && obj == "*":
				g.Scope = ScopeGlobal
			case obj == "*":
				g.Scope = ScopeDatabase
			case g.ObjectType == "FUNCTION" || g.ObjectType == "PROCEDURE":
				g.Scope = ScopeRoutine
			default:
				g.Scope = ScopeTable
			}
		}
		if !c.keyword("TO") {
			return g, fmt.Errorf("expected TO")
		}
		if _, _, err := c.account(); err != nil {
			return g, fmt.Errorf("grantee: %w", err)
		}
		tail = c.s[c.i:]
	} else {
		// Role grant: GRANT `r1`, `r2` TO `u`@`%`
		toIdx := findTop(s, "TO")
		if toIdx < 0 {
			return g, fmt.Errorf("neither ON nor TO found")
		}
		for _, r := range splitTop(s[:toIdx], ',') {
			cs := &scanner{s: r}
			name, err := cs.quoted()
			if err != nil {
				return g, fmt.Errorf("role: %w", err)
			}
			g.Roles = append(g.Roles, name)
		}
		g.Scope = ScopeRole
		c := &scanner{s: s, i: toIdx + len(" TO ")}
		if _, _, err := c.account(); err != nil {
			return g, fmt.Errorf("grantee: %w", err)
		}
		tail = c.s[c.i:]
	}
	g.GrantOption = reGrantOpt.MatchString(blank(tail))
	return g, nil
}

func blank(s string) string {
	return reQuoted.ReplaceAllString(s, "''")
}

// unescapeDB interprets \_ and \% as literal characters and flags unescaped
// wildcards.
func unescapeDB(db string, g *Grant) string {
	if db == "*" {
		return db
	}
	var b strings.Builder
	for i := 0; i < len(db); i++ {
		ch := db[i]
		if ch == '\\' && i+1 < len(db) && (db[i+1] == '_' || db[i+1] == '%' || db[i+1] == '\\') {
			b.WriteByte(db[i+1])
			i++
			continue
		}
		if ch == '_' || ch == '%' {
			g.DatabasePattern = true
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// ParseShowGrants parses all lines of SHOW GRANTS FOR user@host. Lines that
// cannot be parsed are returned as an error naming the line.
func ParseShowGrants(user, host string, lines []string) (*AccountGrants, error) {
	ag := &AccountGrants{User: user, Host: host}
	for _, l := range lines {
		g, err := ParseGrant(l)
		if err != nil {
			return nil, fmt.Errorf("dbadmin: parsing %s: %w", safeQuote(l), err)
		}
		ag.Grants = append(ag.Grants, g)
		if g.Scope == ScopeGlobal {
			t := blank(l)
			setLimit(&ag.Limits.MaxQueriesPerHour, reMaxQ, t)
			setLimit(&ag.Limits.MaxUpdatesPerHour, reMaxU, t)
			setLimit(&ag.Limits.MaxConnectionsPerHour, reMaxC, t)
			setLimit(&ag.Limits.MaxUserConnections, reMaxUC, t)
			ag.RequireSSL = reRequire.MatchString(t)
			if m := reVia.FindStringSubmatch(t); m != nil {
				ag.AuthPlugin = m[1]
				ag.HasPassword = true
			}
			if strings.Contains(strings.ToUpper(t), "IDENTIFIED BY") {
				ag.HasPassword = true
			}
		}
	}
	return ag, nil
}

func setLimit(dst *uint32, re *regexp.Regexp, s string) {
	if m := re.FindStringSubmatch(s); m != nil {
		if n, err := strconv.ParseUint(m[1], 10, 32); err == nil {
			*dst = uint32(n)
		}
	}
}

// DatabaseInfo describes a schema.
type DatabaseInfo struct {
	Name      string
	Charset   string
	Collation string
}

// Account identifies a user@host.
type Account struct{ User, Host string }

// ListDatabases lists non-system databases.
func (a *Admin) ListDatabases(ctx context.Context) ([]DatabaseInfo, error) {
	rows, err := a.Exec.Query(ctx, "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME")
	if err != nil {
		return nil, err
	}
	var out []DatabaseInfo
	for _, r := range rows {
		if len(r) < 3 {
			return nil, fmt.Errorf("dbadmin: unexpected SCHEMATA row with %d columns", len(r))
		}
		if protectedDatabases[strings.ToLower(r[0])] {
			continue
		}
		out = append(out, DatabaseInfo{Name: r[0], Charset: r[1], Collation: r[2]})
	}
	return out, nil
}

// ListUsers lists accounts, omitting protected ones.
func (a *Admin) ListUsers(ctx context.Context) ([]Account, error) {
	rows, err := a.Exec.Query(ctx, "SELECT User, Host FROM mysql.user WHERE User <> '' ORDER BY User, Host")
	if err != nil {
		return nil, err
	}
	var out []Account
	for _, r := range rows {
		if len(r) < 2 {
			return nil, fmt.Errorf("dbadmin: unexpected user row with %d columns", len(r))
		}
		if a.userProtected(r[0]) {
			continue
		}
		out = append(out, Account{User: r[0], Host: r[1]})
	}
	return out, nil
}

// ListGrants runs SHOW GRANTS for the account and parses the result.
func (a *Admin) ListGrants(ctx context.Context, user, host string) (*AccountGrants, error) {
	acct, err := QuoteAccount(user, host)
	if err != nil {
		return nil, err
	}
	rows, err := a.Exec.Query(ctx, "SHOW GRANTS FOR "+acct)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 {
			lines = append(lines, r[0])
		}
	}
	return ParseShowGrants(user, host, lines)
}
