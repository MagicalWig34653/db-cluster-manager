#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 E4 re-run on the E4 lab (tests/p0/e4-manager-galera.sh is sourced as a
# library): the E4 gap work on real VMs, resource exhaustion, restart during
# cleanup, and three manager replicas.
#
# Part A (E4 topology: m1 Swarm manager, m2/m3 workers, one manager process
# with autoResume):
#   cluster/verify/failure  create e4db (operation progress sampled), MariaDB
#                           checks, node kill -> events and notifications
#   backup                  physical backup; Galera position in the manifest
#   rinspect                inspection restore on a WORKER host, approval,
#                           restore-verified gate, query, TTL teardown
#   capacity                plans exceeding host capacity must be refused
#   failcreate              create with an unpullable image, abort, delete
#                           (discard), the name is created again
#   cleanuprestart          delete of a healthy cluster; the manager is
#                           SIGKILLed and an agent killed mid-deletion
#   diskfull                the volume filesystem of one node fills up
#   sentinels               non-owned objects unchanged
#   deleteall               delete e4db through the API
# Part B (three manager replicas on m1..m3, all Swarm managers):
#   hareset hamanager hasetup haswarm hacreate hafailover hadelete
#
# Usage: tests/p0/e4-rerun.sh
#   R_KEEP_VMS=1  keep the VMs; R_PHASES="..." run only these phases;
#   (R_PHASES takes step names from the lists above, not "A" or "B"; unset runs all.)
#   E4_REUSE_VMS=1 skip vms/swarm; R_LOG=FILE; R_APPEND_LOG=1.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${R_LOG:-"$REPO_ROOT/tests/p0/results/e4-rerun.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${R_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
export E4_AUTORESUME=true
# shellcheck source=tests/p0/e4-manager-galera.sh
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"

LOOPIMG=/var/lib/dbcm-e4r-loop.img # m3: file-backed filesystem mounted at VOLBASE
LOOPSIZE=2560M
SENT_SVC=dbcm-e4r-sentinel
SENT_VOL=dbcm-e4r-sentinel-vol
DECOY_VOL=dbcm-e4fail-n1-decoy
SENT_DIR=$VOLBASE/dbcm-e4r-sentinel-dir
APIVM=$M1

# Failed assertions of phases that continue after an expected refusal
# (failcreate, cleanuprestart, deleteall); they make the exit status 1.
ASSERT_FAILS=""
assert_fail() { # PHASE MESSAGE
	ASSERT_FAILS="$ASSERT_FAILS $1"
	result "ASSERTION FAILED ($1): $2"
}

finishr() {
	local rc=$?
	trap - EXIT INT TERM
	if [ -n "$ASSERT_FAILS" ]; then
		say "failed assertions in:$ASSERT_FAILS"
		[ "$rc" != 0 ] || rc=1
	fi
	if [ "${HA_STARTED:-0}" = 1 ] && { [ "$rc" != 0 ] || [ -n "$ASSERT_FAILS" ]; }; then
		ha_logs "exit $rc, failed assertions:${ASSERT_FAILS:- none}" || true
	fi
	if [ "${R_KEEP_VMS:-0}" = 1 ]; then
		say "R_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" -e 's#dbcm-et1\.[A-Za-z0-9._-]*#dbcm-et1.<redacted>#g' \
		-e "s#PASSWORD '\*[0-9A-F]*'#PASSWORD '<hash redacted>'#g" "$LOG"
	say "E4 re-run finished with exit $rc"
	exit "$rc"
}
trap finishr EXIT
trap 'exit 130' INT TERM

step() { case " ${R_PHASES:-all} " in *" all "* | *" $1 "*) return 0 ;; esac; return 1; }

# The API is called on APIVM (part B: the current leader's VM).
api() {
	vm_args "$APIVM" 'sudo /usr/local/bin/dbcm-e4-client -state '"$STATE"'/client.json -base http://127.0.0.1:'"$API_PORT"' -origin http://localhost:'"$API_PORT"' req "$@"' "$@"
}

