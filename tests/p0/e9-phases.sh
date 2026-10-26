# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2016
#
# E9 phases, sourced by tests/p0/e9-ha-proxy-writer.sh after the E4 library
# and the E6/E8 helpers. Part A (manager HA stack) is in this file; parts B
# and C are in e9-phases-proxy.sh.

# ---------------------------------------------------------------- helpers
HA_IDS="1 2 3"
MGR_PORT_META=7445
MGR_PORT_FWD=7447

# api METHOD PATH [BODY] — like the library's api, but over the first running
# VM whose manager answers with something other than 503 (the replicas
# forward to the leader; a VM that is down or a replica without a leader
# is skipped). Client state (passkey, cookies) lives on every VM.
api() {
	local vm out rc
	for vm in $VMS; do
		e8_vm_up "$vm" || continue
		rc=0
		out=$(vm_args "$vm" 'sudo /usr/local/bin/dbcm-e4-client -state '"$STATE"'/client.json -base http://127.0.0.1:'"$API_PORT"' -origin http://localhost:'"$API_PORT"' req "$@"' "$@" 2>&1) || rc=$?
		[ "$rc" = 255 ] && continue
		case "$out" in "HTTP 503"* | *"connection refused"*) continue ;; esac
		printf '%s\n' "$out"
		return "$rc"
	done
	return 1
}

# sync_client SRC-VM — copy the lab client state (passkey, cookies) to the
# other VMs.
sync_client() {
	local vm
	for vm in $VMS; do
		[ "$vm" = "$1" ] && continue
		e8_vm_up "$vm" || continue
		limactl shell --workdir / "$1" -- sudo tar -C / -cf - "${STATE#/}/client.json" | limactl shell --workdir / "$vm" -- sudo sh -c "install -d -m 0700 $STATE && tar -C / -xf -"
	done
}

mgr_svc() { echo "${STACK}_manager-$1"; }
vm_of() { echo "dbcm-lab-m$1"; }
# mgr_cid N — running manager container of replica N on its VM.
mgr_cid() { vm_docker "$(vm_of "$1")" ps -q --no-trunc -f "label=com.docker.swarm.service.name=$(mgr_svc "$1")" -f status=running | head -1; }
# mgr_logs N [TAIL] — service log of replica N without the setup token.
mgr_logs() { vm_args "$M1" 'sudo docker service logs --no-trunc --timestamps --tail "$2" "$1" 2>&1 | grep -v "setup token"' "$(mgr_svc "$1")" "${2:-60}"; }
# ha_probe VM — HTTP code and leader hint of the replica on VM without
# following a forward: a leader answers 200, a follower 503 + X-DCM-Leader.
ha_probe() {
	vm_sh "$1" "curl -s -m 3 -D - -o /dev/null -H 'X-DCM-Forwarded: 1' http://127.0.0.1:$API_PORT/api/v1/setup/status | grep -iE '^HTTP|x-dcm-leader' | tr -d '\r' | tr '\n' ' '"
}
# ha_leader_n — the replica number that answers 200 without forwarding.
ha_leader_n() {
	local n vm
	for n in $HA_IDS; do
		vm=$(vm_of "$n")
		e8_vm_up "$vm" || continue
		case "$(ha_probe "$vm" 2>/dev/null)" in *" 200 "* | "HTTP/1.1 200"*) echo "$n"; return 0 ;; esac
	done
	return 1
}
ha_have_leader() { ha_leader_n >/dev/null; }
ha_all_running() { # EXPECTED — number of running manager tasks
	[ "$(vm_docker "$M1" service ls --format '{{.Name}} {{.Replicas}}' | grep -c "^${STACK}_manager-.* 1/1")" = "${1:-3}" ]
}

# API sampler on a VM: one line "HH:MM:SS.mmm CODE" every 100 ms.
e9_install_samplers() {
	local vm
	for vm in $VMS; do
		vm_root "$vm" 'cat >/usr/local/bin/dbcm-e9-apisample <<"EOF"
#!/bin/sh
# usage: dbcm-e9-apisample SECONDS URL
end=$(( $(date +%s) + $1 ))
while [ "$(date +%s)" -lt "$end" ]; do
  t=$(date -u +%T.%3N)
  c=$(curl -s -m 1 -o /dev/null -w "%{http_code}" "$2")
  echo "$t $c"
  sleep 0.1
done
EOF
cat >/usr/local/bin/dbcm-e9-pxsample <<"EOF"
#!/bin/sh
# usage: dbcm-e9-pxsample SECONDS LABEL — proxy status every 100 ms (restarts
# the exec when the proxy container is gone)
end=$(( $(date +%s) + $1 ))
while [ "$(date +%s)" -lt "$end" ]; do
  left=$(( end - $(date +%s) ))
  docker exec dbcm-e9-proxy /e8/e5client sample -label "$2" -duration "${left}s" -interval 100ms 2>/dev/null || { echo "$(date -u +%T.%3N) $2 0 down - - - -"; sleep 0.2; }
done
EOF
chmod 0755 /usr/local/bin/dbcm-e9-apisample /usr/local/bin/dbcm-e9-pxsample'
	done
}
# e9_apisample_start VM TAG SECONDS — background sampler of the local API.
e9_apisample_start() {
	vm_root "$1" 'install -d -m 0700 "$4/out"; systemd-run -q --unit "e9api-$2" --collect -p StandardOutput=file:"$4/out/$2.api" -p StandardError=file:"$4/out/$2.apierr" /usr/local/bin/dbcm-e9-apisample "$3" "http://127.0.0.1:'"$API_PORT"'/api/v1/setup/status"' "$1" "$2" "$3" "$E8"
}
# e9_apisample_report VM TAG — samples, non-200 count, longest span without a
# 200 (between two 200 samples) and when it started.
e9_apisample_report() {
	vm_root "$1" 'cat "$1/out/$2.api"' "$E8" "$2" | awk '
		function sec(t,  a) { split(t, a, ":"); return a[1]*3600 + a[2]*60 + a[3] }
		{ n++; c[$2]++; t = sec($1)
		  if ($2 == 200) { if (last200 != "" && t - last200 > max) { max = t - last200; from = lastts } last200 = t; lastts = $1 }
		}
		END { printf "samples=%d", n; for (k in c) printf " code%s=%d", k, c[k]; printf " longest_gap_between_200=%.3fs", max; if (from != "") printf " (after %s)", from; printf "\n" }'
}

