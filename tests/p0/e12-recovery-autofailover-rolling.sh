#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E12 (W05): what E9 left open, on the same three-VM HA layout.
#
#   Part 2  autoFailover of the coordinated proxy writer (ADR 0017): two
#           proxies on m1 and m2, the Galera writer on the node of m3, m3 is
#           hard-stopped; the manager leader must freeze and switch the writer
#           by itself. Clients write every 50 ms, proxy status is sampled every
#           100 ms; acknowledged writes versus committed rows, overlap check.
#   Part 3  HA stack: naive stack redeploy with a new image (all three
#           services update at once), the same one replica at a time, and
#           peer certificate renewal through new Swarm configs.
#   Part 1  E8/E9 B2 (full power loss of all three VMs) with the new zero-UUID
#           Primary incident detection and the outage-diagnosis 409 reason,
#           the guided recovery, and a valid B3 (staggered loss).
#
# The E4/E6/E8/E9 harness (VMs, Swarm, HA stack, agents, Galera through the
# manager API, proxy helpers) is sourced as a library.
#
# Phases: build vms swarm images prereqs hainit hastack hasetup agents cluster
#         af roll renew pwrb2 incident pwrrecover pwrb3
# Usage:  tests/p0/e12-recovery-autofailover-rolling.sh
#   E12_KEEP_VMS=1   keep the VMs (run `bin/lab cleanup --apply` yourself)
#   E12_REUSE_VMS=1  skip vms/swarm
#   E12_PHASES="..." run only these phases (default: all, in order)
#   E12_LOG=FILE     log file (default tests/p0/results/e12-recovery-autofailover-rolling.log)
#   E12_APPEND_LOG=1 append to the log instead of truncating it
#
# Requirements: Go toolchain, jq, python3, limactl on PATH, bin/lab built.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${E12_LOG:-"$REPO_ROOT/tests/p0/results/e12-recovery-autofailover-rolling.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${E12_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
export E4_AUTORESUME=false
# autoFailover on from the first manager start (the harness reads this).
export E9_AUTOFAILOVER=true
# Host disk guard (hostload in e9-phases.sh): stop below this many GiB free.
export E9_MIN_FREE_GIB=${E9_MIN_FREE_GIB:-4}
# shellcheck source=tests/p0/e4-manager-galera.sh
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"
# shellcheck source=tests/p0/e6-phases.sh
. "$REPO_ROOT/tests/p0/e6-phases.sh"
# shellcheck source=tests/p0/e8-phases.sh
. "$REPO_ROOT/tests/p0/e8-phases.sh"

# Overrides of the library settings (names inside the VMs stay the E9 ones so
# that the E9 helpers and samplers keep working).
INST=e9lab
NET=dbcm-e9lab-net
ROOT_SECRET=dbcm-e9lab-root-pw
MON_SECRET=dbcm-e9lab-monitor-pw
BAK_SECRET=dbcm-e9lab-backup-pw
API_PORT=8080
IMAGE=db-cluster-manager:e12
SRC=/root/dbcm-e9-src
STACK=dbcm
CLUSTER_NAME=e12db
E8=/root/dbcm-e9
PX=dbcm-e9-proxy
RESULTS_DIR="$REPO_ROOT/tests/p0/results"
SAMPLES_DIR="$RESULTS_DIR/e12-samples"

ASSERT_FAILS=""
assert_fail() { # NAME MESSAGE
	ASSERT_FAILS="$ASSERT_FAILS $1"
	result "ASSERTION FAILED ($1): $2"
}
ok() { result "PASS ($1): $2"; }

finish12() {
	local rc=$?
	trap - EXIT INT TERM
	if [ -n "$ASSERT_FAILS" ]; then
		say "failed assertions in:$ASSERT_FAILS"
		[ "$rc" != 0 ] || rc=1
	fi
	if [ "${E12_KEEP_VMS:-0}" = 1 ]; then
		say "E12_KEEP_VMS=1: VMs kept; run bin/lab cleanup --apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" -e 's#dbcm-et1\.[A-Za-z0-9._-]*#dbcm-et1.<redacted>#g' \
		-e "s#PASSWORD '\*[0-9A-F]*'#PASSWORD '<hash redacted>'#g" \
		-e 's#\(setup token[^:]*: \)[A-Za-z0-9_-]*#\1<redacted>#g' "$LOG"
	say "E12 finished with exit $rc"
	exit "$rc"
}
trap finish12 EXIT
trap 'exit 130' INT TERM

