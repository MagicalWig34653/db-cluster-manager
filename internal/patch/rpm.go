// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"regexp"
	"strings"
)

// rpmAdapter drives dnf or yum (same command line for the used subset).
type rpmAdapter struct{ bin string }

func (a rpmAdapter) Manager() Manager { return Manager(a.bin) }

func (a rpmAdapter) Refresh(ctx context.Context, r Runner) error {
	_, err := run(ctx, r, []int{0}, a.bin, "-q", "makecache")
	return err
}

func rpmInstalled(ctx context.Context, r Runner) ([]Package, error) {
	res, err := run(ctx, r, []int{0}, "rpm", "-qa", `--qf=%{NAME}\t%{VERSION}-%{RELEASE}\t%{ARCH}\n`)
	if err != nil {
		return nil, err
	}
	return parseTSV(res.Stdout), nil
}

func (a rpmAdapter) Installed(ctx context.Context, r Runner) ([]Package, error) {
	return rpmInstalled(ctx, r)
}

func (a rpmAdapter) Updates(ctx context.Context, r Runner) ([]Update, bool, []string, error) {
	res, err := run(ctx, r, []int{0, 100}, a.bin, "-q", "check-update")
	if err != nil {
		return nil, false, nil, err
	}
	us := parseDnfCheckUpdate(res.Stdout)
	var warn []string
	known := true
	sec, err := run(ctx, r, []int{0}, a.bin, "-q", "updateinfo", "list", "--security")
	if err != nil {
		known = false
		warn = append(warn, "security classification unavailable: "+err.Error())
	} else {
		markDnfSecurity(us, parseUpdateinfo(sec.Stdout))
	}
	return us, known, warn, nil
}

// parseDnfCheckUpdate parses "name.arch   version   repo" lines.
func parseDnfCheckUpdate(out string) []Update {
	var us []Update
	for _, l := range lines(out) {
		if strings.HasPrefix(l, "Obsoleting") {
			break
		}
		f := strings.Fields(l)
		if len(f) != 3 || !strings.Contains(f[0], ".") {
			continue // blank, "Last metadata" notices
		}
		i := strings.LastIndex(f[0], ".")
		us = append(us, Update{Name: f[0][:i], Arch: f[0][i+1:], Available: f[1], Source: f[2]})
	}
	sortUpdates(us)
	return us
}

// advisoryEntry is one updateinfo row.
type advisoryEntry struct {
	ID, Severity, Package string
}

// nevra: name-version-release.arch -> name
var nevra = regexp.MustCompile(`^(.+)-[^-]+-[^-]+\.[^.-]+$`)

// parseUpdateinfo parses "RHSA-2024:1234 Important/Sec. openssl-3.0.7-1.el9.x86_64".
func parseUpdateinfo(out string) []advisoryEntry {
	var es []advisoryEntry
	for _, l := range lines(out) {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		m := nevra.FindStringSubmatch(f[len(f)-1])
		if m == nil {
			continue
		}
		es = append(es, advisoryEntry{ID: f[0], Severity: f[1], Package: m[1]})
	}
	return es
}

func markDnfSecurity(us []Update, es []advisoryEntry) {
	for i := range us {
		for _, e := range es {
			if e.Package == us[i].Name {
				us[i].Security = true
				us[i].Advisories = append(us[i].Advisories, e.ID)
			}
		}
	}
}

func (a rpmAdapter) Simulate(ctx context.Context, r Runner, updates []Update, sel Selection) (Simulation, error) {
	chosen, err := Select(updates, sel)
	if err != nil {
		return Simulation{}, err
	}
	if len(chosen) == 0 {
		return Simulation{Source: "update-list", Changes: []Change{}}, nil
	}
	// --assumeno resolves the transaction and exits 1 by design.
	args := append([]string{"-q", "upgrade", "--assumeno"}, names(chosen)...)
	if _, err := run(ctx, r, []int{0, 1}, a.bin, args...); err != nil {
		return Simulation{}, err
	}
	return fromUpdates(chosen), nil
}

