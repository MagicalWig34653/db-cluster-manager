// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// Native agent installation (ADR 0023, builds on ADR 0018): a reviewed
// plan, a read-only preflight over SSH, then one idempotent script. The
// enrollment token travels on stdin only and is never part of the plan.

// InstallMode selects the privilege model of the native agent.
type InstallMode string

// Install modes.
const (
	// InstallReport runs as the dedicated user dbcm-patch with strict
	// systemd hardening. It reports and simulates; apply is impossible.
	InstallReport InstallMode = "report"
	// InstallApply runs as root (package managers need it) with the
	// strongest hardening that still allows package installation, and with
	// -allow-apply.
	InstallApply InstallMode = "apply"
)

// Install errors.
var (
	ErrInstallInvalid   = errors.New("patch: invalid install request")
	ErrInstallPreflight = errors.New("patch: install preflight failed")
	ErrInstallExists    = errors.New("patch: an agent is already installed on this host")
	ErrInstallDigest    = errors.New("patch: install plan digest mismatch")
)

var (
	hexSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fpRe   = regexp.MustCompile(`^[0-9A-Fa-f:]{64,95}$`)
)

// Install paths on the host.
const (
	AgentBinaryPath = "/usr/local/bin/dbcm-agent"
	AgentStateDir   = "/var/lib/dbcm-patch"
	AgentConfigDir  = "/etc/dbcm-patch"
	AgentUnitPath   = "/etc/systemd/system/dbcm-patch-agent.service"
	AgentUnitName   = "dbcm-patch-agent.service"
	agentUser       = "dbcm-patch"
	// DefaultListen is the agent's mTLS port in patch mode.
	DefaultListen = "0.0.0.0:7445"
)

// InstallRequest describes one host. Only public data.
type InstallRequest struct {
	ID            string         `json:"id"`
	Host          provision.Host `json:"host"`
	Mode          InstallMode    `json:"mode"`
	BinaryURL     string         `json:"binaryUrl"`    // https URL of the release binary for the host arch
	BinarySHA256  string         `json:"binarySha256"` // from the signed release metadata
	Version       string         `json:"version"`
	ManagerURL    string         `json:"managerUrl"`
	CAFingerprint string         `json:"caFingerprint"`
	Installation  string         `json:"installation"`
	Listen        string         `json:"listen,omitempty"`
}

var safeVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}$`)
var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var listenRe = regexp.MustCompile(`^[0-9a-fA-F.:\[\]]*:[0-9]{2,5}$`)

// Validate checks the request. All values end up in a shell script, so they
// are restricted to conservative character sets and quoted anyway.
func (r InstallRequest) Validate() error {
	if !safeID.MatchString(r.ID) || !safeID.MatchString(r.Host.ID) || !safeID.MatchString(r.Installation) {
		return ErrInstallInvalid
	}
	if r.Mode != InstallReport && r.Mode != InstallApply {
		return fmt.Errorf("%w: mode", ErrInstallInvalid)
	}
	if !hexSHA.MatchString(r.BinarySHA256) || !safeVersion.MatchString(r.Version) || !fpRe.MatchString(r.CAFingerprint) {
		return fmt.Errorf("%w: checksum, version or fingerprint", ErrInstallInvalid)
	}
	for _, raw := range []string{r.BinaryURL, r.ManagerURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			strings.ContainsAny(raw, " '\"`$\\;&|<>\n\r") {
			return fmt.Errorf("%w: URL %q must be a plain https URL", ErrInstallInvalid, raw)
		}
	}
	if r.Listen != "" && !listenRe.MatchString(r.Listen) {
		return fmt.Errorf("%w: listen", ErrInstallInvalid)
	}
	return nil
}

func (r InstallRequest) listen() string {
	if r.Listen != "" {
		return r.Listen
	}
	return DefaultListen
}

// InstallCheck is the sanitized preflight observation.
type InstallCheck struct {
	OS            string  `json:"os"`
	Arch          string  `json:"arch"`
	Systemd       bool    `json:"systemd"`
	Privilege     string  `json:"privilege"` // root | sudo
	Manager       Manager `json:"manager"`
	AgentPresent  bool    `json:"agentPresent"`
	DiskFreeBytes uint64  `json:"diskFreeBytes"`
}

// InstallPlan is the reviewed document. Digest covers all of it.
type InstallPlan struct {
	Request InstallRequest `json:"request"`
	Check   InstallCheck   `json:"check"`
	Unit    string         `json:"unit"`
	Script  string         `json:"script"`
	Digest  string         `json:"digest"`
}