phase_build() {
	section "Phase build: cross-compile linux/arm64 binaries on the host"
	mkdir -p "$BIN"
	local c
	for c in manager agent dbcm-job; do
		(cd "$REPO_ROOT" && rec "go build ./cmd/$c" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/$c" "./cmd/$c")
	done
	(cd "$REPO_ROOT" && rec "go build ./tests/p0/e4client" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/e4client" ./tests/p0/e4client)
	rec "git revision" git -C "$REPO_ROOT" log -1 --format='%h %s'
}

# ---------------------------------------------------------------- helpers
jq_api() { apij GET "$1" | jq -c "$2"; } # PATH FILTER
# with_timeout SECONDS CMD... — run CMD (also a shell function) in the
# background and stop it after SECONDS (exit 124).
with_timeout() {
	local secs=$1 pid i
	shift
	"$@" &
	pid=$!
	for i in $(seq 1 "$secs"); do
		kill -0 "$pid" 2>/dev/null || { wait "$pid"; return $?; }
		sleep 1
	done
	kill "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	return 124
}
opjson() { apij GET /api/v1/operations | jq -c --arg id "$1" '.items[] | select(.id==$id)'; }
opline() {
	opjson "$1" | jq -r '"\(.state) \(.reason // "") | \(.progress // {} | "\(.stepsDone // 0)/\(.stepsTotal // 0) \(.step // "") \(.command // "") \(.state // "") att=\(.attempts // 0) polls=\(.polls // 0) \(.waiting // "" | .[0:120]) err=\(.lastError // "" | .[0:160])")"'
}
# op_diag OP SERVICE_PREFIX — record why a step keeps failing: the
# operation's reason, waiting and lastError, the manager's runner log lines
# for OP (retries with step and attempt), and the Swarm task states with
# their errors (for example an image that cannot be pulled).
op_error() { opjson "$1" | jq '{state, reason, progress: (.progress // {} | {step, command, target, state, attempts, waiting, lastError})}'; }
op_diag() {
	rec "step error of $1 (reason, waiting, lastError)" op_error "$1" || true
	rec "manager runner log for $1" vm_args "$M1" 'sudo journalctl -u "$1" --no-pager | grep -F "$2" | grep -E "runner: (step retry|operation)" | tail -15 | cut -c1-400' "$UNIT" "$1" || true
	rec "Swarm tasks of $2* (state and error)" vm_args "$M1" 'for s in $(sudo docker service ls --format "{{.Name}}" | grep "^$1"); do sudo docker service ps --no-trunc --format "{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}" "$s"; done' "$2" || true
}
# wait_op OP DEADLINE INTERVAL — poll with progress until a terminal state.
OPSTATE=""
wait_op() {
	local op=$1 deadline=$2 iv=${3:-10} start line
	start=$(date +%s)
	while :; do
		line=$(opline "$op" 2>/dev/null || echo "unreachable")
		say "op $op t+$(($(date +%s) - start))s: $line"
		OPSTATE=${line%% *}
		case "$OPSTATE" in succeeded | failed | aborted) return 0 ;; esac
		[ $(($(date +%s) - start)) -lt "$deadline" ] || { say "op $op not finished after $deadline s"; return 1; }
		sleep "$iv"
	done
}
cluster_body() { # NAME IMAGE MEM_BYTES SIZE
	jq -cn --arg s "$SWARM_ID" --arg n "$1" --arg i "$2" --arg net "$NET" --arg r "$ROOT_SECRET" --argjson m "$3" --argjson z "$4" \
		'{swarm:$s,name:$n,size:$z,image:$i,network:$net,rootPasswordSecret:$r,gcacheSize:"128M",
		  resources:{limits:{milli_cpu:1000,memory_bytes:$m},reservations:{milli_cpu:250,memory_bytes:536870912}},
		  placement:{reserve:{}}}'
}
CREATED_OP=""
create_cluster() { # NAME IMAGE -> CREATED_OP
	SWARM_ID=${SWARM_ID:-$(apij GET /api/v1/swarms | jq -r '.items[0].id')}
	local body resp
	body=$(cluster_body "$1" "$2" 1073741824 3)
	resp=$(api POST /api/v1/clusters "$body") || true
	printf '### create cluster %s\n\n```\n$ api POST /api/v1/clusters %s\n%s\n```\n\n' "$1" "$body" "$resp" >>"$LOG"
	CREATED_OP=$(printf '%s\n' "$resp" | tail -n +2 | jq -r '.id // empty')
	[ -n "$CREATED_OP" ]
}
cid_of() { apij GET /api/v1/clusters | jq -r --arg n "$1" '.items[] | select(.name==$n) | .id'; }
# owned_objects NAME — services and volumes of a cluster name on all VMs.
owned_objects() {
	local vm
	echo "services: $(vm_docker "$M1" service ls --format '{{.Name}}' | grep -c "^dbcm-$1-" || true) ($(vm_docker "$M1" service ls --format '{{.Name}}' | grep "^dbcm-$1-" | tr '\n' ' '))"
	for vm in $VMS; do
		echo "$vm volumes: $(vm_docker "$vm" volume ls --format '{{.Name}}' | grep "^dbcm-$1-" | tr '\n' ' ')"
		echo "$vm containers: $(vm_docker "$vm" ps -a --format '{{.Names}}' | grep "dbcm-$1-" | tr '\n' ' ')"
	done
}
approve_digest() { # OP DIGEST
	rec "approve $1 with the digest" api POST "/api/v1/operations/$1/approve" "{\"digest\":\"$2\"}"
}
# delete_cluster ID -> DEL_OP DEL_DIGEST (operation waits for approval)
DEL_OP="" DEL_DIGEST=""
delete_cluster() {
	local resp
	resp=$(api DELETE "/api/v1/clusters/$1") || true
	printf '### delete cluster %s\n\n```\n%s\n```\n\n' "$1" "$resp" >>"$LOG"
	DEL_OP=$(printf '%s\n' "$resp" | tail -n +2 | jq -r '.operation.id // .id // empty')
	DEL_DIGEST=$(printf '%s\n' "$resp" | tail -n +2 | jq -r '.approval.digest // .operation.approval.digest // .digest // empty')
	say "deletion op $DEL_OP digest ${DEL_DIGEST:0:12}"
	[ -n "$DEL_OP" ] && [ -n "$DEL_DIGEST" ]
}
mgr_restart() {
	vm_args "$1" 'sudo systemd-run --unit "$1" -p Restart=no --collect /usr/local/bin/dbcm-e4-manager -listen "127.0.0.1:$2" -config /etc/dbcm-e4/manager.json' "$UNIT" "$API_PORT"
}
leads_on() { [ "$(vm_sh "$1" "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$API_PORT/api/v1/setup/status")" = 200 ]; }

# ---------------------------------------------------------------- part A
phase_rprep() {
	section "Phase rprep: small volume filesystem on m3 (disk-full test) and non-owned sentinel objects"
	hostload
	rec "file-backed ext4 ($LOOPSIZE) mounted at $VOLBASE on m3 (before the agent mounts it)" vm_root dbcm-lab-m3 '
		set -e
		mountpoint -q "$2" || { [ -f "$1" ] || fallocate -l "$3" "$1"; mkfs.ext4 -q -F "$1"; mount -o loop "$1" "$2"; }
		chmod 0755 "$2"; df -h "$2" | tail -1' "$LOOPIMG" "$VOLBASE" "$LOOPSIZE"
	rec "sentinel service (no installation label) on m3" vm_args "$M1" '
		sudo docker service inspect "$1" >/dev/null 2>&1 || sudo docker service create -q --detach --name "$1" --label dbcm.test=sentinel \
			--constraint node.hostname==lima-dbcm-lab-m3 "$2" sleep infinity' "$SENT_SVC" "$MARIADB_IMAGE"
	local vm
	for vm in $VMS; do
		rec "sentinel volume and decoy volume (other installation) on $vm" vm_args "$vm" '
			sudo docker volume inspect "$1" >/dev/null 2>&1 || sudo docker volume create --label dbcm.test=sentinel "$1" >/dev/null
			sudo docker volume inspect "$2" >/dev/null 2>&1 || sudo docker volume create --label dbcm.installation=other-installation --label dbcm.cluster=other "$2" >/dev/null
			sudo docker volume ls --format "{{.Name}} {{.Labels}}" | grep -E "$1|$2"' "$SENT_VOL" "$DECOY_VOL"
		rec "sentinel directory in the volume base on $vm" vm_root "$vm" 'install -d "$1" && [ -f "$1/data" ] || head -c 4096 /dev/urandom >"$1/data"; sha256sum "$1/data"' "$SENT_DIR"
	done
	wait_until 120 "sentinel service running" sh -c "limactl shell --workdir / $M1 -- sudo docker service ps --filter desired-state=running --format '{{.CurrentState}}' $SENT_SVC | grep -q Running"
	SENT_BEFORE=$(sentinel_snapshot_r)
	printf '### sentinel snapshot before\n\n```\n%s\n```\n\n' "$SENT_BEFORE" >>"$LOG"
}
sentinel_snapshot_r() {
	local vm
	vm_docker "$M1" service inspect -f 'service {{.ID}} version={{.Version.Index}} labels={{.Spec.Labels}}' "$SENT_SVC"
	vm_docker "$M1" service ps --filter desired-state=running --format 'task {{.ID}} {{.Node}}' "$SENT_SVC"
	for vm in $VMS; do
		echo "$vm: $(vm_docker "$vm" volume ls --format '{{.Name}}' | grep -E "^($SENT_VOL|$DECOY_VOL)\$" | tr '\n' ' ') $(vm_root "$vm" 'sha256sum "$1/data" | cut -c1-16' "$SENT_DIR")"
	done
}
SENT_BEFORE=""

