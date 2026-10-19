#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E2: host loss, returning host, empty-volume trap and network
# partition for pinned single-replica Galera services on three dbcm-lab VMs
# (ADR 0004 items 2, 3 and 5). Reuses the E1 setup from common.sh.
#
# Usage (from anywhere):  tests/p0/e2-host-loss.sh
#
# Environment (all optional, explicit, default off):
#   E2_KEEP_VMS=1   do not run the scoped cleanup at the end (development only;
#                   run `bin/lab cleanup -apply` yourself afterwards)
#   E2_REUSE_VMS=1  skip `bin/lab apply` and guest preparation; expects the
#                   prepared VMs from a previous E1/E2 *_KEEP_VMS=1 run
#
# Disruptions are applied only to the lab VM dbcm-lab-m3:
#   - `limactl stop --force` / `limactl start` of dbcm-lab-m3
#   - iptables rules in a dedicated chain DBCM-E2 inside dbcm-lab-m3, removed
#     after the step (plus a 600 s systemd timer inside the VM as safety net)
#   - Docker disabled at boot inside dbcm-lab-m3 for step 4, re-enabled there
#
# Output: raw sanitized log at tests/p0/results/e2-host-loss.log.
# Requirements: bin/lab built (`make build`), limactl on PATH. All docker
# commands run inside the lab VMs; the host Docker daemon is never used.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG="$REPO_ROOT/tests/p0/results/e2-host-loss.log"
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
: >"$LOG"
# shellcheck source=tests/p0/common.sh
. "$REPO_ROOT/tests/p0/common.sh"

VMS="dbcm-lab-m1 dbcm-lab-m2 dbcm-lab-m3"
M1=dbcm-lab-m1
M2=dbcm-lab-m2
M3=dbcm-lab-m3
NET=dbcm-e2-galera
SECRET=dbcm-e2-root-pw
MARIADB_IMAGE=mariadb:11.4
SENTINEL_IMAGE=nginx:1.27-alpine
ONESHOT_IMAGE=busybox:1.37
SENTINEL=sentinel-web
SENTINEL_PORT=18080
INSTALLATION=e2-lab
CLUSTER=e2-galera
GALERA_NAME=dbcm-e2
# The sentinel must not sit on m3, the host this experiment disrupts; an
# unconstrained sentinel would be rescheduled by Swarm on host loss.
SENTINEL_CONSTRAINT="node.hostname==lima-dbcm-lab-m2"
VOL3=/var/lib/dbcm/galera-3
GUARD_VM=/var/lib/dbcm/guard/marker-guard.sh
CHAIN=DBCM-E2
CONCLUSIONS=()

