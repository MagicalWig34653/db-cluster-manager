// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"regexp"
	"strings"
)

// --- apk (Alpine) ---

type apkAdapter struct{}

func (apkAdapter) Manager() Manager { return APK }

func (apkAdapter) Refresh(ctx context.Context, r Runner) error {
	_, err := run(ctx, r, []int{0}, "apk", "update", "-q")
	return err
}

var apkNameVer = regexp.MustCompile(`^(.+?)-(\d[^-]*(?:-r\d+)?)$`)

func splitAPK(s string) (name, ver string, ok bool) {
	m := apkNameVer.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

var apkListLine = regexp.MustCompile(`^(\S+)\s+(\S+)\s+\{[^}]*\}\s+\([^)]*\)\s+\[([^\]]*)\]`)

func parseAPKList(out string) (pkgs []Package, ups []Update) {
	for _, l := range lines(out) {
		m := apkListLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		name, ver, ok := splitAPK(m[1])
		if !ok {
			continue
		}
		if strings.HasPrefix(m[3], "upgradable from: ") {
			u := Update{Name: name, Available: ver, Arch: m[2]}
			if on, ov, ok := splitAPK(strings.TrimPrefix(m[3], "upgradable from: ")); ok && on == name {
				u.Installed = ov
			}
			ups = append(ups, u)
			continue
		}
		pkgs = append(pkgs, Package{Name: name, Version: ver, Arch: m[2]})
	}
	sortPackages(pkgs)
	sortUpdates(ups)
	return pkgs, ups
}

func (apkAdapter) Installed(ctx context.Context, r Runner) ([]Package, error) {
	res, err := run(ctx, r, []int{0}, "apk", "list", "-I")
	if err != nil {
		return nil, err
	}
	p, _ := parseAPKList(res.Stdout)
	return p, nil
}

func (apkAdapter) Updates(ctx context.Context, r Runner) ([]Update, bool, []string, error) {
	res, err := run(ctx, r, []int{0}, "apk", "list", "-u")
	if err != nil {
		return nil, false, nil, err
	}
	_, u := parseAPKList(res.Stdout)
	return u, false, []string{"apk does not classify security updates; use the CVE inventory match"}, nil
}

var apkSim = regexp.MustCompile(`Upgrading (\S+) \((\S+) -> (\S+)\)`)

func (apkAdapter) Simulate(ctx context.Context, r Runner, updates []Update, sel Selection) (Simulation, error) {
	chosen, err := Select(updates, sel)
	if err != nil {
		return Simulation{}, err
	}
	if len(chosen) == 0 {
		return Simulation{Source: "native", Changes: []Change{}}, nil
	}
	args := append([]string{"add", "--upgrade", "--simulate"}, names(chosen)...)
	res, err := run(ctx, r, []int{0}, "apk", args...)
	if err != nil {
		return Simulation{}, err
	}
	cs := []Change{}
	for _, m := range apkSim.FindAllStringSubmatch(res.Stdout, -1) {
		cs = append(cs, Change{Name: m[1], From: m[2], To: m[3]})
	}
	return Simulation{Source: "native", Changes: cs}, nil
}

func (apkAdapter) Apply(ctx context.Context, r Runner, updates []Update, sel Selection) (CmdResult, error) {
	chosen, err := Select(updates, sel)
	if err != nil || len(chosen) == 0 {
		return CmdResult{}, firstErr(err, ErrInvalid)
	}
	args := append([]string{"add", "--upgrade", "-q"}, names(chosen)...)
	return run(ctx, r, []int{0}, "apk", args...)
}

func (apkAdapter) RebootRequired(context.Context, Runner, FS, []Package, string) (Tristate, []string) {
	return Unknown, nil // Alpine offers no reboot indicator
}

// --- pacman (Arch) ---

type pacmanAdapter struct{}

func (pacmanAdapter) Manager() Manager { return Pacman }

// Refresh is deliberately empty: pacman -Sy without -u causes partial
// upgrades. checkupdates uses a temporary sync database.
func (pacmanAdapter) Refresh(context.Context, Runner) error { return nil }

func (pacmanAdapter) Installed(ctx context.Context, r Runner) ([]Package, error) {
	res, err := run(ctx, r, []int{0}, "pacman", "-Q")
	if err != nil {
		return nil, err
	}
	var ps []Package
	for _, l := range lines(res.Stdout) {
		if f := strings.Fields(l); len(f) == 2 {
			ps = append(ps, Package{Name: f[0], Version: f[1]})
		}
	}
	sortPackages(ps)
	return ps, nil
}

func (pacmanAdapter) Updates(ctx context.Context, r Runner) ([]Update, bool, []string, error) {
	// checkupdates exits 2 when nothing is pending.
	res, err := run(ctx, r, []int{0, 2}, "checkupdates")
	if err != nil {
		return nil, false, nil, err
	}
	var us []Update
	for _, l := range lines(res.Stdout) {
		f := strings.Fields(l)
		if len(f) == 4 && f[2] == "->" {
			us = append(us, Update{Name: f[0], Installed: f[1], Available: f[3]})
		}
	}
	sortUpdates(us)
	return us, false, []string{"pacman does not classify security updates; use the CVE inventory match"}, nil
}

// pacmanFull checks the selection covers every pending update: Arch does not
// support partial upgrades.
func pacmanFull(updates []Update, sel Selection) error {
	chosen, err := Select(updates, sel)
	if err != nil {
		return err
	}
	if len(chosen) != len(updates) {
		return ErrPartialUnsupported
	}
	return nil
}

func (pacmanAdapter) Simulate(_ context.Context, _ Runner, updates []Update, sel Selection) (Simulation, error) {
	if err := pacmanFull(updates, sel); err != nil {
		return Simulation{}, err
	}
	return fromUpdates(updates), nil
}

func (pacmanAdapter) Apply(ctx context.Context, r Runner, updates []Update, sel Selection) (CmdResult, error) {
	if err := pacmanFull(updates, sel); err != nil {
		return CmdResult{}, err
	}
	return run(ctx, r, []int{0}, "pacman", "-Syu", "--noconfirm")
}

func (pacmanAdapter) RebootRequired(context.Context, Runner, FS, []Package, string) (Tristate, []string) {
	return Unknown, nil
}
