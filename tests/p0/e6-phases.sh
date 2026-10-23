# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2016
#
# E6 phases, sourced by tests/p0/e5-e6-proxy-outage.sh after the E4 library.
#
# Two full-cluster outages, each stopped by the operator outside the
# manager (the manager has no "stop cluster" command):
#   e6clean: every service scaled to 0 at once (graceful shutdown; Galera
#            writes the seqno and the last node to leave gets
#            safe_to_bootstrap: 1).
#   e6crash: every mariadbd SIGKILLed at once, then every service scaled to
#            0 within the Swarm restart delay so nothing restarts (seqno -1
#            everywhere, as after a power loss).
# The manager must diagnose (read-only), must not start anything by itself,
# and recovery must wait for an explicit, digest-bound approval.

E6_STEPS="e6clean e6diagclean e6recoverclean e6crash e6diagcrash e6recovercrash"

E6_OBSERVE=${E6_OBSERVE:-120}

# phase_upgrade (development only, runs only when named in E5_PHASES):
# rebuild the images from freshly built binaries, restart the agents
# (docker service update --force: each node uses its newly loaded local
# image) and restart the manager from its data directory.
phase_upgrade() {
	case " ${E5_PHASES:-all} " in *" upgrade "*) ;; *) return 0 ;; esac
	section "Phase upgrade (development): new binaries for agents and manager on the kept lab"
	phase_images
	rec "restart agents with the new image" vm_docker "$M1" service update --force -q "dbcm-agent-$INST"
	rec "stop manager unit" vm_args "$M1" 'sudo systemctl stop "$1"; sleep 2; systemctl is-active "$1" || true' "$UNIT"
	rec "start manager unit (same config and data directory)" vm_args "$M1" 'sudo systemd-run --unit "$1" -p Restart=no --collect /usr/local/bin/dbcm-e4-manager -listen "127.0.0.1:$2" -config /etc/dbcm-e4/manager.json' "$UNIT" "$API_PORT"
	wait_until 60 "manager health" vm_sh "$M1" "curl -fsS http://127.0.0.1:$API_PORT/api/v1/health"
	wait_until 120 "manager leads (API not 503)" api GET /api/v1/setup/status
	wait_until 120 "all 3 agents enrolled (agentIdentity true)" agents_enrolled 3
	rec "session after the manager restart" api GET /api/v1/auth/session
}

e6_cluster() { CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}; }
e6_services() { echo "$(svc 1) $(svc 2) $(svc 3)"; }

