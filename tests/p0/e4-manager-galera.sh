#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E4: the REAL manager (bin/manager, managed single-instance
# mode with auth) and the REAL agents (mTLS, enrollment) create a 3-node
# MariaDB Galera cluster on a lab Swarm through the manager's own HTTP API.
#
# Layout:
#   - dbcm-lab-m1..m3 (bin/lab, 2 vCPU / 4 GiB / 20 GiB, lima:user-v2),
#     rootful Docker, Swarm with m1 as the only manager node.
#   - The manager runs as a plain process (transient systemd unit) inside m1
#     and uses m1's local Docker socket; its API listens on m1 loopback only,
#     its enrollment endpoint on m1's user-v2 address (TLS).
#   - The agent and job images are built INSIDE m1 from the cross-compiled
#     binaries (the host Docker daemon is never used) and copied to m2/m3
#     with `docker save | docker load` through the host (no registry).
#   - tests/p0/e4client drives the API like a browser: cookies, Origin and a
#     software passkey (ES256), so the real WebAuthn verification runs.
#
# Usage:   tests/p0/e4-manager-galera.sh
# Environment (optional, default off):
#   E4_KEEP_VMS=1    keep the VMs at the end (development; run
#                    `bin/lab cleanup -apply` yourself afterwards)
#   E4_REUSE_VMS=1   skip VM creation and Swarm setup (VMs of a kept run)
#   E4_PHASES="..."  run only these phases (default: all, in order)
#
# Output: sanitized raw log tests/p0/results/e4-manager-galera.log.
# Requirements: Go toolchain, jq, limactl on PATH.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG="$REPO_ROOT/tests/p0/results/e4-manager-galera.log"
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${E4_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
# shellcheck source=tests/p0/common.sh
. "$REPO_ROOT/tests/p0/common.sh"

VMS="dbcm-lab-m1 dbcm-lab-m2 dbcm-lab-m3"
M1=dbcm-lab-m1
USERV2_PREFIX=192.168.104.
INST=e4lab
NET=dbcm-e4-net
VOLBASE=/var/lib/dbcm/volumes
AGENT_IMAGE=dbcm-agent:e4
JOB_IMAGE=dbcm-job:e4
MARIADB_IMAGE=mariadb:11.4.13@sha256:250e1609e93c0e6aecf64bfbab2d84d5e89d2481540fe77447ff6c855c3b1873
DISTROLESS=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
API_PORT=18480
META_PORT=18490
ENROLL_PORT=7444
ROOT_SECRET=dbcm-e4-root-pw
MON_SECRET=dbcm-e4-monitor-pw
BAK_SECRET=dbcm-e4-backup-pw
BAK_CRED_SECRET=dbcm-e4-backup-cred
BACKEND_SECRET=dbcm-e4-backend
KEY_SECRET=dbcm-e4-recovery-key
UNIT=dbcm-e4-manager
STATE=/root/dbcm-e4 # inside m1 only: passwords, client state, keys (root 0700)
BIN="$REPO_ROOT/.lab/e4/bin"
CLUSTER_NAME=${E4_CLUSTER_NAME:-e4db}

finish() {
	local rc=$?
	trap - EXIT INT TERM
	if [ "${E4_KEEP_VMS:-0}" = 1 ]; then
		say "E4_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" -e 's#dbcm-et1\.[A-Za-z0-9._-]*#dbcm-et1.<redacted>#g' \
		-e "s#PASSWORD '\*[0-9A-F]*'#PASSWORD '<hash redacted>'#g" "$LOG"
	say "E4 finished with exit $rc"
	exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

note() { printf '%s\n\n' "$*" >>"$LOG"; }
hostload() { note "host uptime: $(uptime)"; say "host load: $(sysctl -n vm.loadavg)"; }
result() { printf '**Result:** %s\n\n' "$*" >>"$LOG"; say "RESULT: $*"; }

# vm_args VM SCRIPT ARGS... — run sh SCRIPT with positional ARGS inside VM.
vm_args() {
	require_lab_vm "$1"
	local vm=$1 script=$2
	shift 2
	limactl shell --workdir / "$vm" -- sh -c "$script" sh "$@"
}
# vm_root VM SCRIPT ARGS... — like vm_args, but the script runs as root.
vm_root() {
	require_lab_vm "$1"
	local vm=$1 script=$2
	shift 2
	limactl shell --workdir / "$vm" -- sudo sh -c "$script" sh "$@"
}
uv2_ip() {
	vm_args "$1" 'ip -4 -o addr show | awk "{print \$4}" | cut -d/ -f1 | grep -F "$1" | head -1' "$USERV2_PREFIX"
}

# api METHOD PATH [BODY] — call the manager API from inside m1 through the
# lab client; prints "HTTP <code>" and the body.
api() {
	vm_args "$M1" 'sudo /usr/local/bin/dbcm-e4-client -state '"$STATE"'/client.json -base http://127.0.0.1:'"$API_PORT"' -origin http://localhost:'"$API_PORT"' req "$@"' "$@"
}
# apij METHOD PATH [BODY] — body only (exit 3 on non-2xx).
apij() { api "$@" | tail -n +2; }
# apirec TITLE METHOD PATH [BODY] — call and record in the log.
apirec() {
	local t=$1
	shift
	rec "$t" api "$@"
}

phase() { case " ${E4_PHASES:-all} " in *" all "* | *" $1 "*) return 0 ;; esac; return 1; }

# ---------------------------------------------------------------- phases
phase_vms() {
	section "Phase vms: lab VMs (bin/lab) and rootful Docker"
	hostload
	rec "host free disk before apply" host_free_disk
	(cd "$REPO_ROOT" && rec "bin/lab status before" "$LAB" status)
	(cd "$REPO_ROOT" && rec "bin/lab apply -apply -vms 3 -cpus 2 -ram 4 -disk 20 -network lima:user-v2" \
		"$LAB" apply -apply -vms 3 -cpus 2 -ram 4 -disk 20 -network lima:user-v2)
	local vm
	for vm in $VMS; do
		rec "switch $vm to rootful docker" vm_sh "$vm" '
			set -e
			systemctl --user disable --now docker.service >/dev/null 2>&1 || true
			sudo systemctl unmask containerd.service docker.service docker.socket >/dev/null 2>&1
			sudo systemctl enable --now containerd.service docker.socket docker.service >/dev/null 2>&1
			echo "rootful docker: $(systemctl is-active docker.service)"'
		rec "facts $vm" vm_sh "$vm" '
			echo "arch: $(uname -m) kernel: $(uname -r)"; grep PRETTY_NAME /etc/os-release; echo "nproc: $(nproc)"
			free -m | head -2; df -h / | tail -1
			sudo docker version --format "docker server {{.Server.Version}}"'
	done
	rec "host free disk after apply" host_free_disk
}

phase_swarm() {
	section "Phase swarm: one manager node, two workers"
	hostload
	local a1 vm addr token
	a1=$(uv2_ip "$M1")
	say "m1 user-v2 address: $a1"
	rec "swarm init on m1" vm_docker "$M1" swarm init --advertise-addr "$a1"
	token=$(vm_docker "$M1" swarm join-token -q worker)
	for vm in dbcm-lab-m2 dbcm-lab-m3; do
		addr=$(uv2_ip "$vm")
		rec "swarm join $vm (advertise $addr, token not recorded)" \
			vm_docker "$vm" swarm join --advertise-addr "$addr" --token "$token" "$a1:2377"
	done
	token=""
	rec "docker node ls" vm_docker "$M1" node ls
}

phase_images() {
	section "Phase images: binaries, agent and job images built inside m1, copied to m2/m3"
	hostload
	rec "host-built linux/arm64 binaries (sha256)" sh -c "cd '$BIN' && shasum -a 256 manager agent dbcm-job e4client && go version"
	limactl copy "$BIN/manager" "$BIN/agent" "$BIN/dbcm-job" "$BIN/e4client" "$M1:/tmp/"
	rec "install binaries in m1" vm_sh "$M1" '
		set -e
		sudo install -m 0755 /tmp/manager /usr/local/bin/dbcm-e4-manager
		sudo install -m 0755 /tmp/e4client /usr/local/bin/dbcm-e4-client
		sudo mkdir -p /root/dbcm-e4-build/agent /root/dbcm-e4-build/job
		sudo install -m 0755 /tmp/manager /tmp/agent /root/dbcm-e4-build/agent/
		sudo install -m 0755 /tmp/dbcm-job /root/dbcm-e4-build/job/
		rm -f /tmp/manager /tmp/agent /tmp/dbcm-job /tmp/e4client
		/usr/local/bin/dbcm-e4-manager -version'
	# The agent image mirrors the runtime stage of deploy/Dockerfile (same
	# distroless base, user, entrypoint and binaries); only the build stages
	# are replaced by the host cross-compiled binaries.
	rec "agent image Dockerfile (runtime stage of deploy/Dockerfile)" vm_args "$M1" '
		cat <<EOF | sudo tee /root/dbcm-e4-build/agent/Dockerfile
FROM $1
COPY manager agent /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/manager"]
CMD ["-listen", "0.0.0.0:8080"]
EOF' "$DISTROLESS"
	rec "build agent image in m1" vm_args "$M1" 'sudo docker build -q -t "$1" /root/dbcm-e4-build/agent && sudo docker image inspect --format "{{.Id}} {{.Architecture}} user={{.Config.User}} entrypoint={{.Config.Entrypoint}}" "$1"' "$AGENT_IMAGE"
	# The job image is the final stage of deploy/Dockerfile.jobs.
	rec "job image Dockerfile (final stage of deploy/Dockerfile.jobs)" vm_args "$M1" '
		cat <<EOF | sudo tee /root/dbcm-e4-build/job/Dockerfile
FROM $1
RUN set -eu; for t in mariabackup mbstream mariadb-dump mariadb; do test -x "/usr/bin/\$t"; done; \\
    test "\$(id -u mysql):\$(id -g mysql)" = "999:999"
COPY dbcm-job /usr/local/bin/dbcm-job
USER 999:999
HEALTHCHECK NONE
ENTRYPOINT ["/usr/local/bin/dbcm-job"]
CMD ["-version"]
EOF' "$MARIADB_IMAGE"
	rec "build job image in m1" vm_args "$M1" 'sudo docker build -q -t "$1" /root/dbcm-e4-build/job && sudo docker image inspect --format "{{.Id}} {{.Architecture}} user={{.Config.User}} volumes={{.Config.Volumes}}" "$1" && sudo docker run --rm "$1" -version' "$JOB_IMAGE"
	local vm
	for vm in dbcm-lab-m2 dbcm-lab-m3; do
		rec "copy images to $vm (docker save | docker load through the host)" sh -c \
			"limactl shell --workdir / $M1 -- sudo docker save $AGENT_IMAGE $JOB_IMAGE | limactl shell --workdir / $vm -- sudo docker load"
	done
	for vm in $VMS; do
		rec "pull pinned MariaDB image on $vm" vm_docker "$vm" pull -q "$MARIADB_IMAGE"
	done
}

phase_prereqs() {
	section "Phase prereqs: operator-provided host directories, network and secrets (labelled with the installation)"
	hostload
	# Swarm rejects a task whose bind source does not exist (observed in the
	# first E4 run for /var/lib/dbcm-agent); the agent's state directory and
	# the volume base are host prerequisites (docs/INSTALL.md).
	local vm
	for vm in $VMS; do
		rec "host directories on $vm" vm_root "$vm" 'install -d -m 0700 /var/lib/dbcm-agent && install -d -m 0755 "$1" && ls -ld /var/lib/dbcm-agent "$1"' "$VOLBASE"
	done
	rec "overlay network" vm_args "$M1" 'sudo docker network inspect -f "{{.Name}} exists" "$1" 2>/dev/null || sudo docker network create --driver overlay --attachable --label "dbcm.installation=$2" "$1"' "$NET" "$INST"
	rec "synthetic passwords and secrets (values never logged)" vm_root "$M1" '
		set -e
		sudo install -d -m 0700 "$1"
		for n in root monitor backup; do
			[ -f "$1/$n.pw" ] || head -c 18 /dev/urandom | base64 | tr -d "\n" >"$1/$n.pw"
		done
		printf "[client]\nuser=dbcm_backup\npassword=%s\n" "$(sudo cat "$1/backup.pw")" | sudo tee "$1/backup.cnf" >/dev/null
		sudo chmod 0600 "$1"/*
		mk() { if sudo docker secret inspect "$1" >/dev/null 2>&1; then echo "secret $1 exists"; else sudo docker secret create --label "dbcm.installation=$2" "$1" "$3" >/dev/null && echo "secret $1"; fi; }
		mk "$3" "$2" "$1/root.pw"; mk "$4" "$2" "$1/monitor.pw"; mk "$5" "$2" "$1/backup.pw"; mk "$6" "$2" "$1/backup.cnf"' \
		"$STATE" "$INST" "$ROOT_SECRET" "$MON_SECRET" "$BAK_SECRET" "$BAK_CRED_SECRET"
}

# SFTP backup target on m1: a dedicated lab user, key-only, host key pinned.
phase_backend() {
	section "Phase backend: SFTP backup target on m1 (OpenSSH of the VM, lab user)"
	hostload
	local a1
	a1=$(uv2_ip "$M1")
	rec "lab SFTP user and key" vm_root "$M1" '
		set -e
		id dbcme4bak >/dev/null 2>&1 || sudo useradd -m -s /bin/sh dbcme4bak
		sudo install -d -o dbcme4bak -g dbcme4bak -m 0700 /home/dbcme4bak/.ssh /home/dbcme4bak/backups
		[ -f "$1/sftp_key" ] || sudo ssh-keygen -q -t ed25519 -N "" -f "$1/sftp_key"
		sudo cp "$1/sftp_key.pub" /home/dbcme4bak/.ssh/authorized_keys
		sudo chown dbcme4bak:dbcme4bak /home/dbcme4bak/.ssh/authorized_keys
		sudo chmod 0600 /home/dbcme4bak/.ssh/authorized_keys
		for t in ecdsa ed25519 rsa; do sudo ssh-keygen -lf /etc/ssh/ssh_host_${t}_key.pub | cut -d" " -f1,2,4; done' "$STATE"
	local fp
	fp=$(vm_sh "$M1" 'sudo ssh-keygen -lf /etc/ssh/ssh_host_ecdsa_key.pub | cut -d" " -f2')
	say "pinned host key fingerprint (ecdsa): $fp"
	rec "backend config (private key not logged) and recovery key" vm_root "$M1" '
		set -e
		key=$(sudo cat "$1/sftp_key" | python3 -c "import json,sys; print(json.dumps(sys.stdin.read()))")
		printf "{\"type\":\"sftp\",\"sftp\":{\"addr\":\"%s:22\",\"user\":\"dbcme4bak\",\"dir\":\"/home/dbcme4bak/backups\",\"host_key_fingerprint\":\"%s\",\"private_key_pem\":%s}}\n" "$2" "$3" "$key" | sudo tee "$1/backend.json" >/dev/null
		sudo chmod 0600 "$1/backend.json"
		sudo python3 -c "import json,sys; c=json.load(open(sys.argv[1])); c[\"sftp\"][\"private_key_pem\"]=\"<redacted>\"; print(json.dumps(c))" "$1/backend.json"
		[ -f "$1/recipient.pub" ] || /usr/local/bin/dbcm-e4-client -state "$1/client.json" keygen "$1/recovery.key" >"$1/recipient.pub"
		cat "$1/recipient.pub"
		for x in "$5:backend.json" "$6:recovery.key"; do
			n=${x%%:*}; f=${x#*:}
			if docker secret inspect "$n" >/dev/null 2>&1; then echo "secret $n exists"; else docker secret create --label "dbcm.installation=$4" "$n" "$1/$f" >/dev/null && echo "secret $n"; fi
		done' \
		"$STATE" "$a1" "$fp" "$INST" "$BACKEND_SECRET" "$KEY_SECRET"
}

phase_manager() {
	section "Phase manager: start bin/manager (managed single-instance mode, auth, mTLS agents)"
	hostload
	local a1 pub
	a1=$(uv2_ip "$M1")
	pub=$(vm_sh "$M1" "sudo cat $STATE/recipient.pub")
	rec "manager config" vm_root "$M1" '
		set -e
		sudo install -d -m 0700 /etc/dbcm-e4 /var/lib/dbcm-e4
		cat <<EOF | sudo tee /etc/dbcm-e4/manager.json
{
  "auth": {"enabled": true, "rpId": "localhost", "rpDisplayName": "E4 lab", "origins": ["http://localhost:$2"]},
  "manager": {
    "nodeId": "manager-1", "dataDir": "/var/lib/dbcm-e4", "bind": "127.0.0.1:$3", "bootstrap": true,
    "agentTransport": {
      "enrollListen": "$1:$4", "managerUrl": "https://$1:$4", "agentPort": 7443,
      "monitorUser": "dbcm_monitor", "monitorSecret": "$5", "dbSecrets": ["$6", "$7"]
    },
    "backup": {"backendConfigFile": "$9/backend.json", "scheduleIntervalSeconds": 30},
    "settings": {
      "installationId": "$8", "volumeBase": "/var/lib/dbcm/volumes", "agentImage": "dbcm-agent:e4",
      "accounts": {"monitorUser": "dbcm_monitor", "monitorSecret": "$5", "backupSecret": "$7"},
      "backup": {"jobImage": "dbcm-job:e4", "backendSecret": "dbcm-e4-backend", "dbCredentialSecret": "dbcm-e4-backup-cred",
                 "recipientKey": "${10}", "keySecret": "dbcm-e4-recovery-key"}
    }
  }
}
EOF' "$a1" "$API_PORT" "$META_PORT" "$ENROLL_PORT" "$MON_SECRET" "$ROOT_SECRET" "$BAK_SECRET" "$INST" "$STATE" "$pub"
	rec "start manager unit" vm_args "$M1" 'sudo systemd-run --unit "$1" -p Restart=no --collect /usr/local/bin/dbcm-e4-manager -listen "127.0.0.1:$2" -config /etc/dbcm-e4/manager.json' "$UNIT" "$API_PORT"
	wait_until 60 "manager health" vm_sh "$M1" "curl -fsS http://127.0.0.1:$API_PORT/api/v1/health"
	wait_until 120 "manager leads (API not 503)" api GET /api/v1/setup/status
	rec "manager log (setup token line removed)" vm_args "$M1" 'sudo journalctl -u "$1" --no-pager | grep -v "setup token" | tail -20' "$UNIT"
	rec "listeners in m1" vm_sh "$M1" "sudo ss -ltnp | grep -E 'dbcm-e4|:$API_PORT|:$META_PORT|:$ENROLL_PORT' || true"
}

phase_setup() {
	section "Phase setup: first administrator with a software passkey through the API"
	hostload
	vm_args "$M1" 'sudo journalctl -u "$1" --no-pager | grep -o "setup token[^:]*: [^ ]*" | tail -1 | awk "{print \$NF}" | sudo tee "$2/setup.token" >/dev/null; sudo chmod 0600 "$2/setup.token"' "$UNIT" "$STATE"
	rec "setup status" api GET /api/v1/setup/status
	rec "setup (WebAuthn registration, ES256 software key)" vm_args "$M1" 'sudo /usr/local/bin/dbcm-e4-client -state "$1/client.json" -base "http://127.0.0.1:$2" -origin "http://localhost:$2" setup -token-file "$1/setup.token" -account admin' "$STATE" "$API_PORT"
	rec "session" api GET /api/v1/auth/session
	rec "setup status after" api GET /api/v1/setup/status
}

SWARM_ID=""
phase_swarmapi() {
	section "Phase swarmapi: connect the Swarm, sync, enroll hosts, deploy agents"
	hostload
	rec "connect swarm" api POST /api/v1/swarms '{"name":"lab","dockerHost":"unix:///var/run/docker.sock"}'
	SWARM_ID=$(apij GET /api/v1/swarms | jq -r '.items[0].id')
	say "swarm id: $SWARM_ID"
	rec "sync swarm" api POST "/api/v1/swarms/$SWARM_ID/sync"
	rec "swarms" api GET /api/v1/swarms
	rec "hosts after sync" api GET /api/v1/hosts
	local id
	for id in $(apij GET /api/v1/hosts | jq -r '.items[].nodeId'); do
		rec "enroll host $id" api POST "/api/v1/hosts/$id/enroll" '{}'
	done
	rec "deploy agents" api POST "/api/v1/swarms/$SWARM_ID/agents/deploy"
	rec "agent service" vm_docker "$M1" service ls --filter "label=dbcm.installation=$INST"
	wait_until 300 "all 3 agents enrolled (agentIdentity true)" agents_enrolled 3 ||
		{ rec "agent tasks" vm_docker "$M1" service ps --no-trunc "dbcm-agent-$INST"; rec "agent logs" vm_docker "$M1" service logs --tail 30 "dbcm-agent-$INST"; return 1; }
	rec "hosts after enrollment" api GET /api/v1/hosts
	rec "agent tasks" vm_docker "$M1" service ps --format '{{.Node}} {{.CurrentState}} {{.Error}}' "dbcm-agent-$INST"
}
agents_enrolled() { [ "$(apij GET /api/v1/hosts | jq '[.items[] | select(.agentIdentity==true)] | length')" = "$1" ]; }

CLUSTER_ID=""
OP_ID=""
cluster_id() { apij GET /api/v1/clusters | jq -r --arg n "$CLUSTER_NAME" '.items[] | select(.name==$n) | .id'; }
phase_cluster() {
	section "Phase cluster: POST /clusters and wait for the operation"
	hostload
	SWARM_ID=${SWARM_ID:-$(apij GET /api/v1/swarms | jq -r '.items[0].id')}
	local body
	body=$(jq -cn --arg s "$SWARM_ID" --arg n "$CLUSTER_NAME" --arg i "$MARIADB_IMAGE" --arg net "$NET" --arg r "$ROOT_SECRET" \
		'{swarm:$s,name:$n,size:3,image:$i,network:$net,rootPasswordSecret:$r,gcacheSize:"128M",
		  resources:{limits:{milli_cpu:1000,memory_bytes:1073741824},reservations:{milli_cpu:250,memory_bytes:536870912}},
		  placement:{reserve:{}}}')
	local resp
	resp=$(api POST /api/v1/clusters "$body") || true
	printf '### create cluster\n\n```\n$ api POST /api/v1/clusters %s\n%s\n```\n\n' "$body" "$resp" >>"$LOG"
	OP_ID=$(printf '%s\n' "$resp" | tail -n +2 | jq -r '.id')
	CLUSTER_ID=$(cluster_id)
	say "operation $OP_ID cluster $CLUSTER_ID"
	local start now st
	start=$(date +%s)
	while :; do
		st=$(apij GET /api/v1/operations | jq -r --arg id "$OP_ID" '.items[] | select(.id==$id) | "\(.state) \(.reason // "")"')
		say "operation: $st"
		case "$st" in succeeded*|failed*) break ;; esac
		now=$(date +%s)
		[ $((now - start)) -lt 1500 ] || { say "operation not finished after 1500 s"; break; }
		sleep 20
	done
	rec "operation" api GET /api/v1/operations
	rec "cluster" api GET "/api/v1/clusters/$CLUSTER_ID"
	rec "events" api GET /api/v1/events
	rec "services" vm_docker "$M1" service ls
	rec "galera tasks" vm_sh "$M1" "sudo docker service ps --no-trunc --format '{{.Name}} {{.Node}} {{.CurrentState}} {{.Error}}' \$(sudo docker service ls -q --filter label=dbcm.cluster)"
}


# node helpers: the slot nN runs wherever the manager placed it; find the
# VM from the Swarm task.
svc() { echo "dbcm-$CLUSTER_NAME-n$1"; }
node_vm() { # node_vm N -> VM name running slot nN
	local host
	host=$(vm_docker "$M1" service ps --filter desired-state=running --format '{{.Node}}' "$(svc "$1")" | head -1)
	echo "${host#lima-}"
}
cid() { # cid N -> running container of slot nN on its VM
	vm_docker "$(node_vm "$1")" ps -q --no-trunc -f "label=com.docker.swarm.service.name=$(svc "$1")" -f status=running | head -1
}
# rsql N SQL — SQL as root over the node socket (password from the
# container's own secret mount, never on a command line).
rsql() {
	local c vm
	vm=$(node_vm "$1")
	c=$(vm_docker "$vm" ps -q --no-trunc -f "label=com.docker.swarm.service.name=$(svc "$1")" -f status=running | head -1)
	[ -n "$c" ] || return 1
	printf '%s\n' "$2" | limactl shell --workdir / "$vm" -- sudo docker exec -i "$c" \
		sh -c 'exec mariadb -S /dbcm/run/mysqld.sock -uroot -p"$(cat /run/secrets/'"$ROOT_SECRET"')" -N -B'
}

phase_verify() {
	section "Phase verify: real MariaDB checks"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	local n
	for n in 1 2 3; do
		rec "n$n placement and wsrep status" sh -c "echo vm=\$0; printf '%s\n' \"\$1\"" "$(node_vm "$n")" \
			"$(rsql "$n" "SELECT @@hostname, @@socket, @@datadir; SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_local_state_comment','wsrep_cluster_status','wsrep_ready','wsrep_connected','wsrep_cluster_state_uuid')")"
	done
	rec "write on n1" rsql 1 "CREATE DATABASE IF NOT EXISTS e4; CREATE TABLE IF NOT EXISTS e4.t (id INT PRIMARY KEY, v VARCHAR(64), at TIMESTAMP DEFAULT CURRENT_TIMESTAMP); REPLACE INTO e4.t (id, v) VALUES (1, 'written on n1'); SELECT COUNT(*) FROM e4.t"
	rec "write on n3" rsql 3 "REPLACE INTO e4.t (id, v) VALUES (3, 'written on n3')"
	for n in 1 2 3; do
		rec "read on n$n" rsql "$n" "SELECT id, v FROM e4.t ORDER BY id"
	done
	rec "accounts and grants (n2, replicated)" rsql 2 "SELECT user, host, plugin FROM mysql.user ORDER BY user, host; SHOW GRANTS FOR 'dbcm_monitor'@'localhost'; SHOW GRANTS FOR 'dbcm_backup'@'%'"
	local vm c
	vm=$(node_vm 2)
	c=$(cid 2)
	rec "monitoring account over the node socket (inside the n2 container, password from m1 state file via stdin)" sh -c \
		"limactl shell --workdir / $M1 -- sudo cat $STATE/monitor.pw | limactl shell --workdir / $vm -- sudo docker exec -i $c sh -c 'read -r pw; exec mariadb -S /dbcm/run/mysqld.sock -udbcm_monitor -p\"\$pw\" -N -B -e \"SELECT CURRENT_USER(); SHOW GLOBAL STATUS LIKE \\\"wsrep_cluster_size\\\"\"'"
	rec "volume layout on the n1 host" vm_sh "$(node_vm 1)" "sudo ls -la $VOLBASE/ && sudo ls -la $VOLBASE/*n1*/ && sudo ls -la $VOLBASE/*n1*/run/"
	rec "cluster via API (health)" api GET "/api/v1/clusters/$CLUSTER_ID"
}

phase_failure() {
	section "Phase failure: kill the n3 container, observe the manager"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	local vm c t0 i
	vm=$(node_vm 3)
	c=$(cid 3)
	t0=$(date +%s)
	rec "kill n3 container $c on $vm" vm_docker "$vm" kill "$c"
	for i in $(seq 1 24); do
		say "t+$(($(date +%s) - t0))s cluster: $(apij GET "/api/v1/clusters/$CLUSTER_ID" | jq -c '{state, nodes: [.nodes[] | {id, state, role}]}')"
		sleep 10
	done
	rec "n3 tasks after kill" vm_docker "$M1" service ps --format '{{.Name}} {{.Node}} {{.CurrentState}} {{.Error}}' "$(svc 3)"
	rec "wsrep on n1 after kill" rsql 1 "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_local_state_comment')"
	rec "cluster via API after kill" api GET "/api/v1/clusters/$CLUSTER_ID"
	rec "events after kill" api GET /api/v1/events
}

phase_backup() {
	section "Phase backup: physical backup through the API to the SFTP target"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	rec "start backup" api POST "/api/v1/clusters/$CLUSTER_ID/backups"
	local op start st
	op=$(apij GET /api/v1/operations | jq -r '[.items[] | select(.kind | test("backup"))] | sort_by(.startedAt) | last | .id')
	say "backup operation $op"
	start=$(date +%s)
	while :; do
		st=$(apij GET /api/v1/operations | jq -r --arg id "$op" '.items[] | select(.id==$id) | "\(.state) \(.reason // "")"')
		say "backup operation: $st"
		case "$st" in succeeded*|failed*) break ;; esac
		[ $(($(date +%s) - start)) -lt 900 ] || { say "backup not finished after 900 s"; break; }
		sleep 15
	done
	rec "operations" api GET /api/v1/operations
	rec "backups" api GET /api/v1/backups
	rec "job services" vm_docker "$M1" service ls --filter "label=dbcm.installation=$INST"
	rec "backup objects on the SFTP target" vm_sh "$M1" "sudo find /home/dbcme4bak/backups -type f -printf '%s %p\n' | sort -k2"
	rec "manifest(s)" vm_sh "$M1" "for f in \$(sudo find /home/dbcme4bak/backups -type f -name '*manifest*'); do echo \"== \$f\"; sudo cat \"\$f\"; echo; done"
}

