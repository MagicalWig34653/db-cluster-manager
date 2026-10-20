// SPDX-License-Identifier: AGPL-3.0-only

// Command lab manages the dedicated local test environment: it reports host
// capacity and inventory, plans and creates budgeted Lima VMs, and removes
// only resources recorded in the ownership manifest.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/lab"
)

// cpuCount reports the logical CPU count of the host. It is a variable so
// tests can pin the budget independently of the machine running them.
var cpuCount = runtime.NumCPU

const usage = `usage:
  lab status  [-manifest PATH]
  lab plan    [-manifest PATH] [-vms N] [-cpus C] [-ram GiB] [-disk GiB] [-network lima:user-v2]
  lab apply   [-manifest PATH] [-vms N] [-cpus C] [-ram GiB] [-disk GiB] [-network lima:user-v2] [--apply]
  lab cleanup [-manifest PATH] [--apply]

Without --apply, apply and cleanup only print what they would do.`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, lab.NewExecRunner()))
}

func run(args []string, stdout, stderr io.Writer, r lab.Runner) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "status":
		return cmdStatus(ctx, args[1:], stdout, stderr, r)
	case "plan":
		return cmdCreate(ctx, "plan", args[1:], stdout, stderr, r)
	case "apply":
		return cmdCreate(ctx, "apply", args[1:], stdout, stderr, r)
	case "cleanup":
		return cmdCleanup(ctx, args[1:], stdout, stderr, r)
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "lab: unknown command %q\n%s\n", args[0], usage)
	return 2
}

func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("lab "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("manifest", filepath.Join(lab.LabDir, "manifest.json"), "ownership manifest path")
	return fs, path
}

func parse(fs *flag.FlagSet, args []string, stderr io.Writer) bool {
	if err := fs.Parse(args); err != nil {
		return false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "lab: unexpected arguments %q\n", fs.Args())
		return false
	}
	return true
}

// detectHost determines host capacity through the Runner and refuses an
// incomplete result, so the budget is never computed from guessed values.
func detectHost(ctx context.Context, r lab.Runner) (lab.Host, error) {
	host, err := lab.DetectHost(func(name string, args ...string) (string, error) {
		return r.Run(ctx, name, args...)
	})
	if err != nil {
		return lab.Host{}, fmt.Errorf("lab: cannot detect host capacity: %w", err)
	}
	host.LogicalCPUs = cpuCount()
	if host.LogicalCPUs <= 0 || host.RAMBytes == 0 {
		return lab.Host{}, fmt.Errorf("lab: cannot detect host capacity: %d logical CPUs, %d bytes RAM", host.LogicalCPUs, host.RAMBytes)
	}
	return host, nil
}

func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer, r lab.Runner) int {
	fs, path := newFlagSet("status", stderr)
	if !parse(fs, args, stderr) {
		return 2
	}
	code := 0
	host, err := detectHost(ctx, r)
	if err != nil {
		fmt.Fprintln(stderr, err)
		code = 1
	} else {
		fmt.Fprintf(stdout, "host:\n  logical CPUs: %d\n  RAM: %s GiB\n", host.LogicalCPUs, gibString(host.RAMBytes))
	}
	fmt.Fprintln(stdout, "budget:")
	if err == nil {
		fmt.Fprintf(stdout, "  lab CPU limit: %d\n", host.MaxCPUs())
	} else {
		fmt.Fprintln(stdout, "  lab CPU limit: unknown (host capacity not detected)")
	}
	fmt.Fprintf(stdout, "  lab RAM limit: %s GiB\n  lab disk limit: %s GiB\n", gibString(lab.MaxRAMBytes), gibString(lab.MaxDiskBytes))
	m, err := lab.LoadManifest(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "manifest %s:\n", *path)
	if len(m.Resources) == 0 {
		fmt.Fprintln(stdout, "  (empty)")
	}
	for _, res := range m.Resources {
		fmt.Fprintf(stdout, "  %s %s\n", res.Kind, res.Name)
	}
	snap, err := lab.Snapshot(ctx, r)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	printList(stdout, "inventory", snap)
	printList(stdout, "unrelated (never touched)", lab.Unrelated(snap, m))
	return code
}

