// SPDX-License-Identifier: AGPL-3.0-only

// Command e3agent is a LAB-ONLY simulated agent for P0 experiment E3
// (tests/p0/e3-manager-failover.sh). It is not the product agent and is not
// built into bin/ by `make build`.
//
// It applies the agent-side fencing guard (internal/fencing) to commands that
// the experiment script issues on behalf of a manager, and simulates an
// idempotent step: a step (op, step) is "executed" at most once; a repeated
// command returns the recorded execution instead of executing again.
//
//	e3agent exec    -state DIR -term N -op OP -step STEP
//	e3agent query   -state DIR -op OP -step STEP
//	e3agent highest -state DIR
//
// exec prints one line and exits 0 (EXECUTED or ALREADY) or 3 (REJECT: stale
// term, nothing executed). query prints EXECUTED term=N or NOT-EXECUTED.
// State lives in DIR/term (fencing.FilePersister) and DIR/steps.json.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
)

// record is one simulated step execution.
type record struct {
	Term uint64    `json:"term"`
	At   time.Time `json:"at"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: e3agent exec|query|highest -state DIR [-term N] [-op OP] [-step STEP]")
		return 2
	}
	fs := flag.NewFlagSet("e3agent "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("state", "", "state directory (required)")
	term := fs.Uint64("term", 0, "fencing term of the issuing manager (exec)")
	op := fs.String("op", "", "operation ID")
	step := fs.String("step", "", "step ID")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *dir == "" {
		fmt.Fprintln(stderr, "e3agent: bad arguments")
		return 2
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(stderr, "e3agent:", err)
		return 1
	}
	g, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(*dir, "term")))
	if err != nil {
		fmt.Fprintln(stderr, "e3agent:", err)
		return 1
	}
	switch args[0] {
	case "highest":
		fmt.Fprintf(stdout, "highest=%d\n", g.Highest())
		return 0
	case "exec", "query":
	default:
		fmt.Fprintf(stderr, "e3agent: unknown command %q\n", args[0])
		return 2
	}
	if *op == "" || *step == "" {
		fmt.Fprintln(stderr, "e3agent: -op and -step are required")
		return 2
	}
	path := filepath.Join(*dir, "steps.json")
	recs, err := load(path)
	if err != nil {
		fmt.Fprintln(stderr, "e3agent:", err)
		return 1
	}
	key := *op + "/" + *step
	if args[0] == "query" {
		if r, ok := recs[key]; ok {
			fmt.Fprintf(stdout, "EXECUTED %s term=%d\n", key, r.Term)
		} else {
			fmt.Fprintf(stdout, "NOT-EXECUTED %s\n", key)
		}
		return 0
	}
	// The fencing check comes before any effect, as in internal/agent.
	if err := g.Admit(*term); err != nil {
		if errors.Is(err, fencing.ErrStaleTerm) {
			fmt.Fprintf(stdout, "REJECT %s term=%d: %v\n", key, *term, err)
			return 3
		}
		fmt.Fprintln(stderr, "e3agent:", err)
		return 1
	}
	if r, ok := recs[key]; ok {
		fmt.Fprintf(stdout, "ALREADY %s executed under term=%d; not executed again (highest=%d)\n", key, r.Term, g.Highest())
		return 0
	}
	recs[key] = record{Term: *term, At: time.Now().UTC()}
	if err := save(path, recs); err != nil {
		fmt.Fprintln(stderr, "e3agent:", err)
		return 1
	}
	fmt.Fprintf(stdout, "EXECUTED %s term=%d (highest=%d, executions recorded=%d)\n", key, *term, g.Highest(), len(recs))
	return 0
}

func load(path string) (map[string]record, error) {
	recs := map[string]record{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return recs, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", path, err)
	}
	return recs, nil
}

// save writes atomically (temp file, fsync, rename) so a forced VM stop
// cannot leave a torn record (E2 showed unsynced writes are lost).
func save(path string, recs map[string]record) error {
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".steps-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
