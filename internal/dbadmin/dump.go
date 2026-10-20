// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Dumper streams a logical export (D02). Implementations run the command from
// BuildDumpCommand; this package never executes anything.
type Dumper interface {
	Dump(ctx context.Context, o DumpOptions, w io.Writer) error
}

// Loader streams a logical import.
type Loader interface {
	Load(ctx context.Context, o LoadOptions, r io.Reader) error
}

// Command is an argv-style process description. There is no shell involved.
type Command struct {
	Path string
	Args []string // without argv[0]
	Env  []string // extra KEY=VALUE entries; never carries secrets by default
}

// Runner executes a Command, wiring stdin/stdout. Provided by the caller.
type Runner interface {
	Run(ctx context.Context, c Command, stdin io.Reader, stdout io.Writer) error
}

// Flavor selects the client binaries.
type Flavor string

const (
	FlavorMariaDB Flavor = "mariadb" // mariadb-dump / mariadb
	FlavorMySQL   Flavor = "mysql"   // mysqldump / mysql
)

func (f Flavor) dumpBinary() (string, error) {
	switch f {
	case "", FlavorMariaDB:
		return "mariadb-dump", nil
	case FlavorMySQL:
		return "mysqldump", nil
	}
	return "", invalid("flavor", "%s", safeQuote(string(f)))
}

func (f Flavor) clientBinary() (string, error) {
	switch f {
	case "", FlavorMariaDB:
		return "mariadb", nil
	case FlavorMySQL:
		return "mysql", nil
	}
	return "", invalid("flavor", "%s", safeQuote(string(f)))
}

// DumpOptions selects what to export.
type DumpOptions struct {
	Flavor Flavor
	// OptionFile holds connection data including the password
	// ([client] section, see RenderOptionFile). It must be readable by the
	// owner only and is passed via --defaults-extra-file; secrets never go
	// into argv.
	OptionFile string
	Databases  []string // required, validated
	Tables     []string // optional, only with exactly one database
	NoData     bool     // schema only
	NoRoutines bool     // skip routines, triggers, events
}

func validOptionFile(p string) error {
	if p == "" || len(p) > 4096 || strings.ContainsRune(p, 0) || strings.ContainsAny(p, "\n\r") {
		return invalid("option file path", "empty or contains control characters")
	}
	if !strings.HasPrefix(p, "/") {
		return invalid("option file path", "must be absolute")
	}
	return nil
}

// BuildDumpCommand returns the mariadb-dump / mysqldump invocation. The
// --defaults-extra-file option must be the first argument.
func BuildDumpCommand(o DumpOptions) (Command, error) {
	bin, err := o.Flavor.dumpBinary()
	if err != nil {
		return Command{}, err
	}
	if err := validOptionFile(o.OptionFile); err != nil {
		return Command{}, err
	}
	if len(o.Databases) == 0 {
		return Command{}, invalid("databases", "at least one database required")
	}
	if len(o.Tables) > 0 && len(o.Databases) != 1 {
		return Command{}, invalid("tables", "table selection requires exactly one database")
	}
	args := []string{
		"--defaults-extra-file=" + o.OptionFile,
		"--single-transaction", // consistent InnoDB snapshot without locking the cluster
		"--skip-lock-tables",
		"--hex-blob",
		"--default-character-set=utf8mb4",
		"--quick",
	}
	if !o.NoRoutines {
		args = append(args, "--routines", "--triggers", "--events")
	}
	if o.NoData {
		args = append(args, "--no-data")
	}
	for _, d := range o.Databases {
		if err := ValidateDatabaseName(d); err != nil {
			return Command{}, err
		}
	}
	for _, t := range o.Tables {
		if err := ValidateTableName(t); err != nil {
			return Command{}, err
		}
	}
	if len(o.Tables) > 0 {
		args = append(args, "--", o.Databases[0])
		args = append(args, o.Tables...)
	} else {
		args = append(args, "--databases", "--")
		args = append(args, o.Databases...)
	}
	return Command{Path: bin, Args: args}, nil
}

// LoadOptions selects the import target.
type LoadOptions struct {
	Flavor     Flavor
	OptionFile string
	Database   string // optional default database; dumps made with --databases carry their own
}

// BuildLoadCommand returns the client invocation reading SQL from stdin.
func BuildLoadCommand(o LoadOptions) (Command, error) {
	bin, err := o.Flavor.clientBinary()
	if err != nil {
		return Command{}, err
	}
	if err := validOptionFile(o.OptionFile); err != nil {
		return Command{}, err
	}
	args := []string{"--defaults-extra-file=" + o.OptionFile, "--default-character-set=utf8mb4", "--batch"}
	if o.Database != "" {
		if err := ValidateDatabaseName(o.Database); err != nil {
			return Command{}, err
		}
		args = append(args, "--database="+o.Database)
	}
	return Command{Path: bin, Args: args}, nil
}

// OptionFileData is the content of a client option file.
type OptionFileData struct {
	Host     string
	Port     int
	User     string
	Password Secret
}

// RenderOptionFile renders a [client] option file. The caller must write it
// with mode 0600 into a private directory and delete it afterwards.
func RenderOptionFile(d OptionFileData) (string, error) {
	if err := ValidateHost(d.Host); err != nil {
		return "", err
	}
	if err := ValidateUserName(d.User); err != nil {
		return "", err
	}
	if d.Port < 1 || d.Port > 65535 {
		return "", invalid("port", "out of range")
	}
	pw := d.Password.Reveal()
	if strings.ContainsAny(pw, "\x00\n\r") || pw == "" {
		return "", invalid("password", "empty or contains control characters")
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return fmt.Sprintf("[client]\nhost=%q\nport=%d\nuser=%q\npassword=\"%s\"\n", d.Host, d.Port, d.User, esc.Replace(pw)), nil
}

// CommandDumper adapts a Runner to Dumper and Loader.
type CommandDumper struct{ Runner Runner }

// Dump implements Dumper.
func (c CommandDumper) Dump(ctx context.Context, o DumpOptions, w io.Writer) error {
	if c.Runner == nil {
		return errors.New("dbadmin: no Runner configured")
	}
	cmd, err := BuildDumpCommand(o)
	if err != nil {
		return err
	}
	return c.Runner.Run(ctx, cmd, nil, w)
}

// Load implements Loader.
func (c CommandDumper) Load(ctx context.Context, o LoadOptions, r io.Reader) error {
	if c.Runner == nil {
		return errors.New("dbadmin: no Runner configured")
	}
	cmd, err := BuildLoadCommand(o)
	if err != nil {
		return err
	}
	return c.Runner.Run(ctx, cmd, r, io.Discard)
}

var (
	_ Dumper = CommandDumper{}
	_ Loader = CommandDumper{}
)
