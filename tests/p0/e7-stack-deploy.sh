#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 E7: deploy the manager as the Swarm stack deploy/stack.yaml (ADR 0014)
# from an image built with deploy/Dockerfile, with a configuration produced by
# `manager init` (run from that image). Everything that earlier experiments ran
# as plain processes now runs from the shipped files.
#
# Layout: dbcm-lab-m1..m3 (bin/lab, lima:user-v2, rootful Docker). m1 is the
# only Swarm manager node and hosts the pinned manager task; m2/m3 are workers.
# The source tree (git archive of HEAD) is copied into m1 and the image is built
# there (the base images are pulled from their registries; nothing is pushed);
# the image is copied to m2/m3 with `docker save | docker load` through the
# host. tests/p0/e4client drives the API like a browser (software passkey).
# The phases of tests/p0/e4-manager-galera.sh are reused as a library.
#
# Phases: build vms swarm images prereqs init stack verify agents cluster
#         restart drain peertls
# Usage:  tests/p0/e7-stack-deploy.sh
#   E7_KEEP_VMS=1  keep the VMs (run `bin/lab cleanup -apply` yourself)
#   E7_PHASES="..." run only these phases (E7_REUSE_VMS=1 skips vms/swarm)
#   E7_LOG=FILE    log file (default tests/p0/results/e7-stack-deploy.log)
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${E7_LOG:-"$REPO_ROOT/tests/p0/results/e7-stack-deploy.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${E7_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
export E4_AUTORESUME=false
# shellcheck source=tests/p0/e4-manager-galera.sh
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"

# The library's cluster and verify phases are kept under other names.
eval "$(declare -f phase_cluster | sed '1s/^phase_cluster/lib_cluster/')"
eval "$(declare -f phase_verify | sed '1s/^phase_verify/lib_verify/')"

# Overrides of the library settings: the secret and network names are the
# ones `manager init -installation e7lab` prints.
INST=e7lab
NET=dbcm-e7lab-net
ROOT_SECRET=dbcm-e7lab-root-pw
MON_SECRET=dbcm-e7lab-monitor-pw
BAK_SECRET=dbcm-e7lab-backup-pw
API_PORT=8080
IMAGE=db-cluster-manager:e7
SRC=/root/dbcm-e7-src
CONFIG_NAME=dbcm-manager-config-1
STACK=dbcm
SVC=${STACK}_manager
CLUSTER_NAME=e7db

ASSERT_FAILS=""
assert_fail() { # NAME MESSAGE
	ASSERT_FAILS="$ASSERT_FAILS $1"
	result "ASSERTION FAILED ($1): $2"
}
ok() { result "PASS ($1): $2"; }

finish7() {
	local rc=$?
	trap - EXIT INT TERM
	if [ -n "$ASSERT_FAILS" ]; then
		say "failed assertions in:$ASSERT_FAILS"
		[ "$rc" != 0 ] || rc=1
	fi
	if [ "${E7_KEEP_VMS:-0}" = 1 ]; then
		say "E7_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" -e 's#dbcm-et1\.[A-Za-z0-9._-]*#dbcm-et1.<redacted>#g' \
		-e "s#PASSWORD '\*[0-9A-F]*'#PASSWORD '<hash redacted>'#g" "$LOG"
	say "E7 finished with exit $rc"
	exit "$rc"
}
trap finish7 EXIT
trap 'exit 130' INT TERM

step() { case " ${E7_PHASES:-all} " in *" all "* | *" $1 "*) return 0 ;; esac; return 1; }

# --------------------------------------------------------------- helpers
mgr_cid() { vm_docker "$M1" ps -q --no-trunc -f "label=com.docker.swarm.service.name=$SVC" -f status=running | head -1; }
mgr_health() { vm_sh "$M1" "curl -fsS http://127.0.0.1:$API_PORT/api/v1/health"; }
mgr_leads() { [ "$(vm_sh "$M1" "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$API_PORT/api/v1/setup/status")" = 200 ]; }
# manager_logs — manager service log without the setup token line.
manager_logs() { vm_args "$M1" 'sudo docker service logs --no-trunc --tail "$1" "$2" 2>&1 | grep -v "setup token"' "${1:-40}" "$SVC"; }
svc_task() { vm_docker "$M1" service ps --no-trunc --format '{{.ID}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' "$SVC"; }