phase_rcluster() {
	section "Phase rcluster: create e4db, progress sampled during the operation"
	hostload
	create_cluster "$CLUSTER_NAME" "$MARIADB_IMAGE"
	CLUSTER_ID=$(cluster_id)
	wait_op "$CREATED_OP" 1500 5
	rec "operation (final view)" opjson "$CREATED_OP"
	[ "$OPSTATE" = succeeded ] || { result "create of e4db ended $OPSTATE"; return 1; }
	result "create of $CLUSTER_NAME succeeded"
	rec "cluster" api GET "/api/v1/clusters/$CLUSTER_ID"
}

phase_rfailure() {
	phase_failure
	rec "notifications after the node kill" jq_api /api/v1/notifications '.items[]? | {time, severity, kind, resource, message}'
	local ev
	ev=$(apij GET /api/v1/events | jq -r '[.items[] | select(.message | test("n3"; "i")) | select(.message | test("unhealthy|healthy|unavailable|outage|recovered"; "i"))] | length')
	result "node-outage events naming n3: $ev"
}

phase_rbackup() {
	phase_backup
	local m
	m=$(vm_sh "$M1" "f=\$(sudo find /home/dbcme4bak/backups -type f -name '*manifest*' -printf '%T@ %p\n' | sort -n | tail -1 | cut -d' ' -f2); sudo cat \"\$f\"")
	rec "Galera position and warnings in the newest manifest" sh -c 'printf "%s\n" "$1" | jq -c "{galera, warnings}"' sh "$m"
	if printf '%s' "$m" | jq -e '.galera != null' >/dev/null 2>&1; then
		result "manifest records a Galera position: $(printf '%s' "$m" | jq -c '.galera')"
	else
		result "manifest records NO Galera position"
	fi
}

phase_rinspect() {
	section "Phase rinspect: inspection restore on a WORKER host (m2), verification gate, TTL teardown"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	local host resp op digest
	host=$(apij GET /api/v1/hosts | jq -r '[.items[] | select(.role!="manager") | select(.hostname | test("m2"))][0].nodeId')
	say "inspection host (worker m2): $host"
	resp=$(vm_args "$M1" 'sudo /usr/local/bin/dbcm-e4-client -state "$1/client.json" -base "http://127.0.0.1:$2" -origin "http://localhost:$2" req POST /api/v1/restores/inspect "$3" | sudo tee "$1/inspect.json" >/dev/null; sudo head -1 "$1/inspect.json"; sudo tail -1 "$1/inspect.json" | python3 -c "import json,sys; d=json.load(sys.stdin); d.pop(\"inspectionPassword\",None); print(json.dumps(d))"' \
		"$STATE" "$API_PORT" "{\"cluster\":\"$CLUSTER_ID\",\"host\":\"$host\",\"ttlMinutes\":4}")
	printf '### start inspection on worker %s\n\n```\n%s\n```\n\n' "$host" "$resp" >>"$LOG"
	case "$resp" in "HTTP 202"*) ;; *) result "inspection not started: $(printf '%s' "$resp" | head -2 | tr '\n' ' ')"; return 1 ;; esac
	op=$(printf '%s\n' "$resp" | tail -1 | jq -r .id)
	digest=$(printf '%s\n' "$resp" | tail -1 | jq -r .approval.digest)
	approve_digest "$op" "$digest"
	wait_op "$op" 900 10 || true
	rec "inspection operation (final view)" opjson "$op"
	result "inspection operation on the worker host ended: $OPSTATE"
	rec "instance service placement and args" vm_args "$M1" '
		svc=$(sudo tail -1 "$1/inspect.json" | python3 -c "import json,sys; print(json.load(sys.stdin)[\"service\"])")
		sudo docker service ps --format "{{.Name}} {{.Node}} {{.CurrentState}}" "$svc"
		sudo docker service inspect -f "{{json .Spec.TaskTemplate.ContainerSpec.Args}}" "$svc"' "$STATE" || true
	local svc port
	svc=$(vm_sh "$M1" "sudo tail -1 $STATE/inspect.json" | jq -r .service)
	port=$(vm_sh "$M1" "sudo tail -1 $STATE/inspect.json" | jq -r .port)
	rec "inspection query on m2 with the one-time account (password via stdin)" sh -c \
		"limactl shell --workdir / $M1 -- sudo sh -c \"tail -1 $STATE/inspect.json | python3 -c 'import json,sys; print(json.load(sys.stdin)[\\\"inspectionPassword\\\"])'\" | limactl shell --workdir / dbcm-lab-m2 -- sudo sh -c 'c=\$(docker ps -q -f label=com.docker.swarm.service.name=$svc); read -r pw; exec docker exec -i \$c mariadb -h127.0.0.1 -P$port -udbcm_inspect -p\"\$pw\" -N -B -e \"SELECT CURRENT_USER(); SELECT @@wsrep_on; SELECT id, v FROM e4.t ORDER BY id\"'" || true
	vm_sh "$M1" "sudo rm -f $STATE/inspect.json"
	rec "cluster lastVerifiedRestoreAt" jq_api "/api/v1/clusters/$CLUSTER_ID" '{lastVerifiedRestoreAt, lastBackupAt}'
	rec "inspection secrets before expiry" vm_sh "$M1" "sudo docker secret ls --format '{{.Name}} {{.Labels}}' | grep -i -E 'insp|init' || echo none"
	say "waiting for the TTL teardown (4 min TTL, 30 s schedule)"
	local start
	start=$(date +%s)
	while [ $(($(date +%s) - start)) -lt 600 ]; do
		if ! vm_docker "$M1" service inspect "$svc" >/dev/null 2>&1; then say "instance service gone after $(($(date +%s) - start)) s"; break; fi
		sleep 15
	done
	rec "teardown operations" jq_api /api/v1/operations '.items[] | select(.kind|test("inspect|teardown")) | {id, kind, state, reason, steps: .progress.stepsTotal}'
	rec "instance service after expiry" vm_sh "$M1" "sudo docker service inspect -f '{{.Spec.Name}} still exists' $svc 2>&1 || true"
	rec "inspection secrets after expiry" vm_sh "$M1" "sudo docker secret ls --format '{{.Name}} {{.Labels}}' | grep -i -E 'insp|init' || echo none"
	rec "restore volumes on m2 after expiry" vm_sh dbcm-lab-m2 "sudo docker volume ls --format '{{.Name}}' | grep -i -E 'insp|restore' || echo none"
}

