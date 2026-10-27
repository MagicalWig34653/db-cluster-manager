// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Selection is what an apply or simulation covers. Packages are always
// explicit: a plan never means "everything the manager finds at apply time".
type Selection struct {
	Packages     []string
	SecurityOnly bool
}

// Change is one planned or applied package change.
type Change struct {
	Name string `json:"name"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Simulation is a dry run result.
type Simulation struct {
	Changes []Change `json:"changes"`
	// Source is "native" when the package manager's own simulation was
	// parsed and "update-list" when the changes were derived from the
	// update list because the manager has no parseable dry run.
	Source string `json:"source"`
}

// Adapter hides one package manager. Methods issue only the read-only
// commands documented in ADR 0023, except Refresh and Apply.
type Adapter interface {
	Manager() Manager
	// Refresh updates the package indexes (not the packages). The empty
	// command means the manager has no safe refresh.
	Refresh(ctx context.Context, r Runner) error
	Installed(ctx context.Context, r Runner) ([]Package, error)
	// Updates lists available updates; known is false when the manager
	// could not classify security updates.
	Updates(ctx context.Context, r Runner) (updates []Update, securityKnown bool, warnings []string, err error)
	Simulate(ctx context.Context, r Runner, updates []Update, sel Selection) (Simulation, error)
	Apply(ctx context.Context, r Runner, updates []Update, sel Selection) (CmdResult, error)
	// RebootRequired inspects the host after (or without) an apply.
	RebootRequired(ctx context.Context, r Runner, fs FS, installed []Package, running string) (Tristate, []string)
}

// NewAdapter returns the adapter of a manager.
func NewAdapter(m Manager) (Adapter, error) {
	switch m {
	case APT:
		return aptAdapter{}, nil
	case DNF, YUM:
		return rpmAdapter{bin: string(m)}, nil
	case Zypper:
		return zypperAdapter{}, nil
	case APK:
		return apkAdapter{}, nil
	case Pacman:
		return pacmanAdapter{}, nil
	}
	return nil, fmt.Errorf("%w: manager %q", ErrInvalid, m)
}

// Detect finds the package manager by well-known binary paths. dnf wins over
// yum (on current systems yum is an alias of dnf).
func Detect(fs FS) (Manager, error) {
	for _, c := range []struct {
		m     Manager
		paths []string
	}{
		{APT, []string{"/usr/bin/apt-get", "/bin/apt-get"}},
		{DNF, []string{"/usr/bin/dnf", "/bin/dnf"}},
		{YUM, []string{"/usr/bin/yum", "/bin/yum"}},
		{Zypper, []string{"/usr/bin/zypper", "/bin/zypper"}},
		{APK, []string{"/sbin/apk", "/usr/sbin/apk"}},
		{Pacman, []string{"/usr/bin/pacman", "/bin/pacman"}},
	} {
		for _, p := range c.paths {
			if fs.Exists(p) {
				return c.m, nil
			}
		}
	}
	return "", ErrNoManager
}

var safePkg = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:~-]{0,200}$`)

// ValidPackageName reports whether s is a conservative package name. Names
// reach a command line as separate arguments, but option-like or odd names
// are refused anyway.
func ValidPackageName(s string) bool { return safePkg.MatchString(s) }

// Select returns the updates covered by sel. Explicit packages must all have
// an update, otherwise ErrInvalid.
func Select(updates []Update, sel Selection) ([]Update, error) {
	var out []Update
	if len(sel.Packages) == 0 {
		for _, u := range updates {
			if !sel.SecurityOnly || u.Security {
				out = append(out, u)
			}
		}
		return out, nil
	}
	by := map[string]Update{}
	for _, u := range updates {
		by[u.Name] = u
	}
	seen := map[string]bool{}
	for _, p := range sel.Packages {
		if !ValidPackageName(p) || seen[p] {
			return nil, fmt.Errorf("%w: package %q", ErrInvalid, p)
		}
		seen[p] = true
		u, ok := by[p]
		if !ok || (sel.SecurityOnly && !u.Security) {
			return nil, fmt.Errorf("%w: no matching update for %q", ErrInvalid, p)
		}
		out = append(out, u)
	}
	return out, nil
}

func names(us []Update) []string {
	out := make([]string, 0, len(us))
	seen := map[string]bool{}
	for _, u := range us {
		if !seen[u.Name] {
			seen[u.Name] = true
			out = append(out, u.Name)
		}
	}
	return out
}

func fromUpdates(us []Update) Simulation {
	s := Simulation{Source: "update-list", Changes: []Change{}}
	for _, u := range us {
		s.Changes = append(s.Changes, Change{Name: u.Name, From: u.Installed, To: u.Available})
	}
	return s
}

func run(ctx context.Context, r Runner, okCodes []int, name string, args ...string) (CmdResult, error) {
	res, err := r.Run(ctx, name, args...)
	if err != nil {
		return res, fmt.Errorf("patch: %s: %w", name, err)
	}
	for _, c := range okCodes {
		if res.ExitCode == c {
			return res, nil
		}
	}
	msg := strings.TrimSpace(res.Stderr)
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return res, fmt.Errorf("patch: %s %s exited %d: %s", name, firstArg(args), res.ExitCode, msg)
}

func firstArg(a []string) string {
	if len(a) > 0 {
		return a[0]
	}
	return ""
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r \t")
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// parseTSV parses "name\tversion\tarch" lines.
func parseTSV(out string) []Package {
	var ps []Package
	for _, l := range lines(out) {
		f := strings.Split(l, "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		p := Package{Name: f[0], Version: f[1]}
		if len(f) > 2 {
			p.Arch = f[2]
		}
		ps = append(ps, p)
	}
	sortPackages(ps)
	return ps
}

// naturalLess compares dotted/dashed numeric strings segment-wise; used only
// for the kernel staleness heuristic, never for vulnerability matching.
func naturalLess(a, b string) bool {
	sp := func(s string) []string {
		return regexp.MustCompile(`\d+|[^\d]+`).FindAllString(s, -1)
	}
	x, y := sp(a), sp(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == y[i] {
			continue
		}
		xi, e1 := strconv.Atoi(x[i])
		yi, e2 := strconv.Atoi(y[i])
		if e1 == nil && e2 == nil {
			return xi < yi
		}
		return x[i] < y[i]
	}
	return len(x) < len(y)
}