opstate() { apij GET /api/v1/operations | jq -c --arg id "$1" '.items[] | select(.id==$id) | {state, reason, approval}'; }

phase_inspect() {
	section "Phase inspect: isolated inspection restore of the newest backup (approval bound to the digest)"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	local mgr resp op digest st start
	mgr=$(apij GET /api/v1/hosts | jq -r '[.items[] | select(.role=="manager")][0].nodeId')
	rec "inspection network (operator-provided, labelled)" vm_args "$M1" 'sudo docker network inspect -f "{{.Name}} exists" dbcm-e4-inspect 2>/dev/null || sudo docker network create --driver overlay --attachable --label "dbcm.installation=$1" dbcm-e4-inspect' "$INST"
	# The response carries the one-time inspection password: it stays in a
	# root-only file in m1 and is never logged.
	resp=$(vm_args "$M1" 'sudo /usr/local/bin/dbcm-e4-client -state "$1/client.json" -base "http://127.0.0.1:$2" -origin "http://localhost:$2" req POST /api/v1/restores/inspect "$3" | sudo tee "$1/inspect.json" >/dev/null; sudo head -1 "$1/inspect.json"; sudo tail -1 "$1/inspect.json" | python3 -c "import json,sys; d=json.load(sys.stdin); d.pop(\"inspectionPassword\",None); print(json.dumps(d))"' \
		"$STATE" "$API_PORT" "{\"cluster\":\"$CLUSTER_ID\",\"host\":\"$mgr\",\"ttlMinutes\":60}")
	printf '### start inspection on the manager node %s\n\n```\n%s\n```\n\n' "$mgr" "$resp" >>"$LOG"
	op=$(printf '%s\n' "$resp" | tail -1 | jq -r .id)
	digest=$(printf '%s\n' "$resp" | tail -1 | jq -r .approval.digest)
	sleep 5
	rec "operation before approval" opstate "$op"
	rec "approve with a wrong digest (must be refused)" api POST "/api/v1/operations/$op/approve" '{"digest":"0000000000000000000000000000000000000000000000000000000000000000"}' || true
	rec "approve with the digest" api POST "/api/v1/operations/$op/approve" "{\"digest\":\"$digest\"}"
	start=$(date +%s)
	while :; do
		st=$(apij GET /api/v1/operations | jq -r --arg id "$op" '.items[] | select(.id==$id) | "\(.state) \(.reason // "")"')
		say "inspection operation: $st"
		case "$st" in succeeded* | failed* | *restore-verified*) break ;; esac
		[ $(($(date +%s) - start)) -lt 900 ] || { say "inspection not at its verification gate after 900 s"; break; }
		sleep 15
	done
	rec "inspection query with the one-time account (password from the root-only file via stdin)" vm_args "$M1" '
		svc=$(sudo tail -1 "$1/inspect.json" | python3 -c "import json,sys; print(json.load(sys.stdin)[\"service\"])")
		port=$(sudo tail -1 "$1/inspect.json" | python3 -c "import json,sys; print(json.load(sys.stdin)[\"port\"])")
		c=$(sudo docker ps -q -f "label=com.docker.swarm.service.name=$svc")
		sudo docker service inspect -f "{{json .Spec.TaskTemplate.ContainerSpec.Args}}" "$svc"
		sudo tail -1 "$1/inspect.json" | python3 -c "import json,sys; print(json.load(sys.stdin)[\"inspectionPassword\"])" |
			sudo docker exec -i "$c" sh -c "read -r pw; exec mariadb -h127.0.0.1 -P$port -udbcm_inspect -p\"\$pw\" -N -B -e \"SELECT CURRENT_USER(); SELECT @@wsrep_on; SELECT id, v FROM e4.t ORDER BY id\""' "$STATE"
	vm_sh "$M1" "sudo rm -f $STATE/inspect.json"
	rec "operations after inspection" api GET /api/v1/operations
}

for p in vms swarm images prereqs backend manager setup swarmapi cluster verify failure backup inspect; do
	if phase "$p"; then
		case "$p" in vms | swarm) [ "${E4_REUSE_VMS:-0}" = 1 ] && continue ;; esac
		"phase_$p"
	fi
done