phase_capacity() {
	section "Phase capacity: plans exceeding host capacity are refused"
	hostload
	SWARM_ID=${SWARM_ID:-$(apij GET /api/v1/swarms | jq -r '.items[0].id')}
	rec "hosts (capacity)" jq_api /api/v1/hosts '.items[] | {hostname, role, nanoCpus, memoryBytes}'
	local ops_before ops_after cl_before cl_after
	ops_before=$(apij GET /api/v1/operations | jq '.items | length')
	cl_before=$(apij GET /api/v1/clusters | jq '.items | length')
	rec "3 GiB per node next to e4db (needs 3 GiB free per host; must be refused)" api POST /api/v1/clusters "$(cluster_body e4big "$MARIADB_IMAGE" 3221225472 3)" || true
	rec "16 GiB per node (larger than any host; must be refused)" api POST /api/v1/clusters "$(cluster_body e4huge "$MARIADB_IMAGE" 17179869184 3)" || true
	rec "5 nodes on 3 hosts (must be refused)" api POST /api/v1/clusters "$(cluster_body e4five "$MARIADB_IMAGE" 268435456 5)" || true
	ops_after=$(apij GET /api/v1/operations | jq '.items | length')
	cl_after=$(apij GET /api/v1/clusters | jq '.items | length')
	result "operations $ops_before -> $ops_after, clusters $cl_before -> $cl_after after the refused plans"
	rec "no services of the refused clusters" sh -c "limactl shell --workdir / $M1 -- sudo docker service ls --format '{{.Name}}' | grep -E 'e4big|e4huge|e4five' || echo none"
}

phase_failcreate() {
	section "Phase failcreate: create with an unpullable image, abort, delete (discard), create the same name again"
	hostload
	# Every expected refusal and outcome below is asserted (assert_fail)
	# instead of left to set -e: re-run A2 exited 1 at the refused delete
	# (delete_cluster returned 1 under set -e) and lost the later phases.
	local bad="mariadb:11.4.13@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if ! create_cluster e4fail "$bad"; then
		assert_fail failcreate "create of e4fail with the unpullable image was not accepted"
		return 0
	fi
	local op=$CREATED_OP fid i resp aborted=0
	fid=$(cid_of e4fail)
	for i in $(seq 1 18); do
		say "failing create t+$((i * 10))s: $(opline "$op")"
		sleep 10
	done
	rec "failing create (progress while it polls)" opjson "$op" || true
	op_diag "$op" dbcm-e4fail-
	rec "objects of e4fail before the abort" owned_objects e4fail || true
	rec "abort the failing create" api POST "/api/v1/operations/$op/abort" '{}' || assert_fail failcreate "abort request refused"
	if wait_op "$op" 300 10; then
		if [ "$OPSTATE" = aborted ]; then
			aborted=1
		else
			assert_fail failcreate "create ended $OPSTATE after the abort, expected aborted"
		fi
	else
		assert_fail failcreate "create $op not finished 300 s after the abort"
		op_diag "$op" dbcm-e4fail-
	fi
	rec "aborted create" opjson "$op" || true
	resp=$(api POST /api/v1/clusters "$(cluster_body e4fail "$MARIADB_IMAGE" 1073741824 3)" 2>&1) || true
	printf '### create the same name while the failed one exists (must be refused)\n\n```\n%s\n```\n\n' "$resp" >>"$LOG"
	case "$resp" in
	*cluster_exists*) result "re-create of the existing name refused (cluster_exists)" ;;
	*) assert_fail failcreate "re-create of e4fail was not refused with cluster_exists: $(printf '%s' "$resp" | head -c 200)" ;;
	esac
	if ! delete_cluster "$fid"; then
		if [ "$aborted" = 1 ]; then
			assert_fail failcreate "deletion of the aborted create refused: $(tail -n 4 "$LOG" | tr '\n' ' ' | head -c 200)"
		else
			# Expected consequence of the failed abort: the create still
			# holds the cluster (busy). Asserted above; the rest of the
			# phase needs the deletion.
			result "deletion refused while the create has not ended (consequence of the failed abort)"
		fi
		return 0
	fi
	sleep 10
	rec "deletion before approval (must wait)" opjson "$DEL_OP" || true
	if rec "approve with a wrong digest (must be refused)" api POST "/api/v1/operations/$DEL_OP/approve" '{"digest":"0000000000000000000000000000000000000000000000000000000000000000"}'; then
		assert_fail failcreate "approval with a wrong digest was accepted"
	fi
	approve_digest "$DEL_OP" "$DEL_DIGEST" || assert_fail failcreate "approval with the digest refused"
	wait_op "$DEL_OP" 900 5 || true
	rec "deletion of the failed create (final view)" opjson "$DEL_OP" || true
	result "deletion of the failed create ended: $OPSTATE"
	rec "objects of e4fail after the deletion" owned_objects e4fail || true
	rec "clusters after the deletion" jq_api /api/v1/clusters '.items[] | {id, name, state}' || true
	if [ "$OPSTATE" != succeeded ]; then
		assert_fail failcreate "deletion of the failed create ended $OPSTATE"
		op_diag "$DEL_OP" dbcm-e4fail-
		return 0
	fi
	if ! create_cluster e4fail "$MARIADB_IMAGE"; then
		assert_fail failcreate "create of the reused name e4fail was not accepted"
		return 0
	fi
	wait_op "$CREATED_OP" 1500 10 || true
	rec "create of the reused name (final view)" opjson "$CREATED_OP" || true
	result "create of the reused name e4fail ended: $OPSTATE"
	if [ "$OPSTATE" != succeeded ]; then
		assert_fail failcreate "create of the reused name ended $OPSTATE"
		op_diag "$CREATED_OP" dbcm-e4fail-
	fi
}

