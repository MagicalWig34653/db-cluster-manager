// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// charsets is the allowlist of character sets for CREATE DATABASE.
var charsets = map[string]bool{
	"utf8mb4": true, "utf8mb3": true, "utf8": true, "latin1": true,
	"ascii": true, "binary": true, "ucs2": true, "utf16": true, "utf32": true,
	"cp1250": true, "cp1251": true, "cp1252": true, "latin2": true,
}

// Defaults for databases created without explicit settings.
const (
	DefaultCharset   = "utf8mb4"
	DefaultCollation = "utf8mb4_unicode_ci"
)

var reCollation = regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)+$`)

func validateCharset(cs, coll string) (string, string, error) {
	if cs == "" && coll == "" {
		return DefaultCharset, DefaultCollation, nil
	}
	if cs == "" {
		return "", "", invalid("charset", "collation given without character set")
	}
	if !charsets[cs] {
		return "", "", invalid("charset", "%s is not in the allowlist", safeQuote(cs))
	}
	if coll == "" {
		if cs == DefaultCharset {
			return cs, DefaultCollation, nil
		}
		return cs, "", nil // server default collation of that character set
	}
	if !strings.HasPrefix(coll, cs+"_") || !reCollation.MatchString(coll) {
		return "", "", invalid("collation", "%s does not belong to character set %s", safeQuote(coll), cs)
	}
	return cs, coll, nil
}

func (a *Admin) dbProtected(name string) bool { return protectedDatabases[strings.ToLower(name)] }

func stmt(desc, sql string) Statement {
	return Statement{Description: desc, Display: sql, sql: sql, Idempotent: true}
}

func (a *Admin) newPlan(ctx context.Context) (*Plan, error) {
	w, err := a.requireWriter(ctx)
	if err != nil {
		return nil, err
	}
	p := &Plan{WriterNode: w}
	p.warn(WarnTOI, toiMessage)
	return p, nil
}

func (a *Admin) userExists(ctx context.Context, user, host string) (bool, error) {
	rows, err := a.Exec.Query(ctx, "SELECT COUNT(*) FROM mysql.user WHERE User = ? AND Host = ?", user, host)
	if err != nil {
		return false, err
	}
	return len(rows) > 0 && len(rows[0]) > 0 && rows[0][0] != "0", nil
}

// CreateDatabaseRequest describes CREATE DATABASE.
type CreateDatabaseRequest struct {
	Name      string
	Charset   string // default utf8mb4
	Collation string // default utf8mb4_unicode_ci for utf8mb4
}

func (a *Admin) createDatabaseStmt(r CreateDatabaseRequest) (Statement, error) {
	if err := ValidateDatabaseName(r.Name); err != nil {
		return Statement{}, err
	}
	if a.dbProtected(r.Name) {
		return Statement{}, fmt.Errorf("%w: database %s", ErrProtected, r.Name)
	}
	cs, coll, err := validateCharset(r.Charset, r.Collation)
	if err != nil {
		return Statement{}, err
	}
	q := "CREATE DATABASE IF NOT EXISTS " + quoteIdentRaw(r.Name) + " CHARACTER SET " + cs
	if coll != "" {
		q += " COLLATE " + coll
	}
	return stmt("create database "+r.Name, q), nil
}

// PlanCreateDatabase plans CREATE DATABASE IF NOT EXISTS.
func (a *Admin) PlanCreateDatabase(ctx context.Context, r CreateDatabaseRequest) (*Plan, error) {
	s, err := a.createDatabaseStmt(r)
	if err != nil {
		return nil, err
	}
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	p.Statements = append(p.Statements, s)
	return p, nil
}

// Limits for CreateUser/SetLimits: nil means "leave unchanged / unset".
type LimitsRequest struct {
	MaxQueriesPerHour     *uint32
	MaxUpdatesPerHour     *uint32
	MaxConnectionsPerHour *uint32
	MaxUserConnections    *uint32
}

func (l LimitsRequest) clause() string {
	var parts []string
	add := func(name string, v *uint32) {
		if v != nil {
			parts = append(parts, name+" "+strconv.FormatUint(uint64(*v), 10))
		}
	}
	add("MAX_QUERIES_PER_HOUR", l.MaxQueriesPerHour)
	add("MAX_UPDATES_PER_HOUR", l.MaxUpdatesPerHour)
	add("MAX_CONNECTIONS_PER_HOUR", l.MaxConnectionsPerHour)
	add("MAX_USER_CONNECTIONS", l.MaxUserConnections)
	if len(parts) == 0 {
		return ""
	}
	return " WITH " + strings.Join(parts, " ")
}

// CreateUserRequest describes CREATE USER. An empty Password is generated.
type CreateUserRequest struct {
	User     string
	Host     string
	Password string
	Limits   LimitsRequest
}

func (a *Admin) createUserPlan(ctx context.Context, r CreateUserRequest, p *Plan) error {
	acct, err := QuoteAccount(r.User, r.Host)
	if err != nil {
		return err
	}
	if a.userProtected(r.User) {
		return fmt.Errorf("%w: user %s", ErrProtected, r.User)
	}
	exists, err := a.userExists(ctx, r.User, r.Host)
	if err != nil {
		return err
	}
	if exists {
		p.warn(WarnUserExists, fmt.Sprintf("user %s@%s already exists; its password is left unchanged", r.User, r.Host))
		return nil
	}
	pw := NewSecret(r.Password)
	if r.Password == "" {
		opts := a.Password
		if pw, err = GeneratePassword(opts); err != nil {
			return err
		}
	} else if err := ValidatePassword(r.Password); err != nil {
		return err
	}
	if r.Host == "%" {
		p.warn(WarnWildcardHost, fmt.Sprintf("user %s may connect from any host", r.User))
	}
	tail := r.Limits.clause()
	s := stmt("create user "+r.User+"@"+r.Host, "CREATE USER IF NOT EXISTS "+acct+" IDENTIFIED BY '"+pw.Reveal()+"'"+tail)
	s.Display = "CREATE USER IF NOT EXISTS " + acct + " IDENTIFIED BY '***'" + tail
	s.secret = &pw
	s.secretRef = SecretRef{Kind: "db-user-password", User: r.User, Host: r.Host}
	p.Statements = append(p.Statements, s)
	return nil
}

// PlanCreateUser plans CREATE USER with a generated or supplied password. The
// password is delivered to Admin.Sink during Apply and appears nowhere else.
func (a *Admin) PlanCreateUser(ctx context.Context, r CreateUserRequest) (*Plan, error) {
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.createUserPlan(ctx, r, p); err != nil {
		return nil, err
	}
	return p, nil
}

// PlanResetPassword plans ALTER USER ... IDENTIFIED BY with a new password.
func (a *Admin) PlanResetPassword(ctx context.Context, user, host, password string) (*Plan, error) {
	acct, err := QuoteAccount(user, host)
	if err != nil {
		return nil, err
	}
	if a.userProtected(user) {
		return nil, fmt.Errorf("%w: user %s", ErrProtected, user)
	}
	pw := NewSecret(password)
	if password == "" {
		if pw, err = GeneratePassword(a.Password); err != nil {
			return nil, err
		}
	} else if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	s := stmt("reset password of "+user+"@"+host, "ALTER USER "+acct+" IDENTIFIED BY '"+pw.Reveal()+"'")
	s.Display = "ALTER USER " + acct + " IDENTIFIED BY '***'"
	s.secret = &pw
	s.secretRef = SecretRef{Kind: "db-user-password", User: user, Host: host}
	p.Statements = append(p.Statements, s)
	return p, nil
}

func grantStmts(user, host, db string, set PrivilegeSet) ([]Statement, error) {
	acct, err := QuoteAccount(user, host)
	if err != nil {
		return nil, err
	}
	gdb, err := quoteGrantDB(db)
	if err != nil {
		return nil, err
	}
	privs, all, err := set.Resolve()
	if err != nil {
		return nil, err
	}
	list := "ALL PRIVILEGES"
	if !all {
		list = joinPrivs(privs)
	}
	q := "GRANT " + list + " ON " + gdb + ".* TO " + acct
	if set.GrantOption {
		q += " WITH GRANT OPTION"
	}
	return []Statement{stmt(fmt.Sprintf("grant %s on %s to %s@%s", strings.ToLower(list), db, user, host), q)}, nil
}

// GrantRequest grants a privilege set on one database.
type GrantRequest struct {
	User, Host, Database string
	Set                  PrivilegeSet
}

func (a *Admin) checkGrantTarget(user, db string) error {
	if a.userProtected(user) {
		return fmt.Errorf("%w: user %s", ErrProtected, user)
	}
	if a.dbProtected(db) {
		return fmt.Errorf("%w: database %s", ErrProtected, db)
	}
	return nil
}

// PlanGrant plans GRANT ... ON `db`.*. Re-applying a GRANT is harmless.
func (a *Admin) PlanGrant(ctx context.Context, r GrantRequest) (*Plan, error) {
	if err := a.checkGrantTarget(r.User, r.Database); err != nil {
		return nil, err
	}
	ss, err := grantStmts(r.User, r.Host, r.Database, r.Set)
	if err != nil {
		return nil, err
	}
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	p.Statements = append(p.Statements, ss...)
	return p, nil
}

// PlanRevoke plans REVOKE for the privileges the account actually holds on the
// database (REVOKE of an unheld privilege is an error in MariaDB). The result
// can be empty.
func (a *Admin) PlanRevoke(ctx context.Context, r GrantRequest) (*Plan, error) {
	if err := a.checkGrantTarget(r.User, r.Database); err != nil {
		return nil, err
	}
	acct, err := QuoteAccount(r.User, r.Host)
	if err != nil {
		return nil, err
	}
	gdb, err := quoteGrantDB(r.Database)
	if err != nil {
		return nil, err
	}
	privs, all, err := r.Set.Resolve()
	if err != nil {
		return nil, err
	}
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	ag, err := a.ListGrants(ctx, r.User, r.Host)
	if err != nil {
		return nil, err
	}
	held, ok := ag.DatabaseGrant(r.Database)
	if !ok {
		p.warn(WarnNoop, "the account holds no grant on this database; nothing to revoke")
		return p, nil
	}
	var revoke []Privilege
	switch {
	case all && len(held.Privileges) > 0:
		p.Statements = append(p.Statements, stmt("revoke all on "+r.Database, "REVOKE ALL PRIVILEGES ON "+gdb+".* FROM "+acct))
	case !all:
		heldSet := map[Privilege]bool{}
		for _, h := range held.Privileges {
			heldSet[Privilege(h.Name)] = true
		}
		for _, want := range privs {
			if held.HasAll() || heldSet[want] {
				revoke = append(revoke, want)
			}
		}
		if len(revoke) > 0 {
			p.Statements = append(p.Statements, stmt("revoke "+strings.ToLower(joinPrivs(revoke))+" on "+r.Database,
				"REVOKE "+joinPrivs(revoke)+" ON "+gdb+".* FROM "+acct))
		}
	}
	if r.Set.GrantOption && held.GrantOption {
		p.Statements = append(p.Statements, stmt("revoke grant option on "+r.Database, "REVOKE GRANT OPTION ON "+gdb+".* FROM "+acct))
	}
	if len(p.Statements) == 0 {
		p.warn(WarnNoop, "none of the requested privileges is held; nothing to revoke")
	}
	return p, nil
}

// PlanSetLimits plans ALTER USER ... WITH MAX_USER_CONNECTIONS etc.
func (a *Admin) PlanSetLimits(ctx context.Context, user, host string, l LimitsRequest) (*Plan, error) {
	acct, err := QuoteAccount(user, host)
	if err != nil {
		return nil, err
	}
	if a.userProtected(user) {
		return nil, fmt.Errorf("%w: user %s", ErrProtected, user)
	}
	c := l.clause()
	if c == "" {
		return nil, invalid("limits", "no limit given")
	}
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	p.Statements = append(p.Statements, stmt("set limits of "+user+"@"+host, "ALTER USER IF EXISTS "+acct+c))
	return p, nil
}

// ConfirmToken returns the token a caller must pass to confirm a destructive
// operation, e.g. "drop-database:shop". It forces the UI/API to name the object.
func ConfirmToken(kind, name string) string { return "drop-" + kind + ":" + name }

// PlanDropDatabase plans DROP DATABASE IF EXISTS. confirm must equal
// ConfirmToken("database", name).
func (a *Admin) PlanDropDatabase(ctx context.Context, name, confirm string) (*Plan, error) {
	if err := ValidateDatabaseName(name); err != nil {
		return nil, err
	}
	if a.dbProtected(name) {
		return nil, fmt.Errorf("%w: database %s", ErrProtected, name)
	}
	if confirm != ConfirmToken("database", name) {
		return nil, ErrConfirm
	}
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	p.warn(WarnDestructive, "dropping a database permanently deletes all its tables and data; privileges granted on it are not removed automatically")
	p.warn(WarnLargeDDL, "dropping a database with many or large tables can hold the cluster-wide TOI lock for a long time; prefer a quiet period")
	p.Statements = append(p.Statements, stmt("drop database "+name, "DROP DATABASE IF EXISTS "+quoteIdentRaw(name)))
	return p, nil
}

// PlanDropUser plans DROP USER IF EXISTS. confirm must equal
// ConfirmToken("user", "user@host").
func (a *Admin) PlanDropUser(ctx context.Context, user, host, confirm string) (*Plan, error) {
	acct, err := QuoteAccount(user, host)
	if err != nil {
		return nil, err
	}
	if a.userProtected(user) {
		return nil, fmt.Errorf("%w: user %s", ErrProtected, user)
	}
	if confirm != ConfirmToken("user", user+"@"+host) {
		return nil, ErrConfirm
	}
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	p.warn(WarnDestructive, "dropping a user also removes all its privileges; connected sessions are not terminated")
	p.Statements = append(p.Statements, stmt("drop user "+user+"@"+host, "DROP USER IF EXISTS "+acct))
	return p, nil
}

// PlanRenameUser plans RENAME USER. It checks both names first so that an
// already completed rename yields an empty plan instead of an error.
func (a *Admin) PlanRenameUser(ctx context.Context, user, host, newUser, newHost string) (*Plan, error) {
	from, err := QuoteAccount(user, host)
	if err != nil {
		return nil, err
	}
	to, err := QuoteAccount(newUser, newHost)
	if err != nil {
		return nil, err
	}
	if a.userProtected(user) || a.userProtected(newUser) {
		return nil, fmt.Errorf("%w: rename involves a protected user", ErrProtected)
	}
	oldOK, err := a.userExists(ctx, user, host)
	if err != nil {
		return nil, err
	}
	newOK, err := a.userExists(ctx, newUser, newHost)
	if err != nil {
		return nil, err
	}
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case oldOK && newOK:
		return nil, fmt.Errorf("%w: %s@%s", ErrConflict, newUser, newHost)
	case !oldOK && newOK:
		p.warn(WarnNoop, "the old account no longer exists and the new one does; rename already done")
		return p, nil
	case !oldOK:
		return nil, fmt.Errorf("%w: %s@%s", ErrNotFound, user, host)
	}
	p.warn(WarnRename, "stored procedures, views and events keep their old DEFINER; sessions of the old name are not terminated")
	s := stmt("rename user", "RENAME USER "+from+" TO "+to)
	s.Idempotent = false // guarded by the plan-time existence check only
	p.Statements = append(p.Statements, s)
	return p, nil
}

// RenameDatabaseRequest renames a database by moving its tables, since MariaDB
// has no RENAME DATABASE. The caller supplies the table list (from
// information_schema) so the plan is explicit and reviewable.
type RenameDatabaseRequest struct {
	Old, New  string
	Tables    []string
	Charset   string
	Collation string
}

// PlanRenameDatabase plans CREATE DATABASE new + a single atomic RENAME TABLE.
// The old database is not dropped; drop it with PlanDropDatabase afterwards.
func (a *Admin) PlanRenameDatabase(ctx context.Context, r RenameDatabaseRequest) (*Plan, error) {
	if err := ValidateDatabaseName(r.Old); err != nil {
		return nil, err
	}
	if a.dbProtected(r.Old) {
		return nil, fmt.Errorf("%w: database %s", ErrProtected, r.Old)
	}
	if r.Old == r.New {
		return nil, invalid("database name", "old and new name are identical")
	}
	create, err := a.createDatabaseStmt(CreateDatabaseRequest{Name: r.New, Charset: r.Charset, Collation: r.Collation})
	if err != nil {
		return nil, err
	}
	p, err := a.newPlan(ctx)
	if err != nil {
		return nil, err
	}
	p.Statements = append(p.Statements, create)
	if len(r.Tables) > 0 {
		seen := map[string]bool{}
		pairs := make([]string, 0, len(r.Tables))
		for _, t := range r.Tables {
			if err := ValidateTableName(t); err != nil {
				return nil, err
			}
			if seen[t] {
				return nil, invalid("table name", "duplicate table %s", safeQuote(t))
			}
			seen[t] = true
			pairs = append(pairs, quoteIdentRaw(r.Old)+"."+quoteIdentRaw(t)+" TO "+quoteIdentRaw(r.New)+"."+quoteIdentRaw(t))
		}
		rn := stmt("move tables to "+r.New, "RENAME TABLE "+strings.Join(pairs, ", "))
		rn.Idempotent = false
		p.Statements = append(p.Statements, rn)
		p.warn(WarnNotRepeat, "RENAME TABLE cannot be repeated after it succeeded; do not re-apply a completed plan")
	}
	p.warn(WarnRename, "views, triggers, routines, events and grants are not moved; grants must be re-created for the new database name")
	p.warn(WarnLargeDDL, "RENAME TABLE of many tables holds the cluster-wide TOI lock for its whole duration")
	return p, nil
}

// ProvisionRequest creates a database together with an accompanying user
// (D01): create database, create user with a generated password, grant.
type ProvisionRequest struct {
	Database CreateDatabaseRequest
	User     CreateUserRequest
	Set      PrivilegeSet
}

// PlanProvision combines the three steps into one plan.
func (a *Admin) PlanProvision(ctx context.Context, r ProvisionRequest) (*Plan, error) {
	db, err := a.PlanCreateDatabase(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	u, err := a.PlanCreateUser(ctx, r.User)
	if err != nil {
		return nil, err
	}
	g, err := a.PlanGrant(ctx, GrantRequest{User: r.User.User, Host: r.User.Host, Database: r.Database.Name, Set: r.Set})
	if err != nil {
		return nil, err
	}
	return mergePlans(db, u, g), nil
}