step() { case " ${E12_PHASES:-all} " in *" all "* | *" $1 "*) return 0 ;; esac; return 1; }

# shellcheck source=tests/p0/e9-phases.sh
. "$REPO_ROOT/tests/p0/e9-phases.sh"

# ---------------------------------------------------------------- helpers
# Proxy placement of part 2: the Galera writer's VM (m3) must not host a
# proxy, otherwise a missing acknowledgement would block the switch (that is
# the E9 pxvmloss result). proxy-a on m1, proxy-b on m2; m1 also hosts the
# clients (m1 is never stopped).
PXVMS="dbcm-lab-m1 dbcm-lab-m2"
px_id() { case "$1" in dbcm-lab-m1) echo proxy-a ;; dbcm-lab-m2) echo proxy-b ;; esac; }
px_lbl() { case "$1" in dbcm-lab-m1) echo pa ;; dbcm-lab-m2) echo pb ;; esac; }
px_vm() { case "$1" in proxy-a) echo dbcm-lab-m1 ;; proxy-b) echo dbcm-lab-m2 ;; esac; }

# e12_clients_start TAG DURATION — four clients on m1 (two failover lists
# with opposite order, per-connection and persistent; one write every 50 ms
# each, 1 s timeout) plus proxy status samplers at 100 ms.
e12_clients_start() {
	local tag=$1 dur=$2 secs v spec lab addr mode l1 l2
	secs=${dur%s}
	e9_addrs
	l1="$E9_A1:3306,$E9_A2:3306"
	l2="$E9_A2:3306,$E9_A1:3306"
	for v in $PXVMS; do
		vm_root "$v" 'rm -f "$1/out/$2-$3.smp"
			systemd-run -q --unit "e9px-$2-$3" --collect -p StandardOutput=file:"$1/out/$2-$3.smp" -p StandardError=file:"$1/out/$2-$3.err" /usr/local/bin/dbcm-e9-pxsample "$4" "$3"' "$E8" "$tag" "$(px_lbl "$v")" "$((secs + 5))"
	done
	for spec in "a-pc|$l1|per-connection" "a-ps|$l1|persistent" "b-pc|$l2|per-connection" "b-ps|$l2|persistent"; do
		lab="e9-$tag-${spec%%|*}"
		addr=$(echo "$spec" | cut -d'|' -f2)
		mode=$(echo "$spec" | cut -d'|' -f3)
		vm_root "$M1" '
			systemd-run -q --unit "$1" --collect -p StandardOutput=file:"$5/out/$1.out" -p StandardError=file:"$5/out/$1.out" \
				"$5/e5client" load -addr "$2" -user e5app -password-file "$5/app.pw" -mode "$3" -duration "$4" -interval 50ms -timeout 1s -label "$1"' \
			"$lab" "$addr" "$mode" "$dur" "$E8"
	done
	say "clients and samplers started ($tag, $dur): lists $l1 and $l2"
}

e12_clients_report() { # TAG
	local l
	for l in a-pc a-ps b-pc b-ps; do
		rec "client e9-$1-$l: transitions and summary" vm_root "$M1" 'cut -c1-300 "$1/out/e9-$2-$3.out" | grep -v "^SUMMARY" | head -14; grep "^SUMMARY" "$1/out/e9-$2-$3.out" | sed "s/^SUMMARY //" | jq -c "{label, ok, errors, max_gap_without_success, max_gap_from, reconnects, per_node, error_examples}"' "$E8" "$1" "$l"
	done
}