phase_cleanuprestart() {
	section "Phase cleanuprestart: delete of the healthy e4fail; manager SIGKILL and agent kill mid-deletion"
	hostload
	local fid op
	fid=$(cid_of e4fail)
	if [ -z "$fid" ]; then
		assert_fail cleanuprestart "no cluster e4fail (failcreate did not create the reused name); phase skipped"
		return 0
	fi
	rec "objects of e4fail before the deletion" owned_objects e4fail || true
	if ! delete_cluster "$fid"; then
		assert_fail cleanuprestart "deletion of e4fail refused; phase skipped"
		return 0
	fi
	op=$DEL_OP
	approve_digest "$op" "$DEL_DIGEST" || assert_fail cleanuprestart "approval with the digest refused"
	local start done=0
	start=$(date +%s)
	while [ $(($(date +%s) - start)) -lt 120 ]; do
		done=$(opjson "$op" | jq -r '.progress.stepsDone // 0')
		[ "$done" -ge 1 ] && break
		sleep 1
	done
	say "deletion progress at the kill: $(opline "$op")"
	rec "objects at the kill" owned_objects e4fail
	rec "SIGKILL the manager process" vm_args "$M1" 'sudo systemctl kill -s KILL "$1"; sleep 1; systemctl is-active "$1" || true' "$UNIT"
	# Kill the agent on m3 too (Swarm restarts it); its volume step must retry.
	rec "kill the agent container on m3" vm_sh dbcm-lab-m3 "sudo docker kill \$(sudo docker ps -q -f label=com.docker.swarm.service.name=dbcm-agent-$INST)"
	rec "objects while the manager is down" owned_objects e4fail
	sleep 5
	rec "restart the manager (same config and data)" mgr_restart "$M1"
	wait_until 120 "manager leads again" api GET /api/v1/setup/status
	wait_op "$op" 900 5 || true
	rec "deletion after the restart (final view)" opjson "$op"
	result "deletion across a manager SIGKILL and an agent kill ended: $OPSTATE"
	[ "$OPSTATE" = succeeded ] || assert_fail cleanuprestart "deletion across the restart ended $OPSTATE"
	rec "objects of e4fail after the deletion" owned_objects e4fail
	rec "e4db untouched" owned_objects "$CLUSTER_NAME"
	rec "e4db health" jq_api "/api/v1/clusters/$CLUSTER_ID" '{name, state, nodes: [.nodes[] | "\(.id) \(.state)"]}'
	rec "manager log around the restart" vm_args "$M1" 'sudo journalctl -u "$1" --no-pager | grep -v "setup token" | grep -i -E "resum|taken|operation|leader" | tail -20' "$UNIT" || true
	rec "e4fail volume directories kept on the hosts (documented: host data stays)" sh -c "for v in $VMS; do echo \$v; limactl shell --workdir / \$v -- sudo sh -c 'ls -d $VOLBASE/dbcm-e4fail-* 2>/dev/null || true'; done"
}

phase_diskfull() {
	section "Phase diskfull: the volume filesystem of the node on m3 fills up"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	local n slot="" w=1
	for n in 1 2 3; do [ "$(node_vm "$n")" = dbcm-lab-m3 ] && slot=$n; done
	[ -n "$slot" ] || { result "no e4db node on m3"; return 1; }
	[ "$slot" = 1 ] && w=2
	say "node on m3: n$slot; writes go to n$w"
	rec "volume filesystem on m3 before" vm_root dbcm-lab-m3 'df -m "$1" | tail -1; du -sm "$1"/* 2>/dev/null' "$VOLBASE"
	rec "fill it, leaving about 20 MiB" vm_root dbcm-lab-m3 'avail=$(df -m --output=avail "$1" | tail -1 | tr -d " "); fallocate -l "$((avail - 20))M" "$1/dbcm-e4r-filler"; df -m "$1" | tail -1' "$VOLBASE"
	rec "table for the workload" rsql "$w" "CREATE TABLE IF NOT EXISTS e4.big (id INT AUTO_INCREMENT PRIMARY KEY, pad LONGBLOB)"
	local t0 i
	t0=$(date +%s)
	for i in $(seq 1 40); do
		if ! with_timeout 30 rsql "$w" "INSERT INTO e4.big (pad) VALUES (REPEAT('x', 1048576))" >/dev/null 2>>"$LOG"; then
			say "insert $i failed or timed out at t+$(($(date +%s) - t0))s"
		fi
	done
	say "workload of 40 x 1 MiB done at t+$(($(date +%s) - t0))s"
	for i in $(seq 1 12); do
		say "t+$(($(date +%s) - t0))s cluster: $(apij GET "/api/v1/clusters/$CLUSTER_ID" | jq -c '{state, nodes: [.nodes[] | "\(.id) \(.state)/\(.role // "")"]}') size(n$w)=$(rsql "$w" "SHOW GLOBAL STATUS LIKE 'wsrep_cluster_size'" | awk '{print $2}')"
		sleep 15
	done
	rec "n$slot tasks while full" vm_docker "$M1" service ps --format '{{.Name}} {{.Node}} {{.CurrentState}} {{.Error}}' "$(svc "$slot")"
	rec "n$slot log while full" vm_sh dbcm-lab-m3 "c=\$(sudo docker ps -aq --no-trunc -f label=com.docker.swarm.service.name=$(svc "$slot") | head -1); sudo docker logs --tail 30 \$c 2>&1 | cut -c1-220"
	rec "events while full" jq_api /api/v1/events '.items[-6:][] | {severity, message}'
	rec "free the space" vm_root dbcm-lab-m3 'rm -f "$1/dbcm-e4r-filler"; df -m "$1" | tail -1' "$VOLBASE"
	t0=$(date +%s)
	for i in $(seq 1 24); do
		local st
		st=$(apij GET "/api/v1/clusters/$CLUSTER_ID" | jq -c '{state, nodes: [.nodes[] | "\(.id) \(.state)/\(.role // "")"]}')
		say "after freeing t+$(($(date +%s) - t0))s cluster: $st"
		case "$st" in *'"state":"healthy"'*) [ "$i" -gt 2 ] && break ;; esac
		sleep 15
	done
	rec "n$slot after freeing" rsql "$slot" "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_local_state_comment'); SELECT COUNT(*) FROM e4.big" || true
	rec "n$slot tasks after freeing" vm_docker "$M1" service ps --format '{{.Name}} {{.Node}} {{.CurrentState}} {{.Error}}' "$(svc "$slot")"
	rec "events after freeing" jq_api /api/v1/events '.items[-6:][] | {severity, message}'
	rec "drop the workload table" rsql "$w" "DROP TABLE e4.big" || true
}