func cmdCreate(ctx context.Context, name string, args []string, stdout, stderr io.Writer, r lab.Runner) int {
	fs, path := newFlagSet(name, stderr)
	vms := fs.Int("vms", 3, "number of VMs (dbcm-lab-m1..mN)")
	cpus := fs.Int("cpus", 2, "vCPUs per VM")
	ram := fs.Int("ram", 4, "RAM per VM in GiB")
	disk := fs.Int("disk", 20, "disk per VM in GiB")
	network := fs.String("network", "", "additional Lima network attached at creation; only "+lab.UserV2Network+" (shared Lima state, never removed by lab)")
	var apply *bool
	if name == "apply" {
		apply = fs.Bool("apply", false, "execute the plan (otherwise dry run)")
	}
	if !parse(fs, args, stderr) {
		return 2
	}
	if *vms < 1 || *cpus < 1 || *ram < 1 || *disk < 1 {
		fmt.Fprintln(stderr, "lab: -vms, -cpus, -ram and -disk must be positive")
		return 2
	}
	if *network != "" && *network != lab.UserV2Network {
		fmt.Fprintf(stderr, "lab: -network %q is not supported (only %q)\n", *network, lab.UserV2Network)
		return 2
	}
	host, err := detectHost(ctx, r)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m, err := lab.LoadManifest(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	specs := make([]lab.VMSpec, *vms)
	for i := range specs {
		specs[i] = lab.VMSpec{
			Name:      fmt.Sprintf("%sm%d", lab.NamePrefix, i+1),
			CPUs:      *cpus,
			RAMBytes:  uint64(*ram) << 30,
			DiskBytes: uint64(*disk) << 30,
			Network:   *network,
		}
	}
	actions, err := lab.PlanCreate(lab.Budget{Host: host}, m, specs)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if apply == nil || !*apply {
		printActions(stdout, "planned actions (dry run)", actions)
		if name == "apply" {
			fmt.Fprintln(stdout, "dry run: nothing executed; pass --apply to execute")
		}
		return 0
	}

	pre, err := lab.Snapshot(ctx, r)
	if err != nil {
		fmt.Fprintf(stderr, "lab: refusing to apply without a pre-snapshot: %v\n", err)
		return 1
	}
	printList(stdout, "pre-snapshot", pre)
	if err := recordSnapshot(filepath.Join(filepath.Dir(*path), "pre-apply-snapshot.json"), pre); err != nil {
		fmt.Fprintf(stderr, "lab: refusing to apply: %v\n", err)
		return 1
	}
	printActions(stdout, "executing actions", actions)
	execErr := lab.Execute(ctx, r, m, actions, func() error { return m.Save(*path) })
	code := 0
	if execErr != nil {
		fmt.Fprintln(stderr, execErr)
		code = 1
	}
	post, err := lab.Snapshot(ctx, r)
	if err != nil {
		fmt.Fprintf(stderr, "lab: post-snapshot failed, inventory unverified: %v\n", err)
		return 1
	}
	added, removed := lab.Diff(pre, post)
	printDiff(stdout, added, removed)
	owned := ownedEntries(m)
	var bad []string
	for _, e := range added {
		if !owned[e] {
			bad = append(bad, "unexpected addition "+e)
		}
	}
	for _, e := range removed {
		bad = append(bad, "unexpected removal "+e)
	}
	if len(bad) > 0 {
		fmt.Fprintf(stderr, "lab: ERROR: inventory changed beyond owned additions: %s\n", strings.Join(bad, ", "))
		return 1
	}
	return code
}

func cmdCleanup(ctx context.Context, args []string, stdout, stderr io.Writer, r lab.Runner) int {
	fs, path := newFlagSet("cleanup", stderr)
	apply := fs.Bool("apply", false, "execute the cleanup (otherwise preview)")
	if !parse(fs, args, stderr) {
		return 2
	}
	m, err := lab.LoadManifest(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	actions := lab.PlanCleanup(m)
	if !*apply {
		printActions(stdout, "cleanup preview", actions)
		fmt.Fprintln(stdout, "preview only: nothing executed; pass --apply to execute")
		return 0
	}
	if _, err := detectHost(ctx, r); err != nil {
		fmt.Fprintf(stderr, "%v; refusing to clean up\n", err)
		return 1
	}
	pre, err := lab.Snapshot(ctx, r)
	if err != nil {
		fmt.Fprintf(stderr, "lab: refusing to clean up without a pre-snapshot: %v\n", err)
		return 1
	}
	unrelated := lab.Unrelated(pre, m)
	printList(stdout, "pre-snapshot", pre)
	printList(stdout, "unrelated (must survive)", unrelated)
	printActions(stdout, "executing actions", actions)
	code := 0
	if err := lab.Execute(ctx, r, m, actions, func() error { return m.Save(*path) }); err != nil {
		fmt.Fprintln(stderr, err)
		code = 1
	}
	post, err := lab.Snapshot(ctx, r)
	if err != nil {
		fmt.Fprintf(stderr, "lab: post-snapshot failed, unrelated resources unverified: %v\n", err)
		return 1
	}
	added, removed := lab.Diff(pre, post)
	printDiff(stdout, added, removed)
	present := map[string]bool{}
	for _, e := range post {
		present[e] = true
	}
	var lost []string
	for _, e := range unrelated {
		if !present[e] {
			lost = append(lost, e)
		}
	}
	if len(lost) > 0 {
		fmt.Fprintf(stderr, "lab: ERROR: unrelated resources disappeared during cleanup: %s\n", strings.Join(lost, ", "))
		return 1
	}
	fmt.Fprintf(stdout, "unrelated resources intact (%d)\n", len(unrelated))
	return code
}

// ownedEntries maps manifest resources to their snapshot entry names.
func ownedEntries(m *lab.Manifest) map[string]bool {
	owned := map[string]bool{}
	for _, res := range m.Resources {
		if !lab.IsLabName(res.Name) {
			continue
		}
		switch res.Kind {
		case "lima-vm":
			owned["lima:"+res.Name] = true
		case "colima-profile":
			owned["colima:"+res.Name] = true
		}
	}
	return owned
}

func recordSnapshot(path string, snap []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("record pre-snapshot: %w", err)
	}
	if snap == nil {
		snap = []string{}
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("record pre-snapshot: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("record pre-snapshot: %w", err)
	}
	return nil
}

func printActions(w io.Writer, title string, actions []lab.Action) {
	fmt.Fprintf(w, "%s:\n", title)
	if len(actions) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, a := range actions {
		fmt.Fprintf(w, "  # %s\n  %s\n", a.Describe, strings.Join(a.Command, " "))
	}
}

func printList(w io.Writer, title string, entries []string) {
	fmt.Fprintf(w, "%s:\n", title)
	if len(entries) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, e := range entries {
		fmt.Fprintf(w, "  %s\n", e)
	}
}

func printDiff(w io.Writer, added, removed []string) {
	fmt.Fprintln(w, "post-snapshot diff:")
	fmt.Fprintf(w, "  added: %s\n", joinOrNone(added))
	fmt.Fprintf(w, "  removed: %s\n", joinOrNone(removed))
}

func joinOrNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

func gibString(b uint64) string {
	return fmt.Sprintf("%.1f", float64(b)/float64(1<<30))
}