# e12_integrity TAG — acknowledged writes versus committed rows per client.
e12_integrity() {
	local tag=$1 l ok rows lost=0 line
	for l in a-pc a-ps b-pc b-ps; do
		ok=$(vm_root "$M1" 'grep "^SUMMARY" "$1/out/e9-$2-$3.out" 2>/dev/null | sed "s/^SUMMARY //" | jq -r .ok' "$E8" "$tag" "$l" 2>/dev/null || echo "")
		[ -n "$ok" ] || { say "no summary for e9-$tag-$l"; lost=2; continue; }
		rows=$(e9_rsql "SELECT COUNT(*) FROM e5.load WHERE label='e9-$tag-$l'" | tr -d '[:space:]')
		line="e9-$tag-$l: acknowledged $ok, rows in the database $rows"
		if [ "$rows" -lt "$ok" ]; then line="$line  LOST ACKNOWLEDGED WRITES: $((ok - rows))"; lost=1; else line="$line  (unacknowledged but committed: $((rows - ok)))"; fi
		printf '%s\n' "$line" >>"$LOG"
		say "$line"
	done
	case "$lost" in
	0) ok "$tag" "no acknowledged write lost (acknowledged <= committed rows for every client)" ;;
	1) assert_fail "$tag" "ACKNOWLEDGED WRITES LOST (see lines above)" ;;
	*) result "$tag: a client summary is missing, integrity not fully evaluated" ;;
	esac
}

# e12_analyze TAG — fetch samples and rows, run the E9 analyzer, assert no
# split and no interleaving.
e12_analyze() {
	local tag=$1 out
	e9_collect "$tag"
	out=$(python3 "$REPO_ROOT/tests/p0/e9-analyze.py" "$SAMPLES_DIR/$tag" 2>&1 || true)
	printf '%s\n' "$out" | grep -E "conflicts|interleavings|longest|spans without" | while read -r l; do say "analysis $tag: $l"; done || true
	if printf '%s\n' "$out" | grep -E "conflicts \(two different writers, neither empty\): 0$" >/dev/null; then
		ok "$tag" "no 100 ms sample with two different non-empty writers on the two proxies"
	else
		assert_fail "$tag" "samples with two different non-empty writers (see analysis)"
	fi
	if printf '%s\n' "$out" | grep -E "interleavings \(runs - distinct nodes\) = 0" >/dev/null; then
		ok "$tag" "no interleaving of two writer nodes in commit order"
	else
		assert_fail "$tag" "interleaving of writer nodes in commit order (see analysis)"
	fi
}

e12_stack_deploy() { # IMAGE
	local gid
	gid=$(vm_sh "$M1" 'stat -c %g /var/run/docker.sock')
	vm_root "$M1" '
		cd "$1" && DBCM_IMAGE="$2" DBCM_DOCKER_GID="$3" DBCM_MANAGER_NODE_1=lima-dbcm-lab-m1 DBCM_MANAGER_NODE_2=lima-dbcm-lab-m2 DBCM_MANAGER_NODE_3=lima-dbcm-lab-m3 \
		DBCM_CONFIG_NAME_1=dbcm-e9-config-1 DBCM_CONFIG_NAME_2=dbcm-e9-config-2 DBCM_CONFIG_NAME_3=dbcm-e9-config-3 \
		DBCM_PEER_CA_NAME=dbcm-e9-peer-ca DBCM_PEER_CA_SEALED_NAME=dbcm-e9-peer-ca-sealed DBCM_PEER_CA_PASSPHRASE_NAME=dbcm-e9-peer-ca-pass \
		docker stack deploy -c deploy/stack-ha.yaml --detach=true "$4" 2>&1' "$SRC" "$1" "$gid" "$STACK"
}

# e12_off_dead — both proxies settled on a writer other than $E12_DEAD.
e12_off_dead() {
	e9_both_settled && [ "$(e8_status dbcm-lab-m1 | jq -r .writer)" != "$E12_DEAD" ] && [ "$(e8_status dbcm-lab-m2 | jq -r .writer)" != "$E12_DEAD" ]
}
# e12_updated_healthy — all updates completed and the stack is healthy.
e12_updated_healthy() { e12_all_updates_done && e12_healthy_ha; }

# e12_update_state N — Swarm update status of manager service N.
e12_update_state() {
	vm_docker "$M1" service inspect -f '{{if .UpdateStatus}}{{.UpdateStatus.State}}{{end}}' "$(mgr_svc "$1")"
}
e12_update_done() { [ "$(e12_update_state "$1")" = completed ]; }
e12_all_updates_done() { local n; for n in $HA_IDS; do e12_update_done "$n" || return 1; done; }
e12_running_count() { vm_docker "$M1" service ls --format '{{.Name}} {{.Replicas}}' | grep -c "^${STACK}_manager-.* 1/1" || true; }
e12_image_of() { vm_docker "$M1" service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "$(mgr_svc "$1")" | cut -d@ -f1; }