phase_sentinels() {
	section "Phase sentinels: non-owned objects unchanged"
	local after
	after=$(sentinel_snapshot_r)
	printf '### sentinel snapshot after\n\n```\n%s\n```\n\n' "$after" >>"$LOG"
	if [ -n "$SENT_BEFORE" ] && [ "$after" = "$SENT_BEFORE" ]; then result "sentinel objects unchanged"; else result "sentinel objects CHANGED or no snapshot before"; fi
}

phase_deleteall() {
	section "Phase deleteall: delete e4db through the API (healthy cluster, second deletion)"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	if ! delete_cluster "$CLUSTER_ID"; then
		assert_fail deleteall "deletion of $CLUSTER_NAME refused"
		rec "clusters" api GET /api/v1/clusters || true
		return 0
	fi
	approve_digest "$DEL_OP" "$DEL_DIGEST" || assert_fail deleteall "approval with the digest refused"
	wait_op "$DEL_OP" 900 10 || true
	result "deletion of $CLUSTER_NAME ended: $OPSTATE"
	[ "$OPSTATE" = succeeded ] || assert_fail deleteall "deletion of $CLUSTER_NAME ended $OPSTATE"
	rec "objects of $CLUSTER_NAME after the deletion" owned_objects "$CLUSTER_NAME"
	rec "clusters" api GET /api/v1/clusters
	rec "events (last 8)" jq_api /api/v1/events '.items[-8:][] | {severity, message}'
	CLUSTER_ID=""
}

# ---------------------------------------------------------------- part B
HA_NAME=e4ha
phase_hareset() {
	section "Phase hareset: stop the single manager, remove its agents, promote m2/m3 to Swarm managers"
	hostload
	rec "stop the manager" vm_args "$M1" 'sudo systemctl stop "$1" 2>/dev/null; systemctl is-active "$1" || true' "$UNIT"
	rec "remove the agent service and enrollment secrets of the first installation" vm_args "$M1" '
		sudo docker service rm "dbcm-agent-$1" 2>/dev/null || true
		for s in $(sudo docker secret ls --format "{{.Name}}" | grep "^dbcm-et1\." || true); do sudo docker secret rm "$s" >/dev/null 2>&1 || true; done
		sudo docker secret ls --format "{{.Name}}"' "$INST"
	local vm
	for vm in $VMS; do
		rec "clear agent state and manager data on $vm" vm_root "$vm" 'rm -rf /var/lib/dbcm-agent/* /var/lib/dbcm-e4; ls -A /var/lib/dbcm-agent | wc -l'
	done
	rec "remove the old client state" vm_root "$M1" 'rm -f "$1/client.json" "$1/setup.token"' "$STATE"
	rec "promote m2 and m3" vm_docker "$M1" node promote lima-dbcm-lab-m2 lima-dbcm-lab-m3
	sleep 10
	rec "docker node ls" vm_docker "$M1" node ls
}

