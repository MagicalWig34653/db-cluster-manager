#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E1: Docker Swarm on three dbcm-lab VMs, an unrelated sentinel
# service, and three pinned single-replica Galera services with bind-mounted
# local data directories and identity markers (ADR 0004 assumptions 2 and 3).
#
# Usage (from anywhere):  tests/p0/e1-swarm-galera.sh
#
# Environment (all optional, explicit, default off):
#   E1_KEEP_VMS=1   do not run the scoped cleanup at the end (development only;
#                   run `bin/lab cleanup -apply` yourself afterwards)
#   E1_REUSE_VMS=1  skip `bin/lab apply` and guest preparation; expects the
#                   prepared VMs from a previous E1_KEEP_VMS=1 run and resets
#                   the Swarm on them first
#
# Output: raw sanitized log at tests/p0/results/e1-swarm-galera.log.
# Requirements: bin/lab built (`make build`), limactl on PATH. All docker
# commands run inside the lab VMs; the host Docker daemon is never used.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG="$REPO_ROOT/tests/p0/results/e1-swarm-galera.log"
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
: >"$LOG"
# shellcheck source=tests/p0/common.sh
. "$REPO_ROOT/tests/p0/common.sh"

VMS="dbcm-lab-m1 dbcm-lab-m2 dbcm-lab-m3"
M1=dbcm-lab-m1
NET=dbcm-e1-galera
SECRET=dbcm-e1-root-pw
MARIADB_IMAGE=mariadb:11.4
SENTINEL_IMAGE=nginx:1.27-alpine
ONESHOT_IMAGE=busybox:1.37
SENTINEL=sentinel-web
SENTINEL_PORT=18080
INSTALLATION=e1-lab
CLUSTER=e1-galera