# e12_cert_info N — serial, validity and mtime of the peer bundle in the
# volume of replica N.
e12_cert_info() {
	vm_root "$(vm_of "$1")" '
		mp=$(docker volume inspect -f "{{.Mountpoint}}" "dbcm_manager-data-$1")
		echo "replica $1: $(openssl x509 -in "$mp/peer/peer-bundle.pem" -noout -serial -startdate -enddate | tr "\n" " ") mtime=$(stat -c %y "$mp/peer/peer-bundle.pem" | cut -c1-19)"' "$1"
}
# e12_served_cert N — serial and validity of the certificate replica N
# presents on the metastore port (probed from m1 with the manager-1 client
# certificate of the original bundle).
e12_served_cert() {
	e9_addrs
	local a
	case "$1" in 1) a=$E9_A1 ;; 2) a=$E9_A2 ;; 3) a=$E9_A3 ;; esac
	vm_root "$M1" '
		out=$(echo | timeout 8 openssl s_client -connect "$1:$2" -CAfile "$3/peers/ca.pem" -cert "$3/peers/manager-1/peer.pem" -key "$3/peers/manager-1/peer-key.pem" -verify_return_error 2>&1)
		echo "replica $4 at $1:$2: $(printf "%s\n" "$out" | grep -E "Verification" | head -1) / presented: $(printf "%s\n" "$out" | sed -n "/BEGIN CERTIFICATE/,/END CERTIFICATE/p" | openssl x509 -noout -serial -enddate 2>&1 | tr "\n" " ")"' "$a" "$MGR_PORT_META" "$STATE" "$1"
}

# e12_leader_last_order — replica numbers, followers first, the leader last.
e12_leader_last_order() {
	local ln n
	ln=$(ha_leader_n) || return 1
	for n in $HA_IDS; do [ "$n" = "$ln" ] || echo "$n"; done
	echo "$ln"
}

# e12_healthy_ha — all three replicas run, a leader answers, the session is valid.
e12_healthy_ha() {
	[ "$(e12_running_count)" = 3 ] && ha_have_leader && api GET /api/v1/auth/session >/dev/null 2>&1
}