# ------------------------------------------------------------------ build
e7_build() {
	section "Phase build: lab tooling and the API client (linux/arm64) on the host"
	mkdir -p "$BIN"
	[ -x "$LAB" ] || (cd "$REPO_ROOT" && rec "go build bin/lab" go build -o bin/lab ./cmd/lab)
	(cd "$REPO_ROOT" && rec "go build ./tests/p0/e4client" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/e4client" ./tests/p0/e4client)
	rec "git revision (the image is built from this commit, git archive HEAD)" git -C "$REPO_ROOT" log -1 --format='%h %s'
	rec "uncommitted changes (must be empty for a faithful build)" git -C "$REPO_ROOT" status --short
}

e7_images() {
	section "Phase images: docker build -f deploy/Dockerfile inside m1 (arm64), copy to m2/m3 (no registry push)"
	hostload
	local rev
	rev=$(git -C "$REPO_ROOT" rev-parse --short HEAD)
	limactl copy "$BIN/e4client" "$M1:/tmp/"
	rec "install the API client in m1" vm_sh "$M1" 'sudo install -m 0755 /tmp/e4client /usr/local/bin/dbcm-e4-client && rm -f /tmp/e4client'
	rec "copy the committed source tree into m1" sh -c \
		"cd '$REPO_ROOT' && git archive --format=tar HEAD cmd internal web go.mod go.sum deploy .dockerignore | limactl shell --workdir / $M1 -- sudo sh -c 'rm -rf $SRC && mkdir -p $SRC && tar -x -C $SRC && ls $SRC'"
	rec "docker build -f deploy/Dockerfile (UI + Go binaries + distroless runtime)" vm_root "$M1" '
		cd "$1" && time docker build -f deploy/Dockerfile -t "$2" --build-arg VERSION=0.0.0-e7 --build-arg COMMIT="$3" . 2>&1 | tail -40' "$SRC" "$IMAGE" "$rev"
	rec "image facts" vm_docker "$M1" image inspect --format '{{.Id}} {{.Architecture}} {{.Os}} user={{.Config.User}} entrypoint={{.Config.Entrypoint}} size={{.Size}}' "$IMAGE"
	rec "image runs: manager -version" vm_docker "$M1" run --rm "$IMAGE" -version
	local vm
	for vm in dbcm-lab-m2 dbcm-lab-m3; do
		rec "copy the image to $vm (docker save | docker load through the host)" sh -c \
			"limactl shell --workdir / $M1 -- sudo docker save $IMAGE | limactl shell --workdir / $vm -- sudo docker load"
	done
	for vm in $VMS; do
		rec "pull pinned MariaDB image on $vm" vm_docker "$vm" pull -q "$MARIADB_IMAGE"
	done
}

e7_prereqs() {
	section "Phase prereqs: what manager init instructs (directories, secrets, cluster network), labelled dbcm.installation=$INST"
	hostload
	local vm
	for vm in $VMS; do
		rec "host directories on $vm" vm_root "$vm" 'install -d -m 0755 /var/lib/dbcm-agent "$1" && ls -ld /var/lib/dbcm-agent "$1"' "$VOLBASE"
	done
	rec "state directory for the API client and synthetic passwords (root-only files in m1; the lab monitoring check reads monitor.pw)" vm_root "$M1" 'install -d -m 0700 "$1"' "$STATE"
	rec "database secrets and cluster network on m1 (values never logged)" vm_root "$M1" '
		set -e
		for x in "$2:root" "$3:monitor" "$4:backup"; do
			s=${x%%:*}; f="$6/${x#*:}.pw"
			[ -f "$f" ] || head -c 24 /dev/urandom | base64 | tr -d "\n" >"$f"
			chmod 0600 "$f"
			if docker secret inspect "$s" >/dev/null 2>&1; then echo "secret $s exists"; else
				docker secret create --label "dbcm.installation=$1" "$s" "$f" >/dev/null && echo "secret $s"; fi
		done
		docker network inspect -f "{{.Name}} exists" "$5" 2>/dev/null || docker network create --driver overlay --attachable --label "dbcm.installation=$1" "$5"' \
		"$INST" "$ROOT_SECRET" "$MON_SECRET" "$BAK_SECRET" "$NET" "$STATE"
}

e7_init() {
	section "Phase init: manager init from the image produces the configuration; stored as a Swarm config"
	hostload
	local a1
	a1=$(uv2_ip "$M1")
	say "m1 user-v2 address: $a1"
	rec "docker run --rm $IMAGE init (stdout = configuration, stderr = instructions)" vm_args "$M1" '
		set -e
		sudo rm -f "$1/manager.json"
		sudo docker run --rm "$2" init -rp-id localhost -origin "http://localhost:$3" -manager-addr "$4" \
			-installation "$5" -agent-image "$2" -display-name "E7 lab" >"$6" 2>"$7" || { cat "$7"; exit 1; }
		sudo install -m 0600 "$6" "$1/manager.json"; rm -f "$6"
		echo "--- configuration:"; sudo cat "$1/manager.json"
		echo "--- instructions (stderr):"; cat "$7"' "$STATE" "$IMAGE" "$API_PORT" "$a1" "$INST" "/tmp/e7-manager.json" "/tmp/e7-init.err"
	rec "docker config create $CONFIG_NAME" vm_args "$M1" 'sudo docker config create "$1" "$2/manager.json"' "$CONFIG_NAME" "$STATE"
	rec "docker config ls" vm_docker "$M1" config ls
}

e7_stack() {
	section "Phase stack: docker stack deploy -c deploy/stack.yaml (manager pinned as in ADR 0014)"
	hostload
	local gid host
	gid=$(vm_sh "$M1" 'stat -c %g /var/run/docker.sock')
	host=$(vm_docker "$M1" node inspect self --format '{{.Description.Hostname}}')
	say "docker socket gid $gid, manager node hostname $host"
	rec "docker stack deploy" vm_root "$M1" '
		cd "$1" && DBCM_IMAGE="$2" DBCM_MANAGER_NODE="$3" DBCM_DOCKER_GID="$4" DBCM_CONFIG_NAME="$5" docker stack deploy -c deploy/stack.yaml --detach=true "$6" 2>&1' \
		"$SRC" "$IMAGE" "$host" "$gid" "$CONFIG_NAME" "$STACK" || true
	if ! wait_until 180 "manager task running" sh -c "limactl shell --workdir / $M1 -- sudo docker service ps --filter desired-state=running --format '{{.CurrentState}}' $SVC | grep -q '^Running'"; then
		rec "service ps" svc_task || true
		rec "service inspect" vm_docker "$M1" service inspect --pretty "$SVC" || true
		rec "manager logs" manager_logs 60 || true
		assert_fail stack "the manager task did not reach Running"
		return 1
	fi
	rec "service ps" svc_task
	rec "stack services" vm_docker "$M1" stack services "$STACK"
	rec "service placement, user, mounts and limits as Swarm stored them" vm_docker "$M1" service inspect --format \
		'constraints={{.Spec.TaskTemplate.Placement.Constraints}} replicas={{.Spec.Mode.Replicated.Replicas}} user={{.Spec.TaskTemplate.ContainerSpec.User}} readonly={{.Spec.TaskTemplate.ContainerSpec.ReadOnly}} capdrop={{.Spec.TaskTemplate.ContainerSpec.CapabilityDrop}} networks={{.Spec.TaskTemplate.Networks}} update={{.Spec.UpdateConfig.Order}}/{{.Spec.UpdateConfig.FailureAction}} configs={{len .Spec.TaskTemplate.ContainerSpec.Configs}}' "$SVC"
	local cid
	cid=$(mgr_cid)
	rec "what the container actually got (user, read-only root, caps, network mode, mounts)" vm_docker "$M1" inspect --format \
		'user={{.Config.User}} readonlyrootfs={{.HostConfig.ReadonlyRootfs}} capdrop={{.HostConfig.CapDrop}} netmode={{.HostConfig.NetworkMode}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}{{range .Mounts}} mount[{{.Type}} {{.Source}}->{{.Destination}} rw={{.RW}}]{{end}}' "$cid"
	rec "id of the container process, the data volume ownership and the data directory content" vm_args "$M1" '
		sudo docker exec "$1" /usr/local/bin/manager -version 2>&1 | head -2 || true
		sudo docker volume inspect -f "{{.Name}} {{.Mountpoint}}" "$2_manager-data"
		sudo ls -ln "$(sudo docker volume inspect -f "{{.Mountpoint}}" "$2_manager-data")"' "$cid" "$STACK"
	rec "manager logs (setup token line removed)" manager_logs 40
	rec "listeners on m1" vm_sh "$M1" "sudo ss -ltnp | grep -E ':$API_PORT|:7444|:7445|manager' || true"
}

e7_verify() {
	section "Phase verify: health, UI, authentication"
	hostload
	local out
	wait_until 120 "manager health" mgr_health || { assert_fail health "no health response"; return 1; }
	rec "GET /api/v1/health" vm_sh "$M1" "curl -sS -i http://127.0.0.1:$API_PORT/api/v1/health"
	rec "GET /api/v1/version" vm_sh "$M1" "curl -sS http://127.0.0.1:$API_PORT/api/v1/version"
	rec "container health state" vm_docker "$M1" inspect --format '{{.State.Health.Status}} failing={{.State.Health.FailingStreak}}' "$(mgr_cid)"
	wait_until 120 "manager leads (setup status 200)" mgr_leads || { assert_fail leader "manager never leads"; return 1; }
	out=$(vm_sh "$M1" "curl -sS -i http://127.0.0.1:$API_PORT/ | head -20")
	printf '### GET / (UI)\n\n```\n%s\n```\n\n' "$out" >>"$LOG"
	case "$out" in *"200 OK"*"<!doctype html>"* | *"200 OK"*"<!DOCTYPE html>"* | *"200 OK"*"<html"*) ok ui "GET / returns the UI HTML" ;; *) assert_fail ui "GET / did not return HTML 200" ;; esac
	rec "UI asset referenced by the page" vm_sh "$M1" "p=\$(curl -s http://127.0.0.1:$API_PORT/ | grep -o '/_app/[^\"]*\\.js' | head -1); echo \"asset: \$p\"; curl -sS -o /dev/null -w 'asset status %{http_code} bytes %{size_download} type %{content_type}\\n' http://127.0.0.1:$API_PORT\$p"
	local code
	code=$(vm_sh "$M1" "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$API_PORT/api/v1/clusters")
	say "unauthenticated GET /api/v1/clusters: HTTP $code"
	[ "$code" = 401 ] && ok auth-required "unauthenticated API request refused with 401" || assert_fail auth-required "expected 401, got $code"
	rec "setup status before" api GET /api/v1/setup/status
	vm_args "$M1" 'sudo docker service logs --no-trunc "$1" 2>&1 | grep -o "setup token[^:]*: [^ ]*" | tail -1 | awk "{print \$NF}" | sudo tee "$2/setup.token" >/dev/null; sudo chmod 0600 "$2/setup.token"; echo "token length: $(sudo wc -c <"$2/setup.token")" >&2' "$SVC" "$STATE"
	rec "setup (WebAuthn registration, ES256 software key)" vm_args "$M1" 'sudo /usr/local/bin/dbcm-e4-client -state "$1/client.json" -base "http://127.0.0.1:$2" -origin "http://localhost:$2" setup -token-file "$1/setup.token" -account admin' "$STATE" "$API_PORT" ||
		{ assert_fail setup "administrator setup failed"; return 1; }
	rec "session" api GET /api/v1/auth/session
	rec "authenticated GET /api/v1/clusters" api GET /api/v1/clusters && ok auth "authenticated request accepted after setup" || assert_fail auth "authenticated request refused"
	rec "setup status after" api GET /api/v1/setup/status
}

