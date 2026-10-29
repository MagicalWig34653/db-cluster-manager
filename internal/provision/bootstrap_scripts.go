// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Preflight script and step script generators for the fresh-server bootstrap
// (X18, ADR 0027). All values that reach a script are validated and quoted.
// The scripts are written from the distributions' and Docker's published
// documentation; none has run on a real host yet.

// bootstrapPreflight is read-only. It prints key=value lines.
func bootstrapPreflight(p BootstrapProfile) string {
	var ports []string
	for _, np := range NeededPorts(p) {
		ports = append(ports, strings.SplitN(np, "/", 2)[0])
	}
	portRe := strings.Join(ports, "|")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# dbcm-bootstrap preflight\nset -eu\n. /etc/os-release\n")
	b.WriteString(`printf 'os=%s\n' "${ID:-}"
printf 'version=%s\n' "${VERSION_ID:-}"
printf 'arch=%s\n' "$(uname -m)"
printf 'cpus=%s\n' "$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0)"
printf 'mem=%s\n' "$(awk '/^MemTotal:/ {print $2*1024}' /proc/meminfo)"
printf 'disk=%s\n' "$(df -PB1 / | awk 'NR==2 {print $4}')"
if command -v timedatectl >/dev/null 2>&1 && [ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)" = yes ]; then echo time=yes
elif command -v chronyc >/dev/null 2>&1 && chronyc -n tracking 2>/dev/null | grep -q 'Leap status *: Normal'; then echo time=yes
else echo time=no; fi
if [ -d /run/systemd/system ]; then echo systemd=yes; else echo systemd=no; fi
if [ "$(id -u)" = 0 ]; then echo priv=root; elif sudo -n true >/dev/null 2>&1; then echo priv=sudo; else echo priv=none; fi
m=none
for c in apt-get dnf apk; do if command -v "$c" >/dev/null 2>&1; then m=$c; break; fi; done
case "$m" in apt-get) m=apt;; esac
printf 'pkg=%s\n' "$m"
if command -v docker >/dev/null 2>&1; then printf 'docker=%s\n' "$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unknown)"; else echo docker=none; fi
if [ -f /etc/rancher/k3s/k3s.yaml ] || [ -f /etc/rancher/k3s/k3s-agent.yaml ]; then echo cluster=k3s
elif command -v docker >/dev/null 2>&1 && [ "$(docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || true)" = active ]; then echo cluster=swarm
else echo cluster=none; fi
if [ -e ` + AgentBinaryPathConst + ` ] || [ -e ` + AgentUnitPathConst + ` ]; then echo agent=present; else echo agent=absent; fi
if [ -f ` + bootstrapStateDir + `/agent-native.done ]; then echo agentmarker=yes; else echo agentmarker=no; fi
if command -v restic >/dev/null 2>&1; then echo restic=yes; else echo restic=no; fi
sshd_t=$(sshd -T 2>/dev/null || true)
printf 'sshport=%s\n' "$(printf '%s\n' "$sshd_t" | awk '$1=="port" {print $2; exit}')"
printf 'rootlogin=%s\n' "$(printf '%s\n' "$sshd_t" | awk '$1=="permitrootlogin" {print $2; exit}')"
printf 'pwauth=%s\n' "$(printf '%s\n' "$sshd_t" | awk '$1=="passwordauthentication" {print $2; exit}')"
`)
	b.WriteString("if id -u " + shellQuote(p.ServiceUser) + " >/dev/null 2>&1; then echo svcuser=yes; else echo svcuser=no; fi\n")
	if portRe != "" {
		b.WriteString("command -v ss >/dev/null 2>&1 || { echo 'ss (iproute2) is required for the port check' >&2; exit 1; }\n")
		b.WriteString("busy=$(ss -lntuH 2>/dev/null | awk '{print $5}' | grep -oE ':(" + portRe + ")$' | tr -d ':' | sort -u | tr '\\n' ',' | sed 's/,$//' || true)\n")
		b.WriteString("printf 'busyports=%s\\n' \"${busy:-none}\"\n")
	} else {
		b.WriteString("echo busyports=none\n")
	}
	b.WriteString(`other=""
for n in podman nginx apache2 httpd caddy mysqld mariadbd postgres k3s; do if pgrep -x "$n" >/dev/null 2>&1; then other="$other${other:+,}$n"; fi; done
printf 'other=%s\n' "${other:-none}"
`)
	return b.String()
}