const preflightScript = `set -eu
. /etc/os-release
printf 'os=%s\n' "$ID"
printf 'arch=%s\n' "$(uname -m)"
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then echo systemd=yes; else echo systemd=no; fi
if [ "$(id -u)" = 0 ]; then echo priv=root; elif sudo -n true >/dev/null 2>&1; then echo priv=sudo; else echo priv=none; fi
m=none
for c in apt-get dnf yum zypper apk pacman; do if command -v "$c" >/dev/null 2>&1; then m=$c; break; fi; done
printf 'manager=%s\n' "$m"
if [ -e ` + AgentBinaryPath + ` ] || [ -e ` + AgentUnitPath + ` ]; then echo agent=present; else echo agent=absent; fi
printf 'disk=%s\n' "$(df -PB1 /usr/local 2>/dev/null | awk 'NR==2 {print $4}')"`

// InstallPreflight runs the fixed read-only preflight.
func InstallPreflight(ctx context.Context, r provision.CommandRunner, h provision.Host, c provision.Credential) (InstallCheck, error) {
	out, err := r.Run(ctx, h, c, preflightScript, "")
	if err != nil {
		return InstallCheck{}, ErrInstallPreflight
	}
	v := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		k, val, ok := strings.Cut(l, "=")
		if !ok || v[k] != "" {
			return InstallCheck{}, ErrInstallPreflight
		}
		v[k] = strings.TrimSpace(val)
	}
	var disk uint64
	if _, err := fmt.Sscanf(v["disk"], "%d", &disk); err != nil {
		return InstallCheck{}, ErrInstallPreflight
	}
	m := Manager(v["manager"])
	if v["manager"] == "apt-get" {
		m = APT
	}
	chk := InstallCheck{OS: v["os"], Arch: v["arch"], Systemd: v["systemd"] == "yes", Privilege: v["priv"], Manager: m,
		AgentPresent: v["agent"] == "present", DiskFreeBytes: disk}
	if _, err := NewAdapter(m); err != nil || chk.OS == "" {
		return chk, ErrInstallPreflight
	}
	if !chk.Systemd || (chk.Privilege != "root" && chk.Privilege != "sudo") || disk < 100<<20 ||
		(chk.Arch != "x86_64" && chk.Arch != "aarch64") {
		return chk, ErrInstallPreflight
	}
	if chk.AgentPresent {
		return chk, ErrInstallExists
	}
	return chk, nil
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// UnitFile renders the systemd unit. The report unit runs as the dedicated
// user with strict sandboxing; the apply unit needs root and keeps the
// sandbox as tight as package installation allows.
func UnitFile(r InstallRequest) string {
	args := fmt.Sprintf("%s patch -listen %s -state %s -installation %s -host-id %s -manager-url %s -ca-fingerprint %s -enroll-token-file %s/enroll-token",
		AgentBinaryPath, r.listen(), AgentStateDir, r.Installation, r.Host.ID, r.ManagerURL, r.CAFingerprint, AgentConfigDir)
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Cluster manager patch agent (" + string(r.Mode) + " mode)\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\n")
	if r.Mode == InstallApply {
		b.WriteString("ExecStart=" + args + " -allow-apply\n")
		b.WriteString("User=root\n")
		b.WriteString("ProtectHome=true\nPrivateTmp=true\nProtectKernelTunables=true\nProtectKernelModules=false\nProtectControlGroups=true\nRestrictRealtime=true\nLockPersonality=true\nMemoryDenyWriteExecute=true\nRestrictNamespaces=true\nSystemCallArchitectures=native\n")
	} else {
		b.WriteString("ExecStart=" + args + "\n")
		b.WriteString("User=" + agentUser + "\nGroup=" + agentUser + "\nEnvironment=HOME=" + AgentStateDir + "\n")
		b.WriteString("NoNewPrivileges=true\nProtectSystem=strict\nProtectHome=true\nPrivateTmp=true\nPrivateDevices=true\nProtectKernelTunables=true\nProtectKernelModules=true\nProtectControlGroups=true\nProtectClock=true\nRestrictRealtime=true\nRestrictSUIDSGID=true\nLockPersonality=true\nMemoryDenyWriteExecute=true\nRestrictNamespaces=true\nCapabilityBoundingSet=\nAmbientCapabilities=\nSystemCallArchitectures=native\nReadWritePaths=" + AgentStateDir + "\n")
	}
	b.WriteString("StateDirectory=dbcm-patch\nStateDirectoryMode=0700\nRestart=on-failure\nRestartSec=10\n\n[Install]\nWantedBy=multi-user.target\n")
	return b.String()
}