e7_agents() {
	section "Phase agents: connect the Swarm, enroll hosts, deploy agents; mTLS"
	hostload
	phase_swarmapi || { assert_fail agents "agents did not enroll"; return 1; }
	ok agents "three agents enrolled (agentIdentity true)"
	local vm ip
	rec "agent logs (enrollment and mTLS lines)" vm_args "$M1" 'sudo docker service logs --no-trunc --tail 40 "dbcm-agent-$1" 2>&1 | cut -c1-300' "$INST"
	rec "manager log lines about enrollment" vm_args "$M1" 'sudo docker service logs --no-trunc "$1" 2>&1 | grep -v "setup token" | grep -iE "enrol|agent" | tail -15 | cut -c1-300' "$SVC"
	for vm in dbcm-lab-m2 dbcm-lab-m3; do
		ip=$(uv2_ip "$vm")
		rec "agent port $ip:7443 without a client certificate must be refused" vm_sh "$M1" "curl -sk -m 8 -o /dev/null -w 'http %{http_code}\\n' https://$ip:7443/ ; echo curl-exit \$?"
		rec "agent port $ip:7443 openssl handshake without a client certificate" vm_sh "$M1" "echo | timeout 8 openssl s_client -connect $ip:7443 -brief 2>&1 | head -8"
	done
	rec "enrollment endpoint is TLS: plain HTTP is refused" vm_sh "$M1" "curl -s -m 8 -o /dev/null -w 'http %{http_code}\\n' http://$(uv2_ip "$M1"):7444/ ; echo curl-exit \$?"
	rec "hosts" api GET /api/v1/hosts
}