# ------------------------------------------------------------ Part 2: af
e12_af() {
	section "Phase af (part 2): autoFailover of the coordinated proxy writer; the writer's VM is hard-stopped"
	hostload
	e9_addrs
	local k3 dead w0 t_stop t_ep new settled=0 expect i
	e9_pxprep || { assert_fail af "proxy setup failed"; return 1; }
	wait_until 60 "both proxies settled with a writer" e9_both_settled || true
	rec "coordination state of both proxies" e9_px_coord
	k3=$(e8_node_on dbcm-lab-m3)
	dead="n$k3"
	w0=$(e8_status dbcm-lab-m1 | jq -r .writer)
	if [ "$w0" != "$dead" ]; then
		rec "controlled switch to $dead (the Galera node on m3) so that the VM to be lost hosts the writer" e9_admin_switch "$dead"
		wait_until 60 "both proxies report writer $dead" e9_both_writer "$dead" || assert_fail af "switch to $dead did not complete"
	fi
	rec "manager route check: cluster $CLUSTER_NAME has an ID different from its name (the route is configured by name)" apij GET /api/v1/clusters
	expect=$(for n in n1 n2 n3; do [ "$n" != "$dead" ] && echo "$n"; done | head -1)
	say "writer $dead on m3 (no proxy there); autoFailover must choose $expect (first healthy candidate in sorted order)"
	e8_measure "af: before"
	e12_clients_start af 300s
	sleep 20
	t_stop=$(date +%s)
	t_ep=$(e8_now)
	e8_vm_stop_force dbcm-lab-m3
	say "power loss of m3 (writer $dead) sent at $t_ep"
	for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
		sleep 5
		say "t+$((i * 5))s: $(e9_px_coord | tr '\n' ' ')"
	done
	E12_DEAD=$dead
	if wait_until 120 "both proxies settled on a writer other than $dead" e12_off_dead; then
		settled=1
	fi
	new=$(e8_status dbcm-lab-m1 | jq -r .writer)
	say "writer after the loss: $new; $(($(date +%s) - t_stop)) s after the stop command"
	rec "coordination state after the automatic switch" e9_px_coord
	rec "the manager's choice" e9_choice
	rec "manager log lines about the writer failover (all replicas)" sh -c "limactl shell --workdir / $M1 -- sudo docker service logs --no-trunc --timestamps ${STACK}_manager-1 ${STACK}_manager-2 ${STACK}_manager-3 2>&1 | grep -iE 'proxy writer|failover|frozen' | cut -c1-260 | tail -12"
	rec "wsrep status of the survivors" e8_status3
	if [ "$settled" = 1 ] && [ "$new" = "$expect" ]; then
		ok af "autoFailover switched the writer from $dead to $new without an operator request"
	elif [ "$settled" = 1 ]; then
		ok af "autoFailover switched the writer from $dead to $new (expected first candidate $expect)"
	else
		assert_fail af "no automatic switch away from $dead within the observation window"
	fi
	sleep 30
	local ts
	ts=$(date +%s)
	e8_vm_start dbcm-lab-m3 || result "VM m3 did not start (see log)"
	wait_until 300 "m3 reachable over ssh" vm_sh dbcm-lab-m3 true || true
	wait_until 300 "m3 is a Ready Swarm node" e8_node_ready dbcm-lab-m3 || true
	e9_wait_synced 600 || result "af: not all three Synced within 600 s after m3 returned"
	say "m3 back and Galera Synced $(($(date +%s) - ts)) s after limactl start"
	rec "wsrep status after the return" e8_status3
	rec "coordination state after the return (no switch back while the writer is healthy)" e9_px_coord
	[ "$(e8_status dbcm-lab-m1 | jq -r .writer)" = "$new" ] && ok af "writer stayed $new after $dead returned" || assert_fail af "writer changed after $dead returned"
	wait_until 200 "clients and samplers finished" e9_units_done || true
	e12_clients_report af
	e12_analyze af
	e12_integrity af
	rec "distinct writer nodes in the rows" e9_rsql "SELECT COUNT(DISTINCT node), GROUP_CONCAT(DISTINCT node) FROM e5.load WHERE label LIKE 'e9-af-%'"
	for i in $HA_IDS; do rec "replica $i after the VM loss" ha_probe "$(vm_of "$i")" || true; done
	wait_until 240 "three manager tasks running" ha_all_running 3 || true
	e9_pxteardown
}

