// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"regexp"
	"strings"
)

type aptAdapter struct{}

func (aptAdapter) Manager() Manager { return APT }

func (aptAdapter) Refresh(ctx context.Context, r Runner) error {
	_, err := run(ctx, r, []int{0}, "apt-get", "update", "-qq")
	return err
}

func (aptAdapter) Installed(ctx context.Context, r Runner) ([]Package, error) {
	res, err := run(ctx, r, []int{0}, "dpkg-query", "-W", `-f=${Package}\t${Version}\t${Architecture}\n`)
	if err != nil {
		return nil, err
	}
	return parseTSV(res.Stdout), nil
}

func (aptAdapter) Updates(ctx context.Context, r Runner) ([]Update, bool, []string, error) {
	res, err := run(ctx, r, []int{0}, "apt", "list", "--upgradable")
	if err != nil {
		return nil, false, nil, err
	}
	return parseAptUpgradable(res.Stdout), true, nil, nil
}

// aptListLine: openssl/stable-security 3.0.11-1~deb12u2 arm64 [upgradable from: 3.0.11-1~deb12u1]
var aptListLine = regexp.MustCompile(`^([^/\s]+)/(\S+)\s+(\S+)\s+(\S+)(?:\s+\[upgradable from:\s*(\S+?)\])?`)

func isSecuritySuite(s string) bool { return strings.Contains(strings.ToLower(s), "security") }

func parseAptUpgradable(out string) []Update {
	var us []Update
	for _, l := range lines(out) {
		m := aptListLine.FindStringSubmatch(l)
		if m == nil {
			continue // "Listing...", warnings
		}
		us = append(us, Update{Name: m[1], Available: m[3], Arch: m[4], Installed: m[5], Source: m[2], Security: isSecuritySuite(m[2])})
	}
	sortUpdates(us)
	return us
}

// aptInst: Inst libssl3 [3.0.11-1~deb12u1] (3.0.11-1~deb12u2 Debian-Security:12/stable-security [arm64])
var aptInst = regexp.MustCompile(`^Inst\s+(\S+)(?:\s+\[([^\]]+)\])?\s+\((\S+)\s`)

func parseAptSimulation(out string) []Change {
	var cs []Change
	for _, l := range lines(out) {
		if m := aptInst.FindStringSubmatch(l); m != nil {
			cs = append(cs, Change{Name: m[1], From: m[2], To: m[3]})
		}
	}
	return cs
}

func (aptAdapter) Simulate(ctx context.Context, r Runner, updates []Update, sel Selection) (Simulation, error) {
	chosen, err := Select(updates, sel)
	if err != nil {
		return Simulation{}, err
	}
	if len(chosen) == 0 {
		return Simulation{Source: "native", Changes: []Change{}}, nil
	}
	args := append([]string{"-s", "-qq", "install", "--only-upgrade"}, names(chosen)...)
	res, err := run(ctx, r, []int{0}, "apt-get", args...)
	if err != nil {
		return Simulation{}, err
	}
	cs := parseAptSimulation(res.Stdout)
	if cs == nil {
		cs = []Change{}
	}
	return Simulation{Source: "native", Changes: cs}, nil
}

func (aptAdapter) Apply(ctx context.Context, r Runner, updates []Update, sel Selection) (CmdResult, error) {
	chosen, err := Select(updates, sel)
	if err != nil {
		return CmdResult{}, err
	}
	if len(chosen) == 0 {
		return CmdResult{}, ErrInvalid
	}
	args := append([]string{"DEBIAN_FRONTEND=noninteractive", "apt-get", "-y", "-qq", "-o", "Dpkg::Options::=--force-confold", "install", "--only-upgrade"}, names(chosen)...)
	return run(ctx, r, []int{0}, "env", args...)
}

var kernelImage = regexp.MustCompile(`^linux-image-(\d.*)$`)

func (aptAdapter) RebootRequired(_ context.Context, _ Runner, fs FS, installed []Package, running string) (Tristate, []string) {
	if fs.Exists("/var/run/reboot-required") {
		var reasons []string
		if b, err := fs.ReadFile("/var/run/reboot-required.pkgs"); err == nil {
			reasons = lines(string(b))
		}
		return Yes, reasons
	}
	if running == "" {
		return Unknown, nil
	}
	for _, p := range installed {
		if m := kernelImage.FindStringSubmatch(p.Name); m != nil && naturalLess(running, m[1]) {
			return Yes, []string{p.Name}
		}
	}
	return No, nil
}
