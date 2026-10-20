// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"reflect"
	"testing"
)

func TestParseGrantLines(t *testing.T) {
	cases := []struct {
		line string
		want Grant
	}{
		{"GRANT USAGE ON *.* TO `app`@`%` IDENTIFIED BY PASSWORD '*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19'",
			Grant{Scope: ScopeGlobal, Database: "*", Object: "*", Privileges: []PrivilegeEntry{{Name: "USAGE"}}}},
		{"GRANT SELECT, INSERT, CREATE TEMPORARY TABLES ON `shop`.* TO `app`@`%`",
			Grant{Scope: ScopeDatabase, Database: "shop", Object: "*", Privileges: []PrivilegeEntry{{Name: "SELECT"}, {Name: "INSERT"}, {Name: "CREATE TEMPORARY TABLES"}}}},
		{"GRANT ALL PRIVILEGES ON `my\\_db`.* TO 'app'@'10.0.%' WITH GRANT OPTION",
			Grant{Scope: ScopeDatabase, Database: "my_db", Object: "*", Privileges: []PrivilegeEntry{{Name: "ALL PRIVILEGES"}}, GrantOption: true}},
		{"GRANT SELECT ON `my_%`.* TO `app`@`%`",
			Grant{Scope: ScopeDatabase, Database: "my_%", Object: "*", Privileges: []PrivilegeEntry{{Name: "SELECT"}}, DatabasePattern: true}},
		{"GRANT SELECT (id, `na,me`), UPDATE (`name`) ON `db-1`.`users` TO `app`@`%`",
			Grant{Scope: ScopeTable, Database: "db-1", Object: "users", Privileges: []PrivilegeEntry{{Name: "SELECT", Columns: []string{"id", "na,me"}}, {Name: "UPDATE", Columns: []string{"name"}}}}},
		{"GRANT EXECUTE ON PROCEDURE `shop`.`do_it` TO `app`@`%`",
			Grant{Scope: ScopeRoutine, Database: "shop", Object: "do_it", ObjectType: "PROCEDURE", Privileges: []PrivilegeEntry{{Name: "EXECUTE"}}}},
		{"GRANT SELECT ON TABLE `shop`.`weird ON name` TO `app`@`%`",
			Grant{Scope: ScopeTable, Database: "shop", Object: "weird ON name", ObjectType: "TABLE", Privileges: []PrivilegeEntry{{Name: "SELECT"}}}},
		{"GRANT PROXY ON ''@'' TO 'root'@'localhost' WITH GRANT OPTION",
			Grant{Scope: ScopeProxy, Object: "@", Privileges: []PrivilegeEntry{{Name: "PROXY"}}, GrantOption: true}},
		{"GRANT `reader`, `writer` TO `app`@`%`",
			Grant{Scope: ScopeRole, Roles: []string{"reader", "writer"}}},
		{"GRANT `admin` TO `app`@`%` WITH ADMIN OPTION",
			Grant{Scope: ScopeRole, Roles: []string{"admin"}, GrantOption: true}},
		{"GRANT RELOAD, PROCESS ON *.* TO `ops`@`localhost`",
			Grant{Scope: ScopeGlobal, Database: "*", Object: "*", Privileges: []PrivilegeEntry{{Name: "RELOAD"}, {Name: "PROCESS"}}}},
	}
	for _, c := range cases {
		got, err := ParseGrant(c.line)
		if err != nil {
			t.Errorf("%s: %v", c.line, err)
			continue
		}
		got.Raw = ""
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s\n got %+v\nwant %+v", c.line, got, c.want)
		}
	}
}

func TestParseGrantErrors(t *testing.T) {
	for _, l := range []string{"", "REVOKE x", "GRANT", "GRANT SELECT ON `a`.* FROM x", "GRANT SELECT ON `a`.* TO", "GRANT SELECT ON `a TO x", "GRANT ON x.* TO 'a'@'b'", "GRANT SELECT (a ON x.* TO 'a'@'b'"} {
		if _, err := ParseGrant(l); err == nil {
			t.Errorf("%q parsed", l)
		}
	}
}

func TestParseShowGrantsLimitsAndSSL(t *testing.T) {
	lines := []string{
		"GRANT USAGE ON *.* TO `app`@`%` IDENTIFIED VIA mysql_native_password USING '*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19' REQUIRE SSL WITH MAX_USER_CONNECTIONS 20 MAX_QUERIES_PER_HOUR 1000",
		"GRANT SELECT ON `shop`.* TO `app`@`%`",
	}
	ag, err := ParseShowGrants("app", "%", lines)
	if err != nil {
		t.Fatal(err)
	}
	if ag.Limits.MaxUserConnections != 20 || ag.Limits.MaxQueriesPerHour != 1000 || !ag.RequireSSL || ag.AuthPlugin != "mysql_native_password" || !ag.HasPassword {
		t.Fatalf("%+v", ag)
	}
	if ag.Grants[0].GrantOption {
		t.Fatal("limits misread as grant option")
	}
	if g, ok := ag.DatabaseGrant("shop"); !ok || g.Privileges[0].Name != "SELECT" {
		t.Fatal("DatabaseGrant lookup")
	}
	if _, err := ParseShowGrants("a", "b", []string{"nonsense"}); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestListings(t *testing.T) {
	a, f, _, _ := newAdmin()
	f.grants["app@%"] = []string{"GRANT USAGE ON *.* TO `app`@`%`", "GRANT SELECT ON `shop`.* TO `app`@`%`"}
	ag, err := a.ListGrants(context.Background(), "app", "%")
	if err != nil || len(ag.Grants) != 2 {
		t.Fatalf("%v", err)
	}
	if f.queries[0].SQL != "SHOW GRANTS FOR 'app'@'%'" {
		t.Fatal(f.queries[0].SQL)
	}
	if _, err := a.ListGrants(context.Background(), "a'; DROP", "%"); err == nil {
		t.Fatal("injection accepted")
	}
}

type listExec struct{ *fakeExec }

func (l listExec) Query(ctx context.Context, s string, a ...any) ([][]string, error) {
	switch s {
	case "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME":
		return [][]string{{"information_schema", "utf8", "utf8_general_ci"}, {"mysql", "utf8mb4", "utf8mb4_general_ci"}, {"shop", "utf8mb4", "utf8mb4_unicode_ci"}}, nil
	case "SELECT User, Host FROM mysql.user WHERE User <> '' ORDER BY User, Host":
		return [][]string{{"app", "%"}, {"mariadb.sys", "localhost"}, {"root", "localhost"}}, nil
	}
	return l.fakeExec.Query(ctx, s, a...)
}

func TestListDatabasesAndUsersHideSystemObjects(t *testing.T) {
	a, f, _, _ := newAdmin()
	a.Exec = listExec{f}
	dbs, err := a.ListDatabases(context.Background())
	if err != nil || !reflect.DeepEqual(dbs, []DatabaseInfo{{"shop", "utf8mb4", "utf8mb4_unicode_ci"}}) {
		t.Fatalf("%v %+v", err, dbs)
	}
	us, err := a.ListUsers(context.Background())
	if err != nil || !reflect.DeepEqual(us, []Account{{"app", "%"}}) {
		t.Fatalf("%v %+v", err, us)
	}
}
