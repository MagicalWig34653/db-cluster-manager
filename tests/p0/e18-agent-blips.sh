#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E18 part 1: why does the cluster health show "node ... became
# unknown ... connection refused" (agent port 7443) when a backup job starts?
#
# Builds the E4 layout (real manager, mTLS agents, 3-node Galera, SFTP backup
# target) with the E4 library, then starts several backups while every VM
# records: a 5 Hz TCP probe of 127.0.0.1:7443, `docker events` for the agent
# container (die/start/oom/kill), `docker stats` of the agent container,
# vmstat, and the agent service task history. The manager's own cluster
# events are read from the API afterwards and correlated by time.
#
# Usage:  tests/p0/e18-agent-blips.sh
#   E18_KEEP_VMS=1     keep the VMs
#   E18_PHASES="..."   default: vms swarm images prereqs backend manager setup swarmapi cluster blips
#   E18_BACKUPS=N      number of sequential backups (default 4)
#   E18_LOG=FILE       log (default tests/p0/results/e18-agent-blips.log)
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${E18_LOG:-"$REPO_ROOT/tests/p0/results/e18-agent-blips.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${E18_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
export E4_AUTORESUME=false
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"
CLUSTER_NAME=e18db
SAMPLES_DIR="$REPO_ROOT/tests/p0/results/e18-samples"
OUT=/root/dbcm-e18-out

finish18() {
	local rc=$?
	trap - EXIT INT TERM
	if [ "${E18_KEEP_VMS:-0}" = 1 ]; then
		say "E18_KEEP_VMS=1: VMs kept; run bin/lab cleanup --apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" -e 's#dbcm-et1\.[A-Za-z0-9._-]*#dbcm-et1.<redacted>#g' \
		-e "s#PASSWORD '\*[0-9A-F]*'#PASSWORD '<hash redacted>'#g" "$LOG"
	say "E18 part 1 finished with exit $rc"
	exit "$rc"
}
trap finish18 EXIT
trap 'exit 130' INT TERM

E18_PHASES=${E18_PHASES:-"build vms swarm images prereqs backend manager setup swarmapi cluster blips"}
phase_build() {
	section "Phase build: cross-compile linux/arm64 binaries on the host"
	mkdir -p "$BIN"
	local c
	for c in manager agent dbcm-job; do
		(cd "$REPO_ROOT" && rec "go build ./cmd/$c" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/$c" "./cmd/$c")
	done
	(cd "$REPO_ROOT" && rec "go build ./tests/p0/e4client" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/e4client" ./tests/p0/e4client)
}
agent_svc() { echo "dbcm-agent-$INST"; }

# sampler_start VM — probe, events, stats, vmstat as transient units.
sampler_start() {
	local vm=$1
	vm_root "$vm" 'install -d -m 0700 "$1"; rm -f "$1"/*.smp
		cat >"$1/probe.sh" <<"EOS"
#!/bin/bash
# one line per state change of 127.0.0.1:7443 (ms timestamps), sampled every 0.2 s
prev=""
while :; do
	if timeout 1 bash -c "echo >/dev/tcp/127.0.0.1/7443" 2>/dev/null; then s=open; else s=closed; fi
	if [ "$s" != "$prev" ]; then echo "$(date -u +%H:%M:%S.%3N) $s"; prev=$s; fi
	sleep 0.2
done
EOS
		cat >"$1/stats.sh" <<"EOS"
#!/bin/bash
while :; do
	c=$(docker ps -q -f "label=com.docker.swarm.service.name=$1" | head -1)
	[ -n "$c" ] && echo "$(date -u +%H:%M:%S) $(docker stats --no-stream --format "{{.CPUPerc}} mem={{.MemUsage}} {{.MemPerc}} pids={{.PIDs}}" "$c" 2>&1)"
	sleep 2
done
EOS
		chmod +x "$1/probe.sh" "$1/stats.sh"
		for u in e18probe e18events e18stats e18vmstat; do systemctl stop "$u" 2>/dev/null || true; done
		systemd-run -q --unit e18probe --collect -p StandardOutput=file:"$1/probe.smp" "$1/probe.sh"
		systemd-run -q --unit e18events --collect -p StandardOutput=file:"$1/events.smp" docker events --format "{{.TimeNano}} {{.Type}} {{.Action}} {{.Actor.Attributes.name}}" --filter type=container --filter event=die --filter event=start --filter event=kill --filter event=oom --filter event=stop --filter event=destroy
		systemd-run -q --unit e18stats --collect -p StandardOutput=file:"$1/stats.smp" "$1/stats.sh" '"$(agent_svc)"'
		systemd-run -q --unit e18vmstat --collect -p StandardOutput=file:"$1/vmstat.smp" sh -c "vmstat -t 1"' "$OUT"
}
sampler_stop() {
	vm_root "$1" 'for u in e18probe e18events e18stats e18vmstat; do systemctl stop "$u" 2>/dev/null || true; done'
}

agent_snapshot() { # label
	rec "agent service tasks ($1)" vm_docker "$M1" service ps --no-trunc --format '{{.Name}} {{.ID}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' "$(agent_svc)"
	rec "agent service spec update status and UpdatedAt ($1)" vm_docker "$M1" service inspect -f '{{.UpdatedAt}} version={{.Version.Index}} update={{json .UpdateStatus}} secrets={{len .Spec.TaskTemplate.ContainerSpec.Secrets}}' "$(agent_svc)"
}

phase_blips() {
	section "Phase blips (E18 part 1): backups with samplers on every VM"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	rec "docker version and service list" vm_docker "$M1" service ls
	agent_snapshot before
	local vm i op st start t0 n=${E18_BACKUPS:-4}
	for vm in $VMS; do sampler_start "$vm"; done
	sleep 20
	t0=$(date -u +%H:%M:%S)
	say "samplers running; first backup at $t0 UTC"
	for i in $(seq 1 "$n"); do
		say "backup $i/$n start $(date -u +%H:%M:%S.%3N)"
		api POST "/api/v1/clusters/$CLUSTER_ID/backups" >>"$LOG" 2>&1 || true
		sleep 2
		op=$(apij GET /api/v1/operations | jq -r '[.items[] | select(.kind | test("backup"))] | sort_by(.startedAt) | last | .id')
		start=$(date +%s)
		while :; do
			st=$(apij GET /api/v1/operations | jq -r --arg id "$op" '.items[] | select(.id==$id) | "\(.state) \(.reason // "")"')
			case "$st" in succeeded* | failed*) break ;; esac
			[ $(($(date +%s) - start)) -lt 600 ] || { say "backup $i not finished after 600 s"; break; }
			sleep 2
		done
		say "backup $i/$n end $(date -u +%H:%M:%S.%3N): $st"
		sleep 25
	done
	sleep 30
	for vm in $VMS; do sampler_stop "$vm"; done
	agent_snapshot after
	rec "cluster events (node health transitions)" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/events | tail -n +2 | jq -r '.items[] | select(.message|test(\"unknown|healthy|unhealthy|unavailable|recovered\")) | \"\\(.time) \\(.severity) \\(.message)\"' | cut -c1-260"
	rec "all backup operations" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/operations | tail -n +2 | jq -r '.items[] | select(.kind|test(\"backup\")) | \"\\(.id) \\(.kind) \\(.state) \\(.startedAt)\"'"
	mkdir -p "$SAMPLES_DIR"
	for vm in $VMS; do
		for f in probe events stats vmstat; do
			limactl shell --workdir / "$vm" -- sudo cat "$OUT/$f.smp" >"$SAMPLES_DIR/$vm.$f.txt" 2>/dev/null || true
		done
		rec "agent container log on $vm (restarts and errors only, last 40)" sh -c "limactl shell --workdir / $vm -- sudo sh -c 'for c in \$(docker ps -aq -f label=com.docker.swarm.service.name=$(agent_svc)); do docker logs --timestamps \$c 2>&1; done' | grep -iE 'error|panic|fatal|listen|shutdown|signal|start' | tail -40 | cut -c1-240"
		rec "kernel OOM kills on $vm" vm_root "$vm" 'dmesg 2>/dev/null | grep -iE "out of memory|oom-kill|killed process" | tail -5 || true'
	done
	say "samples saved in $SAMPLES_DIR"
}

for p in $E18_PHASES; do
	case "$p" in vms | swarm) [ "${E4_REUSE_VMS:-0}" = 1 ] && continue ;; esac
	"phase_$p"
done
