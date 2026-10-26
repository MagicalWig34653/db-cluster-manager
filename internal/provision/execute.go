// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrOutcomeUnknown means a mutating remote command may have completed. The
// operation must pause for reconciliation before any later host is touched.
var ErrOutcomeUnknown = errors.New("provision: remote outcome unknown")

// Inspect uses a fixed read-only script. It rejects a host that is already
// part of a Swarm or has local k3s state, even if that cluster looks healthy.
func Inspect(ctx context.Context, r CommandRunner, h Host, c Credential, platform Platform) (Check, error) {
	ports := "2377|7946|4789"
	if platform == Kubernetes {
		ports = "6443|10250|8472"
	}
	dockerCheck := ""
	if platform == Swarm {
		dockerCheck = `
command -v docker >/dev/null
docker_state=$(docker info --format '{{.Swarm.LocalNodeState}}')
printf 'docker=%s\n' "$docker_state"`
	}
	cmd := `set -eu
. /etc/os-release
command -v ss >/dev/null
printf 'os=%s\n' "$ID"
printf 'disk=%s\n' "$(df -PB1 / | awk 'NR==2 {print $4}')"
printf 'time=%s\n' "$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)"
if ss -lntu | grep -Eq '[:.](` + ports + `)([[:space:]]|$)'; then echo ports=busy; else echo ports=free; fi
if [ -f /etc/rancher/k3s/k3s.yaml ] || [ -f /etc/rancher/k3s/k3s-agent.yaml ] || systemctl is-active --quiet k3s-agent 2>/dev/null; then echo cluster=k3s; elif command -v docker >/dev/null 2>&1 && [ "$(docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || true)" = active ]; then echo cluster=swarm; else echo cluster=none; fi` + dockerCheck
	out, err := r.Run(ctx, h, c, cmd, "")
	if err != nil {
		return Check{}, ErrPreflight
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || values[k] != "" {
			return Check{}, ErrPreflight
		}
		values[k] = strings.TrimSpace(v)
	}
	disk, err := strconv.ParseUint(values["disk"], 10, 64)
	if err != nil {
		return Check{}, ErrPreflight
	}
	check := Check{HostID: h.ID, OS: "linux", PortsAvailable: values["ports"] == "free", TimeSynchronized: values["time"] == "yes", DiskBytes: disk}
	if values["cluster"] != "none" {
		check.ClusterID = values["cluster"]
	}
	if values["os"] == "" || values["ports"] == "" || values["time"] == "" || values["cluster"] == "" {
		return Check{}, ErrPreflight
	}
	if check.ClusterID != "" {
		return check, ErrExistingCluster
	}
	if platform == Swarm && values["docker"] != "inactive" {
		return Check{}, ErrPreflight
	}
	if !check.PortsAvailable || !check.TimeSynchronized || disk < 10<<30 {
		return check, ErrPreflight
	}
	return check, nil
}

// ApplyStep runs one fixed command after repeating preflight. A join token is
// fetched from the seed manager for this attempt and passed through SSH stdin;
// neither it nor command output is returned or journaled.
func ApplyStep(ctx context.Context, r CommandRunner, p Plan, step Step, credentials map[string]Credential) error {
	if p.Platform != Swarm {
		return errors.New("provision: Kubernetes execution is experimental and disabled")
	}
	var host *Host
	for i := range p.Hosts {
		if p.Hosts[i].ID == step.HostID {
			host = &p.Hosts[i]
			break
		}
	}
	if host == nil {
		return ErrInvalid
	}
	expected := command(p.Platform, step.Action)
	if step.Action == "init" {
		expected = "ADVERTISE_ADDRESS=" + shellQuote(host.Address) + "; export ADVERTISE_ADDRESS; " + expected
	}
	if step.Command != expected {
		return ErrInvalid
	}
	cred, ok := credentials[host.ID]
	if !ok {
		return ErrInvalid
	}
	if _, err := Inspect(ctx, r, *host, cred, p.Platform); err != nil {
		return err
	}
	cmd := step.Command
	stdin := ""
	if step.Action != "init" {
		seed := p.Hosts[0]
		seedCred, ok := credentials[seed.ID]
		if !ok {
			return ErrInvalid
		}
		role := "worker"
		if host.Role == "manager" {
			role = "manager"
		}
		token, err := r.Run(ctx, seed, seedCred, "docker swarm join-token -q "+role, "")
		if err != nil || strings.TrimSpace(token) == "" || strings.ContainsAny(strings.TrimSpace(token), " \n\t\r") {
			return errors.New("provision: join token unavailable")
		}
		stdin = strings.TrimSpace(token) + "\n"
		cmd = "read -r JOIN_TOKEN; export JOIN_TOKEN; MANAGER_ADDRESS=" + shellQuote(seed.Address+":2377") + "; export MANAGER_ADDRESS; " + cmd
	}
	_, err := r.Run(ctx, *host, cred, cmd, stdin)
	if err != nil {
		return fmt.Errorf("%w: step %s", ErrOutcomeUnknown, step.ID)
	}
	return nil
}