finish() {
	local rc=$?
	trap - EXIT INT TERM
	{
		printf '\n## Conclusions recorded by the script\n\n'
		local c
		for c in "${CONCLUSIONS[@]+"${CONCLUSIONS[@]}"}"; do printf -- '- %s\n' "$c"; done
		printf '\n'
	} >>"$LOG"
	if [ "${E2_KEEP_VMS:-0}" = 1 ]; then
		say "E2_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		# Rules, disabled units and timers live inside dbcm-lab-m3 only and
		# disappear with the VM.
		lab_cleanup || rc=1
		rec "Lima user-v2 usernet daemons after cleanup (recorded, not touched)" \
			sh -c 'ps -axo pid,command | grep "[l]imactl usernet" || echo none' || true
		rec "shared Lima user-v2 state directory (recorded, not touched)" \
			sh -c 'ls -d "$HOME/.lima/_networks/user-v2" && ls "$HOME/.lima/_networks/user-v2" || echo absent' || true
	fi
	sanitize_log "$LOG"
	say "E2 finished with exit $rc (log: tests/p0/results/e2-host-loss.log)"
	exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

conclude() { # conclude TEXT — record a result line (log and summary)
	CONCLUSIONS+=("$*")
	printf '**Result:** %s\n\n' "$*" >>"$LOG"
	say "RESULT: $*"
}

since() { # since T0 LABEL — record elapsed seconds since T0
	local s=$(($(date +%s) - $1))
	printf '%s: %ss\n' "$2" "$s" >>"$LOG"
	say "$2: ${s}s"
}

# ---------------------------------------------------------------- helpers
NID3=""
node3_state_is() { [ "$(vm_docker "$M1" node inspect --format '{{.Status.State}}' "$NID3")" = "$1" ]; }
size_is() { [ "$(wsrep "$1" wsrep_cluster_size)" = "$2" ]; }
status_not_primary() { local s; s=$(wsrep "$1" wsrep_cluster_status) && [ -n "$s" ] && [ "$s" != Primary ]; }
no_galera3_container() { [ -z "$(galera_cid 3)" ]; }

tasks3() {
	vm_docker "$M1" service ps --no-trunc \
		--format '{{.ID}} node={{.Node}} desired={{.DesiredState}} current="{{.CurrentState}}" err="{{.Error}}"' galera-3
}

# galera-3 must never run anywhere but m3.
assert_not_elsewhere() {
	local elsewhere
	elsewhere=$(vm_docker "$M1" service ps --format '{{.Node}} {{.CurrentState}}' galera-3 |
		grep -E '^[^ ]+ Running' | grep -v 'lima-dbcm-lab-m3' || true)
	if [ -z "$elsewhere" ]; then
		conclude "$1: no galera-3 task runs on m1/m2 (pinned service not rescheduled elsewhere)"
	else
		conclude "$1: galera-3 task RUNNING ELSEWHERE: $elsewhere"
	fi
}

wsrep_status() { # wsrep_status N
	sql "$1" "SHOW GLOBAL STATUS WHERE Variable_name IN
		('wsrep_cluster_size','wsrep_cluster_status','wsrep_local_state_comment','wsrep_ready',
		 'wsrep_cluster_state_uuid','wsrep_last_committed')"
}

# consistency LABEL N... — row count, content hash and CHECKSUM TABLE on the
# given nodes; records CONSISTENT/INCONSISTENT.
consistency() {
	local label=$1 n out first="" ok=1 nodes
	shift
	nodes=$*
	nodes=${nodes// /,}
	{
		printf '### data consistency: %s\n\n```\n' "$label"
	} >>"$LOG"
	for n in "$@"; do
		out=$(sql "$n" "SELECT COUNT(*), COALESCE(SUM(CRC32(CONCAT_WS('#',id,note,v))),0) FROM e2.probe;
			CHECKSUM TABLE e2.probe EXTENDED" | tr '\t\n' '  ' | sed 's/ *$//') || out="QUERY FAILED"
		printf 'galera-%s: rows/crc-sum/checksum = %s\n' "$n" "$out" >>"$LOG"
		[ -n "$first" ] || first=$out
		[ "$out" = "$first" ] && [ "$out" != "QUERY FAILED" ] || ok=0
	done
	printf '```\n\n' >>"$LOG"
	if [ "$ok" = 1 ]; then
		conclude "$label: data CONSISTENT on galera-$nodes ($first)"
	else
		conclude "$label: data INCONSISTENT across galera-$nodes"
	fi
}

join_log3() { # join_log3 CID [SINCE] — IST/SST lines of a galera-3 container
	vm_sh "$M3" "sudo docker logs ${2:+--since $2} $1 2>&1 | grep -E 'Found saved state|Local state|Group state|IST uuid|Prepared IST receiver|SST started|SST completed|IST completed|SST received|Receiving IST:|IST received|Synchronized with group|Initializing database|Database files initialized|non-Primary|Primary|marker-guard' | tail -25"
}

marker_field() { # marker_field KEY JSON
	printf '%s' "$2" | sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"
}

# start_m3 TITLE — limactl start for m3. Lima's docker template re-runs its
# rootless-Docker user provisioning and probe on every boot; a failing probe
# makes `limactl start` exit non-zero although the VM runs. Record and go on
# when the VM is Running and reachable.
start_m3() {
	local rc=0
	rec "$1" limactl start --tty=false --timeout 6m "$M3" || rc=$?
	if [ "$rc" != 0 ]; then
		rec "m3 state after limactl start exit $rc" sh -c "limactl list --format '{{.Name}} {{.Status}}' | grep '^$M3 '"
		vm_sh "$M3" true || die "$M3 not reachable after limactl start"
		conclude "limactl start of $M3 exited $rc, but the VM is Running and reachable (see log)"
	fi
	rec "Docker daemons on m3 after boot" vm_sh "$M3" \
		'echo "rootful: $(systemctl is-active docker.service)"; echo "rootless user: $(systemctl --user is-active docker.service)"' || true
}

# Files written inside m3 must be synced before a forced stop: the first
# development run lost the installed guard and a `systemctl disable` to the
# power-cut (`limactl stop --force`), see the results document.
install_guard() {
	rec "install marker-guard.sh into $M3 at $GUARD_VM" \
		limactl shell --workdir / "$M3" -- sudo sh -c \
		"mkdir -p $(dirname "$GUARD_VM") && cat >$GUARD_VM && chmod 755 $GUARD_VM && sync && sha256sum $GUARD_VM" \
		<"$REPO_ROOT/tests/p0/marker-guard.sh"
}

guard_check_host_side() { # guard_check_host_side EXPECTED_UUID
	vm_sh "$M3" "sudo env DBCM_MARKER=$VOL3/dbcm-identity.json DBCM_DATADIR=$VOL3/data \
		DBCM_EXPECTED_NODE_UUID=$1 DBCM_EXPECTED_INSTALLATION=$INSTALLATION DBCM_EXPECTED_CLUSTER=$CLUSTER \
		sh $GUARD_VM"
}

reset_lab_extras() {
	section "Reset E2 leftovers (E2_REUSE_VMS)"
	rec "reset m3 extras" vm_sh "$M3" 'sudo iptables -D INPUT -j DBCM-E2 2>/dev/null; sudo iptables -D OUTPUT -j DBCM-E2 2>/dev/null; sudo iptables -F DBCM-E2 2>/dev/null; sudo iptables -X DBCM-E2 2>/dev/null; sudo systemctl stop dbcm-e2-heal.timer 2>/dev/null; sudo systemctl enable --now docker.socket docker.service >/dev/null 2>&1; echo ok'
}

# ---------------------------------------------------------------- step 1
seed_data() {
	section "E2 Step 1 (end): synthetic data and identity"
	rec "create and fill e2.probe on galera-1 (200 synthetic rows)" sql 1 "CREATE DATABASE IF NOT EXISTS e2;
		CREATE TABLE IF NOT EXISTS e2.probe (id INT AUTO_INCREMENT PRIMARY KEY, note VARCHAR(64), v INT);
		INSERT INTO e2.probe (note, v) WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < 200)
			SELECT CONCAT('synthetic-', i), i * 7 FROM s;
		SELECT COUNT(*) FROM e2.probe;"
	local n
	for n in 1 2 3; do rec "wsrep status galera-$n" wsrep_status "$n"; done
	consistency "after setup" 1 2 3
	NID3=$(node_id 3)
	MARKER3=$(vm_sh "$M3" "sudo cat $VOL3/dbcm-identity.json")
	UUID3=$(marker_field node_uuid "$MARKER3")
	printf '### galera-3 identity marker at setup\n\n```\n%s\n```\n\n' "$MARKER3" >>"$LOG"
	say "m3 node id $NID3, galera-3 node_uuid $UUID3"
}

# ---------------------------------------------------------------- step 2
step2_host_loss() {
	section "Step 2: host loss (limactl stop --force $M3)"
	rec "galera-3 tasks before" tasks3
	rec "node ls before" vm_docker "$M1" node ls
	local t0
	t0=$(date +%s)
	rec "limactl stop --force $M3" limactl stop --force "$M3"
	since "$t0" "VM stop command returned after"
	wait_until 180 "Swarm reports m3 down" node3_state_is down
	since "$t0" "Swarm node state down after"
	wait_until 180 "galera-1 sees cluster size 2" size_is 1 2
	since "$t0" "Galera cluster size 2 on galera-1 after"
	rec "node ls with m3 lost" vm_docker "$M1" node ls
	local n
	for n in 1 2; do rec "wsrep status galera-$n (m3 lost)" wsrep_status "$n"; done
	rec "galera-3 tasks right after node down" tasks3
	say "observing the scheduler for 60 s"
	sleep 60
	rec "galera-3 tasks 60 s later" tasks3
	rec "galera-3 service ps (default view)" vm_docker "$M1" service ps galera-3
	assert_not_elsewhere "host loss"
	rec "write on galera-1 during host loss" sql 1 \
		"INSERT INTO e2.probe (note, v) VALUES ('written-while-m3-lost', 1), ('written-while-m3-lost', 2); SELECT COUNT(*) FROM e2.probe"
	rec "write on galera-2 during host loss" sql 2 \
		"INSERT INTO e2.probe (note, v) VALUES ('written-on-galera-2-while-m3-lost', 3); SELECT COUNT(*) FROM e2.probe"
	consistency "majority during host loss" 1 2
	if size_is 1 2 && [ "$(wsrep 1 wsrep_cluster_status)" = Primary ]; then
		conclude "host loss: m3 Down in Swarm, Galera majority Primary with size 2, writes on galera-1 and galera-2 succeeded"
	else
		conclude "host loss: majority NOT Primary/size 2"
	fi
}

# ---------------------------------------------------------------- step 3
step3_return() {
	section "Step 3: returning host (limactl start $M3)"
	local t0 cid
	t0=$(date +%s)
	start_m3 "limactl start $M3"
	since "$t0" "VM start command returned after"
	wait_until 180 "Swarm reports m3 ready" node3_state_is ready
	since "$t0" "Swarm node state ready after"
	rec "m3 address after restart" vm_sh "$M3" 'ip -4 -o addr show eth0 | awk "{print \$4}"'
	wait_until 600 "galera-3 Synced with cluster size 3" synced_with_size 3 3 || {
		rec "galera-3 tasks (rejoin failed)" tasks3 || true
		rec "galera-3 service logs" vm_sh "$M1" "sudo docker service logs --tail 80 galera-3" || true
		return 1
	}
	since "$t0" "galera-3 Synced, cluster size 3 after VM start"
	rec "galera-3 tasks after return" tasks3
	cid=$(galera_cid 3)
	rec "galera-3 rejoin log (IST/SST)" join_log3 "$cid"
	rec "grastate on m3" vm_sh "$M3" "sudo cat $VOL3/data/grastate.dat"
	local after
	after=$(vm_sh "$M3" "sudo cat $VOL3/dbcm-identity.json")
	printf '### identity marker after return\n\n```\nbefore: %s\nafter:  %s\n```\n\n' "$MARKER3" "$after" >>"$LOG"
	if [ "$after" = "$MARKER3" ]; then
		conclude "returning host: identity marker at the volume root unchanged (node_uuid $UUID3)"
	else
		conclude "returning host: identity marker CHANGED"
	fi
	install_guard
	if rec "marker-guard check (host side in m3, expected node_uuid from setup)" guard_check_host_side "$UUID3"; then
		conclude "returning host: marker-guard check ALLOW (marker matches expected identity)"
	else
		conclude "returning host: marker-guard check REFUSED"
	fi
	local n
	for n in 1 2 3; do rec "wsrep status galera-$n after return" wsrep_status "$n"; done
	consistency "after host return" 1 2 3
	assert_not_elsewhere "returning host"
}

# ---------------------------------------------------------------- step 4
guard_selftest() {
	vm_sh "$M3" '
		G='"$GUARD_VM"'; T=$(mktemp -d); mkdir -p $T/data
		export DBCM_MARKER=$T/marker.json DBCM_DATADIR=$T/data DBCM_EXPECTED_NODE_UUID=u-1 DBCM_EXPECTED_INSTALLATION=i-1 DBCM_EXPECTED_CLUSTER=c-1
		run() { printf "%-46s" "$1:"; sh $G true 2>/dev/null; echo "exit $?"; }
		echo "{\"node_uuid\":\"u-1\",\"installation_id\":\"i-1\",\"cluster_id\":\"c-1\",\"node\":\"galera-3\"}" >$T/marker.json
		run "matching marker, empty datadir"
		touch $T/data/ibdata1; run "matching marker, non-empty datadir"
		echo "{\"node_uuid\":\"u-2\",\"installation_id\":\"i-1\",\"cluster_id\":\"c-1\",\"node\":\"galera-3\"}" >$T/marker.json
		run "foreign node_uuid"
		echo "{\"node_uuid\":\"u-1\",\"installation_id\":\"i-9\",\"cluster_id\":\"c-1\",\"node\":\"galera-3\"}" >$T/marker.json
		run "foreign installation"
		rm $T/marker.json; run "marker missing, non-empty datadir"
		rm $T/data/ibdata1; run "marker missing, empty datadir"
		(DBCM_EXPECTED_NODE_UUID=; run "expected identity not configured")
		rm -rf $T'
}

step4_empty_volume() {
	section "Step 4: empty volume trap"
	rec "disable Docker at boot inside $M3 (lab VM only)" vm_sh "$M3" \
		'sudo systemctl disable docker.service docker.socket 2>&1; sudo sync; systemctl is-enabled docker.service docker.socket || true'
	rec "limactl stop --force $M3" limactl stop --force "$M3"
	wait_until 180 "Swarm reports m3 down" node3_state_is down
	wait_until 180 "galera-1 sees cluster size 2" size_is 1 2
	start_m3 "limactl start $M3 (rootful Docker disabled at boot)"
	rec "Docker state on m3 after boot" vm_sh "$M3" 'systemctl is-active docker.service docker.socket || true'
	vm_sh "$M3" 'systemctl is-active --quiet docker.service' && die "rootful Docker is running on m3 although disabled; refusing to wipe the volume under a running service"
	rec "guard script present on m3 after the forced stop" vm_sh "$M3" "sudo sha256sum $GUARD_VM"
	rec "wipe galera-3 datadir contents and remove the marker (volume lost/replaced)" vm_sh "$M3" "
		sudo find $VOL3/data -mindepth 1 -delete
		sudo rm -f $VOL3/dbcm-identity.json
		sudo ls -la $VOL3 $VOL3/data"
	rec "marker-guard decision matrix (self-test inside m3, synthetic temp dirs)" guard_selftest
	rec "marker-guard on the real wiped volume (host side)" guard_check_host_side "$UUID3" || true

	# 4a: the E1 service spec unchanged.
	local t0
	t0=$(date +%s)
	rec "re-enable and start Docker inside $M3" vm_sh "$M3" \
		'sudo systemctl enable --now docker.socket docker.service >/dev/null 2>&1; systemctl is-active docker.service'
	wait_until 180 "Swarm reports m3 ready" node3_state_is ready
	since "$t0" "m3 ready after Docker start"
	say "observing the E1 spec (marker file bind mount) for 45 s"
	sleep 45
	rec "4a galera-3 tasks with the E1 spec (marker file bind-mounted)" tasks3
	rec "4a cluster size on galera-1" wsrep 1 wsrep_cluster_size
	rec "4a datadir on m3" vm_sh "$M3" "sudo ls -A $VOL3/data | head; echo entries: \$(sudo ls -A $VOL3/data | wc -l)"
	if vm_docker "$M1" service ps --no-trunc --format '{{.Error}}' galera-3 | grep -q 'bind source path does not exist'; then
		conclude "empty volume 4a: E1 spec task rejected only because the marker FILE bind source is missing (incidental mount failure, Swarm retries; not an identity check)"
	else
		conclude "empty volume 4a: no bind-mount rejection observed with the E1 spec"
	fi

	# 4b: plain service without the marker file mount.
	t0=$(date +%s)
	rec "4b remove the marker file mount (plain Swarm service, no guard)" \
		vm_docker "$M1" service update --detach --mount-rm /etc/dbcm/identity.json galera-3
	if wait_until 600 "4b galera-3 Synced with cluster size 3" synced_with_size 3 3; then
		since "$t0" "4b plain service: galera-3 Synced, size 3 after"
		conclude "empty volume 4b: plain Swarm service STARTED on the empty volume, initialized a new datadir and joined the cluster as galera-3; nothing checked identity"
	else
		conclude "empty volume 4b: plain service did not reach Synced within 600 s"
	fi
	rec "4b galera-3 tasks" tasks3
	rec "4b galera-3 log (init + SST)" join_log3 "$(galera_cid 3)"
	rec "4b volume on m3 (marker still absent)" vm_sh "$M3" "sudo ls -la $VOL3; sudo cat $VOL3/data/grastate.dat"
	consistency "after 4b unguarded rejoin" 1 2 3

	# 4c: guarded service.
	rec "4c scale galera-3 to 0" vm_docker "$M1" service update --detach --replicas 0 galera-3
	wait_until 180 "galera-3 container gone" no_galera3_container
	wait_until 120 "galera-1 sees cluster size 2" size_is 1 2
	rec "4c wipe galera-3 datadir again (marker still absent)" vm_sh "$M3" "
		sudo find $VOL3/data -mindepth 1 -delete; sudo ls -la $VOL3 $VOL3/data"
	t0=$(date +%s)
	rec "4c add the pre-start guard as entrypoint wrapper, replicas 1" vm_docker "$M1" service update --detach \
		--replicas 1 \
		--mount-add "type=bind,src=$VOL3,dst=/dbcm,readonly" \
		--mount-add "type=bind,src=$GUARD_VM,dst=/dbcm-guard/marker-guard.sh,readonly" \
		--entrypoint "/dbcm-guard/marker-guard.sh docker-entrypoint.sh" \
		--env-add "DBCM_EXPECTED_NODE_UUID=$UUID3" \
		--env-add "DBCM_EXPECTED_INSTALLATION=$INSTALLATION" \
		--env-add "DBCM_EXPECTED_CLUSTER=$CLUSTER" \
		galera-3
	rec "4c service spec (entrypoint, args head, mounts)" vm_docker "$M1" service inspect --format \
		'command={{json .Spec.TaskTemplate.ContainerSpec.Command}} mounts={{range .Spec.TaskTemplate.ContainerSpec.Mounts}}{{.Source}}->{{.Target}} {{end}}' galera-3
	say "observing the guarded service for 45 s"
	sleep 45
	rec "4c galera-3 tasks (guarded)" tasks3
	rec "4c guard output" vm_sh "$M1" "sudo docker service logs --raw galera-3 2>&1 | grep marker-guard | tail -6"
	rec "4c datadir on m3 (must stay empty)" vm_sh "$M3" "echo entries: \$(sudo ls -A $VOL3/data | wc -l)"
	rec "4c cluster size on galera-1" wsrep 1 wsrep_cluster_size
	local failed entries
	failed=$(vm_docker "$M1" service ps --no-trunc --format '{{.Error}}' galera-3 | grep -c 'exit (67)' || true)
	entries=$(vm_sh "$M3" "sudo ls -A $VOL3/data | wc -l" | tr -d ' ')
	since "$t0" "4c observation window"
	if [ "$failed" -ge 1 ] && [ "$entries" = 0 ] && size_is 1 2; then
		conclude "empty volume 4c: guarded service REFUSED to start (exit 67, $failed failed tasks in the window, Swarm keeps restarting it), datadir stayed empty, cluster stayed at size 2"
	else
		conclude "empty volume 4c: guard outcome unexpected (exit-67 tasks=$failed, datadir entries=$entries)"
	fi

	# 4d: explicit manager "new identity" operation.
	t0=$(date +%s)
	write_marker 3
	local marker_new uuid_new
	marker_new=$(vm_sh "$M3" "sudo cat $VOL3/dbcm-identity.json")
	uuid_new=$(marker_field node_uuid "$marker_new")
	printf '### 4d new identity marker\n\n```\n%s\n```\n\n' "$marker_new" >>"$LOG"
	rec "4d set expected node_uuid to the new identity" vm_docker "$M1" service update --detach \
		--env-add "DBCM_EXPECTED_NODE_UUID=$uuid_new" galera-3
	if wait_until 600 "4d galera-3 Synced with cluster size 3" synced_with_size 3 3; then
		since "$t0" "4d new identity: galera-3 Synced, size 3 after"
		conclude "empty volume 4d: after an explicit new-identity step (new marker node_uuid $uuid_new, expected UUID updated) the guard allowed the start and galera-3 joined"
	else
		conclude "empty volume 4d: new-identity start did not reach Synced"
	fi
	rec "4d galera-3 log (guard decision, init, SST)" join_log3 "$(galera_cid 3)"
	rec "4d galera-3 tasks" tasks3
	consistency "after 4d new identity" 1 2 3
	assert_not_elsewhere "empty volume"
	MARKER3=$marker_new
	UUID3=$uuid_new
}

# ---------------------------------------------------------------- step 5
step5_partition() {
	section "Step 5: partition m3 from m1/m2 (iptables inside $M3)"
	local a1 a2 a3 ssh_src
	a1=$(vm_sh "$M1" 'ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1')
	a2=$(vm_sh "$M2" 'ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1')
	a3=$(vm_sh "$M3" 'ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1')
	ssh_src=$(vm_sh "$M3" 'echo "$SSH_CONNECTION"' | awk '{print $1}')
	printf '### addresses\n\n```\nm1=%s m2=%s m3=%s\nlimactl shell SSH source as seen by m3: %s\n```\n\n' "$a1" "$a2" "$a3" "$ssh_src" >>"$LOG"
	case "$ssh_src" in
	"$a1" | "$a2") die "limactl shell reaches m3 from a peer address ($ssh_src); refusing to partition" ;;
	esac
	rec "write heal script and arm 600 s safety timer inside $M3" vm_sh "$M3" "
		printf '%s\n' '#!/bin/sh' \
			'iptables -D INPUT -j $CHAIN 2>/dev/null' 'iptables -D OUTPUT -j $CHAIN 2>/dev/null' \
			'iptables -F $CHAIN 2>/dev/null' 'iptables -X $CHAIN 2>/dev/null' 'exit 0' | sudo tee /run/dbcm-e2-heal.sh >/dev/null
		sudo systemd-run --unit dbcm-e2-heal --on-active=600 /bin/sh /run/dbcm-e2-heal.sh 2>&1
		systemctl list-timers dbcm-e2-heal.timer --no-pager | head -2"
	local cid_before t0
	cid_before=$(galera_cid 3)
	t0=$(date +%s)
	rec "apply partition rules (chain $CHAIN, drop m1/m2 both directions)" vm_sh "$M3" "
		set -e
		sudo iptables -N $CHAIN
		for a in $a1 $a2; do sudo iptables -A $CHAIN -s \$a -j DROP; sudo iptables -A $CHAIN -d \$a -j DROP; done
		sudo iptables -I INPUT 1 -j $CHAIN
		sudo iptables -I OUTPUT 1 -j $CHAIN
		sudo iptables -S | grep $CHAIN"
	rec "ping m3 -> m1 during partition (expect loss)" vm_sh "$M3" "ping -c2 -W1 $a1 | tail -2" || true
	if wait_until 120 "galera-3 leaves Primary" status_not_primary 3; then
		since "$t0" "galera-3 non-Primary after"
	fi
	wait_until 120 "galera-1 sees cluster size 2" size_is 1 2 && since "$t0" "majority size 2 after"
	rec "wsrep status galera-3 (isolated)" wsrep_status 3 || true
	rec "write attempt on isolated galera-3 (expected to fail)" sql 3 \
		"INSERT INTO e2.probe (note, v) VALUES ('must-not-commit-on-isolated-node', 0)" && w3=ok || w3=failed
	rec "read attempt on isolated galera-3" sql 3 "SELECT COUNT(*) FROM e2.probe" || true
	rec "write on galera-1 during partition" sql 1 \
		"INSERT INTO e2.probe (note, v) VALUES ('written-during-partition', 4), ('written-during-partition', 5); SELECT COUNT(*) FROM e2.probe"
	rec "read on galera-2 during partition" sql 2 "SELECT COUNT(*) FROM e2.probe"
	for n in 1 2; do rec "wsrep status galera-$n (majority)" wsrep_status "$n"; done
	local st3
	st3=$(wsrep 3 wsrep_cluster_status || echo unknown)
	if [ "$w3" = failed ] && [ "$st3" != Primary ] && size_is 1 2; then
		conclude "partition: isolated galera-3 is $st3 and refused the write; majority galera-1/2 Primary with size 2 kept accepting writes"
	else
		conclude "partition: unexpected (galera-3 status=$st3, isolated write=$w3)"
	fi
	if wait_until 120 "Swarm reports m3 down" node3_state_is down; then
		since "$t0" "Swarm node state down after"
	fi
	rec "node ls during partition" vm_docker "$M1" node ls
	rec "galera-3 tasks during partition (manager view)" tasks3
	rec "galera-3 container on m3 during partition (local view)" vm_docker "$M3" ps \
		--format '{{.ID}} {{.Status}}' -f label=com.docker.swarm.service.name=galera-3
	say "holding the partition 30 s more"
	sleep 30
	local th
	th=$(date +%s)
	rec "heal: remove rules, stop safety timer" vm_sh "$M3" "
		sudo sh /run/dbcm-e2-heal.sh; sudo systemctl stop dbcm-e2-heal.timer 2>/dev/null || true
		echo remaining $CHAIN rules: \$(sudo iptables -S | grep -c $CHAIN || true)
		echo timer: \$(systemctl is-active dbcm-e2-heal.timer || true)"
	wait_until 180 "Swarm reports m3 ready" node3_state_is ready && since "$th" "m3 ready after heal"
	wait_until 600 "galera-3 Synced with cluster size 3" synced_with_size 3 3 || {
		rec "galera-3 service logs" vm_sh "$M1" "sudo docker service logs --tail 80 galera-3" || true
		return 1
	}
	since "$th" "galera-3 Synced, size 3 after heal"
	sleep 10 # let Swarm settle on possibly replaced tasks
	wait_until 300 "galera-3 Synced with cluster size 3 (settled)" synced_with_size 3 3
	local cid_after
	cid_after=$(galera_cid 3)
	rec "galera-3 tasks after heal" tasks3
	printf '### galera-3 container across the partition\n\n```\nbefore: %s\nafter:  %s\n```\n\n' "$cid_before" "$cid_after" >>"$LOG"
	rec "galera-3 rejoin log since partition" join_log3 "$cid_after" "$(($(date +%s) - t0 + 5))s"
	local n
	for n in 1 2 3; do rec "wsrep status galera-$n after heal" wsrep_status "$n"; done
	rec "no isolated write committed" sql 1 "SELECT COUNT(*) AS isolated_rows FROM e2.probe WHERE note='must-not-commit-on-isolated-node'"
	consistency "after partition heal" 1 2 3
	if [ "$cid_after" = "$cid_before" ]; then
		conclude "partition heal: galera-3 rejoined in the SAME container (Swarm did not replace the task)"
	else
		conclude "partition heal: Swarm REPLACED the galera-3 container after the partition (new task on m3)"
	fi
	assert_not_elsewhere "partition"
}

# ---------------------------------------------------------------- step 6
step6_sentinel() {
	section "Step 6: sentinel comparison and unrelated resources"
	local after
	after=$(sentinel_snapshot)
	printf '### sentinel snapshot after\n\n```\nbefore: %s\nafter:  %s\n```\n\n' "$SENTINEL_BEFORE" "$after" >>"$LOG"
	if [ "$after" = "$SENTINEL_BEFORE" ]; then
		conclude "sentinel UNCHANGED (task, node, container, image, start time, restarts, response hash)"
	else
		conclude "sentinel CHANGED"
		return 1
	fi
	rec "unrelated entries before cleanup" lab_status_unrelated
}

section "E2 setup: E1 steps 1-4 via common.sh (section numbers below are E1's)"
rec "limactl version" limactl --version
if [ "${E2_REUSE_VMS:-0}" = 1 ]; then
	reset_lab_extras
	reset_swarm
else
	step1_vms
	prepare_guests
fi
record_guest_facts
step2_swarm
step3_sentinel
step4_galera
seed_data
step2_host_loss
step3_return
step4_empty_volume
step5_partition
step6_sentinel
