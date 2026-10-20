// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestBuildDumpCommandArgv(t *testing.T) {
	c, err := BuildDumpCommand(DumpOptions{OptionFile: "/run/dbcm/x/my.cnf", Databases: []string{"shop", "blog"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--defaults-extra-file=/run/dbcm/x/my.cnf", "--single-transaction", "--skip-lock-tables", "--hex-blob",
		"--default-character-set=utf8mb4", "--quick", "--routines", "--triggers", "--events", "--databases", "--", "shop", "blog"}
	if c.Path != "mariadb-dump" || !reflect.DeepEqual(c.Args, want) || len(c.Env) != 0 {
		t.Fatalf("%+v", c)
	}
	c, err = BuildDumpCommand(DumpOptions{Flavor: FlavorMySQL, OptionFile: "/o.cnf", Databases: []string{"shop"}, Tables: []string{"a", "b"}, NoData: true, NoRoutines: true})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"--defaults-extra-file=/o.cnf", "--single-transaction", "--skip-lock-tables", "--hex-blob",
		"--default-character-set=utf8mb4", "--quick", "--no-data", "--", "shop", "a", "b"}
	if c.Path != "mysqldump" || !reflect.DeepEqual(c.Args, want) {
		t.Fatalf("%+v", c)
	}
}

func TestBuildDumpCommandRejects(t *testing.T) {
	for _, o := range []DumpOptions{
		{OptionFile: "/o", Databases: nil}, {OptionFile: "", Databases: []string{"a"}}, {OptionFile: "rel/o", Databases: []string{"a"}},
		{OptionFile: "/o\n--password=x", Databases: []string{"a"}}, {OptionFile: "/o", Databases: []string{"--all-databases"}},
		{OptionFile: "/o", Databases: []string{"a; rm -rf /"}}, {OptionFile: "/o", Databases: []string{"a", "b"}, Tables: []string{"t"}},
		{OptionFile: "/o", Databases: []string{"a"}, Tables: []string{"-x"}}, {OptionFile: "/o", Databases: []string{"a"}, Flavor: "percona; id"},
	} {
		if _, err := BuildDumpCommand(o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestBuildLoadCommand(t *testing.T) {
	c, err := BuildLoadCommand(LoadOptions{OptionFile: "/o.cnf", Database: "shop"})
	if err != nil || c.Path != "mariadb" || !reflect.DeepEqual(c.Args, []string{"--defaults-extra-file=/o.cnf", "--default-character-set=utf8mb4", "--batch", "--database=shop"}) {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := BuildLoadCommand(LoadOptions{OptionFile: "/o.cnf", Database: "a --execute=x"}); err == nil {
		t.Fatal("bad db accepted")
	}
}

func TestRenderOptionFile(t *testing.T) {
	s, err := RenderOptionFile(OptionFileData{Host: "proxy.example", Port: 3306, User: "dump", Password: NewSecret(`p#ss"w\rd12`)})
	if err != nil {
		t.Fatal(err)
	}
	want := "[client]\nhost=\"proxy.example\"\nport=3306\nuser=\"dump\"\npassword=\"p#ss\\\"w\\\\rd12\"\n"
	if s != want {
		t.Fatalf("%q", s)
	}
	for _, d := range []OptionFileData{
		{Host: "h", Port: 1, User: "u", Password: NewSecret("a\nb")}, {Host: "h", Port: 1, User: "u"},
		{Host: "h", Port: 0, User: "u", Password: NewSecret("x")}, {Host: "h'", Port: 1, User: "u", Password: NewSecret("x")},
	} {
		if _, err := RenderOptionFile(d); err == nil {
			t.Errorf("%+v accepted", d)
		}
	}
}

type fakeRunner struct {
	cmd   Command
	stdin string
}

func (f *fakeRunner) Run(_ context.Context, c Command, in io.Reader, out io.Writer) error {
	f.cmd = c
	if in != nil {
		b, _ := io.ReadAll(in)
		f.stdin = string(b)
	}
	_, _ = out.Write([]byte("-- dump\n"))
	return nil
}

func TestCommandDumperStreams(t *testing.T) {
	r := &fakeRunner{}
	d := CommandDumper{Runner: r}
	var buf bytes.Buffer
	if err := d.Dump(context.Background(), DumpOptions{OptionFile: "/o", Databases: []string{"a"}}, &buf); err != nil || buf.String() != "-- dump\n" {
		t.Fatalf("%v %q", err, buf.String())
	}
	if err := d.Load(context.Background(), LoadOptions{OptionFile: "/o"}, strings.NewReader("SELECT 1;")); err != nil || r.stdin != "SELECT 1;" || r.cmd.Path != "mariadb" {
		t.Fatalf("%v %+v", err, r)
	}
	if err := (CommandDumper{}).Dump(context.Background(), DumpOptions{}, &buf); err == nil {
		t.Fatal("nil runner")
	}
}