e7_cluster() {
	section "Phase cluster: Galera cluster create through the API of the stack-deployed manager"
	hostload
	CLUSTER_ID=""
	lib_cluster || true
	local st
	st=$(apij GET /api/v1/operations | jq -r --arg id "$OP_ID" '.items[] | select(.id==$id) | .state')
	[ "$st" = succeeded ] && ok cluster "create operation succeeded" || { assert_fail cluster "create operation state: $st"; return 1; }
	lib_verify || assert_fail cluster-verify "MariaDB checks failed"
}

# marker of the cluster state for the restart and drain phases
cluster_summary() { apij GET "/api/v1/clusters/$(cluster_id)" | jq -c '{state, nodes: [.nodes[] | {id, state, role}]}'; }
galera_sync() { [ "$(rsql 1 "SHOW GLOBAL STATUS LIKE 'wsrep_cluster_size'" | awk '{print $2}')" = 3 ]; }

e7_restart() {
	section "Phase restart: manager container kill, forced update, behaviour of the running cluster"
	hostload
	CLUSTER_ID=$(cluster_id)
	rec "before: cluster" cluster_summary
	rec "before: manager task and container" svc_task
	local old t0
	old=$(mgr_cid)
	t0=$(date +%s)
	rec "docker kill the manager container $old" vm_docker "$M1" kill "$old"
	wait_until 180 "new manager container is running" sh -c "test -n \"\$(limactl shell --workdir / $M1 -- sudo docker ps -q --no-trunc -f label=com.docker.swarm.service.name=$SVC -f status=running | grep -v $old)\""
	wait_until 120 "manager health after kill" mgr_health || assert_fail restart-kill "no health after the kill"
	wait_until 120 "manager leads again" mgr_leads || assert_fail restart-kill "manager does not lead after the kill"
	say "kill to leading: $(($(date +%s) - t0)) s"
	rec "tasks after the kill" svc_task
	rec "restart count" vm_docker "$M1" inspect --format '{{.RestartCount}} started={{.State.StartedAt}}' "$(mgr_cid)"
	rec "session survives the restart (old cookie, no new login)" api GET /api/v1/auth/session &&
		ok restart-session "the session cookie of before the kill is still valid" || assert_fail restart-session "session lost"
	rec "installation survived: setup status" api GET /api/v1/setup/status
	rec "cluster after the kill" cluster_summary
	[ -n "$(cluster_id)" ] && ok restart-data "cluster record and configuration survived in the data volume" || assert_fail restart-data "cluster record missing"
	rec "manager log after the restart" manager_logs 30
	rec "galera still synced (3 nodes) while the manager was down and back" galera_sync && ok restart-galera "wsrep_cluster_size 3" || assert_fail restart-galera "cluster size changed"
	rec "agents still enrolled" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/hosts | tail -n +2 | jq -c '[.items[] | {nodeId, agentIdentity, agent: .agentState}]'"

	section "Phase restart (2): docker service update --force (stop-first)"
	old=$(mgr_cid)
	t0=$(date +%s)
	rec "service update --force" vm_docker "$M1" service update --force --detach=false "$SVC" || true
	wait_until 120 "manager health after update" mgr_health || assert_fail restart-update "no health after the update"
	wait_until 120 "manager leads after update" mgr_leads || assert_fail restart-update "manager does not lead after the update"
	say "update to leading: $(($(date +%s) - t0)) s"
	[ "$(mgr_cid)" != "$old" ] && ok restart-update "stop-first update replaced the container and the manager leads again" || assert_fail restart-update "container unchanged"
	rec "tasks after the update (history shows stop-first)" svc_task
	rec "session after the update" api GET /api/v1/auth/session || assert_fail restart-update "session lost after the update"
	rec "cluster after the update" cluster_summary
}

