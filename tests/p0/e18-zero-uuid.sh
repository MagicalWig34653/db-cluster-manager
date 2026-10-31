#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E18 part 2: the guided zero-UUID recovery (ADR 0028/0029) on
# a real cluster. Needs the kept VMs and the running cluster of part 1
# (tests/p0/e18-agent-blips.sh with E18_KEEP_VMS=1, cluster e18db).
#
#   A  default (autoRecover off): full power loss of all three VMs until the
#      zero-UUID Primary appears; the manager must do nothing by itself; a
#      human starts the recovery (POST), approves the prepare phase and then
#      the recovery phase with the digests.
#   B  autoRecover on (PUT .../auto-recover, administrator): the same loss;
#      the leader starts, approves and runs both phases by itself.
#
# Both check the data (sentinels), the new state UUID and the notifications
# in the UI inbox (GET /api/v1/notifications).
#
# Usage:  E4_REUSE_VMS=1 tests/p0/e18-zero-uuid.sh
#   E18_PHASES="a b"   default both
#   E18_KEEP_VMS=1     keep the VMs at the end
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${E18_LOG:-"$REPO_ROOT/tests/p0/results/e18-zero-uuid.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${E18_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
export E4_AUTORESUME=false
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"
. "$REPO_ROOT/tests/p0/e6-phases.sh"
. "$REPO_ROOT/tests/p0/e8-phases.sh"
CLUSTER_NAME=e18db

ASSERT_FAILS=""
assert_fail() { ASSERT_FAILS="$ASSERT_FAILS $1"; result "ASSERTION FAILED ($1): $2"; }
ok() { result "PASS ($1): $2"; }

finish18() {
	local rc=$?
	trap - EXIT INT TERM
	[ -z "$ASSERT_FAILS" ] || { say "failed assertions in:$ASSERT_FAILS"; [ "$rc" != 0 ] || rc=1; }
	if [ "${E18_KEEP_VMS:-0}" = 1 ]; then
		say "E18_KEEP_VMS=1: VMs kept; run bin/lab cleanup --apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" -e "s#PASSWORD '\*[0-9A-F]*'#PASSWORD '<hash redacted>'#g" "$LOG"
	say "E18 part 2 finished with exit $rc"
	exit "$rc"
}
trap finish18 EXIT
trap 'exit 130' INT TERM

# The passkey signature counter may roll back with a power loss: raise it.
e8_login() {
	rec "manager login (signature counter raised first)" vm_args "$M1" '
		sudo sh -c "jq \".counter += 50\" \"$1/client.json\" >\"$1/client.json.new\" && mv \"$1/client.json.new\" \"$1/client.json\"" || true
		sudo /usr/local/bin/dbcm-e4-client -state "$1/client.json" -base http://127.0.0.1:'"$API_PORT"' -origin http://localhost:'"$API_PORT"' login' "$STATE" || true
}
zero_state() {
	local s
	s=$(e8_status3 2>/dev/null) || true
	printf '%s\n' "$s" | grep -q 'wsrep_cluster_status Primary'
}
zero_uuid_seen() { # any node reports the zero state UUID
	local n u
	for n in 1 2 3; do
		u=$(rsql "$n" "SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='wsrep_cluster_state_uuid'" 2>/dev/null | tr -d '[:space:]') || continue
		[ "$u" = 00000000-0000-0000-0000-000000000000 ] && return 0
	done
	return 1
}
recstatus() { apij GET "/api/v1/clusters/$CLUSTER_ID/zero-uuid-recovery"; }
notifs() { apij GET /api/v1/notifications | jq -c '.items[] | select((.kind // .type // "") | test("recovery|zero")) | {time: (.time // .createdAt), kind: (.kind // .type), severity, title: (.title // .message)}' | cut -c1-300; }
events_recent() { apij GET /api/v1/events | jq -r '.items[] | select(.message | test("zero|recover|snapshot|bootstrap";"i")) | "\(.time) \(.severity) \(.message)"' | tail -25 | cut -c1-260; }

power_loss_all() {
	local vm t0 pids=""
	hostload
	rec "wsrep status before the power loss (UUID must be non-zero)" e8_status3
	t0=$(date +%s)
	for vm in $VMS; do
		limactl stop --force "$vm" >>"$LOG" 2>&1 &
		pids="$pids $!"
	done
	# shellcheck disable=SC2086
	wait $pids || true
	say "all three VMs force-stopped in $(($(date +%s) - t0)) s"
	sleep 20
	for vm in $VMS; do
		e8_vm_start "$vm" || { result "VM $vm did not start"; return 1; }
		wait_until 300 "$vm reachable over ssh" vm_sh "$vm" true || true
	done
	rec "restart the lab manager unit" vm_args "$M1" 'sudo systemd-run --unit "$1" -p Restart=no --collect /usr/local/bin/dbcm-e4-manager -listen "127.0.0.1:$2" -config /etc/dbcm-e4/manager.json' "$UNIT" "$API_PORT"
	wait_until 60 "manager health" vm_sh "$M1" "curl -fsS http://127.0.0.1:$API_PORT/api/v1/health" || true
	wait_until 180 "manager leads (API not 503)" api GET /api/v1/setup/status || true
	e8_login
}

# wait_zero_primary — a Primary with the zero UUID must appear (E9/E12: it did
# every time); record honestly if the cluster instead restored itself.
wait_zero_primary() {
	local i
	for i in $(seq 1 24); do
		if zero_uuid_seen; then return 0; fi
		if e8_synced3 2>/dev/null; then return 1; fi
		sleep 10
	done
	return 1
}

verify_recovered() { # TAG SENTINELS
	local u
	wait_until 600 "Galera cluster back to 3 synced nodes" e8_synced3 || true
	u=$(e6_uuid 1 2>/dev/null | tr -d '[:space:]' || true)
	rec "wsrep status after the recovery" e8_status3
	rec "sentinels on every node ($2 expected)" e8_sentinels_all "$2"
	if [ -n "$u" ] && [ "$u" != 00000000-0000-0000-0000-000000000000 ]; then ok "$1" "non-zero state UUID again ($u)"; else assert_fail "$1" "state UUID zero or unknown after the recovery"; fi
	e8_synced3 && ok "$1" "three nodes Synced" || assert_fail "$1" "not three Synced nodes"
	if e8_sentinels_all "$2" >/dev/null 2>&1; then ok "$1" "sentinels $2 on every node"; else assert_fail "$1" "sentinels differ from $2"; fi
}

phase_a() {
	section "Phase A: zero-UUID recovery, default (autoRecover off), approval-gated"
	e8_login
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	rec "recovery status before (autoRecover must be false)" recstatus
	[ "$(recstatus | jq -r .autoRecover)" = false ] && ok A "autoRecover is off by default" || assert_fail A "autoRecover not off by default"
	rec "sentinels" rsql 1 "CREATE DATABASE IF NOT EXISTS e8; CREATE TABLE IF NOT EXISTS e8.sentinel (id INT PRIMARY KEY, note VARCHAR(64), node VARCHAR(64), at TIMESTAMP(3) DEFAULT CURRENT_TIMESTAMP(3)); REPLACE INTO e8.sentinel (id, note, node) VALUES (1, 'A before the power loss', @@hostname), (2, 'A second', @@hostname)"
	e6_hosts
	power_loss_all || return 1
	if ! wait_zero_primary; then
		result "A: no zero-UUID Primary appeared (cluster restored itself or never formed); the recovery path was NOT exercised in this run"
		rec "wsrep status" e8_status3
		return 1
	fi
	rec "wsrep status with the zero UUID" e8_status3
	say "zero-UUID Primary present; observing 150 s hands off (autoRecover off)"
	local i
	for i in 1 2 3 4 5 6 7 8 9 10; do sleep 15; say "t+$((i * 15))s ops: $(e6_ops | wc -l | tr -d ' ') ; status: $(recstatus | jq -c '{autoRecover, prepare: .prepare.state, recovery: .recovery.state}' 2>/dev/null)"; done
	rec "operations after the observation window" e6_ops
	rec "events (incident)" events_recent
	rec "notifications about recovery or zero UUID" notifs
	if e6_ops | grep -q 'zero-uuid'; then assert_fail A "a zero-uuid operation exists although autoRecover is off"; else ok A "no recovery operation was started by the manager (autoRecover off)"; fi
	local out
	out=$(api GET "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" || true)
	printf '### outage diagnosis (GET) with the zero-UUID Primary\n\n```\n%s\n```\n\n' "$out" >>"$LOG"
	rec "operator: start the recovery (POST zero-uuid-recovery)" api POST "/api/v1/clusters/$CLUSTER_ID/zero-uuid-recovery" '{}'
	local prep digest
	sleep 3
	prep=$(recstatus | jq -r .prepare.id)
	digest=$(recstatus | jq -r .prepare.approval.digest)
	rec "approve prepare with a WRONG digest (must be refused)" api POST "/api/v1/operations/$prep/approve" '{"digest":"0000000000000000000000000000000000000000000000000000000000000000"}' || true
	rec "approve prepare with the digest" api POST "/api/v1/operations/$prep/approve" "{\"digest\":\"$digest\"}"
	approve_phase2 A
	verify_recovered A "1,2"
	rec "recovery status at the end" recstatus
	rec "notifications at the end" notifs
	rec "events at the end" events_recent
}

approve_phase2() { # TAG — wait for phase 2, approve it with its digest
	local i rid digest aw
	for i in $(seq 1 60); do
		rid=$(recstatus | jq -r '.recovery.id // ""')
		aw=$(recstatus | jq -r '.recovery.awaitingApproval // false')
		say "recovery run: prepare=$(recstatus | jq -r '.prepare.state') recovery=$(recstatus | jq -r '.recovery.state // "none"') awaiting=$aw"
		[ "$aw" = true ] && break
		sleep 10
	done
	if [ "$aw" != true ]; then assert_fail "$1" "phase 2 never awaited approval"; return 1; fi
	rec "phase 2 plan: candidate and diagnosis" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/clusters/$CLUSTER_ID/zero-uuid-recovery | tail -n +2 | jq -c '.recovery | {state, candidate, awaitingApproval, diagnosis}' | cut -c1-900"
	digest=$(recstatus | jq -r '.recovery.approval.digest')
	rec "approve phase 2 with the digest" api POST "/api/v1/operations/$rid/approve" "{\"digest\":\"$digest\"}"
	wait_for_done "$1"
}
wait_for_done() { # TAG
	local i st
	for i in $(seq 1 90); do
		st=$(recstatus | jq -r '.recovery.state // "none"')
		say "recovery operation: $st"
		case "$st" in succeeded | failed | aborted) break ;; esac
		sleep 10
	done
	[ "$st" = succeeded ] && ok "$1" "recovery operation succeeded" || assert_fail "$1" "recovery operation ended as $st"
}

