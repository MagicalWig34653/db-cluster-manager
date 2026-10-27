// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func ok(out string) CmdResult { return CmdResult{Stdout: out} }

const aptList = `Listing...
openssl/stable-security 3.0.11-1~deb12u2 arm64 [upgradable from: 3.0.11-1~deb12u1]
curl/stable-security,stable-updates 7.88.1-10+deb12u5 arm64 [upgradable from: 7.88.1-10+deb12u4]
tzdata/stable-updates 2024a-0+deb12u1 all [upgradable from: 2023c-5]
`

func TestParseAptUpgradable(t *testing.T) {
	us := parseAptUpgradable(aptList)
	if len(us) != 3 {
		t.Fatalf("%+v", us)
	}
	if us[0].Name != "curl" || !us[0].Security || us[0].Installed != "7.88.1-10+deb12u4" {
		t.Fatalf("%+v", us[0])
	}
	if us[2].Name != "tzdata" || us[2].Security || us[2].Arch != "all" {
		t.Fatalf("%+v", us[2])
	}
}

func TestParseAptSimulation(t *testing.T) {
	out := "Inst libssl3 [3.0.11-1~deb12u1] (3.0.11-1~deb12u2 Debian-Security:12/stable-security [arm64])\nConf libssl3 (3.0.11-1~deb12u2 Debian-Security:12/stable-security [arm64])\nInst newpkg (1.0 stable [arm64])\n"
	cs := parseAptSimulation(out)
	want := []Change{{"libssl3", "3.0.11-1~deb12u1", "3.0.11-1~deb12u2"}, {"newpkg", "", "1.0"}}
	if !reflect.DeepEqual(cs, want) {
		t.Fatalf("%+v", cs)
	}
}

func aptRunner() *FakeRunner {
	return &FakeRunner{Responses: map[string]CmdResult{
		"uname -rm": ok("6.1.0-18-arm64 aarch64\n"),
		`dpkg-query -W -f=${Package}\t${Version}\t${Architecture}\n`: ok("openssl\t3.0.11-1~deb12u1\tarm64\ncurl\t7.88.1-10+deb12u4\tarm64\nlinux-image-6.1.0-18-arm64\t6.1.76-1\tarm64\n"),
		"apt list --upgradable": ok(aptList),
	}}
}

func TestCollectAptReadOnly(t *testing.T) {
	r := aptRunner()
	fs := FakeFS{"/usr/bin/apt-get": "", "/etc/os-release": "ID=debian\nVERSION_ID=\"12\"\nPRETTY_NAME=\"Debian GNU/Linux 12\"\n"}
	rep, err := Collector{HostID: "h1", Runner: r, FS: fs, Now: func() time.Time { return time.Unix(1, 0) }}.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Manager != APT || rep.OS.ID != "debian" || rep.OS.Kernel != "6.1.0-18-arm64" || len(rep.Installed) != 3 || len(rep.SecurityUpdates()) != 2 || !rep.SecurityKnown {
		t.Fatalf("%+v", rep)
	}
	if rep.RebootRequired != No {
		t.Fatalf("reboot %v", rep.RebootRequired)
	}
	for _, c := range r.Calls {
		if strings.Contains(c, "install") || strings.Contains(c, "update") && !strings.Contains(c, "list") {
			t.Fatalf("mutating command in report mode: %s", c)
		}
	}
}

func TestAptRebootRequired(t *testing.T) {
	a := aptAdapter{}
	fs := FakeFS{"/var/run/reboot-required": "", "/var/run/reboot-required.pkgs": "linux-image-6.1.0-20-arm64\n"}
	if s, why := a.RebootRequired(ctx, nil, fs, nil, "6.1.0-18-arm64"); s != Yes || len(why) != 1 {
		t.Fatal(s, why)
	}
	inst := []Package{{Name: "linux-image-6.1.0-20-arm64"}}
	if s, _ := a.RebootRequired(ctx, nil, FakeFS{}, inst, "6.1.0-18-arm64"); s != Yes {
		t.Fatal("stale kernel not detected", s)
	}
	if s, _ := a.RebootRequired(ctx, nil, FakeFS{}, inst, ""); s != Unknown {
		t.Fatal("unknown kernel must be unknown", s)
	}
}