# ----------------------------------------------------------------- build
e9_build() {
	section "Phase build: lab tooling, the API client, proxy and e5client (linux/arm64) on the host"
	mkdir -p "$BIN"
	[ -x "$LAB" ] || (cd "$REPO_ROOT" && rec "go build bin/lab" go build -o bin/lab ./cmd/lab)
	local c
	for c in e4client e5client; do
		(cd "$REPO_ROOT" && rec "go build ./tests/p0/$c" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/$c" "./tests/p0/$c")
	done
	(cd "$REPO_ROOT" && rec "go build ./cmd/proxy" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/proxy" ./cmd/proxy)
	rec "git revision (the image is built from this commit, git archive HEAD)" git -C "$REPO_ROOT" log -1 --format='%h %s'
	rec "uncommitted changes (must be empty for a faithful build)" git -C "$REPO_ROOT" status --short
	rec "host tools" sh -c 'jq --version; python3 --version; limactl --version'
}

e9_vms() { phase_vms; }

e9_swarm() {
	section "Phase swarm: three Swarm manager nodes"
	hostload
	local a1 vm addr token
	a1=$(uv2_ip "$M1")
	say "m1 user-v2 address: $a1"
	rec "swarm init on m1" vm_docker "$M1" swarm init --advertise-addr "$a1"
	token=$(vm_docker "$M1" swarm join-token -q manager)
	for vm in dbcm-lab-m2 dbcm-lab-m3; do
		addr=$(uv2_ip "$vm")
		rec "swarm join $vm as manager (advertise $addr, token not recorded)" \
			vm_docker "$vm" swarm join --advertise-addr "$addr" --token "$token" "$a1:2377"
	done
	token=""
	rec "docker node ls" vm_docker "$M1" node ls
}

e9_images() {
	section "Phase images: docker build -f deploy/Dockerfile inside m1 (arm64), copy to m2/m3; tools"
	hostload
	local rev vm
	rev=$(git -C "$REPO_ROOT" rev-parse --short HEAD)
	for vm in $VMS; do
		limactl copy "$BIN/e4client" "$BIN/e5client" "$BIN/proxy" "$vm:/tmp/"
		rec "install lab tools on $vm" vm_root "$vm" '
			set -e
			install -m 0755 /tmp/e4client /usr/local/bin/dbcm-e4-client
			install -d -m 0700 "$1" "$1/out"
			install -m 0755 /tmp/e5client /tmp/proxy "$1/"
			rm -f /tmp/e4client /tmp/e5client /tmp/proxy' "$E8"
	done
	e9_install_samplers
	rec "apt packages (jq on m1, socat and openssl everywhere)" sh -c "for v in $VMS; do limactl shell --workdir / \$v -- sudo sh -c 'DEBIAN_FRONTEND=noninteractive apt-get install -y -qq jq socat openssl >/dev/null 2>&1; echo \$(hostname): \$(jq --version) \$(socat -V | sed -n 2p) \$(openssl version)'; done"
	rec "copy the committed source tree into m1" sh -c \
		"cd '$REPO_ROOT' && git archive --format=tar HEAD cmd internal web go.mod go.sum deploy .dockerignore | limactl shell --workdir / $M1 -- sudo sh -c 'rm -rf $SRC && mkdir -p $SRC && tar -x -C $SRC && ls $SRC'"
	rec "docker build -f deploy/Dockerfile (UI + Go binaries + distroless runtime)" vm_root "$M1" '
		cd "$1" && time docker build -f deploy/Dockerfile -t "$2" --build-arg VERSION=0.0.0-e9 --build-arg COMMIT="$3" . 2>&1 | tail -40' "$SRC" "$IMAGE" "$rev"
	rec "image facts" vm_docker "$M1" image inspect --format '{{.Id}} {{.Architecture}} {{.Os}} user={{.Config.User}} entrypoint={{.Config.Entrypoint}} size={{.Size}}' "$IMAGE"
	for vm in dbcm-lab-m2 dbcm-lab-m3; do
		rec "copy the image to $vm (docker save | docker load through the host)" sh -c \
			"limactl shell --workdir / $M1 -- sudo docker save $IMAGE | limactl shell --workdir / $vm -- sudo docker load"
	done
	for vm in $VMS; do
		rec "pull pinned MariaDB image on $vm" vm_docker "$vm" pull -q "$MARIADB_IMAGE"
	done
}

e9_prereqs() {
	section "Phase prereqs: host directories, database secrets and cluster network (as manager init instructs)"
	hostload
	local vm
	for vm in $VMS; do
		rec "host directories on $vm" vm_root "$vm" 'install -d -m 0755 /var/lib/dbcm-agent "$1" && ls -ld /var/lib/dbcm-agent "$1"' "$VOLBASE"
	done
	rec "state directory and synthetic database secrets on m1 (values never logged)" vm_root "$M1" '
		set -e
		install -d -m 0700 "$6"
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

# --------------------------------------------------------------- Part A
JQ_PROXYWRITERS='.manager.proxyWriters = [{clusterId: $c, initialWriter: "n1", candidates: ["n1","n2","n3"], autoFailover: ($af == "true"),
	members: [{id: "proxy-a", tokenFile: "/var/lib/dbcm/proxy/proxy-a.token"}, {id: "proxy-b", tokenFile: "/var/lib/dbcm/proxy/proxy-b.token"}],
	adminTokenFile: "/var/lib/dbcm/proxy/admin.token"}]'

e9_hainit() {
	section "Phase hainit: peer CA and certificates, proxy tokens, three replica configurations, Swarm secrets/configs, local volumes"
	hostload
	local a1 a2 a3 peers i vm cfg
	a1=$(uv2_ip dbcm-lab-m1) a2=$(uv2_ip dbcm-lab-m2) a3=$(uv2_ip dbcm-lab-m3)
	peers="manager-1=$a1:$MGR_PORT_META,manager-2=$a2:$MGR_PORT_META,manager-3=$a3:$MGR_PORT_META"
	say "replica addresses $a1 $a2 $a3"
	rec "peer CA, per-replica bundles (manager -issue-peer-certs from the image) and proxy/admin tokens (values never logged)" vm_root "$M1" '
		set -e
		d="$1"; rm -rf "$d/peers" "$d/tokens"
		install -d -m 0700 "$d/tokens"
		[ -f "$d/ca.pass" ] || { head -c 24 /dev/urandom | base64 | tr -d "\n=" >"$d/ca.pass"; chmod 0600 "$d/ca.pass"; }
		docker run --rm --user 0 -e DBCM_PEER_CA_PASSPHRASE="$(cat "$d/ca.pass")" -v "$d:/work" "$2" -issue-peer-certs /work/peers -installation "$3" -peer-ids manager-1,manager-2,manager-3
		for t in proxy-a proxy-b admin; do head -c 24 /dev/urandom | base64 | tr -d "\n=+/" >"$d/tokens/$t.token"; done
		chmod 0600 "$d"/tokens/*
		find "$d/peers" -type f | sort | xargs ls -l | cut -c1-10,30-' "$STATE" "$IMAGE" "$INST"
	for i in $HA_IDS; do
		cfg="$STATE/manager-$i.json"
		rec "manager init for replica $i (stdout = configuration, stderr = instructions)" vm_root "$M1" '
			set -e
			docker run --rm "$2" init -rp-id localhost -origin "http://localhost:$3" -manager-addr "$4" -installation "$5" -node-id "manager-$6" \
				-peers "$7" -agent-image "$2" -display-name "E9 lab" >"$1.tmp" 2>"$1.err" || { cat "$1.err"; exit 1; }
			jq --arg af "$8" --arg c "$9" "${10}" "$1.tmp" >"$1"
			rm -f "$1.tmp"; chmod 0600 "$1"
			echo "--- configuration (replica $6):"; cat "$1"
			echo "--- instructions (stderr):"; cat "$1.err"' "$cfg" "$IMAGE" "$API_PORT" "$(uv2_ip "$(vm_of "$i")")" "$INST" "$i" "$peers" "${E9_AUTOFAILOVER:-false}" "$CLUSTER_NAME" "$JQ_PROXYWRITERS"
	done
	rec "Swarm secrets for the peer CA and configs per replica" vm_root "$M1" '
		set -e
		d="$1"
		mk() { docker secret inspect "$1" >/dev/null 2>&1 && echo "secret $1 exists" || { docker secret create "$1" "$2" >/dev/null && echo "secret $1"; }; }
		mk dbcm-e9-peer-ca "$d/peers/ca.pem"; mk dbcm-e9-peer-ca-sealed "$d/peers/ca.sealed"; mk dbcm-e9-peer-ca-pass "$d/ca.pass"
		for i in 1 2 3; do
			docker config inspect "dbcm-e9-config-$i" >/dev/null 2>&1 && echo "config $i exists" || { docker config create "dbcm-e9-config-$i" "$d/manager-$i.json" >/dev/null && echo "config $i"; }
		done
		docker secret ls; docker config ls' "$STATE"
	for i in $HA_IDS; do
		vm=$(vm_of "$i")
		rec "volume dbcm_manager-data-$i on $vm: own peer bundle and the shared proxy tokens, owned by 65532" sh -c \
			"limactl shell --workdir / $M1 -- sudo tar -C $STATE -cf - peers/manager-$i/peer-bundle.pem tokens | limactl shell --workdir / $vm -- sudo sh -c '
				set -e
				docker volume create dbcm_manager-data-$i >/dev/null
				mp=\$(docker volume inspect -f \"{{.Mountpoint}}\" dbcm_manager-data-$i)
				d=\$(mktemp -d) && tar -C \$d -xf -
				install -d -m 0700 \$mp/peer \$mp/proxy
				install -m 0600 \$d/peers/manager-$i/peer-bundle.pem \$mp/peer/peer-bundle.pem
				for t in proxy-a proxy-b admin; do install -m 0600 \$d/tokens/\$t.token \$mp/proxy/\$t.token; done
				rm -rf \$d
				chown -R 65532:65532 \$mp
				ls -lnR \$mp | cut -c1-60'"
	done
}

e9_hastack() {
	section "Phase hastack: docker stack deploy -c deploy/stack-ha.yaml (three pinned manager services)"
	hostload
	local gids vm gid i
	gids=""
	for vm in $VMS; do gids="$gids $(vm_sh "$vm" 'stat -c %g /var/run/docker.sock')"; done
	say "docker socket group ids:$gids"
	gid=$(echo "$gids" | awk '{print $1}')
	[ "$(echo "$gids" | tr ' ' '\n' | grep -v '^$' | sort -u | wc -l | tr -d ' ')" = 1 ] || assert_fail hastack "the Docker socket group ids differ: $gids"
	rec "docker stack deploy" vm_root "$M1" '
		cd "$1" && DBCM_IMAGE="$2" DBCM_DOCKER_GID="$3" DBCM_MANAGER_NODE_1=lima-dbcm-lab-m1 DBCM_MANAGER_NODE_2=lima-dbcm-lab-m2 DBCM_MANAGER_NODE_3=lima-dbcm-lab-m3 \
		DBCM_CONFIG_NAME_1=dbcm-e9-config-1 DBCM_CONFIG_NAME_2=dbcm-e9-config-2 DBCM_CONFIG_NAME_3=dbcm-e9-config-3 \
		DBCM_PEER_CA_NAME=dbcm-e9-peer-ca DBCM_PEER_CA_SEALED_NAME=dbcm-e9-peer-ca-sealed DBCM_PEER_CA_PASSPHRASE_NAME=dbcm-e9-peer-ca-pass \
		docker stack deploy -c deploy/stack-ha.yaml --detach=true "$4" 2>&1' "$SRC" "$IMAGE" "$gid" "$STACK" || true
	if ! wait_until 240 "three manager tasks running" ha_all_running 3; then
		for i in $HA_IDS; do rec "tasks of replica $i" vm_docker "$M1" service ps --no-trunc --format '{{.ID}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' "$(mgr_svc "$i")" || true; done
		for i in $HA_IDS; do rec "log of replica $i" mgr_logs "$i" 40 || true; done
		assert_fail hastack "not all three manager tasks reached Running"
		return 1
	fi
	rec "stack services" vm_docker "$M1" stack services "$STACK"
	rec "placement as Swarm stored it" sh -c "for i in 1 2 3; do limactl shell --workdir / $M1 -- sudo docker service inspect --format '{{.Spec.Name}} constraints={{.Spec.TaskTemplate.Placement.Constraints}} replicas={{.Spec.Mode.Replicated.Replicas}} user={{.Spec.TaskTemplate.ContainerSpec.User}} readonly={{.Spec.TaskTemplate.ContainerSpec.ReadOnly}} capdrop={{.Spec.TaskTemplate.ContainerSpec.CapabilityDrop}} secrets={{len .Spec.TaskTemplate.ContainerSpec.Secrets}} update={{.Spec.UpdateConfig.Order}}/{{.Spec.UpdateConfig.FailureAction}}' ${STACK}_manager-\$i; done"
	wait_until 120 "a replica leads" ha_have_leader || {
		for i in $HA_IDS; do rec "log of replica $i" mgr_logs "$i" 40 || true; done
		assert_fail hastack "no replica leads"
		return 1
	}
	for i in $HA_IDS; do rec "replica $i: probe without forwarding (leader 200, follower 503 + X-DCM-Leader)" ha_probe "$(vm_of "$i")"; done
	rec "listeners on the replica VMs (7445 metastore, 7447 API forwarding, 8080 loopback API, 7444 enrollment)" sh -c "for v in $VMS; do limactl shell --workdir / \$v -- sudo ss -ltn | grep -E ':7444|:7445|:7447|:$API_PORT' | sed \"s/^/\$v /\"; done"
	rec "container health state per replica" sh -c "for i in 1 2 3; do limactl shell --workdir / dbcm-lab-m\$i -- sudo docker ps --filter label=com.docker.swarm.service.name=${STACK}_manager-\$i --format '{{.Names}} {{.Status}}'; done"
	for i in $HA_IDS; do rec "log of replica $i" mgr_logs "$i" 30 || true; done
	ok hastack "three replicas running, leader replica $(ha_leader_n)"
}

e9_hasetup() {
	section "Phase hasetup: administrator, data-key recipients of the followers, session on every replica"
	hostload
	local ln lvm n vm key out
	ln=$(ha_leader_n) || { assert_fail hasetup "no leader"; return 1; }
	lvm=$(vm_of "$ln")
	say "leader replica: manager-$ln on $lvm"
	for n in $HA_IDS; do
		vm_args "$M1" 'sudo docker service logs --no-trunc "$1" 2>&1 | grep -o "setup token[^:]*: [^ ]*" | tail -1 | awk "{print \$NF}"' "$(mgr_svc "$n")" >"${TMPDIR:-/tmp}/e9-tok.$$" || true
		if [ -s "${TMPDIR:-/tmp}/e9-tok.$$" ]; then
			vm_args "$M1" 'sudo install -d -m 0700 "$1"; sudo tee "$1/setup.token" >/dev/null; sudo chmod 0600 "$1/setup.token"' "$STATE" <"${TMPDIR:-/tmp}/e9-tok.$$"
			say "setup token found in the log of replica $n"
			break
		fi
	done
	rm -f "${TMPDIR:-/tmp}/e9-tok.$$"
	rec "setup through m1 (a write; m1 is $( [ "$ln" = 1 ] && echo the leader || echo a follower and forwards))" vm_args "$M1" 'sudo /usr/local/bin/dbcm-e4-client -state "$1/client.json" -base "http://127.0.0.1:$2" -origin "http://localhost:$2" setup -token-file "$1/setup.token" -account admin' "$STATE" "$API_PORT" ||
		{ assert_fail hasetup "administrator setup failed"; return 1; }
	sync_client "$M1"
	rec "session via m1" api GET /api/v1/auth/session
	for n in $HA_IDS; do
		[ "$n" = "$ln" ] && continue
		vm=$(vm_of "$n")
		out=$(vm_args "$vm" 'sudo docker exec "$1" /usr/local/bin/manager -print-replica-key -config /etc/dbcm/manager.json 2>&1' "$(mgr_cid "$n")") || true
		key=$(printf '%s\n' "$out" | awk '{print $5}' | grep '^dbcmpk1:' | head -1)
		printf '### -print-replica-key on replica %s\n\n```\n%s\n```\n\n' "$n" "$(printf '%s' "$out" | cut -c1-200)" >>"$LOG"
		[ -n "$key" ] || { assert_fail hasetup "no public key from replica $n"; continue; }
		rec "register manager-$n as data-key recipient (through VM $vm: forwarded when $vm is a follower)" sh -c \
			"limactl shell --workdir / $vm -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req PUT /api/v1/settings/replicas/manager-$n/data-key-recipient '{\"publicKey\":\"$key\"}'" ||
			assert_fail hasetup "registration of manager-$n refused"
	done
	rec "keyring status (every replica holds the current key)" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/settings/replicas | tail -n +2 | jq -c '{currentKey, recoverable, replicas: [.replicas[] | {id, leader, hasCurrentKey}]}'"
	if [ "$(apij GET /api/v1/settings/replicas | jq '[.replicas[] | select(.hasCurrentKey)] | length')" != 3 ]; then
		assert_fail hasetup "not every replica holds the current data key"
	else
		ok hasetup "all three replicas hold the current data key"
	fi
}

e9_hafwd() {
	section "Phase hafwd: leader election state and follower request forwarding"
	hostload
	local ln n vm
	ln=$(ha_leader_n) || { assert_fail hafwd "no leader"; return 1; }
	say "leader replica: manager-$ln"
	for n in $HA_IDS; do rec "replica $n (X-DCM-Forwarded: 1 suppresses forwarding)" ha_probe "$(vm_of "$n")"; done
	for n in $HA_IDS; do
		vm=$(vm_of "$n")
		rec "replica $n on $vm: plain request without the suppress header (a follower forwards to the leader)" vm_sh "$vm" "curl -s -m 5 -D - -o /dev/null http://127.0.0.1:$API_PORT/api/v1/setup/status | grep -iE '^HTTP|x-dcm' | tr -d '\r' | tr '\n' ' '"
	done
	for n in $HA_IDS; do
		[ "$n" = "$ln" ] && continue
		vm=$(vm_of "$n")
		rec "authenticated read via follower $n (session cookie replicated, forwarded to the leader)" sh -c \
			"limactl shell --workdir / $vm -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/auth/session | head -c 300" &&
			ok hafwd "authenticated GET answered by follower $n via forwarding"
		rec "write via follower $n (PUT audit-neutral: re-register its own key)" sh -c \
			"limactl shell --workdir / $vm -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/settings/replicas | head -c 200" || true
		rec "established forwarding connections on follower $n to the leader's :$MGR_PORT_FWD" vm_sh "$vm" "ss -tn state established '( dport = :$MGR_PORT_FWD )' | head -5 || true"
	done
	for n in $HA_IDS; do rec "log of replica $n (forwarding and election lines)" sh -c "limactl shell --workdir / $M1 -- sudo docker service logs --no-trunc --timestamps $(mgr_svc "$n") 2>&1 | grep -v 'setup token' | grep -iE 'forward|leader|elect|term|peer|tls|handshake' | cut -c1-230 | tail -15"; done
	rec "proxy-writers endpoint on a follower (not forwarded by design: followers answer 503, the proxy tries every manager URL)" sh -c "for n in 1 2 3; do limactl shell --workdir / dbcm-lab-m\$n -- curl -s -m 3 -o /dev/null -w \"replica \$n: HTTP %{http_code}\n\" -H 'Authorization: Bearer x' -H 'X-Proxy-ID: proxy-a' http://127.0.0.1:$API_PORT/api/v1/proxy-writers/$CLUSTER_NAME; done"
}

e9_hatls() {
	section "Phase hatls: peer mTLS handshakes and a replica with a wrong certificate"
	hostload
	local a1 a2 n out
	a1=$(uv2_ip dbcm-lab-m1) a2=$(uv2_ip dbcm-lab-m2)
	rec "manager-1 certificate subject, SANs, issuer and validity" vm_root "$M1" 'openssl x509 -in "$1/peers/manager-1/peer.pem" -noout -subject -issuer -dates -ext subjectAltName,extendedKeyUsage 2>&1' "$STATE"
	# Handshakes from m1 against the replica on m2, ports 7445 (metastore) and 7447 (API forwarding).
	for port in $MGR_PORT_META $MGR_PORT_FWD; do
		rec "openssl s_client to $a2:$port WITH the manager-1 peer certificate (must complete; server requests a client certificate)" vm_root "$M1" '
			echo | timeout 8 openssl s_client -connect "$1:$2" -CAfile "$3/peers/ca.pem" -cert "$3/peers/manager-1/peer.pem" -key "$3/peers/manager-1/peer-key.pem" -verify_return_error 2>&1 | grep -E "Verification|Protocol|Cipher|subject|issuer|alert|error|Verify return" | head -12' "$a2" "$port" "$STATE" || true
		rec "openssl s_client to $a2:$port WITHOUT a client certificate (must be refused)" vm_root "$M1" '
			(echo "GET / HTTP/1.0"; echo; sleep 1) | timeout 8 openssl s_client -connect "$1:$2" -CAfile "$3/peers/ca.pem" 2>&1 | grep -E "Verification|alert|error|Verify return|read:|SSL_|Post-Handshake|closed" | head -10' "$a2" "$port" "$STATE" || true
	done
	rec "rogue CA and a certificate for manager-1 signed by it (generated with manager -issue-peer-certs from the image, other passphrase)" vm_root "$M1" '
		set -e
		rm -rf "$1/rogue"; install -d -m 0700 "$1/rogue"
		docker run --rm --user 0 -e DBCM_PEER_CA_PASSPHRASE="rogue-passphrase-$(head -c 6 /dev/urandom | base64 | tr -d "/+=")" -v "$1:/work" "$2" -issue-peer-certs /work/rogue -installation "$3" -peer-ids manager-1,manager-2,manager-3
		ls "$1/rogue" "$1/rogue/manager-3"' "$STATE" "$IMAGE" "$INST"
	for port in $MGR_PORT_META $MGR_PORT_FWD; do
		rec "openssl s_client to $a2:$port with the ROGUE-signed manager-1 certificate (must be refused)" vm_root "$M1" '
			(echo "GET / HTTP/1.0"; echo; sleep 1) | timeout 8 openssl s_client -connect "$1:$2" -CAfile "$3/peers/ca.pem" -cert "$3/rogue/manager-1/peer.pem" -key "$3/rogue/manager-1/peer-key.pem" 2>&1 | grep -E "Verification|alert|error|Verify return|read:|SSL_|closed" | head -10' "$a2" "$port" "$STATE" || true
	done
	rec "log lines of replica 2 about the refused handshakes (last 20 lines mentioning tls/certificate/handshake/bad)" sh -c "limactl shell --workdir / $M1 -- sudo docker service logs --no-trunc --timestamps ${STACK}_manager-2 2>&1 | grep -iE 'tls|certificate|handshake|bad|unknown|denied|reject' | cut -c1-260 | tail -20"

	# An impostor: replica 3's service is stopped, and on m3 a container with
	# replica 3's identity but a certificate of the rogue CA tries to join.
	local vm3=dbcm-lab-m3 before after
	before=$(ha_leader_n || true)
	rec "scale replica 3 to 0 (the impostor takes its place)" vm_docker "$M1" service scale -d "${STACK}_manager-3=0"
	wait_until 90 "replica 3 stopped" sh -c "[ -z \"\$(limactl shell --workdir / $vm3 -- sudo docker ps -q -f label=com.docker.swarm.service.name=${STACK}_manager-3)\" ]" || true
	rec "prepare the impostor: manager-3 configuration that trusts the REAL peer CA but presents a certificate of the rogue CA, fresh data volume" vm_root "$M1" '
		set -e
		d="$1"; r="$d/rogue"
		cat "$r/manager-3/peer.pem" "$r/manager-3/peer-key.pem" > "$r/peer-bundle.pem"
		cp "$d/peers/ca.pem" "$r/real-ca.pem"
		jq ".manager.dataDir = \"/rogueData\"
			| .manager.peerTLS.caFile = \"/rogue/real-ca.pem\" | .manager.peerTLS.certFile = \"/rogue/peer-bundle.pem\" | .manager.peerTLS.keyFile = \"/rogue/peer-bundle.pem\"
			| del(.manager.peerTLS.renew)" "$d/manager-3.json" > "$r/manager.json"
		jq -c ".manager.peerTLS, .manager.nodeId, (.manager.peers | map(.id))" "$r/manager.json"' "$STATE"
	rec "ship the impostor files to m3 and start the impostor (host network, other API port, no Docker socket)" sh -c \
		"limactl shell --workdir / $M1 -- sudo tar -C $STATE -cf - rogue | limactl shell --workdir / $vm3 -- sudo sh -c 'rm -rf /root/e9-rogue && mkdir -p /root/e9-rogue && tar -C /root/e9-rogue -xf - && docker rm -f e9-impostor >/dev/null 2>&1; docker volume rm -f e9-impostor-data >/dev/null 2>&1; docker volume create e9-impostor-data >/dev/null && docker run -d --name e9-impostor --network host --user 0 -v /root/e9-rogue/rogue:/rogue:ro -v e9-impostor-data:/rogueData $IMAGE -listen 127.0.0.1:18080 -config /rogue/manager.json && sleep 1 && docker ps --filter name=e9-impostor --format \"{{.Names}} {{.Status}}\"'"
	sleep 40
	rec "impostor log (it must not join the quorum)" vm_args "$vm3" 'sudo docker logs --timestamps --tail 40 e9-impostor 2>&1 | grep -v "setup token" | cut -c1-260'
	rec "leader and follower logs about the impostor's connection attempts (tls/certificate lines)" sh -c "for i in 1 2; do echo \"== replica \$i\"; limactl shell --workdir / $M1 -- sudo docker service logs --no-trunc --timestamps ${STACK}_manager-\$i --since 2m 2>&1 | grep -iE 'tls|certificate|handshake|bad|unknown|denied|reject|identity' | cut -c1-260 | tail -12; done"
	rec "the impostor's connection attempts as seen by the kernel on m3 and the others (established 7445/7447)" sh -c "for v in dbcm-lab-m1 dbcm-lab-m2 dbcm-lab-m3; do echo \"== \$v\"; limactl shell --workdir / \$v -- sudo ss -tn '( sport = :$MGR_PORT_META or dport = :$MGR_PORT_META )' | head -6; done"
	after=$(ha_leader_n || true)
	say "leader before the impostor: replica ${before:-none}; with the impostor: replica ${after:-none}"
	[ -n "$after" ] && ok hatls "the two genuine replicas kept a leader (replica $after) while the wrong-certificate impostor tried to join" || assert_fail hatls "no leader with the impostor present"
	rec "cluster membership as the leader sees it (replica list)" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/settings/replicas | tail -n +2 | jq -c '{replicas: [.replicas[] | {id, leader, hasCurrentKey}]}'"
	rec "remove the impostor and its volume (labelled by name e9-impostor*)" vm_args "$vm3" 'sudo docker rm -f e9-impostor; sudo docker volume rm -f e9-impostor-data; sudo rm -rf /root/e9-rogue'
	rec "scale replica 3 back to 1" vm_docker "$M1" service scale -d "${STACK}_manager-3=1"
	wait_until 180 "three manager tasks running" ha_all_running 3 || assert_fail hatls "replica 3 did not return"
	wait_until 120 "replica 3 answers (follower or leader)" sh -c "limactl shell --workdir / $vm3 -- curl -s -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:$API_PORT/api/v1/setup/status | grep -q 200" || true
	for n in $HA_IDS; do rec "replica $n after the impostor test" ha_probe "$(vm_of "$n")"; done
	rec "replica 3 log after its return" mgr_logs 3 25 || true
}

# e9_sample_phase TAG SECONDS VM — API sampler on VM for the next SECONDS.
e9_hakill() {
	section "Phase hakill: SIGKILL of the leader replica's container; API continuity through a follower"
	hostload
	local ln lvm fvm fn t0 i newl
	ln=$(ha_leader_n) || { assert_fail hakill "no leader"; return 1; }
	lvm=$(vm_of "$ln")
	fn=$(for n in $HA_IDS; do [ "$n" != "$ln" ] && echo "$n"; done | head -1)
	fvm=$(vm_of "$fn")
	say "leader replica $ln on $lvm; sampling the API on follower VM $fvm (replica $fn) at 100 ms"
	e9_apisample_start "$fvm" hakill 60
	sleep 5
	t0=$(vm_sh "$fvm" 'date -u +%T.%3N')
	rec "docker kill the leader's container at ~$t0 (follower VM clock)" vm_docker "$lvm" kill "$(mgr_cid "$ln")"
	for i in 1 2 3 4 5 6 7 8; do
		sleep 3
		say "t+$((i * 3))s leader: $(ha_leader_n 2>/dev/null || echo none); running tasks: $(vm_docker "$M1" service ls --format '{{.Name}} {{.Replicas}}' | grep "${STACK}_manager" | tr '\n' ' ')"
	done
	sleep 25
	rec "API sampled on follower VM $fvm during the leader kill" e9_apisample_report "$fvm" hakill
	rec "non-200 samples on $fvm (first and last, with time)" vm_root "$fvm" 'grep -v " 200$" "$1/out/hakill.api" | sed -n "1p;\$p"; echo "count: $(grep -vc " 200$" "$1/out/hakill.api")"' "$E8"
	rec "tasks of the killed replica (Swarm restarted it)" vm_docker "$M1" service ps --format '{{.ID}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' "$(mgr_svc "$ln")"
	rec "log of the killed replica after its restart" mgr_logs "$ln" 25 || true
	newl=$(ha_leader_n || true)
	rec "log of the new leader (replica ${newl:-?})" mgr_logs "${newl:-1}" 25 || true
	for n in $HA_IDS; do rec "replica $n after the kill" ha_probe "$(vm_of "$n")"; done
	[ -n "$newl" ] && [ "$newl" != "$ln" ] && ok hakill "leader moved from replica $ln to replica $newl; the killed replica was restarted by Swarm" ||
		result "hakill: leader after the kill is replica ${newl:-none} (was $ln)"
	rec "session survives the leader change (replicated session)" api GET /api/v1/auth/session || assert_fail hakill "session lost"
}

e9_hadrain() {
	section "Phase hadrain: drain the node of the leader replica (pinned service cannot move) and activate it again"
	hostload
	local ln lvm node fvm fn t0 i
	ln=$(ha_leader_n) || { assert_fail hadrain "no leader"; return 1; }
	lvm=$(vm_of "$ln")
	fn=$(for n in $HA_IDS; do [ "$n" != "$ln" ] && echo "$n"; done | tail -1)
	fvm=$(vm_of "$fn")
	node=$(vm_docker "$lvm" node inspect self --format '{{.ID}}')
	say "leader replica $ln on $lvm; sampling the API on $fvm"
	e9_apisample_start "$fvm" hadrain 150
	sleep 3
	rec "before: node ls" vm_docker "$fvm" node ls
	t0=$(date +%s)
	rec "docker node update --availability drain $lvm" vm_docker "$fvm" node update --availability drain "$node"
	for i in 1 2 3 4 5 6; do
		sleep 10
		say "t+$((i * 10))s leader: $(ha_leader_n 2>/dev/null || echo none); $(vm_docker "$fvm" service ls --format '{{.Name}} {{.Replicas}}' | grep "${STACK}_manager" | tr '\n' ' ')"
	done
	rec "tasks of the drained replica (the constraint allows no other node)" vm_docker "$fvm" service ps --no-trunc --format '{{.ID}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' "$(mgr_svc "$ln")"
	rec "service list while drained" vm_docker "$fvm" service ls
	rec "other replicas keep serving: probes" sh -c "for n in 1 2 3; do [ \$n = $ln ] && continue; echo \"replica \$n: \$(limactl shell --workdir / dbcm-lab-m\$n -- curl -s -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:$API_PORT/api/v1/setup/status)\"; done"
	rec "volume of the drained replica is kept on its node" vm_docker "$lvm" volume ls --filter "name=${STACK}_manager-data"
	rec "activate $lvm again" vm_docker "$fvm" node update --availability active "$node"
	wait_until 240 "three manager tasks running" ha_all_running 3 || assert_fail hadrain "the drained replica did not return after activation"
	say "drain start to all three running again: $(($(date +%s) - t0)) s"
	sleep 20
	rec "API sampled on $fvm across drain and activation" e9_apisample_report "$fvm" hadrain
	for n in $HA_IDS; do rec "replica $n after the drain cycle" ha_probe "$(vm_of "$n")"; done
	rec "log of the returned replica" mgr_logs "$ln" 20 || true
	rec "session and data after the drain cycle" api GET /api/v1/auth/session && ok hadrain "session valid after the drain cycle" || assert_fail hadrain "session lost"
	rec "keyring status after the drain cycle" sh -c "limactl shell --workdir / $M1 -- sudo /usr/local/bin/dbcm-e4-client -state $STATE/client.json -base http://127.0.0.1:$API_PORT -origin http://localhost:$API_PORT req GET /api/v1/settings/replicas | tail -n +2 | jq -c '{currentKey, replicas: [.replicas[] | {id, leader, hasCurrentKey}]}'"
}

e9_agents() {
	section "Phase agents: connect the Swarm through the HA manager, enroll hosts, deploy agents"
	hostload
	phase_swarmapi || { assert_fail agents "agents did not enroll"; return 1; }
	ok agents "three agents enrolled through the replicated manager"
}

e9_cluster() {
	section "Phase cluster: Galera cluster $CLUSTER_NAME through the replicated manager"
	hostload
	CLUSTER_ID=""
	phase_cluster || true
	local st
	st=$(apij GET /api/v1/operations | jq -r --arg id "$OP_ID" '.items[] | select(.id==$id) | .state')
	[ "$st" = succeeded ] && ok cluster "create operation succeeded" || { assert_fail cluster "create operation state: $st"; return 1; }
	phase_verify || assert_fail cluster-verify "MariaDB checks failed"
}

# shellcheck source=tests/p0/e9-phases-proxy.sh
. "$REPO_ROOT/tests/p0/e9-phases-proxy.sh"
