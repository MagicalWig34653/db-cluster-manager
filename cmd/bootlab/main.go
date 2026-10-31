// SPDX-License-Identifier: AGPL-3.0-only

// Command bootlab drives the fresh-server bootstrap (ADR 0027, lab E17 part
// a) against one dbcm-lab-* VM through its forwarded SSH port with lab-own
// credentials. It is a lab harness, not a product binary: it uses the real
// provision.BootstrapService with the real SSH runner and host key probe; only
// the manager-side actions (container agent, sealed key store) are stubs.
//
// The address must be loopback; the harness refuses anything else.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

type noManager struct{ keyOut string }

func (noManager) DeployContainerAgent(context.Context, provision.Host) error { return nil }
func (m noManager) StoreDedicatedKey(_ context.Context, _ provision.Host, c provision.Credential) error {
	if m.keyOut == "" {
		return nil
	}
	return os.WriteFile(m.keyOut, []byte(c.PrivateKey), 0o600)
}

func main() {
	var (
		port     = flag.Int("port", 0, "forwarded SSH port of the lab VM on 127.0.0.1")
		user     = flag.String("user", "root", "login user")
		keyFile  = flag.String("key", "", "lab-own private key file")
		password = flag.String("password", "", "lab-own password (instead of -key)")
		id       = flag.String("id", "lab", "host and plan ID")
		fw       = flag.Bool("firewall", true, "enable the firewall option")
		unatt    = flag.Bool("unattended", false, "enable unattended security upgrades")
		lockdown = flag.Bool("lockdown", false, "plan and run the dedicated key and root login lockdown")
		dedOut   = flag.String("dedicated-key-out", "", "write the generated dedicated private key here (lockdown)")
		probeCmd = flag.String("run", "", "instead of a bootstrap: run this command with -key as -user (verification after a lockdown)")
		timeout  = flag.Duration("timeout", 25*time.Minute, "overall run timeout")
	)
	flag.Parse()
	if *port == 0 || (*keyFile == "" && *password == "") {
		fmt.Fprintln(os.Stderr, "bootlab: -port and (-key or -password) required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cred := provision.Credential{Password: *password}
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		must(err)
		cred = provision.Credential{PrivateKey: string(b)}
	}
	if *probeCmd != "" {
		hk, err := provision.SSHHostKeyProbe{Port: *port}.Probe(ctx, provision.Host{Address: "127.0.0.1"})
		must(err)
		cred.KnownHosts = hk.KnownHosts
		if strings.HasPrefix(*probeCmd, "@") {
			b, err := os.ReadFile((*probeCmd)[1:])
			must(err)
			*probeCmd = string(b)
		}
		out, err := provision.SSHRunner{Port: *port, Timeout: time.Minute}.Run(ctx, provision.Host{ID: *id, Address: "127.0.0.1", User: *user}, cred, *probeCmd, "")
		fmt.Print(out)
		if err != nil {
			fmt.Printf("RESULT error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	host := provision.Host{ID: *id, Address: "127.0.0.1", User: *user, Role: "node"}
	if ip := net.ParseIP(host.Address); ip == nil || !ip.IsLoopback() {
		must(fmt.Errorf("refusing non-loopback address"))
	}
	profile := provision.BootstrapProfile{ServiceUser: "dbcm", Firewall: *fw, UnattendedUpgrades: *unatt,
		DedicatedKey: *lockdown, DisableRootLogin: *lockdown, Runtime: "docker",
		Cluster: provision.ClusterChoice{Platform: "swarm", Role: "init"}}

	j := opjournal.New()
	runner := provision.SSHRunner{Port: *port, Timeout: 5 * time.Minute}
	agents := patch.BootstrapAgentScripts{}
	deps := provision.BootstrapDeps{Runner: runner, Agents: agents, Manager: noManager{*dedOut}, Journal: j}
	env := provision.BootstrapEnv{Installation: "e17-lab"}
	svc := provision.NewBootstrapService(deps, env, provision.SSHHostKeyProbe{Port: *port}, agents)

	v, err := svc.Draft(ctx, provision.DraftInput{Request: provision.BootstrapRequest{ID: *id, Host: host, Profile: profile}, Credential: cred})
	must(err)
	fmt.Printf("DRAFT status=%s hostkey=%s %s\n", v.Status, v.HostKey.Type, v.HostKey.Fingerprint)
	v, err = svc.ConfirmFingerprint(ctx, *id, v.HostKey.Fingerprint)
	must(err)
	plan, err := svc.Preview(*id)
	must(err)
	fmt.Printf("PLAN status=%s blocked=%v os=%s %s arch=%s privilege=%s docker=%q digest=%s\n", v.Status, plan.Blocked,
		plan.Check.OS, plan.Check.Version, plan.Check.Arch, plan.Check.Privilege, plan.Check.Docker, plan.Digest)
	for _, f := range plan.Findings {
		fmt.Printf("FINDING %s %s: %s\n", f.Severity, f.Code, f.Message)
	}
	for _, s := range plan.Steps {
		fmt.Printf("STEP %s phase=%s runAs=%s mutating=%v gate=%q: %s\n", s.ID, s.Phase, s.RunAs, s.Mutating, s.Gate, s.Title)
	}
	if b, err := json.MarshalIndent(plan, "", " "); err == nil {
		_ = os.WriteFile(fmt.Sprintf("/private/tmp/claude-501/-Users-linolaske-Documents-Code-Projekte-GoLand-db-cluster-manager/c49659e3-5bad-4186-be13-1a1ce87f8e33/scratchpad/plan-%s.json", *id), b, 0o600)
	}
	if plan.Blocked {
		fmt.Println("RESULT blocked")
		os.Exit(1)
	}
	_, err = svc.Approve(*id, plan.Digest)
	must(err)
	_, err = svc.Run(ctx, *id)
	must(err)
	wait(ctx, svc, *id, provision.StatusMainComplete, provision.StatusCompleted)
	if *lockdown {
		_, err = svc.ApproveLockdown(*id, plan.LockdownDigest)
		must(err)
		_, err = svc.Run(ctx, *id)
		must(err)
		wait(ctx, svc, *id, provision.StatusCompleted)
	}
	fmt.Println("RESULT ok")
}

func wait(ctx context.Context, svc *provision.BootstrapService, id string, want ...provision.Status) {
	last := ""
	for ctx.Err() == nil {
		v := svc.Status(id)
		var b strings.Builder
		for _, p := range v.Progress {
			fmt.Fprintf(&b, "%s=%s ", p.ID, p.State)
		}
		if s := b.String(); s != last {
			fmt.Printf("PROGRESS status=%s %s\n", v.Status, s)
			last = s
		}
		for _, w := range want {
			if v.Status == w {
				return
			}
		}
		if v.Status == provision.StatusPaused || v.Status == provision.StatusUnknown || v.Status == provision.StatusCancelled || v.Status == provision.StatusBlocked {
			fmt.Printf("RESULT stopped status=%s error=%q\n", v.Status, v.Error)
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("RESULT timeout")
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fmt.Printf("RESULT error: %v\n", err)
		os.Exit(1)
	}
}