// AgentBinaryPathConst and AgentUnitPathConst mirror internal/patch (which
// provision cannot import).
const (
	AgentBinaryPathConst = "/usr/local/bin/dbcm-agent"
	AgentUnitPathConst   = "/etc/systemd/system/dbcm-patch-agent.service"
)

// BootstrapInspect runs the fixed read-only preflight over the runner. It
// returns ErrExistingCluster, together with the observation, for a host that
// already belongs to a Swarm or k3s cluster.
func BootstrapInspect(ctx context.Context, r CommandRunner, h Host, c Credential, p BootstrapProfile) (BootstrapCheck, error) {
	out, err := r.Run(ctx, h, c, bootstrapPreflight(p), "")
	if err != nil {
		return BootstrapCheck{}, ErrPreflight
	}
	v := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, val, ok := strings.Cut(line, "=")
		if !ok {
			return BootstrapCheck{}, ErrPreflight
		}
		v[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	for _, k := range []string{"os", "version", "arch", "mem", "disk", "time", "priv", "docker", "cluster", "agent", "svcuser", "busyports"} {
		if _, ok := v[k]; !ok {
			return BootstrapCheck{}, ErrPreflight
		}
	}
	num := func(k string) uint64 {
		n, _ := strconv.ParseUint(v[k], 10, 64)
		return n
	}
	arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[v["arch"]]
	if arch == "" {
		arch = v["arch"]
	}
	list := func(s string) []string {
		if s == "" || s == "none" {
			return []string{}
		}
		return strings.Split(s, ",")
	}
	chk := BootstrapCheck{
		HostID: h.ID, OS: v["os"], Version: v["version"], Arch: arch, CPUs: int(num("cpus")), MemBytes: num("mem"), DiskBytes: num("disk"),
		TimeSynchronized: v["time"] == "yes", Systemd: v["systemd"] == "yes", Privilege: v["priv"], PackageManager: v["pkg"], Docker: v["docker"],
		AgentPresent: v["agent"] == "present", AgentMarker: v["agentmarker"] == "yes", ResticPresent: v["restic"] == "yes",
		SSHPort: int(num("sshport")), SSHRootLogin: v["rootlogin"], SSHPasswordAuth: v["pwauth"], ServiceUserExists: v["svcuser"] == "yes",
		BusyPorts: list(v["busyports"]), OtherSoftware: list(v["other"]),
	}
	if chk.SSHPort == 0 {
		chk.SSHPort = 22
	}
	if v["cluster"] != "none" && v["cluster"] != "" {
		chk.ClusterID = v["cluster"]
		return chk, ErrExistingCluster
	}
	return chk, nil
}

type stepDef struct {
	id, title, phase string
	cond             string // shell condition that is true when the step is done
	body             string
	cleanup          string
	mutating         bool
	runAs            string
	unprivileged     bool
	stdin            string
	gate             string
	managerAction    string
	expect           bool
}

func marker(step string) string { return bootstrapStateDir + "/" + step + ".done" }

func header(planID, stepID, kind string) string {
	return "#!/bin/sh\n# dbcm-bootstrap " + planID + " " + stepID + " " + kind + "\nset -eu\n"
}

func (d stepDef) toStep(planID string) BootstrapStep {
	st := BootstrapStep{ID: d.id, Title: d.title, Phase: d.phase, Mutating: d.mutating, RunAs: d.runAs, Unprivileged: d.unprivileged,
		Stdin: d.stdin, ManagerAction: d.managerAction, Gate: d.gate, Expect: "todo"}
	if st.RunAs == "" {
		st.RunAs = "root"
	}
	if d.expect {
		st.Expect = "done"
	}
	if d.managerAction != "" {
		st.RunAs = "manager"
		st.Cleanup = d.cleanup
		return st
	}
	cond := d.cond
	if cond == "" {
		cond = "false"
	}
	if d.mutating {
		cond = "[ -f " + marker(d.id) + " ] || { " + cond + "; }"
	}
	run := header(planID, d.id, "run")
	run += "if " + cond + "; then echo dbcm:result=unchanged; exit 0; fi\n" + d.body
	if d.mutating {
		run += "mkdir -p " + bootstrapStateDir + "\n: > " + marker(d.id) + "\n"
	}
	run += "echo dbcm:result=changed\n"
	st.Script = run
	if d.mutating {
		st.Check = header(planID, d.id, "check") + "if " + cond + "; then echo dbcm:done=yes; else echo dbcm:done=no; fi\n"
	}
	if d.cleanup != "" {
		st.Cleanup = header(planID, d.id, "cleanup") + d.cleanup + "rm -f " + marker(d.id) + "\n"
	}
	return st
}

func pkgInstall(pm string, pkgs ...string) string {
	switch pm {
	case "apt":
		return "DEBIAN_FRONTEND=noninteractive apt-get update -y\nDEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends " + strings.Join(pkgs, " ") + "\n"
	case "dnf":
		return "dnf install -y " + strings.Join(pkgs, " ") + "\n"
	}
	return "apk add --no-cache " + strings.Join(pkgs, " ") + "\n"
}

func serviceEnable(sup OSSupport, unit string) string {
	if sup.Init == "openrc" {
		return "rc-update add " + unit + " default\nrc-service " + unit + " start\n"
	}
	return "systemctl enable --now " + unit + "\n"
}

func dockerBody(sup OSSupport) (string, string) {
	switch sup.PackageManager {
	case "apt":
		distro := sup.ID
		return `DOCKER_KEY_FPR=` + shellQuote(DockerKeyFingerprint) + `
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/docker.asc" https://download.docker.com/linux/` + distro + `/gpg
got=$(gpg --show-keys --with-colons "$tmp/docker.asc" | awk -F: '$1=="fpr" {print $10; exit}')
[ "$got" = "$DOCKER_KEY_FPR" ] || { echo 'docker repository key fingerprint mismatch' >&2; exit 1; }
install -m 0755 -d /etc/apt/keyrings
gpg --dearmor < "$tmp/docker.asc" > "$tmp/docker.gpg"
install -m 0644 "$tmp/docker.gpg" /etc/apt/keyrings/dbcm-docker.gpg
. /etc/os-release
printf 'deb [arch=%s signed-by=/etc/apt/keyrings/dbcm-docker.gpg] https://download.docker.com/linux/` + distro + ` %s stable\n' "$(dpkg --print-architecture)" "$VERSION_CODENAME" > /etc/apt/sources.list.d/dbcm-docker.list
DEBIAN_FRONTEND=noninteractive apt-get update -y
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io
systemctl enable --now docker
`, "rm -f /etc/apt/sources.list.d/dbcm-docker.list /etc/apt/keyrings/dbcm-docker.gpg\n# The Docker engine and its data are not removed automatically. Remove them yourself after checking for containers, volumes and images you need.\n"
	case "dnf":
		return `DOCKER_KEY_FPR=` + shellQuote(DockerKeyFingerprint) + `
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/docker.asc" https://download.docker.com/linux/centos/gpg
got=$(gpg --show-keys --with-colons "$tmp/docker.asc" | awk -F: '$1=="fpr" {print $10; exit}')
[ "$got" = "$DOCKER_KEY_FPR" ] || { echo 'docker repository key fingerprint mismatch' >&2; exit 1; }
rpm --import "$tmp/docker.asc"
dnf install -y dnf-plugins-core
dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
dnf install -y docker-ce docker-ce-cli containerd.io
systemctl enable --now docker
`, "rm -f /etc/yum.repos.d/docker-ce.repo\n# The Docker engine and its data are not removed automatically. Remove them yourself after checking for containers, volumes and images you need.\n"
	}
	return `apk add --no-cache docker
rc-update add docker default
rc-service docker start
`, "# The Docker engine and its data are not removed automatically. Remove them yourself after checking for containers, volumes and images you need.\n"
}

func firewallBody(sup OSSupport, sshPort int, ports []string) (string, string) {
	var b, c strings.Builder
	ssh := fmt.Sprintf("%d/tcp", sshPort)
	if sup.Firewall == "ufw" {
		b.WriteString(pkgInstall("apt", "ufw"))
		b.WriteString("ufw default deny incoming\nufw default allow outgoing\n")
		b.WriteString("ufw allow " + ssh + "\n")
		for _, p := range ports {
			b.WriteString("ufw allow " + p + "\n")
			c.WriteString("ufw delete allow " + p + " || true\n")
		}
		b.WriteString("ufw --force enable\n")
		return b.String(), c.String() + "# The firewall itself stays enabled; the SSH rule is kept.\n"
	}
	b.WriteString(pkgInstall("dnf", "firewalld"))
	b.WriteString("systemctl enable --now firewalld\nfirewall-cmd --permanent --add-port=" + ssh + "\n")
	for _, p := range ports {
		b.WriteString("firewall-cmd --permanent --add-port=" + p + "\n")
		c.WriteString("firewall-cmd --permanent --remove-port=" + p + " || true\n")
	}
	b.WriteString("firewall-cmd --reload\n")
	return b.String(), c.String() + "firewall-cmd --reload || true\n# The firewall itself stays enabled; the SSH rule is kept.\n"
}

func unattendedBody(sup OSSupport) string {
	if sup.PackageManager == "apt" {
		return pkgInstall("apt", "unattended-upgrades") +
			"printf 'APT::Periodic::Update-Package-Lists \"1\";\\nAPT::Periodic::Unattended-Upgrade \"1\";\\n' > /etc/apt/apt.conf.d/20auto-upgrades\n"
	}
	return pkgInstall("dnf", "dnf-automatic") +
		"sed -i -e 's/^upgrade_type *=.*/upgrade_type = security/' -e 's/^apply_updates *=.*/apply_updates = yes/' /etc/dnf/automatic.conf\n" +
		"systemctl enable --now dnf-automatic.timer\n"
}

func buildSteps(req BootstrapRequest, plan BootstrapPlan, env BootstrapEnv, agents AgentScripts) ([]BootstrapStep, error) {
	p, sup, chk := req.Profile, plan.OS, plan.Check
	pm := sup.PackageManager
	user := shellQuote(p.ServiceUser)
	var defs []stepDef

	// Baseline.
	base := []string{"ca-certificates", "curl", "sudo"}
	switch pm {
	case "apt":
		base = append(base, "gnupg")
	case "dnf":
		base = []string{"ca-certificates", "sudo", "gnupg2"}
	}
	condBase := "command -v curl >/dev/null 2>&1 && command -v sudo >/dev/null 2>&1 && command -v visudo >/dev/null 2>&1"
	bodyBase := pkgInstall(pm, base...)
	if pm == "dnf" {
		bodyBase += "command -v curl >/dev/null 2>&1 || dnf install -y curl\n"
	}
	defs = append(defs, stepDef{id: "base-packages", title: "Install base packages (curl, sudo, certificates)", phase: "baseline", cond: condBase, body: bodyBase,
		cleanup: "# Base packages are kept; other software may depend on them.\n", mutating: true})
	if !chk.TimeSynchronized {
		unit := "chrony"
		if pm == "dnf" || sup.Init == "openrc" {
			unit = "chronyd"
		}
		defs = append(defs, stepDef{id: "time-sync", title: "Enable time synchronization (chrony)", phase: "baseline", cond: "false",
			body: pkgInstall(pm, "chrony") + serviceEnable(sup, unit), cleanup: "# chrony is kept; a host without time synchronization breaks certificates.\n", mutating: true})
	}
	sudoers := "/etc/sudoers.d/90-dbcm"
	defs = append(defs, stepDef{id: "service-user", title: "Create the service user " + p.ServiceUser + " with sudo", phase: "baseline", mutating: true, expect: chk.ServiceUserExists,
		cond: "id -u " + user + " >/dev/null 2>&1 && [ -f " + sudoers + " ]",
		body: "USERNAME=" + user + `
if ! id -u "$USERNAME" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/sh "$USERNAME" 2>/dev/null || adduser -D -s /bin/sh "$USERNAME"
fi
passwd -l "$USERNAME" >/dev/null 2>&1 || true
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$USERNAME" > ` + sudoers + `.new
chmod 0440 ` + sudoers + `.new
visudo -cf ` + sudoers + `.new >/dev/null || { rm -f ` + sudoers + `.new; echo 'sudoers validation failed' >&2; exit 1; }
mv ` + sudoers + `.new ` + sudoers + `
`,
		cleanup: "rm -f " + sudoers + "\n# The user and its home directory are kept. Remove them yourself with userdel after checking for files you need.\n"})
	defs = append(defs, stepDef{id: "sshd-audit", title: "Audit sshd settings (read-only, changes nothing)", phase: "baseline",
		body: `if command -v sshd >/dev/null 2>&1; then
  sshd -T 2>/dev/null | awk '/^(permitrootlogin|passwordauthentication|pubkeyauthentication|port) / {print "dbcm:note=sshd " $1 "=" $2}'
else
  echo 'dbcm:note=sshd binary not found'
fi
`})
	if p.Firewall && sup.Firewall != "" {
		body, cleanup := firewallBody(sup, chk.SSHPort, NeededPorts(p))
		defs = append(defs, stepDef{id: "firewall", title: "Enable the " + sup.Firewall + " firewall (SSH is allowed first)", phase: "baseline", cond: "false", body: body, cleanup: cleanup, mutating: true})
	}
	if p.UnattendedUpgrades && sup.Unattended != "" {
		defs = append(defs, stepDef{id: "unattended-upgrades", title: "Enable " + sup.Unattended + " (security updates)", phase: "baseline", cond: "false", body: unattendedBody(sup),
			cleanup: "rm -f /etc/apt/apt.conf.d/20auto-upgrades\n# The package is kept.\n", mutating: true})
	}

	// Runtime.
	if p.Runtime == "docker" {
		body, cleanup := dockerBody(sup)
		defs = append(defs, stepDef{id: "runtime-docker", title: "Install the Docker engine (" + sup.Docker + ")", phase: "runtime",
			cond: "command -v docker >/dev/null 2>&1", body: body, cleanup: cleanup, mutating: true, expect: chk.Docker != "" && chk.Docker != "none"})
	}
	if p.Restic {
		pkgs := []string{"restic"}
		body := pkgInstall(pm, pkgs...)
		if pm == "dnf" {
			body = "dnf install -y epel-release\n" + body
		}
		defs = append(defs, stepDef{id: "restic", title: "Install restic", phase: "capability", cond: "command -v restic >/dev/null 2>&1", body: body,
			cleanup: "# restic is kept.\n", mutating: true, expect: chk.ResticPresent})
	}

	// Agents.
	var native *stepDef
	if p.NativeAgent && sup.NativeAgent {
		mode := "report"
		if p.PatchApply {
			mode = "apply"
		}
		a, err := agents.Native(AgentInstallSpec{Host: req.Host, Mode: mode, ResticPaths: p.ResticPaths, Release: env.Release,
			ManagerURL: env.ManagerURL, CAFingerprint: env.CAFingerprint, Installation: env.Installation})
		if err != nil {
			return nil, fmt.Errorf("%w: native agent: %v", ErrInvalid, err)
		}
		native = &stepDef{id: "agent-native", title: "Install the native agent (" + mode + " mode) and enroll it", phase: "agent", mutating: true, stdin: "enrollment",
			cond: "false", body: stripShebang(a.Install) + "\n", cleanup: stripShebang(a.Cleanup) + "\n", expect: chk.AgentMarker}
		defs = append(defs, *native)
	}
	switch p.Cluster.Platform {
	case "swarm":
		act, stdin := "init", ""
		body := "ADVERTISE_ADDRESS=" + shellQuote(req.Host.Address) + "; export ADVERTISE_ADDRESS\n" + command(Swarm, "init") + "\n"
		if p.Cluster.Role != "init" {
			act, stdin = p.Cluster.Role, "join"
			body = "read -r JOIN_TOKEN; export JOIN_TOKEN\nMANAGER_ADDRESS=" + shellQuote(p.Cluster.ManagerAddress) + "; export MANAGER_ADDRESS\n" + command(Swarm, act) + "\n"
		}
		defs = append(defs, stepDef{id: "cluster-swarm", title: "Swarm " + act, phase: "cluster", mutating: true, stdin: stdin,
			cond: `[ "$(docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || true)" = active ]`, body: body,
			cleanup: "# Leaving a Swarm can break quorum. Run `docker swarm leave` yourself after checking the cluster (never --force on a manager).\n"})
	case "k3s":
		act := p.Cluster.Role
		defs = append(defs, stepDef{id: "cluster-k3s", title: "k3s " + act + " (preview only, execution disabled)", phase: "cluster", mutating: true,
			cond: "false", body: command(Kubernetes, act) + "\n", cleanup: "# k3s is not executed; nothing to undo.\n"})
	}
	if p.ContainerAgent {
		defs = append(defs, stepDef{id: "agent-container", title: "Deploy the container agent as a Swarm service (manager action)", phase: "agent", mutating: true,
			managerAction: "deploy-container-agent", cleanup: "# Remove the container agent service in the manager (Swarm > Services).\n"})
	}

	// Lockdown (separate approval).
	if p.DedicatedKey {
		home := `home=$(getent passwd ` + user + ` | cut -d: -f6)` + "\n"
		keyfile := `"$home/.ssh/authorized_keys"`
		defs = append(defs,
			stepDef{id: "lockdown-install-key", title: "Install the dedicated public key for " + p.ServiceUser, phase: "lockdown", gate: "lockdown", mutating: true, cond: "false",
				body: home + `install -d -m 0700 -o ` + user + ` "$home/.ssh"
touch ` + keyfile + `
chmod 0600 ` + keyfile + `
chown ` + user + ` ` + keyfile + `
tmp=$(mktemp)
cat > "$tmp" <<'DBCM_KEY_EOF'
` + plan.DedicatedKey + `
DBCM_KEY_EOF
grep -qxFf "$tmp" ` + keyfile + ` || cat "$tmp" >> ` + keyfile + `
rm -f "$tmp"
`,
				cleanup: home + `tmp=$(mktemp)
cat > "$tmp" <<'DBCM_KEY_EOF'
` + plan.DedicatedKey + `
DBCM_KEY_EOF
if [ -f ` + keyfile + ` ]; then grep -vxFf "$tmp" ` + keyfile + ` > "$tmp.new" || true; cat "$tmp.new" > ` + keyfile + `; rm -f "$tmp.new"; fi
rm -f "$tmp"
`},
			stepDef{id: "lockdown-verify-key", title: "Verify a login with the dedicated key and sudo (new connection)", phase: "lockdown", gate: "lockdown", runAs: "dedicated", unprivileged: true,
				body: "id -un\nsudo -n true\necho 'dbcm:note=dedicated key login and sudo work'\n"},
			stepDef{id: "lockdown-store-key", title: "Seal the dedicated private key in the manager (before anything is locked)", phase: "lockdown", gate: "lockdown", mutating: true,
				managerAction: "store-dedicated-key", cleanup: "# Delete the sealed host credential in the manager if the host is not managed.\n"})
		if p.DisableRootLogin {
			defs = append(defs,
				stepDef{id: "lockdown-disable-root", title: "Disable SSH root login (with an automatic revert if confirmation fails)", phase: "lockdown", gate: "lockdown", mutating: true,
					cond: "sshd -T 2>/dev/null | grep -qi '^permitrootlogin no'",
					body: `[ -d /etc/ssh/sshd_config.d ] || { echo 'sshd_config.d is missing; refusing to edit sshd_config' >&2; exit 1; }
grep -Eqi '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config.d/' /etc/ssh/sshd_config || { echo 'sshd_config does not include sshd_config.d' >&2; exit 1; }
conf=` + lockdownConf + `
reload_sshd() { systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || rc-service sshd reload; }
printf 'PermitRootLogin no\n' > "$conf.new"
mv "$conf.new" "$conf"
sshd -t || { rm -f "$conf"; echo 'sshd configuration test failed; reverted' >&2; exit 1; }
nohup sh -c 'sleep ` + strconv.Itoa(revertSeconds) + `; rm -f ` + lockdownConf + `; systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || rc-service sshd reload' >/dev/null 2>&1 </dev/null &
echo $! > ` + revertPidFile + `
reload_sshd
sshd -T 2>/dev/null | grep -qi '^permitrootlogin no' || { rm -f "$conf"; reload_sshd; kill "$(cat ` + revertPidFile + `)" 2>/dev/null || true; rm -f ` + revertPidFile + `; echo 'root login is still enabled; reverted' >&2; exit 1; }
`,
					cleanup: "rm -f " + lockdownConf + "\nsshd -t && { systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || rc-service sshd reload; }\nrm -f " + revertPidFile + "\n"},
				stepDef{id: "lockdown-confirm", title: "Confirm over a new dedicated-key connection and cancel the automatic revert", phase: "lockdown", gate: "lockdown", runAs: "dedicated", mutating: true, cond: "false",
					body: `sshd -T 2>/dev/null | grep -qi '^permitrootlogin no' || { echo 'root login is not disabled' >&2; exit 1; }
if [ -f ` + revertPidFile + ` ]; then kill "$(cat ` + revertPidFile + `)" 2>/dev/null || true; rm -f ` + revertPidFile + `; fi
`, cleanup: "# Nothing to undo; the lockdown-disable-root cleanup removes the sshd drop-in.\n"})
		}
	}

	steps := make([]BootstrapStep, 0, len(defs))
	for _, d := range defs {
		steps = append(steps, d.toStep(req.ID))
	}
	return steps, nil
}

func stripShebang(s string) string {
	if strings.HasPrefix(s, "#!") {
		if _, rest, ok := strings.Cut(s, "\n"); ok {
			return rest
		}
		return ""
	}
	return s
}

// CleanupScript renders the cleanup plan: the cleanup of every step in
// reverse order. It only reverts what the steps added (files, rules, units
// it created) and never prunes, force-removes or deletes data. The product
// does not execute it; the operator reviews and runs it.
func CleanupScript(p BootstrapPlan) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# dbcm-bootstrap " + p.ID + " cleanup-plan\nset -u\n")
	for i := len(p.Steps) - 1; i >= 0; i-- {
		s := p.Steps[i]
		if s.Cleanup == "" {
			continue
		}
		b.WriteString("\n# --- " + s.ID + ": " + s.Title + "\n")
		for _, line := range strings.Split(stripShebang(s.Cleanup), "\n") {
			if strings.HasPrefix(line, "# dbcm-bootstrap ") || strings.HasPrefix(line, "set -eu") {
				continue
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