e7_drain() {
	section "Phase drain: drain the pinned manager node m1 and activate it again"
	hostload
	local node t0 t1
	node=$(vm_docker "$M1" node inspect self --format '{{.ID}}')
	rec "before: node ls" vm_docker "$M1" node ls
	rec "before: services" vm_docker "$M1" service ls
	t0=$(date +%s)
	rec "docker node update --availability drain m1" vm_docker "$M1" node update --availability drain "$node"
	sleep 45
	rec "manager tasks after drain (the constraint allows no other node)" svc_task
	rec "services after drain" vm_docker "$M1" service ls
	rec "manager health during drain (expected: down)" mgr_health || true
	rec "task state detail" vm_docker "$M1" service ps --no-trunc --format '{{.Name}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' "$SVC"
	rec "galera services during drain" vm_docker "$M1" service ls --filter "label=dbcm.cluster"
	rec "agent and galera tasks during drain (node, state, error)" vm_args "$M1" 'for s in dbcm-agent-$1 dbcm-$2-n1 dbcm-$2-n2 dbcm-$2-n3; do sudo docker service ps --format "{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}" "$s" | head -4; done' "$INST" "$CLUSTER_NAME"
	rec "volumes of the manager are retained on m1" vm_docker "$M1" volume ls --filter "name=${STACK}_manager-data"
	rec "activate m1 again" vm_docker "$M1" node update --availability active "$node"
	wait_until 240 "manager task running again" sh -c "limactl shell --workdir / $M1 -- sudo docker service ps --filter desired-state=running --format '{{.CurrentState}}' $SVC | grep -q '^Running'" ||
		assert_fail drain "manager task did not come back"
	wait_until 180 "manager health after activate" mgr_health || assert_fail drain "no health after reactivation"
	wait_until 180 "manager leads after activate" mgr_leads || assert_fail drain "manager does not lead after reactivation"
	say "drain start to leading again: $(($(date +%s) - t0)) s"
	rec "tasks after activate" svc_task
	rec "session and data after the drain cycle" api GET /api/v1/auth/session &&
		ok drain-data "same installation (session valid) and cluster record present after drain and activate" || assert_fail drain "session lost"
	rec "cluster right after the drain cycle (the manager has just started)" cluster_summary
	t1=$(date +%s)
	wait_until 300 "cluster healthy again in the manager's view" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/clusters | tail -n +2 | jq -e '.items[0].state==\"healthy\"'" &&
		ok drain-cluster "cluster reported healthy $(($(date +%s) - t1)) s after the manager came back" || assert_fail drain-cluster "cluster not healthy after reactivation"
	rec "cluster after the drain cycle" cluster_summary
	rec "galera size after the drain cycle" galera_sync || assert_fail drain-galera "wsrep_cluster_size is not 3"
	rec "agents after the drain cycle" vm_docker "$M1" service ls --filter "name=dbcm-agent-$INST"
	rec "manager log after reactivation" manager_logs 30
	rec "all services after the drain cycle" vm_docker "$M1" service ls
}

e7_peertls() {
	section "Phase peertls: peer TLS evidence"
	local rep
	rep=$(vm_docker "$M1" service inspect --format '{{.Spec.Mode.Replicated.Replicas}}' "$SVC")
	result "deploy/stack.yaml deploys $rep replica by design (ADR 0014, item 4): replicas>1 is NOT supported by the stack, so no peer-TLS refusal can be shown from it. A wrong-certificate replica refusal is evidenced for plain processes in tests/p0/results/e4-rerun-B2.md; the per-replica Swarm-service layout of docs/INSTALL.md (Three-replica variant) is still not run."
	rec "metastore listener in the stack manager (single replica, loopback)" vm_sh "$M1" "sudo ss -ltn | grep -E ':7445' || true"
}

# ------------------------------------------------------------------ run
for p in build vms swarm images prereqs init stack verify agents cluster restart drain peertls; do
	step "$p" || continue
	case "$p" in vms | swarm) [ "${E7_REUSE_VMS:-0}" = 1 ] && continue ;; esac
	case "$p" in
	vms) phase_vms ;;
	swarm) phase_swarm ;;
	*) "e7_$p" ;;
	esac
done