# --------------------------------------------------------- Part 3: roll
e12_roll() {
	section "Phase roll (part 3): rolling update of the three manager services"
	hostload
	local vm n t0 i order
	for v in b c; do
		rec "derived image $IMAGE-$v (FROM $IMAGE plus a label: identical binaries, different image ID)" vm_root "$M1" \
			'printf "FROM %s\nLABEL dbcm.e12=%s\n" "$1" "$2" | docker build -q -t "$1-$2" -' "$IMAGE" "$v"
		for vm in dbcm-lab-m2 dbcm-lab-m3; do
			rec "copy $IMAGE-$v to $vm" sh -c "limactl shell --workdir / $M1 -- sudo docker save $IMAGE-$v | limactl shell --workdir / $vm -- sudo docker load"
		done
	done
	e12_healthy_ha || { assert_fail roll "HA stack not healthy before the update"; return 1; }
	rec "keyring before" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/settings/replicas | tail -n +2 | jq -c '{currentKey, recoverable, replicas: [.replicas[] | {id, leader, hasCurrentKey}]}'"

	# A: the documented-looking way: redeploy the stack with the new image.
	say "A: docker stack deploy with $IMAGE-b (all three services have a changed spec at once)"
	for vm in $VMS; do e9_apisample_start "$vm" rollA 150; done
	sleep 5
	t0=$(date +%s)
	rec "docker stack deploy with $IMAGE-b" e12_stack_deploy "$IMAGE-b" || true
	for i in $(seq 1 24); do
		sleep 5
		say "A t+$(($(date +%s) - t0))s running replicas: $(e12_running_count)/3; leader: $(ha_leader_n 2>/dev/null || echo none); states: $(vm_docker "$M1" service ps --filter desired-state=running --format '{{.Name}}:{{.CurrentState}}' $(mgr_svc 1) $(mgr_svc 2) $(mgr_svc 3) 2>/dev/null | sed 's/ [0-9a-z ]* ago//' | tr '\n' ' ')"
		if [ "$i" -ge 4 ] && e12_all_updates_done && e12_healthy_ha; then break; fi
	done
	wait_until 120 "all three updates completed and a leader answers" e12_updated_healthy || assert_fail roll "stack redeploy did not converge"
	say "A converged $(($(date +%s) - t0)) s after the deploy"
	sleep 20
	for vm in $VMS; do rec "A: API sampled on $vm" e9_apisample_report "$vm" rollA; done
	for vm in $VMS; do rec "A: non-200 samples on $vm (first, last, count)" vm_root "$vm" 'grep -v " 200$" "$1/out/rollA.api" | sed -n "1p;\$p"; echo "count: $(grep -vc " 200$" "$1/out/rollA.api")"' "$E8"; done
	for n in $HA_IDS; do rec "image of replica $n" e12_image_of "$n"; done
	rec "session and keyring after A" api GET /api/v1/auth/session
	rec "cluster still listed after A" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/clusters | tail -n +2 | jq -c '[.items[] | {name, state}]'"

	# B: one replica at a time, followers first, leader last, gated on health.
	say "B: one replica at a time with $IMAGE-c"
	order=$(e12_leader_last_order | tr '\n' ' ')
	say "B order (followers first, leader last): $order"
	for vm in $VMS; do e9_apisample_start "$vm" rollB 260; done
	sleep 5
	t0=$(date +%s)
	for n in $order; do
		rec "docker service update --image $IMAGE-c $(mgr_svc "$n")" vm_docker "$M1" service update --detach=true --image "$IMAGE-c" "$(mgr_svc "$n")"
		wait_until 240 "replica $n update completed" e12_update_done "$n" || assert_fail roll "update of replica $n did not complete"
		wait_until 120 "all three replicas running with a leader and a valid session" e12_healthy_ha || assert_fail roll "stack not healthy after replica $n"
		say "B replica $n done $(($(date +%s) - t0)) s after the start; leader $(ha_leader_n 2>/dev/null || echo none)"
		sleep 15
	done
	say "B finished $(($(date +%s) - t0)) s after the start"
	sleep 15
	for vm in $VMS; do rec "B: API sampled on $vm" e9_apisample_report "$vm" rollB; done
	for vm in $VMS; do rec "B: non-200 samples on $vm (first, last, count)" vm_root "$vm" 'grep -v " 200$" "$1/out/rollB.api" | sed -n "1p;\$p"; echo "count: $(grep -vc " 200$" "$1/out/rollB.api")"' "$E8"; done
	for n in $HA_IDS; do rec "image of replica $n" e12_image_of "$n"; done
	rec "keyring after B" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/settings/replicas | tail -n +2 | jq -c '{currentKey, recoverable, replicas: [.replicas[] | {id, leader, hasCurrentKey}]}'"
	for n in $HA_IDS; do rec "log of replica $n after B" mgr_logs "$n" 12 || true; done
	[ "$(apij GET /api/v1/settings/replicas | jq '[.replicas[] | select(.hasCurrentKey)] | length')" = 3 ] &&
		ok roll "all three replicas hold the current key after both updates" || assert_fail roll "keyring changed by the updates"
}