phase_b() {
	section "Phase B: zero-UUID recovery with autoRecover on"
	e8_login
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	rec "enable autoRecover (administrator)" api PUT "/api/v1/clusters/$CLUSTER_ID/auto-recover" '{"enabled":true}'
	[ "$(recstatus | jq -r .autoRecover)" = true ] && ok B "autoRecover on" || assert_fail B "autoRecover not on"
	rec "audit/events of the opt-in" events_recent
	rec "sentinels" rsql 1 "REPLACE INTO e8.sentinel (id, note, node) VALUES (3, 'B before the power loss', @@hostname), (4, 'B second', @@hostname)"
	e6_hosts
	power_loss_all || return 1
	if ! wait_zero_primary; then
		result "B: no zero-UUID Primary appeared; the autoRecover path was NOT exercised in this run"
		rec "wsrep status" e8_status3
		return 1
	fi
	say "zero-UUID Primary present; waiting for the policy to act by itself (no human call from here on)"
	local i st
	for i in $(seq 1 120); do
		st=$(recstatus | jq -c '{autoRecover, prepare: {s: .prepare.state, auto: .prepare.auto}, recovery: {s: .recovery.state, auto: .recovery.auto, candidate: .recovery.candidate}}' 2>/dev/null || echo unavailable)
		say "t+$((i * 10))s $st"
		case "$(recstatus | jq -r '.recovery.state // "none"')" in succeeded | failed | aborted) break ;; esac
		sleep 10
	done
	rec "recovery status at the end" recstatus
	rec "operations" e6_ops
	rec "notifications (planned, ended)" notifs
	rec "events" events_recent
	if [ "$(recstatus | jq -r '.prepare.auto // false')" = true ] && [ "$(recstatus | jq -r '.recovery.auto // false')" = true ]; then ok B "both phases were approved by policy:autoRecover"; else assert_fail B "a phase was not auto-approved (see status)"; fi
	[ "$(recstatus | jq -r '.recovery.state // "none"')" = succeeded ] && ok B "autoRecover run succeeded" || assert_fail B "autoRecover run did not succeed"
	verify_recovered B "1,2,3,4"
	rec "disable autoRecover again" api PUT "/api/v1/clusters/$CLUSTER_ID/auto-recover" '{"enabled":false}'
}

for p in ${E18_PHASES:-a b}; do "phase_$p" || say "phase $p returned non-zero"; done
