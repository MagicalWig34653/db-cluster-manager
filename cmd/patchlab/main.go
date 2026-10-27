// SPDX-License-Identifier: AGPL-3.0-only

// Command patchlab drives the patch management lab experiment E10
// (tests/p0/e10-patch.sh) against one dbcm-lab-* Lima VM.
//
// It is a lab harness, not a product binary. It addresses only VMs whose name
// passes lab.IsLabName, reaches them through `limactl shell` (never through
// the host's Docker or package manager) and prints a line-oriented log. It
// exercises, on the real guest OS:
//
//  1. the reviewed native-agent install plan (preflight, digest check,
//     checksum failure, install, hardening, removal),
//  2. report mode (OS, packages, updates, security classification),
//  3. the report-only guarantees (no apply without approval or without the
//     apply-mode agent),
//  4. approval, apply, verification and history through patch.Service,
//  5. reboot-required detection, the reboot policy and the Galera guard.
//
// Not covered here: the SSH transport (limactl shell stands in for it) and
// enrollment/mTLS (the installed unit is switched to the loopback lab
// transport after the install has been verified).
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/lab"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

var verified []string
var notVerified []string

func say(f string, a ...any) {
	fmt.Printf("%s "+f+"\n", append([]any{time.Now().UTC().Format("15:04:05Z")}, a...)...)
}
func ok(what string)   { verified = append(verified, what); say("VERIFIED: %s", what) }
func nok(what string)  { notVerified = append(notVerified, what); say("NOT VERIFIED: %s", what) }
func section(s string) { fmt.Printf("\n## %s\n\n", s) }
func check(cond bool, what string, detail ...any) {
	if cond {
		ok(what)
		return
	}
	say("FAILED: %s %v", what, detail)
	notVerified = append(notVerified, "FAILED: "+what)
}

// limaRunner implements provision.CommandRunner through limactl shell.
type limaRunner struct{}