phase_hamanager() {
	section "Phase hamanager: three manager replicas (m1..m3), replicated keyring, peer mTLS, metastore over user-v2"
	hostload
	HA_STARTED=1
	local a1 a2 a3 pub vm i a peers
	a1=$(uv2_ip dbcm-lab-m1) a2=$(uv2_ip dbcm-lab-m2) a3=$(uv2_ip dbcm-lab-m3)
	say "replica addresses: $a1 $a2 $a3"
	peers="[{\"id\":\"manager-1\",\"address\":\"$a1:$META_PORT\"},{\"id\":\"manager-2\",\"address\":\"$a2:$META_PORT\"},{\"id\":\"manager-3\",\"address\":\"$a3:$META_PORT\"}]"
	pub=$(vm_sh "$M1" "sudo cat $STATE/recipient.pub")
	# The documented three-replica variant (docs/INSTALL.md): keyring (the
	# first leader creates the data key; the others are registered as
	# recipients in hasetup) and metastore peer mTLS with certificates from
	# manager -issue-peer-certs. The CA passphrase is random and discarded;
	# ca.sealed stays on m1 and is deleted with the VM.
	rec "no shared data key file (keyring mode)" vm_root "$M1" 'rm -f /etc/dbcm-e4/data.key; ls /etc/dbcm-e4 2>/dev/null || true'
	rec "peer certificates for manager-1..3 on m1" vm_root "$M1" '
		set -e
		rm -rf "$1/peers"
		DBCM_PEER_CA_PASSPHRASE=$(head -c 24 /dev/urandom | base64) /usr/local/bin/dbcm-e4-manager -issue-peer-certs "$1/peers" -installation "$2" -peer-ids manager-1,manager-2,manager-3
		install -d -m 0700 /etc/dbcm-e4/peer
		cp "$1/peers/manager-1/ca.pem" "$1/peers/manager-1/peer.pem" "$1/peers/manager-1/peer-key.pem" /etc/dbcm-e4/peer/
		ls -l /etc/dbcm-e4/peer | cut -c1-10,40-' "$STATE" "$INST"
	i=1
	for vm in dbcm-lab-m2 dbcm-lab-m3; do
		i=$((i + 1))
		rec "copy binaries to $vm" sh -c "limactl copy '$BIN/manager' '$BIN/e4client' $vm:/tmp/ && limactl shell --workdir / $vm -- sudo sh -c 'install -m 0755 /tmp/manager /usr/local/bin/dbcm-e4-manager && install -m 0755 /tmp/e4client /usr/local/bin/dbcm-e4-client && rm -f /tmp/manager /tmp/e4client'"
		rec "copy peer certificate of manager-$i and backend config to $vm (host pipe, not stored on the host)" sh -c \
			"limactl shell --workdir / $M1 -- sudo tar -C ${STATE}/peers/manager-$i -cf - ca.pem peer.pem peer-key.pem | limactl shell --workdir / $vm -- sudo sh -c 'rm -f /etc/dbcm-e4/data.key; install -d -m 0700 /etc/dbcm-e4/peer && tar -C /etc/dbcm-e4/peer -xf - && ls -l /etc/dbcm-e4/peer | cut -c1-10,40-' &&
			limactl shell --workdir / $M1 -- sudo tar -C / -cf - ${STATE#/}/backend.json | limactl shell --workdir / $vm -- sudo sh -c 'install -d -m 0700 $STATE && tar -C / -xf - && ls -l $STATE/backend.json | cut -c1-10,40-'"
	done
	i=0
	for vm in $VMS; do
		i=$((i + 1))
		a=$(uv2_ip "$vm")
		rec "replica config manager-$i on $vm" vm_root "$vm" '
			set -e
			install -d -m 0700 /etc/dbcm-e4 /var/lib/dbcm-e4
			cat <<EOF | tee /etc/dbcm-e4/manager.json
{
  "auth": {"enabled": true, "rpId": "localhost", "rpDisplayName": "E4 lab", "origins": ["http://localhost:$2"]},
  "manager": {
    "nodeId": "manager-${11}", "dataDir": "/var/lib/dbcm-e4", "keyring": true,
    "peerTLS": {"installation": "$8", "caFile": "/etc/dbcm-e4/peer/ca.pem", "certFile": "/etc/dbcm-e4/peer/peer.pem", "keyFile": "/etc/dbcm-e4/peer/peer-key.pem"},
    "bind": "$1:$3", "advertise": "$1:$3", "peers": ${12}, "bootstrap": true, "autoResume": true,
    "agentTransport": {
      "enrollListen": "$1:$4", "managerUrl": "https://$1:$4", "agentPort": 7443,
      "monitorUser": "dbcm_monitor", "monitorSecret": "$5", "dbSecrets": ["$6", "$7"]
    },
    "backup": {"backendConfigFile": "$9/backend.json", "scheduleIntervalSeconds": 30},
    "settings": {
      "installationId": "$8", "volumeBase": "/var/lib/dbcm/volumes", "agentImage": "dbcm-agent:e4",
      "accounts": {"monitorUser": "dbcm_monitor", "monitorSecret": "$5", "backupSecret": "$7"},
      "backup": {"jobImage": "dbcm-job:e4", "backendSecret": "dbcm-e4-backend", "dbCredentialSecret": "dbcm-e4-backup-cred",
                 "recipientKey": "${10}", "keySecret": "dbcm-e4-recovery-key", "inspectionNetwork": "dbcm-e4-inspect"}
    }
  }
}
EOF' "$a" "$API_PORT" "$META_PORT" "$ENROLL_PORT" "$MON_SECRET" "$ROOT_SECRET" "$BAK_SECRET" "$INST" "$STATE" "$pub" "$i" "$peers"
	done
	for vm in $VMS; do rec "start replica on $vm" mgr_restart "$vm"; done
	# On a timeout the exit trap captures the replica logs (ha_logs).
	wait_until 120 "a replica leads" find_leader
	say "leader VM: $APIVM"
	rec "keyring bootstrap and leadership in the leader log" vm_args "$APIVM" 'sudo journalctl -u "$1" --no-pager | grep -E "leadership|keyring|data key" | grep -v "setup token" | tail -10' "$UNIT" || true
	for vm in $VMS; do
		rec "replica on $vm: setup/status code and leader header" vm_sh "$vm" "curl -s -D - -o /dev/null http://127.0.0.1:$API_PORT/api/v1/setup/status | grep -i -E '^HTTP|x-dcm-leader' | tr -d '\r'"
	done
	rec "metastore listeners" sh -c "for v in $VMS; do limactl shell --workdir / \$v -- sudo ss -ltnp | grep -E ':$META_PORT|:$API_PORT|:$ENROLL_PORT' | sed \"s/^/\$v /\"; done"
}
# ha_logs WHY — capture every replica's manager log (journal of the unit)
# and its state before the exit trap deletes the VMs (re-run B lost them).
# Written to tests/p0/results/<log name>-replicas.log; setup tokens are
# redacted.
HA_STARTED=0
ha_logs() {
	local vm out
	out="${LOG%.log}-replicas.log"
	{
		printf '# Manager replica logs (%s), %s\n\n' "$1" "$(date -u +%FT%TZ)"
		for vm in $VMS; do
			printf '## %s\n\n```\n' "$vm"
			limactl shell --workdir / "$vm" -- sudo sh -c 'systemctl status "$1" --no-pager 2>&1 | head -5; ls -la /var/lib/dbcm-e4 /etc/dbcm-e4 /etc/dbcm-e4/peer 2>&1; journalctl -u "$1" --no-pager -n 300 2>&1' sh "$UNIT" 2>&1 |
				sed -E 's/(setup token[^:]*: )[^ ]*/\1<redacted>/' || true
			printf '```\n\n'
		done
	} >>"$out"
	sanitize_log "$out"
	say "replica logs ($1) written to ${out#"$REPO_ROOT"/}"
}
find_leader() {
	local vm
	for vm in $VMS; do
		if leads_on "$vm"; then APIVM=$vm; return 0; fi
	done
	return 1
}
sync_client() { # copy the client state (passkey, cookies) from APIVM to the other VMs
	local vm
	for vm in $VMS; do
		[ "$vm" = "$APIVM" ] && continue
		limactl shell --workdir / "$APIVM" -- sudo tar -C / -cf - "${STATE#/}/client.json" | limactl shell --workdir / "$vm" -- sudo sh -c "install -d -m 0700 $STATE && tar -C / -xf -"
	done
}

phase_hasetup() {
	section "Phase hasetup: first administrator on the leader replica; client state copied to the other VMs"
	find_leader
	say "leader VM: $APIVM"
	vm_args "$APIVM" 'sudo journalctl -u "$1" --no-pager | grep -o "setup token[^:]*: [^ ]*" | tail -1 | awk "{print \$NF}" | sudo tee "$2/setup.token" >/dev/null; sudo chmod 0600 "$2/setup.token"' "$UNIT" "$STATE"
	rec "setup on the leader $APIVM" vm_args "$APIVM" 'sudo /usr/local/bin/dbcm-e4-client -state "$1/client.json" -base "http://127.0.0.1:$2" -origin "http://localhost:$2" setup -token-file "$1/setup.token" -account admin' "$STATE" "$API_PORT"
	sync_client
	rec "session on the leader" api GET /api/v1/auth/session
	# Keyring: register the other replicas as data-key recipients (each
	# prints its public identity), so that any of them can lead later.
	local vm i key
	i=0
	for vm in $VMS; do
		i=$((i + 1))
		[ "$vm" = "$APIVM" ] && continue
		key=$(vm_sh "$vm" "sudo /usr/local/bin/dbcm-e4-manager -print-replica-key -config /etc/dbcm-e4/manager.json" | awk '{print $5}')
		say "replica on $vm: public key ${key:0:20}..."
		local id=manager-$i
		rec "register $id ($vm) as a data-key recipient" api PUT "/api/v1/settings/replicas/$id/data-key-recipient" "{\"publicKey\":\"$key\"}" ||
			assert_fail hasetup "registration of $id refused"
	done
	rec "keyring status (every replica holds the current key)" jq_api /api/v1/settings/replicas '{currentKey, recoverable, replicas: [.replicas[] | {id, leader, hasCurrentKey}]}' || true
	if [ "$(apij GET /api/v1/settings/replicas | jq '[.replicas[] | select(.hasCurrentKey)] | length')" != 3 ]; then
		assert_fail hasetup "not every replica holds the current data key"
	fi
}

phase_haswarm() {
	find_leader
	phase_swarmapi
	rec "hosts (all Swarm managers now)" jq_api /api/v1/hosts '.items[] | {hostname, role, agentIdentity}'
}

phase_hacreate() {
	section "Phase hacreate: create e4ha; the leader replica is SIGKILLed while the operation runs"
	hostload
	find_leader
	local old=$APIVM op t0 newleader=""
	CLUSTER_NAME=$HA_NAME
	create_cluster "$HA_NAME" "$MARIADB_IMAGE"
	op=$CREATED_OP
	CLUSTER_ID=$(cluster_id)
	local start
	start=$(date +%s)
	while [ $(($(date +%s) - start)) -lt 300 ]; do
		[ "$(opjson "$op" | jq -r '.progress.stepsDone // 0')" -ge 4 ] && break
		sleep 2
	done
	say "progress at the kill: $(opline "$op")"
	rec "services at the kill" owned_objects "$HA_NAME"
	sync_client
	t0=$(date +%s)
	rec "SIGKILL the leader replica on $old" vm_args "$old" 'sudo systemctl kill -s KILL "$1"; sleep 1; systemctl is-active "$1" || true' "$UNIT"
	while [ $(($(date +%s) - t0)) -lt 120 ]; do
		local vm
		for vm in $VMS; do
			[ "$vm" = "$old" ] && continue
			if leads_on "$vm"; then newleader=$vm; break 2; fi
		done
		sleep 1
	done
	[ -n "$newleader" ] || { result "no new leader within 120 s"; return 1; }
	result "new leader $newleader answers the API $(($(date +%s) - t0)) s after the SIGKILL of the leader on $old"
	APIVM=$newleader
	rec "session on the new leader (replicated session)" api GET /api/v1/auth/session
	wait_op "$op" 1500 10 || true
	rec "create after the failover (final view)" opjson "$op"
	result "create across a leader SIGKILL ended: $OPSTATE"
	rec "objects of e4ha (exactly 3 services, one volume per slot)" owned_objects "$HA_NAME"
	rec "tasks of e4ha" vm_sh "$M1" "sudo docker service ps --format '{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}}' \$(sudo docker service ls -q --filter label=dbcm.cluster)"
	rec "restart the old leader replica on $old" mgr_restart "$old"
	sleep 15
	rec "old leader rejoined as follower" vm_sh "$old" "curl -s -D - -o /dev/null http://127.0.0.1:$API_PORT/api/v1/setup/status | grep -i -E '^HTTP|x-dcm-leader' | tr -d '\r'"
	rec "operations (no duplicate create)" jq_api /api/v1/operations '.items[] | {id, kind, state, cluster}'
	[ "$OPSTATE" = succeeded ]
}

phase_hafailover() {
	section "Phase hafailover: database serving during a second leader loss; a backup across it"
	hostload
	find_leader
	CLUSTER_NAME=$HA_NAME
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	local n
	for n in 1 2 3; do
		rec "e4ha n$n wsrep" rsql "$n" "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_local_state_comment')"
	done
	rec "table on e4ha" rsql 1 "CREATE DATABASE IF NOT EXISTS e4; CREATE TABLE IF NOT EXISTS e4.t (id INT PRIMARY KEY, v VARCHAR(64))"
	local old=$APIVM ok=0 fail=0 i t0 newleader="" resp bop
	resp=$(api POST "/api/v1/clusters/$CLUSTER_ID/backups") || true
	printf '### start backup before the kill\n\n```\n%s\n```\n\n' "$resp" >>"$LOG"
	bop=$(printf '%s\n' "$resp" | tail -n +2 | jq -r '.id // .operation.id // empty')
	sync_client
	t0=$(date +%s)
	rec "SIGKILL the leader replica on $old" vm_args "$old" 'sudo systemctl kill -s KILL "$1"' "$UNIT"
	for i in $(seq 1 30); do
		if rsql 2 "REPLACE INTO e4.t (id, v) VALUES ($i, 'during leader loss')" >/dev/null 2>&1; then ok=$((ok + 1)); else fail=$((fail + 1)); fi
		if [ -z "$newleader" ]; then
			local vm
			for vm in $VMS; do [ "$vm" != "$old" ] && leads_on "$vm" && { newleader=$vm; say "new leader $vm after $(($(date +%s) - t0)) s"; break; }; done
		fi
	done
	result "writes during the leader loss: $ok ok, $fail failed over $(($(date +%s) - t0)) s"
	[ -n "$newleader" ] || { wait_until 120 "a replica leads" find_leader; newleader=$APIVM; }
	APIVM=$newleader
	[ -z "$bop" ] || { wait_op "$bop" 900 10 || true; rec "backup across the leader loss (final view)" opjson "$bop"; result "backup started before the leader SIGKILL ended: $OPSTATE"; }
	rec "rows on n3" rsql 3 "SELECT COUNT(*) FROM e4.t"
	rec "restart the old leader replica on $old" mgr_restart "$old"
	sleep 10
	for n in $VMS; do
		rec "replica on $n" vm_sh "$n" "curl -s -D - -o /dev/null http://127.0.0.1:$API_PORT/api/v1/setup/status | grep -i -E '^HTTP|x-dcm-leader' | tr -d '\r'"
	done
}

phase_hadelete() {
	section "Phase hadelete: delete e4ha through the replicated manager"
	find_leader
	CLUSTER_NAME=$HA_NAME
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	delete_cluster "$CLUSTER_ID"
	approve_digest "$DEL_OP" "$DEL_DIGEST"
	wait_op "$DEL_OP" 900 10 || true
	result "deletion of e4ha ended: $OPSTATE"
	rec "objects of e4ha after the deletion" owned_objects "$HA_NAME"
	phase_sentinels
	rec "host resources at the end" sh -c "for v in $VMS; do echo \$v; limactl shell --workdir / \$v -- sh -c 'free -m | head -2; df -h / | tail -1; uptime'; done"
}

A_STEPS="build vms swarm images prereqs rprep backend manager setup swarmapi rcluster verify rfailure rbackup rinspect capacity failcreate cleanuprestart diskfull sentinels deleteall"
B_STEPS="hareset hamanager hasetup haswarm hacreate hafailover hadelete"
for p in $A_STEPS $B_STEPS; do
	if step "$p"; then
		case "$p" in vms | swarm) [ "${E4_REUSE_VMS:-0}" = 1 ] && continue ;; esac
		"phase_$p"
	fi
done