finish() {
	local rc=$?
	trap - EXIT INT TERM
	if [ "${E1_KEEP_VMS:-0}" = 1 ]; then
		say "E1_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		lab_cleanup || rc=1
		# The Lima user-v2 network daemon is started by Lima for the lab VMs;
		# bin/lab does not track it. Record (do not touch) its state.
		rec "Lima user-v2 usernet daemons after cleanup (recorded, not touched)" \
			sh -c 'ps -axo pid,command | grep "[l]imactl usernet" || echo none' || true
	fi
	sanitize_log "$LOG"
	say "E1 finished with exit $rc (log: tests/p0/results/e1-swarm-galera.log)"
	exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

vm_of() { echo "dbcm-lab-m$1"; } # galera-N runs on dbcm-lab-mN

# ---------------------------------------------------------------- step 1
step1_vms() {
	section "Step 1: lab VMs"
	rec "host free disk before apply" host_free_disk
	(cd "$REPO_ROOT" && rec "bin/lab status before" "$LAB" status)
	(cd "$REPO_ROOT" && rec "bin/lab apply -apply -vms 3 -cpus 2 -ram 4 -disk 20" \
		"$LAB" apply -apply -vms 3 -cpus 2 -ram 4 -disk 20)
	rec "host free disk after apply" host_free_disk
}

# Guest preparation, both inside the lab VMs only:
# 1. Lima's default user-mode network gives every VM 192.168.5.15 and no
#    VM-to-VM path. Attach the documented Lima user-v2 network (user-mode,
#    VM-to-VM capable) to the lab VMs via `limactl edit`.
# 2. template:docker runs rootless Docker with dockerd/containerd system units
#    masked. Swarm ports and overlay networking would sit inside the
#    rootlesskit namespace, so the rootful daemon is enabled instead.
prepare_guests() {
	section "Step 1b: guest preparation (lab VMs only)"
	local vm
	for vm in $VMS; do
		rec "default network of $vm" vm_sh "$vm" 'ip -4 -o addr show eth0 | awk "{print \$2, \$4}"'
	done
	for vm in $VMS; do
		require_lab_vm "$vm"
		rec "stop $vm for network edit" limactl stop "$vm"
		rec "attach lima:user-v2 to $vm" limactl edit --tty=false --network=lima:user-v2 "$vm"
		rec "start $vm" limactl start --tty=false "$vm"
	done
	for vm in $VMS; do
		rec "switch $vm to rootful docker" vm_sh "$vm" '
			set -e
			systemctl --user disable --now docker.service >/dev/null 2>&1 || true
			sudo systemctl unmask containerd.service docker.service docker.socket >/dev/null 2>&1
			sudo systemctl enable --now containerd.service docker.socket docker.service >/dev/null 2>&1
			echo "rootless user docker: $(systemctl --user is-active docker.service || true)"
			echo "rootful docker: $(systemctl is-active docker.service)"'
	done
}

record_guest_facts() {
	section "Guest facts"
	rec "limactl list" limactl list
	local vm
	for vm in $VMS; do
		rec "facts $vm" vm_sh "$vm" '
			echo "hostname: $(hostname)"; echo "arch: $(uname -m) kernel: $(uname -r)"
			grep PRETTY_NAME /etc/os-release; echo "nproc: $(nproc)"; free -m | head -2
			df -h / | tail -1
			ip -4 -o addr show eth0 | awk "{print \"eth0:\", \$4}"
			sudo docker version --format "docker server {{.Server.Version}} {{range .Server.Components}}{{.Name}}={{.Version}} {{end}}"
			sudo docker info --format "security: {{.SecurityOptions}} cgroup: {{.CgroupDriver}} storage: {{.Driver}}"'
	done
	rec "VM-to-VM name resolution and ping" vm_sh "$M1" '
		for h in m1 m2 m3; do
			getent hosts lima-dbcm-lab-$h.internal
			ping -c2 -W2 lima-dbcm-lab-$h.internal | tail -1
		done'
}

reset_swarm() {
	section "Reset previous Swarm state (E1_REUSE_VMS)"
	local vm
	for vm in $VMS; do
		rec "leave swarm on $vm" vm_sh "$vm" 'sudo docker swarm leave --force 2>&1 || true; ids=$(sudo docker ps -aq -f label=com.docker.swarm.service.name); [ -z "$ids" ] || sudo docker rm -f $ids >/dev/null; sudo rm -rf /var/lib/dbcm'
	done
}

# ---------------------------------------------------------------- step 2
ADDR1=""
step2_swarm() {
	section "Step 2: Swarm (one manager, two workers)"
	ADDR1=$(vm_sh "$M1" 'ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1')
	say "m1 advertise address: $ADDR1"
	local vm
	for vm in dbcm-lab-m2 dbcm-lab-m3; do
		rec "TCP reachability $vm -> m1 ($ADDR1) port 22" vm_sh "$vm" "nc -z -w3 $ADDR1 22 && echo reachable"
	done
	rec "swarm init on m1" vm_docker "$M1" swarm init --advertise-addr "$ADDR1"
	local token
	token=$(vm_docker "$M1" swarm join-token -q worker)
	for vm in dbcm-lab-m2 dbcm-lab-m3; do
		local addr
		addr=$(vm_sh "$vm" 'ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1')
		# The join command line contains the token; it is not echoed.
		rec "swarm join $vm as worker (advertise $addr, token not recorded)" \
			vm_docker "$vm" swarm join --advertise-addr "$addr" --token "$token" "$ADDR1:2377"
	done
	token=""
	rec "docker node ls" vm_docker "$M1" node ls
}

node_id() { # node_id N -> Swarm node ID of dbcm-lab-mN
	vm_docker "$(vm_of "$1")" info --format '{{.Swarm.NodeID}}'
}

# ---------------------------------------------------------------- step 3
sentinel_snapshot() { # prints: task node container image started response-sha
	local task node cid image vm started sha
	task=$(vm_docker "$M1" service ps -q --filter desired-state=running "$SENTINEL" | head -1)
	node=$(vm_docker "$M1" inspect --format '{{.NodeID}}' "$task")
	cid=$(vm_docker "$M1" inspect --format '{{.Status.ContainerStatus.ContainerID}}' "$task")
	image=$(vm_docker "$M1" service inspect --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "$SENTINEL")
	vm=$(vm_docker "$M1" node inspect --format '{{.Description.Hostname}}' "$node" | sed 's/^lima-//')
	started=$(vm_docker "$vm" inspect --format '{{.State.StartedAt}} restarts={{.RestartCount}}' "$cid")
	sha=$(vm_sh "$M1" "curl -fsS http://127.0.0.1:$SENTINEL_PORT/ | sha256sum | cut -d' ' -f1")
	printf '%s %s %s %s %s %s\n' "$task" "$node" "$cid" "$image" "$started" "$sha"
}

step3_sentinel() {
	section "Step 3: unrelated sentinel service"
	rec "create sentinel" vm_docker "$M1" service create --detach --name "$SENTINEL" \
		--replicas 1 --label dbcm.test=sentinel \
		--publish "published=$SENTINEL_PORT,target=80" "$SENTINEL_IMAGE"
	wait_until 180 "sentinel answers on :$SENTINEL_PORT" \
		vm_sh "$M1" "curl -fsS http://127.0.0.1:$SENTINEL_PORT/ >/dev/null"
	rec "sentinel service ps" vm_docker "$M1" service ps --no-trunc "$SENTINEL"
	SENTINEL_BEFORE=$(sentinel_snapshot)
	printf '### sentinel snapshot before\n\n```\ntask node container image started restarts response-sha256\n%s\n```\n\n' \
		"$SENTINEL_BEFORE" >>"$LOG"
	say "sentinel before: $SENTINEL_BEFORE"
}

# ---------------------------------------------------------------- step 4
galera_args() { # galera_args BOOTSTRAP(0|1) -> mariadbd options
	local a="--wsrep_on=ON --wsrep_provider=/usr/lib/galera/libgalera_smm.so"
	a="$a --wsrep_cluster_name=dbcm-e1 --wsrep_cluster_address=gcomm://galera-1,galera-2,galera-3"
	a="$a --wsrep_sst_method=mariabackup --binlog_format=ROW --innodb_autoinc_lock_mode=2"
	# No --innodb_buffer_pool_size here: wsrep_sst_mariabackup passes the
	# mariadbd options on to mariadb-backup, which rejects it (observed).
	a="$a --wsrep_provider_options=gcache.size=128M"
	[ "$1" = 1 ] && a="$a --wsrep-new-cluster"
	echo "$a"
}

write_marker() { # write_marker N — one-shot container creates dirs and marker
	local n=$1 vm uuid
	vm=$(vm_of "$n")
	uuid=$(vm_sh "$vm" 'cat /proc/sys/kernel/random/uuid')
	local json
	json="{\"node_uuid\":\"$uuid\",\"installation_id\":\"$INSTALLATION\",\"cluster_id\":\"$CLUSTER\",\"node\":\"galera-$n\"}"
	vm_sh "$vm" "sudo mkdir -p /var/lib/dbcm/galera-$n"
	# Marker at the volume root (outside the datadir) and a second copy inside
	# the datadir, to observe which one survives SST.
	rec "one-shot marker container for galera-$n" vm_docker "$vm" run --rm \
		--label dbcm.installation="$INSTALLATION" --label dbcm.node="galera-$n" \
		-v "/var/lib/dbcm/galera-$n:/vol" "$ONESHOT_IMAGE" sh -c "
			set -e
			mkdir -p /vol/data
			printf '%s\n' '$json' >/vol/dbcm-identity.json
			cp /vol/dbcm-identity.json /vol/data/dbcm-identity.json
			chown 999:999 /vol/data /vol/data/dbcm-identity.json
			ls -la /vol /vol/data; cat /vol/dbcm-identity.json"
}

# SST: the donor's wsrep_sst_mariabackup connects as OS user mysql over the
# socket. The image creates 'mysql'@'localhost' (unix_socket, no password)
# only with MARIADB_MYSQL_LOCALHOST_USER=1; without it SST fails with
# "Access denied for user 'mysql'@'localhost'" (observed).
galera_create() { # galera_create N BOOTSTRAP
	local n=$1 boot=$2 nid
	nid=$(node_id "$n")
	# shellcheck disable=SC2046 # word splitting of the mariadbd options is intended
	rec "create service galera-$n pinned to node $nid (bootstrap=$boot)" \
		vm_docker "$M1" service create --detach --name "galera-$n" --replicas 1 \
		--constraint "node.id==$nid" --network "$NET" --endpoint-mode dnsrr \
		--hostname "galera-$n" --restart-condition any --stop-grace-period 60s \
		--label dbcm.installation="$INSTALLATION" --label dbcm.cluster="$CLUSTER" --label dbcm.node="galera-$n" \
		--secret "$SECRET" --env "MARIADB_ROOT_PASSWORD_FILE=/run/secrets/$SECRET" \
		--env MARIADB_MYSQL_LOCALHOST_USER=1 \
		--env "MARIADB_MYSQL_LOCALHOST_GRANTS=RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR" \
		--mount "type=bind,src=/var/lib/dbcm/galera-$n/data,dst=/var/lib/mysql" \
		--mount "type=bind,src=/var/lib/dbcm/galera-$n/dbcm-identity.json,dst=/etc/dbcm/identity.json,readonly" \
		"$MARIADB_IMAGE" $(galera_args "$boot")
}

galera_cid() { # galera_cid N -> running container ID of galera-N on its VM
	vm_docker "$(vm_of "$1")" ps -q --no-trunc -f "label=com.docker.swarm.service.name=galera-$1" -f status=running | head -1
}

sql() { # sql N SQL — run SQL on galera-N as root via the secret
	local cid
	cid=$(galera_cid "$1")
	[ -n "$cid" ] || return 1
	printf '%s\n' "$2" | limactl shell --workdir / "$(vm_of "$1")" -- sudo docker exec -i "$cid" \
		sh -c 'exec mariadb -uroot -p"$(cat /run/secrets/'"$SECRET"')" -N -B'
}

wsrep() { # wsrep N VAR -> value
	sql "$1" "SHOW GLOBAL STATUS LIKE '$2'" | awk '{print $2}'
}
synced_with_size() { # synced_with_size N SIZE
	[ "$(wsrep "$1" wsrep_local_state_comment)" = Synced ] && [ "$(wsrep "$1" wsrep_cluster_size)" = "$2" ]
}

step4_galera() {
	section "Step 4: pinned Galera services"
	local vm n
	for vm in $VMS; do
		rec "pull images on $vm" vm_docker "$vm" pull -q "$MARIADB_IMAGE"
		vm_docker "$vm" pull -q "$ONESHOT_IMAGE" >/dev/null
		vm_docker "$vm" pull -q "$SENTINEL_IMAGE" >/dev/null # only for the digest record
	done
	rec "MariaDB image digest and arch (m1)" vm_docker "$M1" image inspect \
		--format '{{.RepoDigests}} {{.Architecture}} version={{index .Config.Labels "org.opencontainers.image.version"}}' "$MARIADB_IMAGE"
	rec "busybox/nginx image digests (m1)" vm_docker "$M1" image inspect \
		--format '{{.RepoTags}} {{.RepoDigests}} {{.Architecture}}' "$ONESHOT_IMAGE" "$SENTINEL_IMAGE"
	rec "create overlay network $NET" vm_docker "$M1" network create --driver overlay --attachable \
		--label dbcm.installation="$INSTALLATION" --label dbcm.cluster="$CLUSTER" "$NET"
	rec "create synthetic root password secret" vm_sh "$M1" \
		"head -c 18 /dev/urandom | base64 | sudo docker secret create --label dbcm.installation=$INSTALLATION $SECRET -"
	for n in 1 2 3; do write_marker "$n"; done
	printf '### Galera options\n\n```\nbootstrap (galera-1 initially): %s\njoiners and galera-1 after formation: %s\n```\n\n' \
		"$(galera_args 1)" "$(galera_args 0)" >>"$LOG"

	galera_create 1 1
	wait_until 300 "galera-1 bootstrapped (Synced, size 1)" synced_with_size 1 1 ||
		{ rec "galera-1 logs" vm_sh "$M1" "sudo docker service logs --tail 60 galera-1"; return 1; }
	galera_create 2 0
	wait_until 300 "galera-2 joined (Synced, size 2)" synced_with_size 2 2 ||
		{ rec "galera-2 logs" vm_sh "$M1" "sudo docker service logs --tail 80 galera-2"; return 1; }
	galera_create 3 0
	wait_until 300 "galera-3 joined (Synced, size 3)" synced_with_size 3 3 ||
		{ rec "galera-3 logs" vm_sh "$M1" "sudo docker service logs --tail 80 galera-3"; return 1; }
	for n in 2 3; do
		rec "galera-$n join method in log" vm_sh "$(vm_of "$n")" \
			"sudo docker logs $(galera_cid "$n") 2>&1 | grep -iE 'SST|IST' | grep -iE 'request|complete|receiv|prepar|method' | head -12"
	done

	# Remove --wsrep-new-cluster from galera-1 so a later restart rejoins
	# instead of bootstrapping a second cluster.
	local old1
	old1=$(galera_cid 1)
	rec "update galera-1 to non-bootstrap args" vm_docker "$M1" service update --detach \
		--args "$(galera_args 0)" galera-1
	wait_until 420 "galera-1 rejoined (Synced, size 3)" galera1_new_and_synced "$old1"
	rec "galera-1 rejoin method in log" vm_sh "$M1" \
		"sudo docker logs $(galera_cid 1) 2>&1 | grep -iE 'SST|IST' | grep -iE 'request|complete|receiv|prepar|method' | head -12"
	rec "service list" vm_docker "$M1" service ls
	for n in 1 2 3; do
		rec "galera-$n tasks (placement)" vm_docker "$M1" service ps --no-trunc \
			--format '{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' "galera-$n"
	done
	rec "node ID map" vm_docker "$M1" node ls --format '{{.ID}} {{.Hostname}} {{.ManagerStatus}}'
}

galera1_new_and_synced() {
	local cid
	cid=$(galera_cid 1)
	[ -n "$cid" ] && [ "$cid" != "$1" ] && synced_with_size 1 3
}

# ---------------------------------------------------------------- step 5
step5_verify() {
	section "Step 5: cluster state and replication"
	local n
	for n in 1 2 3; do
		rec "wsrep status galera-$n" sql "$n" "SHOW GLOBAL STATUS WHERE Variable_name IN
			('wsrep_cluster_size','wsrep_cluster_status','wsrep_local_state_comment','wsrep_ready',
			 'wsrep_connected','wsrep_cluster_state_uuid','wsrep_incoming_addresses','wsrep_last_committed')"
	done
	rec "write on galera-1" sql 1 "CREATE DATABASE IF NOT EXISTS e1;
		CREATE TABLE IF NOT EXISTS e1.probe (id INT AUTO_INCREMENT PRIMARY KEY, note VARCHAR(64), at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO e1.probe (note) VALUES ('synthetic-row-from-galera-1');
		SELECT id, note FROM e1.probe;"
	for n in 2 3; do
		rec "read on galera-$n" sql "$n" "SELECT id, note, @@hostname FROM e1.probe"
	done
}

# ---------------------------------------------------------------- step 6
step6_kill() {
	section "Step 6: kill galera-2 container"
	local old new t0 t_restart t_synced
	old=$(galera_cid 2)
	rec "galera-2 container before kill" vm_docker dbcm-lab-m2 ps --no-trunc \
		--format '{{.ID}} {{.Names}} {{.Status}}' -f label=com.docker.swarm.service.name=galera-2
	t0=$(date +%s)
	rec "docker kill galera-2 container" vm_docker dbcm-lab-m2 kill "$old"
	# Write on galera-1 in the background so the restart polling below is not
	# delayed; its duration shows how long writes stall until the dead member
	# leaves the Galera view.
	local wfile
	wfile=$(mktemp "${TMPDIR:-/tmp}/dbcm-e1-write.XXXXXX")
	(
		ws=$(date +%s)
		r=$(sql 1 "INSERT INTO e1.probe (note) VALUES ('written-while-galera-2-down'); SELECT COUNT(*) FROM e1.probe" 2>&1) || r="FAILED: $r"
		printf 'rows after write: %s\nwrite started %ss after kill, took %ss\n' "$r" "$((ws - t0))" "$(($(date +%s) - ws))" >"$wfile"
	) &
	local wpid=$!
	local deadline=$((t0 + 180))
	while :; do
		new=$(galera_cid 2 || true)
		if [ -n "$new" ] && [ "$new" != "$old" ]; then break; fi
		[ "$(date +%s)" -lt "$deadline" ] || die "galera-2 was not restarted within 180s"
		sleep 1
	done
	t_restart=$(($(date +%s) - t0))
	say "galera-2 new container after ${t_restart}s"
	wait "$wpid" || true
	{
		printf '### write on galera-1 while galera-2 is down\n\n```\n'
		cat "$wfile"
		printf '```\n\n'
	} >>"$LOG"
	rm -f "$wfile"
	rec "cluster size on galera-1 while galera-2 rejoins" wsrep 1 wsrep_cluster_size
	deadline=$((t0 + 420))
	while ! synced_with_size 2 3 2>/dev/null; do
		[ "$(date +%s)" -lt "$deadline" ] || die "galera-2 not Synced with size 3 within 420s"
		sleep 1
	done
	t_synced=$(($(date +%s) - t0))
	say "galera-2 Synced, cluster size 3 after ${t_synced}s"
	printf '### recovery timing\n\n```\nold container: %s\nnew container: %s\nnew container running after: %ss\nSynced with cluster size 3 after: %ss\n```\n\n' \
		"$old" "$new" "$t_restart" "$t_synced" >>"$LOG"
	rec "galera-2 task history (same node?)" vm_docker "$M1" service ps --no-trunc \
		--format '{{.ID}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' galera-2
	rec "galera-2 rejoin log (IST/SST)" vm_sh dbcm-lab-m2 \
		"sudo docker logs $new 2>&1 | grep -E 'Found saved state|Group state|Local state|IST uuid|Prepared IST receiver|SST started|SST completed|IST completed|SST received|Receiving IST:|IST received|Synchronized with group'"
	rec "read on galera-2 after rejoin" sql 2 "SELECT id, note FROM e1.probe ORDER BY id"
	local n
	for n in 1 2 3; do
		rec "cluster size/status galera-$n after rejoin" sql "$n" \
			"SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_cluster_status','wsrep_local_state_comment')"
	done
	for n in 1 2 3; do
		rec "identity markers on $(vm_of "$n")" vm_sh "$(vm_of "$n")" \
			"sudo cat /var/lib/dbcm/galera-$n/dbcm-identity.json; echo '-- in datadir:'; sudo ls -la /var/lib/dbcm/galera-$n/data/dbcm-identity.json 2>&1; sudo cat /var/lib/dbcm/galera-$n/data/grastate.dat"
	done
}

# ---------------------------------------------------------------- step 7
step7_sentinel() {
	section "Step 7: sentinel comparison"
	local after
	after=$(sentinel_snapshot)
	printf '### sentinel snapshot after\n\n```\nbefore: %s\nafter:  %s\n```\n\n' "$SENTINEL_BEFORE" "$after" >>"$LOG"
	if [ "$after" = "$SENTINEL_BEFORE" ]; then
		say "sentinel unchanged (task, node, container, image, start time, response)"
		echo "sentinel: UNCHANGED" >>"$LOG"
	else
		say "sentinel CHANGED: before=$SENTINEL_BEFORE after=$after"
		echo "sentinel: CHANGED" >>"$LOG"
		return 1
	fi
	rec "unrelated entries before cleanup" lab_status_unrelated
}

if [ "${E1_REUSE_VMS:-0}" = 1 ]; then
	reset_swarm
else
	step1_vms
	prepare_guests
fi
record_guest_facts
step2_swarm
step3_sentinel
step4_galera
step5_verify
step6_kill
step7_sentinel