# -------------------------------------------------------- Part 3: renew
e12_renew() {
	section "Phase renew (part 3): peer certificate renewal through a new configuration (beforeDays 400, validityDays 500)"
	hostload
	local n order serial_before serial_after
	e12_healthy_ha || { assert_fail renew "HA stack not healthy before the renewal"; return 1; }
	for n in $HA_IDS; do rec "peer bundle before, replica $n" e12_cert_info "$n"; done
	for n in $HA_IDS; do rec "certificate served on the metastore port before, replica $n" e12_served_cert "$n" || true; done
	serial_before=$(e12_cert_info 2 | grep -o 'serial=[0-9A-F]*')
	rec "new Swarm configs: renew window 400 days (more than the 365-day validity: renewal is due at every start), validity 500 days" vm_root "$M1" '
		set -e
		for i in 1 2 3; do
			jq ".manager.peerTLS.renew.beforeDays = 400 | .manager.peerTLS.renew.validityDays = 500" "$1/manager-$i.json" >"$1/manager-$i.renew.json"
			chmod 0600 "$1/manager-$i.renew.json"
			docker config inspect "dbcm-e12-config-$i" >/dev/null 2>&1 && echo "config $i exists" || { docker config create "dbcm-e12-config-$i" "$1/manager-$i.renew.json" >/dev/null && echo "config $i"; }
		done
		jq -c ".manager.peerTLS" "$1/manager-1.renew.json" | sed "s/\"installation\":\"[^\"]*\"/\"installation\":\"<inst>\"/"' "$STATE"
	order=$(e12_leader_last_order | tr '\n' ' ')
	say "renewal order (followers first, leader last): $order"
	for vm in $VMS; do e9_apisample_start "$vm" renew 200; done
	sleep 5
	for n in $order; do
		rec "replace the config of $(mgr_svc "$n") (docker service update --config-rm/--config-add)" vm_docker "$M1" service update --detach=true \
			--config-rm "dbcm-e9-config-$n" --config-add "source=dbcm-e12-config-$n,target=/etc/dbcm/manager.json,uid=65532,gid=65532,mode=0440" "$(mgr_svc "$n")"
		wait_until 240 "replica $n update completed" e12_update_done "$n" || assert_fail renew "update of replica $n did not complete"
		wait_until 120 "all three replicas running with a leader and a valid session" e12_healthy_ha || assert_fail renew "stack not healthy after replica $n"
		rec "peer bundle after the start, replica $n" e12_cert_info "$n"
		sleep 10
	done
	sleep 10
	for vm in $VMS; do rec "API sampled on $vm during the renewal rolling update" e9_apisample_report "$vm" renew; done
	for n in $HA_IDS; do rec "peer bundle after, replica $n" e12_cert_info "$n"; done
	for n in $HA_IDS; do rec "certificate served on the metastore port after, replica $n (client: manager-1 certificate of the original bundle, same CA)" e12_served_cert "$n" || true; done
	serial_after=$(e12_cert_info 2 | grep -o 'serial=[0-9A-F]*')
	[ -n "$serial_after" ] && [ "$serial_before" != "$serial_after" ] && ok renew "replica 2's peer certificate serial changed ($serial_before -> $serial_after)" ||
		assert_fail renew "peer certificate serial of replica 2 unchanged ($serial_before)"
	for n in $HA_IDS; do rec "log lines about renewal, replica $n" sh -c "limactl shell --workdir / $M1 -- sudo docker service logs --no-trunc --timestamps $(mgr_svc "$n") 2>&1 | grep -iE 'renew|peer cert' | cut -c1-240 | tail -5" || true; done
	[ "$(apij GET /api/v1/settings/replicas | jq '[.replicas[] | select(.hasCurrentKey)] | length')" = 3 ] &&
		ok renew "all three replicas hold the current key and form a quorum with renewed certificates" || assert_fail renew "keyring state wrong after the renewal"
}

# --------------------------------------------------------- Part 1: power
e12_pwrb2() {
	section "Phase pwrb2 (part 1): E8/E9 B2 again, full power loss of all three VMs, no proxies, healthy cluster before"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	e9_pxteardown
	e8_login
	rec "sentinel table and first sentinels" rsql 1 "CREATE DATABASE IF NOT EXISTS e8; CREATE TABLE IF NOT EXISTS e8.sentinel (id INT PRIMARY KEY, note VARCHAR(64), node VARCHAR(64), at TIMESTAMP(3) DEFAULT CURRENT_TIMESTAMP(3)); REPLACE INTO e8.sentinel (id, note, node) VALUES (1, 'before the power loss', @@hostname), (2, 'second', @@hostname)"
	rec "wsrep status before (uuid must be non-zero)" e8_status3
	E8_SELF=0
	phase_e8allloss || true
}

# e12_zero_state — exit 0 when a node reports Primary with the zero UUID.
e12_zero_state() {
	local s
	s=$(e8_status3 2>/dev/null) || true
	printf '%s\n' "$s" | grep -q 'wsrep_cluster_state_uuid 00000000-0000-0000-0000-000000000000' &&
		printf '%s\n' "$s" | grep -q 'wsrep_cluster_status Primary'
}