# e6_grastate — ground truth: grastate.dat of every node, read as root on
# its host (the slot's host is fixed by the node constraint).
E6_HOSTS="" # "N:VM ..." recorded while the cluster still runs
e6_hosts() {
	local n
	E6_HOSTS=""
	# The slot's host is fixed by its node constraint: the newest task's node
	# is valid even while the service is scaled to 0.
	local host
	for n in 1 2 3; do
		host=$(vm_docker "$M1" service ps --format '{{.Node}}' "$(svc "$n")" | head -1)
		E6_HOSTS="$E6_HOSTS $n:${host#lima-}"
	done
	say "slot hosts:$E6_HOSTS"
}
e6_grastate() {
	local e n vm
	for e in $E6_HOSTS; do
		n=${e%%:*} vm=${e#*:}
		echo "== n$n on $vm"
		vm_root "$vm" 'cat "$1"/dbcm-'"$CLUSTER_NAME"'-n'"$n"'-*/data/grastate.dat; ls -l "$1"/dbcm-'"$CLUSTER_NAME"'-n'"$n"'-*/data/gvwstate.dat 2>/dev/null || echo "no gvwstate.dat"' "$VOLBASE"
	done
}
e6_running_tasks() { # number of running database tasks of the cluster
	vm_docker "$M1" service ps --format '{{.CurrentState}}' $(e6_services) 2>/dev/null | grep -c '^Running' || true
}
e6_none_running() { [ "$(e6_running_tasks)" = 0 ]; }
e6_specs() { # replicas and bootstrap flag of each service
	local s
	for s in $(e6_services); do
		vm_docker "$M1" service inspect -f '{{.Spec.Name}} replicas={{.Spec.Mode.Replicated.Replicas}} version={{.Version.Index}} bootstrap={{range .Spec.TaskTemplate.ContainerSpec.Args}}{{if eq . "--wsrep-new-cluster"}}yes{{end}}{{end}}' "$s"
	done
}
e6_ops() { apij GET /api/v1/operations | jq -c '.items[] | {id, kind, state, reason}'; }
e6_op() { apij GET /api/v1/operations | jq --arg id "$1" '.items[] | select(.id==$id)'; }
e6_events() { apij GET /api/v1/events | jq -c '.items[-10:][] | {severity, message}'; }
e6_positions() {
	local n
	for n in 1 2 3; do echo "n$n: $(rsql "$n" "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_last_committed','wsrep_cluster_size')" | tr '\n\t' '  ')"; done
}
e6_task_starts() {
	local s t
	for s in $(e6_services); do
		for t in $(vm_docker "$M1" service ps -q --filter desired-state=running "$s"); do
			vm_docker "$M1" inspect --format "$s {{.Status.State}} since {{.Status.Timestamp}}" "$t"
		done
	done
}
e6_node_starts() {
	local e n vm c
	for e in $E6_HOSTS; do
		n=${e%%:*} vm=${e#*:}
		c=$(cid "$n")
		echo "== n$n"
		vm_docker "$vm" logs --since 10m "$c" 2>&1 | grep -E "wsrep-new-cluster|Bootstrapping|bootstrap|Quorum results|Synchronized with group|SST|IST received|safe_to_bootstrap|Recovered position|restore pc" | cut -c1-200 | head -12
	done
}
E6_UUID=""
e6_uuid() { rsql "$1" "SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='wsrep_cluster_state_uuid'"; }
e6_rows() { rsql "$1" "SELECT COUNT(*) FROM e4.t; SELECT id, v FROM e4.t WHERE id >= 100 ORDER BY id"; }

e6_observe_hands_off() { # SECONDS — the manager must not start or change anything
	local before after i
	before=$(e6_specs)
	for i in $(seq 1 $(($1 / 15))); do
		sleep 15
		say "t+$((i * 15))s running tasks: $(e6_running_tasks); cluster: $(apij GET "/api/v1/clusters/$CLUSTER_ID" | jq -c '{state, nodes: [.nodes[] | "\(.id) \(.state)/\(.role // "")"]}')"
	done
	after=$(e6_specs)
	rec "service specs before the observation window" printf '%s\n' "$before"
	rec "service specs after the observation window" printf '%s\n' "$after"
	if [ "$before" = "$after" ] && e6_none_running; then
		result "hands off: no service spec changed and no database task ran for $1 s"
	else
		result "CHANGED during the observation window (see specs above)"
	fi
	rec "operations (no new operation may appear)" e6_ops
}

phase_e6clean() {
	section "Phase e6clean (E6): marker row, then every service scaled to 0 at once (operator, outside the manager)"
	hostload
	e6_cluster
	e6_hosts
	E6_UUID=$(e6_uuid 1)
	say "cluster state UUID before the clean outage: $E6_UUID"
	rec "marker row before the clean outage (n2)" rsql 2 "REPLACE INTO e4.t (id, v) VALUES (100, 'before clean outage')"
	rec "rows before (n1)" e6_rows 1
	rec "wsrep positions before" e6_positions
	rec "scale all three to 0 (one command)" vm_docker "$M1" service scale -d $(for s in $(e6_services); do printf '%s=0 ' "$s"; done)
	wait_until 180 "no database task running" e6_none_running
	rec "tasks" vm_docker "$M1" service ps --format '{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}}' $(e6_services)
	rec "ground truth: grastate.dat per node" e6_grastate
}

e6_diagnose() { # TITLE
	rec "$1" api GET "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" || true
}

phase_e6diagclean() {
	section "Phase e6diagclean (E6): manager diagnosis after the clean outage; the manager must stay hands off"
	hostload
	e6_cluster
	[ -n "$E6_HOSTS" ] || e6_hosts
	sleep 20
	rec "cluster via API" api GET "/api/v1/clusters/$CLUSTER_ID"
	e6_diagnose "outage diagnosis (clean)"
	e6_observe_hands_off "$E6_OBSERVE"
	e6_diagnose "outage diagnosis again (deterministic?)"
	rec "events" e6_events
}

# e6_recover CANDIDATE TITLE — guided recovery through the API with the
# digest-bound approval; waits for the operation.
e6_recover() {
	local cand=$1 resp op digest st start
	resp=$(api POST "/api/v1/clusters/$CLUSTER_ID/outage-recovery" "{\"candidate\":\"$cand\"}") || true
	printf '### request guided recovery (candidate %s)\n\n```\n%s\n```\n\n' "$cand" "$resp" >>"$LOG"
	case "$resp" in "HTTP 202"*) ;; *) result "recovery not started: $(printf '%s' "$resp" | head -3 | tr '\n' ' ' | cut -c1-300)"; return 1 ;; esac
	op=$(printf '%s\n' "$resp" | tail -n +2 | jq -r .id)
	digest=$(printf '%s\n' "$resp" | tail -n +2 | jq -r .approval.digest)
	sleep 20
	rec "operation before approval (must wait)" opstate "$op"
	rec "service specs before approval (nothing started)" e6_specs
	rec "approve with a wrong digest (must be refused)" api POST "/api/v1/operations/$op/approve" '{"digest":"0000000000000000000000000000000000000000000000000000000000000000"}' || true
	rec "approve with the digest" api POST "/api/v1/operations/$op/approve" "{\"digest\":\"$digest\"}"
	start=$(date +%s)
	while :; do
		st=$(apij GET /api/v1/operations | jq -r --arg id "$op" '.items[] | select(.id==$id) | "\(.state) \(.reason // "")"')
		say "recovery operation: $st | running tasks $(e6_running_tasks)"
		case "$st" in succeeded* | failed* | aborted*) break ;; esac
		[ $(($(date +%s) - start)) -lt 900 ] || { say "recovery not finished after 900 s"; break; }
		sleep 10
	done
	rec "recovery operation" e6_op "$op"
	# Neither the API nor the logs show the steps; the start order is read
	# from the Swarm tasks and the nodes' own logs.
	rec "start order: running tasks (state timestamp)" e6_task_starts
	rec "start order: bootstrap and join lines in the node logs" e6_node_starts
	case "$st" in succeeded*) return 0 ;; esac
	return 1
}