func (a rpmAdapter) Apply(ctx context.Context, r Runner, updates []Update, sel Selection) (CmdResult, error) {
	chosen, err := Select(updates, sel)
	if err != nil || len(chosen) == 0 {
		return CmdResult{}, firstErr(err, ErrInvalid)
	}
	args := append([]string{"-y", "-q", "upgrade"}, names(chosen)...)
	return run(ctx, r, []int{0}, a.bin, args...)
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

var kernelCore = map[string]bool{"kernel": true, "kernel-core": true}

func (a rpmAdapter) RebootRequired(ctx context.Context, r Runner, fs FS, installed []Package, running string) (Tristate, []string) {
	if fs.Exists("/usr/bin/needs-restarting") {
		res, err := r.Run(ctx, "needs-restarting", "-r")
		if err == nil && res.ExitCode == 1 {
			return Yes, nil
		}
		if err == nil && res.ExitCode == 0 {
			return No, nil
		}
	}
	return rpmKernelStale(installed, running)
}

// rpmKernelStale compares the running kernel release (uname -r, which ends in
// .arch) with the installed kernel packages.
func rpmKernelStale(installed []Package, running string) (Tristate, []string) {
	if running == "" {
		return Unknown, nil
	}
	seen := false
	for _, p := range installed {
		if !kernelCore[p.Name] {
			continue
		}
		seen = true
		if naturalLess(running, p.Version+"."+p.Arch) {
			return Yes, []string{p.Name}
		}
	}
	if !seen {
		return Unknown, nil
	}
	return No, nil
}

// --- zypper ---

type zypperAdapter struct{}

func (zypperAdapter) Manager() Manager { return Zypper }

func (zypperAdapter) Refresh(ctx context.Context, r Runner) error {
	_, err := run(ctx, r, []int{0}, "zypper", "--quiet", "--non-interactive", "refresh")
	return err
}

func (zypperAdapter) Installed(ctx context.Context, r Runner) ([]Package, error) {
	return rpmInstalled(ctx, r)
}

func (zypperAdapter) Updates(ctx context.Context, r Runner) ([]Update, bool, []string, error) {
	res, err := run(ctx, r, []int{0}, "zypper", "--quiet", "--non-interactive", "list-updates")
	if err != nil {
		return nil, false, nil, err
	}
	us := parseZypperUpdates(res.Stdout)
	var warn []string
	// The patch dry run names the packages the security patches would
	// upgrade; it changes nothing.
	sec, err := run(ctx, r, []int{0, 100, 101, 102, 103}, "zypper", "--non-interactive", "patch", "--dry-run", "--category", "security")
	if err != nil {
		warn = append(warn, "security classification unavailable: "+err.Error())
		return us, false, warn, nil
	}
	set := map[string]bool{}
	for _, n := range parseZypperUpgradeNames(sec.Stdout) {
		set[n] = true
	}
	for i := range us {
		us[i].Security = set[us[i].Name]
	}
	return us, true, nil, nil
}

// parseZypperUpdates: "v | repo | name | current | available | arch".
func parseZypperUpdates(out string) []Update {
	var us []Update
	for _, l := range lines(out) {
		f := strings.Split(l, "|")
		if len(f) < 6 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if f[0] == "S" || f[2] == "Name" || strings.HasPrefix(f[0], "-") {
			continue
		}
		us = append(us, Update{Name: f[2], Installed: f[3], Available: f[4], Arch: f[5], Source: f[1]})
	}
	sortUpdates(us)
	return us
}

// parseZypperUpgradeNames reads the "going to be upgraded:" block of a dry run.
func parseZypperUpgradeNames(out string) []string {
	var ns []string
	in := false
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(l, "going to be upgraded:"):
			in = true
		case in && strings.HasPrefix(l, " "):
			ns = append(ns, strings.Fields(l)...)
		default:
			in = false
		}
	}
	return ns
}

func (zypperAdapter) Simulate(ctx context.Context, r Runner, updates []Update, sel Selection) (Simulation, error) {
	chosen, err := Select(updates, sel)
	if err != nil {
		return Simulation{}, err
	}
	if len(chosen) == 0 {
		return Simulation{Source: "update-list", Changes: []Change{}}, nil
	}
	args := append([]string{"--non-interactive", "update", "--dry-run"}, names(chosen)...)
	if _, err := run(ctx, r, []int{0}, "zypper", args...); err != nil {
		return Simulation{}, err
	}
	return fromUpdates(chosen), nil
}

func (zypperAdapter) Apply(ctx context.Context, r Runner, updates []Update, sel Selection) (CmdResult, error) {
	chosen, err := Select(updates, sel)
	if err != nil || len(chosen) == 0 {
		return CmdResult{}, firstErr(err, ErrInvalid)
	}
	args := append([]string{"--non-interactive", "update"}, names(chosen)...)
	return run(ctx, r, []int{0, 100, 101, 102, 103}, "zypper", args...)
}

func (zypperAdapter) RebootRequired(ctx context.Context, r Runner, _ FS, installed []Package, running string) (Tristate, []string) {
	res, err := r.Run(ctx, "zypper", "--quiet", "needs-rebooting")
	if err == nil {
		switch res.ExitCode {
		case 102:
			return Yes, nil
		case 0:
			return No, nil
		}
	}
	return rpmKernelStale(installed, running)
}