// InstallScript renders the idempotent install script. It reads the
// enrollment token from stdin, downloads the binary, verifies the SHA-256
// before installing it and refuses to overwrite an existing agent.
func InstallScript(r InstallRequest, unit string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\numask 077\n# cluster-manager native patch agent install " + r.ID + " / " + r.Host.ID + "\n")
	b.WriteString("IFS= read -r TOKEN\n[ -n \"$TOKEN\" ] || { echo 'enrollment token required on stdin' >&2; exit 1; }\n")
	b.WriteString("if [ -e " + AgentBinaryPath + " ] || [ -e " + AgentUnitPath + " ]; then echo 'agent already installed' >&2; exit 1; fi\n")
	b.WriteString("tmp=$(mktemp -d)\ntrap 'rm -rf \"$tmp\"' EXIT\n")
	b.WriteString("if command -v curl >/dev/null 2>&1; then curl -fsSL --proto '=https' --tlsv1.2 -o \"$tmp/agent\" " + q(r.BinaryURL) + "; else wget -q -O \"$tmp/agent\" " + q(r.BinaryURL) + "; fi\n")
	b.WriteString("[ \"$(sha256sum \"$tmp/agent\" | cut -d' ' -f1)\" = " + q(r.BinarySHA256) + " ] || { echo 'binary checksum mismatch' >&2; exit 1; }\n")
	b.WriteString("install -m 0755 -o root -g root \"$tmp/agent\" " + AgentBinaryPath + "\n")
	b.WriteString("install -d -m 0755 " + AgentConfigDir + "\ninstall -d -m 0700 " + AgentStateDir + "\n")
	if r.Mode == InstallReport {
		b.WriteString("id " + agentUser + " >/dev/null 2>&1 || useradd --system --no-create-home --home-dir " + AgentStateDir + " --shell /usr/sbin/nologin " + agentUser + " 2>/dev/null || adduser -S -D -H -h " + AgentStateDir + " -s /sbin/nologin " + agentUser + "\n")
		b.WriteString("chown " + agentUser + " " + AgentStateDir + "\n")
	}
	b.WriteString("printf '%s\\n' \"$TOKEN\" > " + AgentConfigDir + "/enroll-token\n")
	if r.Mode == InstallReport {
		b.WriteString("chown " + agentUser + " " + AgentConfigDir + "/enroll-token\n")
	}
	b.WriteString("chmod 0600 " + AgentConfigDir + "/enroll-token\n")
	b.WriteString("cat > " + AgentUnitPath + " <<'DBCM_UNIT_EOF'\n" + unit + "DBCM_UNIT_EOF\n")
	b.WriteString("chmod 0644 " + AgentUnitPath + "\nsystemctl daemon-reload\nsystemctl enable --now " + AgentUnitName + "\n")
	return b.String()
}

// BuildInstallPlan builds the reviewed plan from a request and a passing
// preflight.
func BuildInstallPlan(r InstallRequest, chk InstallCheck) (InstallPlan, error) {
	if err := r.Validate(); err != nil {
		return InstallPlan{}, err
	}
	if !chk.Systemd || chk.AgentPresent || (chk.Privilege != "root" && chk.Privilege != "sudo") {
		return InstallPlan{}, ErrInstallPreflight
	}
	unit := UnitFile(r)
	p := InstallPlan{Request: r, Check: chk, Unit: unit, Script: InstallScript(r, unit)}
	b, _ := json.Marshal(struct {
		R InstallRequest
		C InstallCheck
		U string
		S string
	}{p.Request, p.Check, p.Unit, p.Script})
	sum := sha256.Sum256(b)
	p.Digest = hex.EncodeToString(sum[:])
	return p, nil
}

// ApplyInstall repeats the preflight and runs the script. The token goes to
// stdin only; command output is not returned.
func ApplyInstall(ctx context.Context, r provision.CommandRunner, p InstallPlan, digest, token string, c provision.Credential) error {
	if digest == "" || digest != p.Digest {
		return ErrInstallDigest
	}
	again, err := BuildInstallPlan(p.Request, p.Check)
	if err != nil || again.Digest != p.Digest {
		return ErrInstallDigest
	}
	if token == "" || strings.ContainsAny(token, " \n\r\t") {
		return ErrInstallInvalid
	}
	if _, err := InstallPreflight(ctx, r, p.Request.Host, c); err != nil {
		return err
	}
	cmd := "sh -c " + q(p.Script)
	if p.Check.Privilege == "sudo" {
		cmd = "sudo -n " + cmd
	}
	if _, err := r.Run(ctx, p.Request.Host, c, cmd, token+"\n"); err != nil {
		return fmt.Errorf("patch: install script failed on host %s", p.Request.Host.ID)
	}
	return nil
}