e6_verify_recovered() { # TITLE-SUFFIX ROW-ID
	local n
	for n in 1 2 3; do
		rec "n$n after recovery ($1)" rsql "$n" "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_local_state_comment','wsrep_cluster_status','wsrep_cluster_state_uuid','wsrep_last_committed')"
	done
	local u
	u=$(e6_uuid 1)
	if [ -n "$E6_UUID" ] && [ "$u" = "$E6_UUID" ]; then
		result "$1 recovery kept the cluster state UUID $u"
	else
		result "$1 recovery CHANGED the cluster state UUID: before ${E6_UUID:-unknown}, after $u"
	fi
	rec "rows after recovery ($1, n3)" e6_rows 3
	rec "write after recovery ($1, n1)" rsql 1 "REPLACE INTO e4.t (id, v) VALUES ($2, 'after $1 recovery'); SELECT COUNT(*) FROM e4.t"
	rec "service specs after recovery" e6_specs
	rec "cluster via API after recovery" api GET "/api/v1/clusters/$CLUSTER_ID"
	rec "ground truth after recovery" e6_grastate
}

phase_e6recoverclean() {
	section "Phase e6recoverclean (E6): guided recovery after explicit approval"
	hostload
	e6_cluster
	[ -n "$E6_HOSTS" ] || e6_hosts
	local cand
	cand=$(apij GET "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" | jq -r '.candidate // ""')
	say "diagnosis candidate: ${cand:-none}"
	[ -n "$cand" ] || { result "no candidate after the clean outage"; return 1; }
	rec "request with a candidate that differs from the diagnosis (must be refused)" api POST "/api/v1/clusters/$CLUSTER_ID/outage-recovery" \
		"{\"candidate\":\"$( [ "$cand" = n1 ] && echo n2 || echo n1)\"}" || true
	e6_recover "$cand"
	e6_verify_recovered clean 101
}