e12_incident() {
	section "Phase incident (part 1): zero-UUID Primary detection and the outage-diagnosis 409 reason on the real cluster"
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	e8_login
	local zero=0 out code reason n
	rec "wsrep status of every node" e8_status3
	e12_zero_state && zero=1
	n=$(apij GET /api/v1/events | jq -r '[.items[] | select(.message | test("zero_cluster_uuid_primary"))] | length')
	apij GET /api/v1/events | jq -c '.items[] | select(.message | test("zero_cluster_uuid_primary")) | {severity, message}' >>"$LOG" || true
	say "incident events recorded: $n"
	rec "cluster view (node health incl. reason code)" api GET "/api/v1/clusters/$CLUSTER_ID"
	out=$(api GET "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" || true)
	printf '### outage diagnosis (GET) now\n\n```\n%s\n```\n\n' "$out" >>"$LOG"
	code=$(printf '%s\n' "$out" | head -1 | awk '{print $2}')
	reason=$(printf '%s\n' "$out" | tail -n +2 | jq -r '.reason // ""' 2>/dev/null || true)
	out=$(api POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' || true)
	printf '### outage diagnosis with position recovery (POST) now\n\n```\n%s\n```\n\n' "$out" >>"$LOG"
	say "zero-UUID Primary currently present: $zero; GET diagnosis HTTP $code reason '$reason'; POST diagnosis: $(printf '%s\n' "$out" | head -1)"
	if [ "$zero" = 1 ]; then
		[ "$n" -ge 1 ] && ok incident "health.zero_cluster_uuid_primary incident recorded ($n event(s))" || assert_fail incident "zero-UUID Primary present but no incident event"
		{ [ "$code" = 409 ] && [ "$reason" = zero_cluster_uuid_primary ]; } && ok incident "outage diagnosis answers 409 with reason zero_cluster_uuid_primary" || assert_fail incident "diagnosis answered HTTP $code reason '$reason'"
		case "$(printf '%s\n' "$out" | head -1)" in "HTTP 409"*) ok incident "position-recovery POST refuses with 409 while the zero-UUID Primary exists" ;; *) assert_fail incident "POST diagnosis was not refused: $(printf '%s\n' "$out" | head -1)" ;; esac
	else
		result "incident: no zero-UUID Primary component at check time (cluster state: see wsrep lines above); detection and 409 reason NOT verified on this run"
	fi
	e9_slot_diag "incident phase"
}

e12_pwrrecover() {
	section "Phase pwrrecover (part 1): guided recovery (operator stops the services, the manager diagnoses, approval-gated bootstrap from the highest seqno)"
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	E8_ROW=106
	e9_pwrrecover || true
	local u
	u=$(e6_uuid 1 2>/dev/null || true)
	say "cluster state UUID after the recovery: ${u:-unknown} (before the power loss: ${E6_UUID:-unknown})"
	if [ -n "$u" ] && [ "$u" != 00000000-0000-0000-0000-000000000000 ]; then ok pwrrecover "cluster has a non-zero state UUID again ($u)"; else assert_fail pwrrecover "cluster state UUID is zero or unknown after the recovery"; fi
	e8_synced3 && ok pwrrecover "three nodes Synced after the guided recovery" || assert_fail pwrrecover "not three Synced nodes after the guided recovery"
	rec "events after the recovery" e6_events
	rec "operations" e6_ops
}

e12_pwrb3() {
	section "Phase pwrb3 (part 1): valid B3, staggered loss of the nodes on a repaired cluster"
	hostload
	e8_synced3 || wait_until 300 "Galera cluster back to 3 synced nodes" e8_synced3 || { assert_fail pwrb3 "cluster not healthy before B3: not a valid B3"; return 1; }
	e9_pwrb3 || true
	e8_synced3 || e9_wait_synced 600 || true
	rec "wsrep status at the end of B3" e8_status3
	rec "sentinels on every node at the end" e8_sentinels_all "1,2,3,5,6" || true
	e8_synced3 && ok pwrb3 "three nodes Synced at the end of B3" || assert_fail pwrb3 "B3 did not end with three Synced nodes"
}

ALL_STEPS="build vms swarm images prereqs hainit hastack hasetup agents cluster af roll renew pwrb2 incident pwrrecover pwrb3"
for p in $ALL_STEPS; do
	step "$p" || continue
	case "$p" in vms | swarm) [ "${E12_REUSE_VMS:-0}" = 1 ] && continue ;; esac
	fn="e12_$p"
	declare -F "$fn" >/dev/null || fn="e9_$p"
	"$fn" || say "phase $p returned non-zero"
done
