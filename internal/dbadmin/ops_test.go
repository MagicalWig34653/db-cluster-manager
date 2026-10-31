// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func planSQL(p *Plan) []string {
	var out []string
	for _, s := range p.Statements {
		out = append(out, s.sql)
	}
	return out
}

func hasWarn(p *Plan, code string) bool {
	for _, w := range p.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestCreateDatabaseExactSQL(t *testing.T) {
	a, f, _, _ := newAdmin()
	ctx := context.Background()
	cases := []struct {
		req  CreateDatabaseRequest
		want string
	}{
		{CreateDatabaseRequest{Name: "shop"}, "CREATE DATABASE IF NOT EXISTS `shop` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"},
		{CreateDatabaseRequest{Name: "legacy", Charset: "latin1"}, "CREATE DATABASE IF NOT EXISTS `legacy` CHARACTER SET latin1"},
		{CreateDatabaseRequest{Name: "x", Charset: "utf8mb4", Collation: "utf8mb4_bin"}, "CREATE DATABASE IF NOT EXISTS `x` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"},
	}
	for _, c := range cases {
		p, err := a.PlanCreateDatabase(ctx, c.req)
		if err != nil {
			t.Fatal(err)
		}
		if got := planSQL(p); !reflect.DeepEqual(got, []string{c.want}) {
			t.Errorf("got %q want %q", got, c.want)
		}
		if !hasWarn(p, WarnTOI) || !p.Idempotent() {
			t.Error("missing TOI warning or not idempotent")
		}
	}
	if len(f.execs) != 0 {
		t.Fatal("planning must not execute")
	}
}

func TestCreateDatabaseRejects(t *testing.T) {
	a, _, _, _ := newAdmin()
	ctx := context.Background()
	for _, r := range []CreateDatabaseRequest{
		{Name: "a`b"}, {Name: "ok", Charset: "utf8mb4; DROP"}, {Name: "ok", Charset: "klingon"},
		{Name: "ok", Collation: "utf8mb4_bin"}, {Name: "ok", Charset: "latin1", Collation: "utf8mb4_bin"},
		{Name: "ok", Charset: "utf8mb4", Collation: "utf8mb4_bin; DROP DATABASE x"},
		{Name: "ok", Charset: "utf8mb4", Collation: "utf8mb4_"}, {Name: "ok", Charset: "utf8mb4", Collation: "utf8mb4_bin' --"},
	} {
		if _, err := a.PlanCreateDatabase(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", r, err)
		}
	}
	for _, n := range []string{"mysql", "information_schema", "PERFORMANCE_SCHEMA", "sys"} {
		if _, err := a.PlanCreateDatabase(ctx, CreateDatabaseRequest{Name: n}); !errors.Is(err, ErrProtected) {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestCreateUserGeneratedPasswordFlow(t *testing.T) {
	a, f, sink, _ := newAdmin()
	ctx := context.Background()
	mx := uint32(20)
	p, err := a.PlanCreateUser(ctx, CreateUserRequest{User: "app", Host: "10.0.%", Limits: LimitsRequest{MaxUserConnections: &mx}})
	if err != nil {
		t.Fatal(err)
	}
	st := p.Statements[0]
	if st.Display != "CREATE USER IF NOT EXISTS 'app'@'10.0.%' IDENTIFIED BY '***' WITH MAX_USER_CONNECTIONS 20" {
		t.Fatalf("display %q", st.Display)
	}
	pw := st.secret.Reveal()
	if strings.Contains(st.Display, pw) || len(f.execs) != 0 {
		t.Fatal("password leaked into display or executed early")
	}
	if want := "CREATE USER IF NOT EXISTS 'app'@'10.0.%' IDENTIFIED BY '" + pw + "' WITH MAX_USER_CONNECTIONS 20"; st.sql != want {
		t.Fatalf("sql %q", st.sql)
	}
	res, err := a.Apply(ctx, p)
	if err != nil || res.Completed() != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if sink.got[SecretRef{Kind: "db-user-password", User: "app", Host: "10.0.%"}] != pw {
		t.Fatal("sink did not receive the password")
	}
	if len(f.execs[0].Args) != 0 {
		t.Fatal("unexpected args")
	}
}

func TestCreateUserExistingKeepsPassword(t *testing.T) {
	a, f, _, _ := newAdmin()
	f.users["app@%"] = true
	p, err := a.PlanCreateUser(context.Background(), CreateUserRequest{User: "app", Host: "%"})
	if err != nil || !p.Empty() || !hasWarn(p, WarnUserExists) {
		t.Fatalf("%v %+v", err, p)
	}
	if !reflect.DeepEqual(f.queries[0].Args, []any{"app", "%"}) {
		t.Fatalf("existence check must be parameterized: %+v", f.queries[0])
	}
}

func TestCreateUserWildcardHostWarningAndRejects(t *testing.T) {
	a, _, _, _ := newAdmin()
	ctx := context.Background()
	p, err := a.PlanCreateUser(ctx, CreateUserRequest{User: "app", Host: "%", Password: "Sup3rS3cret!pw"})
	if err != nil || !hasWarn(p, WarnWildcardHost) {
		t.Fatalf("%v", err)
	}
	for _, r := range []CreateUserRequest{
		{User: "app", Host: "%", Password: "pw'; DROP USER x; --"}, {User: "app", Host: "%", Password: "short"},
		{User: "a'b", Host: "%"}, {User: "app", Host: "h'--"}, {User: "", Host: "%"},
	} {
		if _, err := a.PlanCreateUser(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if _, err := a.PlanCreateUser(ctx, CreateUserRequest{User: "root", Host: "%"}); !errors.Is(err, ErrProtected) {
		t.Error("root not protected")
	}
	a.ProtectedUsers = []string{"dbcm_mgr"}
	if _, err := a.PlanCreateUser(ctx, CreateUserRequest{User: "dbcm_mgr", Host: "%"}); !errors.Is(err, ErrProtected) {
		t.Error("extra protected user not honored")
	}
}

func TestResetPasswordRedactedAndDelivered(t *testing.T) {
	a, f, sink, _ := newAdmin()
	p, err := a.PlanResetPassword(context.Background(), "app", "%", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Statements[0].Display != "ALTER USER 'app'@'%' IDENTIFIED BY '***'" {
		t.Fatal(p.Statements[0].Display)
	}
	if _, err := a.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	pw := sink.got[SecretRef{Kind: "db-user-password", User: "app", Host: "%"}]
	if f.execs[0].SQL != "ALTER USER 'app'@'%' IDENTIFIED BY '"+pw+"'" {
		t.Fatal(f.execs[0].SQL)
	}
}

func TestGrantPresetsExactSQL(t *testing.T) {
	a, _, _, _ := newAdmin()
	ctx := context.Background()
	cases := []struct {
		set  PrivilegeSet
		want string
	}{
		{PrivilegeSet{Preset: PresetReadOnly}, "GRANT SELECT, SHOW VIEW ON `my\\_db`.* TO 'app'@'%'"},
		{PrivilegeSet{Preset: PresetReadWrite}, "GRANT SELECT, INSERT, UPDATE, DELETE, CREATE TEMPORARY TABLES, LOCK TABLES, EXECUTE, SHOW VIEW ON `my\\_db`.* TO 'app'@'%'"},
		{PrivilegeSet{Preset: PresetAdmin}, "GRANT ALL PRIVILEGES ON `my\\_db`.* TO 'app'@'%'"},
		{PrivilegeSet{Preset: PresetAdmin, GrantOption: true}, "GRANT ALL PRIVILEGES ON `my\\_db`.* TO 'app'@'%' WITH GRANT OPTION"},
		{PrivilegeSet{Preset: PresetCustom, Custom: []Privilege{"update", "select", "Select", "create  view"}}, "GRANT SELECT, UPDATE, CREATE VIEW ON `my\\_db`.* TO 'app'@'%'"},
	}
	for _, c := range cases {
		p, err := a.PlanGrant(ctx, GrantRequest{User: "app", Host: "%", Database: "my_db", Set: c.set})
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Statements[0].sql; got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}

func TestGrantRejects(t *testing.T) {
	a, _, _, _ := newAdmin()
	ctx := context.Background()
	bad := []PrivilegeSet{
		{}, {Preset: "root"}, {Preset: PresetCustom}, {Preset: PresetCustom, Custom: []Privilege{"SUPER"}},
		{Preset: PresetCustom, Custom: []Privilege{"SELECT, FILE"}}, {Preset: PresetCustom, Custom: []Privilege{"ALL PRIVILEGES"}},
		{Preset: PresetCustom, Custom: []Privilege{"SELECT ON *.* TO x; --"}}, {Preset: PresetReadOnly, Custom: []Privilege{"SELECT"}},
		{Preset: PresetCustom, Custom: []Privilege{"GRANT OPTION"}}, {Preset: PresetCustom, Custom: []Privilege{"PROCESS"}},
	}
	for _, s := range bad {
		if _, err := a.PlanGrant(ctx, GrantRequest{User: "app", Host: "%", Database: "d", Set: s}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", s, err)
		}
	}
	for _, r := range []GrantRequest{
		{User: "app", Host: "%", Database: "*"}, {User: "app", Host: "%", Database: "a`.*; --"},
		{User: "app", Host: "%", Database: "a%"}, {User: "x'", Host: "%", Database: "d"},
	} {
		r.Set = PrivilegeSet{Preset: PresetReadOnly}
		if _, err := a.PlanGrant(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if _, err := a.PlanGrant(ctx, GrantRequest{User: "app", Host: "%", Database: "mysql", Set: PrivilegeSet{Preset: PresetReadOnly}}); !errors.Is(err, ErrProtected) {
		t.Error("mysql schema grant allowed")
	}
}

func TestRevokeOnlyHeldPrivileges(t *testing.T) {
	a, f, _, _ := newAdmin()
	ctx := context.Background()
	f.grants["app@%"] = []string{
		"GRANT USAGE ON *.* TO `app`@`%` IDENTIFIED BY PASSWORD '*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19'",
		"GRANT SELECT, INSERT ON `shop`.* TO `app`@`%` WITH GRANT OPTION",
	}
	p, err := a.PlanRevoke(ctx, GrantRequest{User: "app", Host: "%", Database: "shop",
		Set: PrivilegeSet{Preset: PresetCustom, Custom: []Privilege{"SELECT", "DELETE"}, GrantOption: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"REVOKE SELECT ON `shop`.* FROM 'app'@'%'", "REVOKE GRANT OPTION ON `shop`.* FROM 'app'@'%'"}
	if got := planSQL(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	p, err = a.PlanRevoke(ctx, GrantRequest{User: "app", Host: "%", Database: "shop", Set: PrivilegeSet{Preset: PresetAdmin}})
	if err != nil || planSQL(p)[0] != "REVOKE ALL PRIVILEGES ON `shop`.* FROM 'app'@'%'" {
		t.Fatalf("%v %q", err, planSQL(p))
	}
	p, err = a.PlanRevoke(ctx, GrantRequest{User: "app", Host: "%", Database: "other", Set: PrivilegeSet{Preset: PresetReadOnly}})
	if err != nil || !p.Empty() || !hasWarn(p, WarnNoop) {
		t.Fatalf("%v %+v", err, p)
	}
	p, err = a.PlanRevoke(ctx, GrantRequest{User: "app", Host: "%", Database: "shop", Set: PrivilegeSet{Preset: PresetCustom, Custom: []Privilege{"DROP"}}})
	if err != nil || !p.Empty() {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestRevokeFromAllPrivileges(t *testing.T) {
	a, f, _, _ := newAdmin()
	f.grants["app@%"] = []string{"GRANT USAGE ON *.* TO `app`@`%`", "GRANT ALL PRIVILEGES ON `shop`.* TO `app`@`%`"}
	p, err := a.PlanRevoke(context.Background(), GrantRequest{User: "app", Host: "%", Database: "shop", Set: PrivilegeSet{Preset: PresetReadOnly}})
	if err != nil || planSQL(p)[0] != "REVOKE SELECT, SHOW VIEW ON `shop`.* FROM 'app'@'%'" {
		t.Fatalf("%v %q", err, planSQL(p))
	}
}

func TestSetLimits(t *testing.T) {
	a, _, _, _ := newAdmin()
	ctx := context.Background()
	mc, q := uint32(50), uint32(0)
	p, err := a.PlanSetLimits(ctx, "app", "%", LimitsRequest{MaxUserConnections: &mc, MaxQueriesPerHour: &q})
	if err != nil || p.Statements[0].sql != "ALTER USER IF EXISTS 'app'@'%' WITH MAX_QUERIES_PER_HOUR 0 MAX_USER_CONNECTIONS 50" {
		t.Fatalf("%v %q", err, planSQL(p))
	}
	if _, err := a.PlanSetLimits(ctx, "app", "%", LimitsRequest{}); !errors.Is(err, ErrInvalid) {
		t.Error("empty limits accepted")
	}
}

func TestDropRequiresConfirmation(t *testing.T) {
	a, _, _, _ := newAdmin()
	ctx := context.Background()
	if _, err := a.PlanDropDatabase(ctx, "shop", ""); !errors.Is(err, ErrConfirm) {
		t.Error("no token accepted")
	}
	if _, err := a.PlanDropDatabase(ctx, "shop", ConfirmToken("database", "other")); !errors.Is(err, ErrConfirm) {
		t.Error("wrong token accepted")
	}
	p, err := a.PlanDropDatabase(ctx, "shop", "drop-database:shop")
	if err != nil || p.Statements[0].sql != "DROP DATABASE IF EXISTS `shop`" || !hasWarn(p, WarnDestructive) || !hasWarn(p, WarnLargeDDL) {
		t.Fatalf("%v %+v", err, p)
	}
	if _, err := a.PlanDropDatabase(ctx, "mysql", ConfirmToken("database", "mysql")); !errors.Is(err, ErrProtected) {
		t.Error("mysql droppable")
	}
	if _, err := a.PlanDropUser(ctx, "app", "%", "drop-user:app@x"); !errors.Is(err, ErrConfirm) {
		t.Error("wrong user token accepted")
	}
	p, err = a.PlanDropUser(ctx, "app", "%", "drop-user:app@%")
	if err != nil || p.Statements[0].sql != "DROP USER IF EXISTS 'app'@'%'" {
		t.Fatalf("%v", err)
	}
	if _, err := a.PlanDropUser(ctx, "root", "localhost", "drop-user:root@localhost"); !errors.Is(err, ErrProtected) {
		t.Error("root droppable")
	}
}

func TestRenameUser(t *testing.T) {
	a, f, _, _ := newAdmin()
	ctx := context.Background()
	if _, err := a.PlanRenameUser(ctx, "old", "%", "new", "%"); !errors.Is(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
	f.users["old@%"] = true
	p, err := a.PlanRenameUser(ctx, "old", "%", "new", "10.%")
	if err != nil || p.Statements[0].sql != "RENAME USER 'old'@'%' TO 'new'@'10.%'" || p.Idempotent() {
		t.Fatalf("%v %q", err, planSQL(p))
	}
	f.users["new@10.%"] = true
	if _, err := a.PlanRenameUser(ctx, "old", "%", "new", "10.%"); !errors.Is(err, ErrConflict) {
		t.Errorf("%v", err)
	}
	delete(f.users, "old@%")
	p, err = a.PlanRenameUser(ctx, "old", "%", "new", "10.%")
	if err != nil || !p.Empty() {
		t.Fatalf("already-renamed should be empty: %v", err)
	}
	if _, err := a.PlanRenameUser(ctx, "old", "%", "x'", "%"); !errors.Is(err, ErrInvalid) {
		t.Error("bad new name accepted")
	}
}

func TestRenameDatabase(t *testing.T) {
	a, _, _, _ := newAdmin()
	ctx := context.Background()
	p, err := a.PlanRenameDatabase(ctx, RenameDatabaseRequest{Old: "a", New: "b", Tables: []string{"t1", "t2"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CREATE DATABASE IF NOT EXISTS `b` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
		"RENAME TABLE `a`.`t1` TO `b`.`t1`, `a`.`t2` TO `b`.`t2`",
	}
	if got := planSQL(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	if p.Idempotent() || !hasWarn(p, WarnNotRepeat) || !hasWarn(p, WarnRename) {
		t.Fatal("warnings/idempotency wrong")
	}
	for _, r := range []RenameDatabaseRequest{
		{Old: "a", New: "a"}, {Old: "a", New: "b", Tables: []string{"t`"}}, {Old: "a", New: "b", Tables: []string{"t", "t"}},
		{Old: "mysql", New: "b"}, {Old: "a", New: "sys"},
	} {
		if _, err := a.PlanRenameDatabase(ctx, r); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

func TestProvisionCombinesPlan(t *testing.T) {
	a, f, sink, _ := newAdmin()
	ctx := context.Background()
	p, err := a.PlanProvision(ctx, ProvisionRequest{
		Database: CreateDatabaseRequest{Name: "shop"},
		User:     CreateUserRequest{User: "shop_app", Host: "10.%"},
		Set:      PrivilegeSet{Preset: PresetReadWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Statements) != 3 || len(p.Warnings) != 1 {
		t.Fatalf("%d statements, warnings %+v", len(p.Statements), p.Warnings)
	}
	if _, err := a.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	if len(f.execs) != 3 || !strings.HasPrefix(f.execs[2].SQL, "GRANT SELECT, INSERT") || len(sink.got) != 1 {
		t.Fatalf("%q", f.sqls())
	}
	if _, err := a.PlanProvision(ctx, ProvisionRequest{Database: CreateDatabaseRequest{Name: "shop"}, User: CreateUserRequest{User: "u", Host: "%"}}); err == nil {
		t.Error("provision without privilege set accepted")
	}
}