phase_e6crash() {
	section "Phase e6crash (E6): SIGKILL every mariadbd at once, then scale to 0 within the restart delay"
	hostload
	e6_cluster
	e6_hosts
	E6_UUID=$(e6_uuid 1)
	say "cluster state UUID before the crash: $E6_UUID"
	rec "marker row before the crash (n3)" rsql 3 "REPLACE INTO e4.t (id, v) VALUES (102, 'before crash')"
	local e n vm c pids=""
	for e in $E6_HOSTS; do
		n=${e%%:*} vm=${e#*:}
		c=$(cid "$n")
		limactl shell --workdir / "$vm" -- sudo docker kill --signal KILL "$c" >>"$LOG" 2>&1 &
		pids="$pids $!"
	done
	# shellcheck disable=SC2086
	wait $pids || true
	say "SIGKILL sent to all three at $(date -u +%H:%M:%S)"
	rec "scale all three to 0 right after the kill" vm_docker "$M1" service scale -d $(for s in $(e6_services); do printf '%s=0 ' "$s"; done)
	wait_until 120 "no database task running" e6_none_running
	rec "tasks (no restart may have happened)" vm_docker "$M1" service ps --format '{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' $(e6_services)
	rec "ground truth: grastate.dat per node" e6_grastate
}

phase_e6diagcrash() {
	section "Phase e6diagcrash (E6): manager diagnosis after the crash; the manager must stay hands off"
	hostload
	e6_cluster
	[ -n "$E6_HOSTS" ] || e6_hosts
	sleep 20
	e6_diagnose "outage diagnosis (crash, read-only GET)"
	e6_observe_hands_off "$E6_OBSERVE"
	rec "diagnosis with recovered positions (POST: mariadbd --wsrep-recover on each node)" api POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' || true
	rec "service specs after the position recovery (nothing started)" e6_specs
	rec "ground truth after the position recovery (grastate unchanged)" e6_grastate
	rec "one-off containers left on the hosts (must be none)" e6_leftover_containers
	rec "diagnosis with recovered positions again (same answer?)" api POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' || true
}

e6_leftover_containers() {
	local vm
	for vm in $VMS; do
		echo "$vm: $(vm_docker "$vm" ps -a -q --filter label=dbcm.job=wsrep-recover | wc -l | tr -d ' ') wsrep-recover container(s)"
	done
}

phase_e6recovercrash() {
	section "Phase e6recovercrash (E6): recovery after the crash"
	hostload
	e6_cluster
	[ -n "$E6_HOSTS" ] || e6_hosts
	local cand
	cand=$(apij POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' | jq -r '.candidate // ""')
	say "diagnosis candidate (recovered positions): ${cand:-none}"
	[ -n "$cand" ] || { result "no candidate after the crash even with recovered positions"; return 1; }
	rec "request with a candidate that differs from the diagnosis (must be refused)" api POST "/api/v1/clusters/$CLUSTER_ID/outage-recovery" \
		"{\"candidate\":\"$( [ "$cand" = n1 ] && echo n2 || echo n1)\"}" || true
	e6_recover "$cand"
	e6_verify_recovered crash 103
}