func TestAptSimulateAndApplyUseExplicitPackages(t *testing.T) {
	r := aptRunner()
	r.Responses["apt-get -s -qq install --only-upgrade openssl"] = ok("Inst openssl [3.0.11-1~deb12u1] (3.0.11-1~deb12u2 Debian-Security:12/stable-security [arm64])\n")
	r.Responses["env DEBIAN_FRONTEND=noninteractive apt-get -y -qq -o Dpkg::Options::=--force-confold install --only-upgrade openssl"] = ok("")
	ups := parseAptUpgradable(aptList)
	a := aptAdapter{}
	sim, err := a.Simulate(ctx, r, ups, Selection{Packages: []string{"openssl"}})
	if err != nil || sim.Source != "native" || len(sim.Changes) != 1 {
		t.Fatal(sim, err)
	}
	if _, err := a.Apply(ctx, r, ups, Selection{Packages: []string{"openssl"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(ctx, r, ups, Selection{Packages: []string{"tzdata"}, SecurityOnly: true}); !errors.Is(err, ErrInvalid) {
		t.Fatal("non-security package accepted under securityOnly:", err)
	}
	if _, err := a.Apply(ctx, r, ups, Selection{Packages: []string{"--allow-downgrades"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("option-like package accepted")
	}
	if _, err := a.Apply(ctx, r, ups, Selection{Packages: []string{"notpending"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("package without update accepted")
	}
}

func TestDetect(t *testing.T) {
	for p, want := range map[string]Manager{"/usr/bin/apt-get": APT, "/usr/bin/dnf": DNF, "/usr/bin/yum": YUM, "/usr/bin/zypper": Zypper, "/sbin/apk": APK, "/usr/bin/pacman": Pacman} {
		if got, err := Detect(FakeFS{p: ""}); err != nil || got != want {
			t.Fatal(p, got, err)
		}
	}
	if _, err := Detect(FakeFS{}); !errors.Is(err, ErrNoManager) {
		t.Fatal(err)
	}
	if m, _ := Detect(FakeFS{"/usr/bin/dnf": "", "/usr/bin/yum": ""}); m != DNF {
		t.Fatal("dnf must win over yum")
	}
}

const dnfCheck = `
Last metadata expiration check: 0:10:00 ago.
openssl.aarch64          1:3.0.7-28.el9_4          baseos
kernel-core.aarch64      5.14.0-427.28.1.el9_4     baseos
tzdata.noarch            2024a-1.el9               baseos

Obsoleting Packages
foo.aarch64   1.0   appstream
`

const updateinfo = `RLSA-2024:4623 Important/Sec. openssl-1:3.0.7-28.el9_4.aarch64
RLSA-2024:4000 Moderate/Sec.  kernel-core-5.14.0-427.28.1.el9_4.aarch64
`

func TestParseDnf(t *testing.T) {
	us := parseDnfCheckUpdate(dnfCheck)
	if len(us) != 3 {
		t.Fatalf("%+v", us)
	}
	markDnfSecurity(us, parseUpdateinfo(updateinfo))
	got := map[string]bool{}
	for _, u := range us {
		got[u.Name] = u.Security
	}
	if !got["openssl"] || !got["kernel-core"] || got["tzdata"] {
		t.Fatalf("%+v", us)
	}
	if us[1].Advisories[0] != "RLSA-2024:4623" && us[0].Advisories[0] != "RLSA-2024:4623" {
		t.Fatalf("%+v", us)
	}
}

func TestDnfUpdatesExitCodesAndSecurityFallback(t *testing.T) {
	r := &FakeRunner{Responses: map[string]CmdResult{
		"dnf -q check-update":               {Stdout: dnfCheck, ExitCode: 100},
		"dnf -q updateinfo list --security": {Stderr: "boom", ExitCode: 1},
	}}
	us, known, warn, err := (rpmAdapter{bin: "dnf"}).Updates(ctx, r)
	if err != nil || known || len(us) != 3 || len(warn) != 1 {
		t.Fatal(us, known, warn, err)
	}
	r.Responses["dnf -q check-update"] = CmdResult{Stderr: "repo error", ExitCode: 1}
	if _, _, _, err := (rpmAdapter{bin: "dnf"}).Updates(ctx, r); err == nil {
		t.Fatal("exit 1 of check-update is an error")
	}
}

func TestRPMKernelStale(t *testing.T) {
	inst := []Package{{Name: "kernel-core", Version: "5.14.0-427.28.1.el9_4", Arch: "aarch64"}, {Name: "kernel-core", Version: "5.14.0-362.8.1.el9_3", Arch: "aarch64"}}
	if s, _ := rpmKernelStale(inst, "5.14.0-362.8.1.el9_3.aarch64"); s != Yes {
		t.Fatal(s)
	}
	if s, _ := rpmKernelStale(inst, "5.14.0-427.28.1.el9_4.aarch64"); s != No {
		t.Fatal(s)
	}
	if s, _ := rpmKernelStale(nil, "5.14"); s != Unknown {
		t.Fatal(s)
	}
}

func TestDnfNeedsRestarting(t *testing.T) {
	r := &FakeRunner{Responses: map[string]CmdResult{"needs-restarting -r": {ExitCode: 1}}}
	s, _ := (rpmAdapter{bin: "dnf"}).RebootRequired(ctx, r, FakeFS{"/usr/bin/needs-restarting": ""}, nil, "")
	if s != Yes {
		t.Fatal(s)
	}
}

const zypperLU = `S | Repository | Name    | Current Version | Available Version | Arch
--+------------+---------+-----------------+-------------------+-------
v | Update     | openssl | 3.1.4-1.1       | 3.1.4-2.1         | aarch64
v | Update     | vim     | 9.0-1           | 9.1-1             | aarch64
`

func TestParseZypper(t *testing.T) {
	us := parseZypperUpdates(zypperLU)
	if len(us) != 2 || us[0].Name != "openssl" || us[0].Available != "3.1.4-2.1" || us[0].Installed != "3.1.4-1.1" {
		t.Fatalf("%+v", us)
	}
	names := parseZypperUpgradeNames("Loading repository data...\nThe following 2 packages are going to be upgraded:\n  libopenssl3 openssl-3\n  curl\n\n2 packages to upgrade.\n")
	if !reflect.DeepEqual(names, []string{"libopenssl3", "openssl-3", "curl"}) {
		t.Fatalf("%v", names)
	}
}

func TestZypperSecurityFromPatchDryRun(t *testing.T) {
	r := &FakeRunner{Responses: map[string]CmdResult{
		"zypper --quiet --non-interactive list-updates":                ok(zypperLU),
		"zypper --non-interactive patch --dry-run --category security": ok("The following 1 package is going to be upgraded:\n"),
	}}
	r.Responses["zypper --non-interactive patch --dry-run --category security"] = ok("The following 2 packages are going to be upgraded:\n  openssl\n\n")
	us, known, _, err := (zypperAdapter{}).Updates(ctx, r)
	if err != nil || !known || !us[0].Security || us[1].Security {
		t.Fatal(us, known, err)
	}
}

const apkU = `openssl-3.1.4-r0 aarch64 {openssl} (Apache-2.0) [upgradable from: openssl-3.1.3-r0]
py3-pip-23.3-r0 noarch {py3-pip} (MIT) [upgradable from: py3-pip-23.1-r0]
`
const apkI = `busybox-1.36.1-r0 aarch64 {busybox} (GPL-2.0-only) [installed]
libssl3-3.1.3-r0 aarch64 {openssl} (Apache-2.0) [installed]
`

func TestParseAPK(t *testing.T) {
	_, us := parseAPKList(apkU)
	if len(us) != 2 || us[0].Name != "openssl" || us[0].Installed != "3.1.3-r0" || us[1].Name != "py3-pip" || us[1].Available != "23.3-r0" {
		t.Fatalf("%+v", us)
	}
	ps, _ := parseAPKList(apkI)
	if len(ps) != 2 || ps[1].Name != "libssl3" || ps[1].Version != "3.1.3-r0" {
		t.Fatalf("%+v", ps)
	}
	r := &FakeRunner{Responses: map[string]CmdResult{"apk list -u": ok(apkU)}}
	_, known, warn, err := (apkAdapter{}).Updates(ctx, r)
	if err != nil || known || len(warn) != 1 {
		t.Fatal(known, warn, err)
	}
}

func TestAPKSimulate(t *testing.T) {
	_, us := parseAPKList(apkU)
	r := &FakeRunner{Responses: map[string]CmdResult{"apk add --upgrade --simulate openssl": ok("(1/1) Upgrading openssl (3.1.3-r0 -> 3.1.4-r0)\nOK: 10 MiB in 20 packages\n")}}
	sim, err := (apkAdapter{}).Simulate(ctx, r, us, Selection{Packages: []string{"openssl"}})
	if err != nil || len(sim.Changes) != 1 || sim.Changes[0].To != "3.1.4-r0" {
		t.Fatal(sim, err)
	}
}

func TestPacman(t *testing.T) {
	r := &FakeRunner{Responses: map[string]CmdResult{
		"checkupdates":            ok("linux 6.6.1.arch1-1 -> 6.6.2.arch1-1\nopenssl 3.1.3-1 -> 3.1.4-1\n"),
		"pacman -Syu --noconfirm": ok(""),
	}}
	p := pacmanAdapter{}
	us, known, _, err := p.Updates(ctx, r)
	if err != nil || known || len(us) != 2 {
		t.Fatal(us, err)
	}
	if _, err := p.Apply(ctx, r, us, Selection{Packages: []string{"openssl"}}); !errors.Is(err, ErrPartialUnsupported) {
		t.Fatal("partial upgrade must be refused:", err)
	}
	if _, err := p.Apply(ctx, r, us, Selection{}); err != nil {
		t.Fatal(err)
	}
	r2 := &FakeRunner{Responses: map[string]CmdResult{"checkupdates": {ExitCode: 2}}}
	if us, _, _, err := p.Updates(ctx, r2); err != nil || len(us) != 0 {
		t.Fatal("exit 2 means no updates", err)
	}
}

func TestNewAdapterRejectsUnknown(t *testing.T) {
	if _, err := NewAdapter("emerge"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
