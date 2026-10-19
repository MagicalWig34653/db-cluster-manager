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

# Lab setup steps 1-4 (VMs, guest preparation, Swarm, sentinel, pinned Galera
# services) live in common.sh and are shared with E2.

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