func (limaRunner) Run(ctx context.Context, h provision.Host, _ provision.Credential, command, stdin string) (string, error) {
	if !lab.IsLabName(h.Address) {
		return "", fmt.Errorf("refusing non-lab VM %q", h.Address)
	}
	cmd := exec.CommandContext(ctx, "limactl", "shell", "--workdir", "/", h.Address, "--", "sh", "-c", command)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

var vm string

func sh(ctx context.Context, script string) (string, error) {
	return limaRunner{}.Run(ctx, provision.Host{Address: vm}, provision.Credential{}, script, "")
}

// limaAgent sends AgentCommands to the lab transport inside the VM.
type limaAgent struct{}

func (limaAgent) Do(ctx context.Context, _ string, c patch.AgentCommand) (patch.AgentResult, error) {
	b, _ := json.Marshal(c)
	out, err := limaRunner{}.Run(ctx, provision.Host{Address: vm}, provision.Credential{},
		`curl -sS --max-time 900 -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' --data-binary @- http://127.0.0.1:7445`+patch.CommandPath, string(b))
	if err != nil {
		return patch.AgentResult{}, err
	}
	i := strings.LastIndex(out, "\n")
	if i < 0 {
		return patch.AgentResult{}, errors.New("no status")
	}
	var body struct {
		patch.AgentResult
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(out[:i]), &body)
	switch strings.TrimSpace(out[i+1:]) {
	case "200":
		return body.AgentResult, nil
	case "403":
		return body.AgentResult, fmt.Errorf("%w: %s", patch.ErrReadOnly, body.Error)
	case "409":
		return body.AgentResult, fmt.Errorf("%w: %s", patch.ErrStale, body.Error)
	}
	return body.AgentResult, fmt.Errorf("agent: %s: %s", strings.TrimSpace(out[i+1:]), body.Error)
}

type galeraDir struct{}

func (galeraDir) Member(context.Context, string) (string, bool, error) {
	return "lab-galera", true, nil
}

func main() {
	binary := flag.String("binary", "", "linux/arm64 agent binary to install (required)")
	flag.StringVar(&vm, "vm", "", "lab VM name (required)")
	n := flag.Int("apply-packages", 2, "number of updates to apply")
	flag.Parse()
	if !lab.IsLabName(vm) || *binary == "" {
		fmt.Fprintln(os.Stderr, "patchlab: -vm must be a dbcm-lab- VM and -binary is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	if err := run(ctx, *binary, *n); err != nil {
		fmt.Println("FATAL:", err)
		os.Exit(1)
	}
	section("Summary for " + vm)
	for _, v := range verified {
		fmt.Println("VERIFIED:", v)
	}
	for _, v := range notVerified {
		fmt.Println("NOT VERIFIED / FAILED:", v)
	}
	for _, v := range notVerified {
		if strings.HasPrefix(v, "FAILED") {
			os.Exit(3)
		}
	}
}

func serveBinary(path string) (url, sha string, caPEM []byte, stop func(), err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", nil, nil, err
	}
	sum := sha256.Sum256(data)
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dbcm patchlab CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(6 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "host.lima.internal"}, DNSNames: []string{"host.lima.internal"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(6 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return "", "", nil, nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/dbcm-agent", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) })
	srv := &http.Server{Handler: mux, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}}
	go func() { _ = srv.ServeTLS(l, "", "") }()
	port := l.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("https://host.lima.internal:%d/dbcm-agent", port), hex.EncodeToString(sum[:]),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), func() { _ = srv.Close() }, nil
}

func installReq(mode patch.InstallMode, url, sha string) patch.InstallRequest {
	return patch.InstallRequest{ID: "lab-" + string(mode), Host: provision.Host{ID: vm, Address: vm, User: "lab", Role: "worker"}, Mode: mode,
		BinaryURL: url, BinarySHA256: sha, Version: "lab", ManagerURL: "https://manager.lab.invalid:7444",
		CAFingerprint: strings.Repeat("AB", 32), Installation: "e10-lab"}
}

func install(ctx context.Context, mode patch.InstallMode, url, sha string, wrongSum bool) (patch.InstallPlan, error) {
	r := limaRunner{}
	req := installReq(mode, url, sha)
	if wrongSum {
		req.BinarySHA256 = strings.Repeat("0", 64)
	}
	chk, err := patch.InstallPreflight(ctx, r, req.Host, provision.Credential{})
	if err != nil {
		return patch.InstallPlan{}, fmt.Errorf("preflight: %w (%+v)", err, chk)
	}
	plan, err := patch.BuildInstallPlan(req, chk)
	if err != nil {
		return plan, err
	}
	say("preflight: %+v", chk)
	say("plan digest: %s", plan.Digest)
	return plan, patch.ApplyInstall(ctx, r, plan, plan.Digest, "lab-enrollment-token", provision.Credential{})
}

func uninstall(ctx context.Context) {
	_, err := sh(ctx, "sudo -n sh -c "+shq(patch.UninstallScript()))
	out, _ := sh(ctx, "ls "+patch.AgentBinaryPath+" "+patch.AgentUnitPath+" "+patch.AgentConfigDir+" "+patch.AgentStateDir+" 2>&1 | grep -vc 'No such file'; id dbcm-patch 2>&1 | grep -c uid")
	check(err == nil && strings.Fields(out)[0] == "0" && strings.Fields(out)[1] == "0", "agent removal leaves no unit, binary, directories or user", out, err)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func labSwitch(ctx context.Context, allowApply bool) error {
	extra := ""
	if allowApply {
		extra = " -allow-apply"
	}
	cmd := fmt.Sprintf(`sudo -n sed -i 's#^ExecStart=.*#ExecStart=%s patch -insecure-lab -listen 127.0.0.1:7445 -state %s -host-id %s%s#' %s && sudo -n systemctl daemon-reload && sudo -n systemctl restart %s`,
		patch.AgentBinaryPath, patch.AgentStateDir, vm, extra, patch.AgentUnitPath, patch.AgentUnitName)
	if _, err := sh(ctx, cmd); err != nil {
		return err
	}
	for i := 0; i < 30; i++ {
		if out, _ := sh(ctx, `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7445`+patch.CommandPath); strings.TrimSpace(out) == "405" {
			return nil
		}
		time.Sleep(time.Second)
	}
	logs, _ := sh(ctx, "sudo -n journalctl -u "+patch.AgentUnitName+" --no-pager -n 20")
	return fmt.Errorf("lab transport did not come up: %s", logs)
}

func summarize(r patch.HostReport) {
	say("report: os=%s %s kernel=%s arch=%s manager=%s installed=%d updates=%d security=%d securityKnown=%v reboot=%s",
		r.OS.ID, r.OS.VersionID, r.OS.Kernel, r.OS.Arch, r.Manager, len(r.Installed), len(r.Updates), len(r.SecurityUpdates()), r.SecurityKnown, r.RebootRequired)
	for i, u := range r.Updates {
		if i < 8 {
			say("  update %s %s -> %s security=%v adv=%v", u.Name, u.Installed, u.Available, u.Security, u.Advisories)
		}
	}
	for _, w := range r.Warnings {
		say("  warning: %s", w)
	}
}

func newService(ag patch.HostAgent) (*patch.Service, *[]notify.Event) {
	var events []notify.Event
	svc := &patch.Service{KV: &vuln.MemoryKV{}, Journal: opjournal.New(), Agents: ag, Guard: patch.AllowAllGuard{},
		Term: func() (uint64, error) { return 1, nil }}
	svc.Notify = func(_ context.Context, e notify.Event) { events = append(events, e) }
	return svc, &events
}

func eligible(r patch.HostReport, n int) []string {
	skip := []string{"linux-", "kernel", "systemd", "libc", "glibc", "openssh", "cloud-", "grub", "udev", "dbus", "shim", "dracut", "initramfs"}
	var out []string
	pick := func(sec bool) {
		for _, u := range r.Updates {
			if len(out) >= n || u.Security != sec {
				continue
			}
			bad := false
			for _, s := range skip {
				if strings.HasPrefix(u.Name, s) {
					bad = true
				}
			}
			if !bad && patch.ValidPackageName(u.Name) {
				out = append(out, u.Name)
			}
		}
	}
	pick(true)
	pick(false)
	return out
}
