# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2016
#
# E8 phases, sourced by tests/p0/e8-proxy-redundancy-powerloss.sh after the
# E4 library and the E6 helpers.
#
# Layout (gate A): the clients run on m1 (host, outside any proxy). Two proxy
# containers (cmd/proxy, published on host port 3306, attached to the
# cluster's attachable overlay network) run on m2 and m3, each with a
# keepalived (VRRP unicast, nopreempt, VIP 192.168.104.250) whose check
# script asks the proxy's own status endpoint for "has_writer". m1 hosts the
# manager and the only Swarm manager, so it is never stopped in gate A; both
# proxies are pinned (prefer-node) to the Galera node that runs on m1, so the
# loss of a proxy VM (which also hosts a Galera node) does not change the
# writer and the measured gap is the proxy/VIP/client failover alone.

E8_STEPS="e8prep e8vip e8baseline e8proxyloss e8vmloss e8integrity e8teardown e8node e8allloss"

E8=/root/dbcm-e8 # inside every VM: binaries, configs, passwords (root 0700)
PX=dbcm-e8-proxy
VIP=192.168.104.250
PXVMS="dbcm-lab-m2 dbcm-lab-m3"
E8_APP='-user e5app -password-file '$E8'/app.pw'

# ---------------------------------------------------------------- helpers
e8_vm_up() { [ "$(limactl list --format '{{.Status}}' "$1" 2>/dev/null)" = Running ]; }
e8_now() { vm_sh "$M1" 'date -u +%T.%3N'; }
e8_other_px() { # VM -> the other proxy VM
	local v
	for v in $PXVMS; do [ "$v" = "$1" ] || echo "$v"; done
}
e8_node_on() { # VM -> slot number of the Galera node running there
	local n
	for n in 1 2 3; do
		[ "$(node_vm "$n")" = "$1" ] && {
			echo "$n"
			return 0
		}
	done
	return 1
}

# e8_measure LABEL — resource snapshot: host load/CPU/disk, then per running
# lab VM load, memory, container CPU/memory.
e8_measure() {
	say "resources: $1"
	{
		printf '### resources: %s\n\n```\n' "$1"
		date -u +%FT%TZ
		echo "host: $(sysctl -n vm.loadavg)"
		top -l 2 -n 0 -s 1 2>/dev/null | grep 'CPU usage' | tail -1 || true
		host_free_disk
		echo "VM allocation: $(limactl list --format '{{.Name}} {{.Status}} cpus={{.CPUs}} mem={{.Memory}}' 2>/dev/null | grep '^dbcm-lab-' | tr '\n' ';')"
		echo "VM disk files (du, MiB): $(du -sk "$HOME"/.lima/dbcm-lab-m* 2>/dev/null | awk '{printf "%s=%d ", $2, $1/1024}' | sed "s#$HOME/.lima/##g")"
	} >>"$LOG"
	local vm
	for vm in $VMS; do
		if e8_vm_up "$vm"; then
			vm_sh "$vm" 'echo "== $(hostname): loadavg $(cut -d" " -f1-3 /proc/loadavg), $(free -m | awk "/Mem:/{printf \"mem used %d MiB of %d MiB\", \$3, \$2}")"; sudo docker stats --no-stream --format "{{.Name}} cpu={{.CPUPerc}} mem={{.MemUsage}}" 2>/dev/null | cut -c1-110' >>"$LOG" 2>&1 || echo "== $vm: measurement failed" >>"$LOG"
		else
			echo "== $vm: not running" >>"$LOG"
		fi
	done
	printf '```\n\n' >>"$LOG"
}

# e8_vm_start VM — limactl start; one retry after a pause (host may be under
# foreign load), then give up.
e8_vm_start() {
	require_lab_vm "$1"
	local t0
	t0=$(date +%s)
	if ! rec "limactl start $1" limactl start "$1"; then
		say "limactl start $1 failed; pausing 30 s for one retry"
		sleep 30
		rec "limactl start $1 (retry)" limactl start "$1" || return 1
	fi
	say "$1 started after $(($(date +%s) - t0)) s"
}
e8_vm_stop_force() {
	require_lab_vm "$1"
	rec "limactl stop --force $1 (power loss)" limactl stop --force "$1"
}
e8_node_ready() { vm_docker "$M1" node ls --format '{{.Hostname}} {{.Status}}' | grep -qx "lima-$1 Ready"; }

e8_synced3() {
	local n s
	for n in 1 2 3; do
		s=$(rsql "$n" "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_local_state_comment')" 2>/dev/null | tr '\n\t' '  ') || return 1
		case "$s" in *"wsrep_cluster_size 3"*Synced*) ;; *) return 1 ;; esac
	done
}
e8_status3() { # N lines: wsrep status of each node (best effort)
	local n
	for n in 1 2 3; do echo "n$n: $(rsql "$n" "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_cluster_status','wsrep_local_state_comment','wsrep_last_committed')" 2>&1 | tr '\n\t' '  ')"; done
}

# ---------------------------------------------------------------- gate A
e8_proxy_config() { # MODE PREFERRED(nK or empty)
	local pref=""
	[ -n "$2" ] && pref="\"preferred_id\": \"$2\", "
	cat <<EOF
{
  "write_listen": "0.0.0.0:3306",
  "read_listen": "0.0.0.0:3307",
  "control_listen": "127.0.0.1:9306",
  "nodes": [
    {"id": "n1", "address": "$(svc 1):3306", "role": "writer-eligible"},
    {"id": "n2", "address": "$(svc 2):3306", "role": "writer-eligible"},
    {"id": "n3", "address": "$(svc 3):3306", "role": "writer-eligible"}
  ],
  "policy": {"mode": "$1", ${pref}"read": "exclude-writer", "stale_after": "5s", "drain_deadline": "10s"},
  "probe": {"liveness": "mysql-greeting", "sync": "galera", "interval": "1s", "timeout": "1s",
            "monitor": {"user": "dbcm_e5mon", "password_file": "monitor.pw", "tls": "disabled"}},
  "limits": {"max_connections": 200, "idle_timeout": "10m", "dial_timeout": "2s", "handshake_timeout": "5s"}
}
EOF
}
e8_status() { vm_docker "$1" exec "$PX" /e8/e5client status | tail -n +2; }
e8_has_writer() { vm_docker "$1" exec "$PX" /e8/e5client status | grep -Eq 'has_writer"?: *true'; }
e8_brief() { e8_status "$1" | jq -c '{writer, has_writer, writer_changes, nodes: [.nodes[] | "\(.id) \(.health)/\(.sync) conns=\(.connections)"]}'; }
e8_both_writer() { # NODE-ID — both proxies report this writer
	local v
	for v in $PXVMS; do [ "$(e8_status "$v" | jq -r .writer)" = "$1" ] || return 1; done
}
e8_set_policy() { # MODE PREFERRED
	local v
	for v in $PXVMS; do
		e8_proxy_config "$1" "$2" | vm_root "$v" 'cat >"$1/proxy.json"' "$E8"
		vm_docker "$v" kill -s HUP "$PX" >/dev/null
	done
}
e8_vip_holder() { # VMs currently holding the VIP (more than one = split VIP)
	local v
	for v in $PXVMS; do
		e8_vm_up "$v" || continue
		vm_args "$v" 'ip -4 -o addr show | grep -q " $1/"' "$VIP" && echo "$v"
	done
	return 0
}
e8_vip_held() { [ -n "$(e8_vip_holder)" ]; }
e8_vip_single() { [ "$(e8_vip_holder | wc -l | tr -d ' ')" = 1 ]; }
e8c() { vm_root "$M1" "$E8/e5client \"\$@\"" "$@"; } # e5client on m1

phase_e8prep() {
	section "Phase e8prep (gate A): accounts, binaries, two proxy containers on m2 and m3"
	hostload
	local vm
	for vm in $VMS; do
		limactl copy "$BIN/proxy" "$BIN/e5client" "$vm:/tmp/"
		rec "install proxy and e5client in $E8 on $vm" vm_root "$vm" '
			set -e
			install -d -m 0700 "$1" "$1/out"
			install -m 0755 /tmp/proxy /tmp/e5client "$1/"
			rm -f /tmp/proxy /tmp/e5client' "$E8"
	done
	rec "generate passwords on m1 (values never logged)" vm_root "$M1" '
		for n in monitor app; do [ -f "$1/$n.pw" ] || head -c 18 /dev/urandom | base64 | tr -d "/+\n" >"$1/$n.pw"; done
		chmod 0600 "$1"/*.pw; ls -l "$1"/*.pw' "$E8"
	for vm in $PXVMS; do
		vm_root "$M1" 'cat "$1/monitor.pw"' "$E8" | vm_root "$vm" 'cat >"$1/monitor.pw"; chmod 0600 "$1/monitor.pw"' "$E8"
	done
	local mpw apw
	mpw=$(vm_root "$M1" 'cat "$1/monitor.pw"' "$E8")
	apw=$(vm_root "$M1" 'cat "$1/app.pw"' "$E8")
	rsql 1 "CREATE USER IF NOT EXISTS 'dbcm_e5mon'@'%' IDENTIFIED BY '$mpw';
		CREATE USER IF NOT EXISTS 'e5app'@'%' IDENTIFIED BY '$apw';
		CREATE DATABASE IF NOT EXISTS e5;
		CREATE TABLE IF NOT EXISTS e5.load (id BIGINT AUTO_INCREMENT PRIMARY KEY, node VARCHAR(64), label VARCHAR(64), at TIMESTAMP(3) DEFAULT CURRENT_TIMESTAMP(3));
		GRANT ALL ON e5.* TO 'e5app'@'%';" >/dev/null
	mpw="" apw=""
	rec "proxy accounts exist (grants, n2)" rsql 2 "SHOW GRANTS FOR 'dbcm_e5mon'@'%'; SHOW GRANTS FOR 'e5app'@'%'"
	for vm in $PXVMS; do
		e8_proxy_config single-writer-stable "" | vm_root "$vm" 'cat >"$1/proxy.json"' "$E8"
		rec "start $PX on $vm (published on host port 3306, overlay network $NET)" vm_args "$vm" '
			sudo docker rm -f "$1" >/dev/null 2>&1 || true
			sudo docker run -d --name "$1" --hostname "$1" --network "$2" -p 3306:3306 --restart unless-stopped --label dbcm.e8=lab \
				-v "$3:/e8:ro" --entrypoint /e8/proxy "$4" -config /e8/proxy.json' "$PX" "$NET" "$E8" "$MARIADB_IMAGE"
	done
	for vm in $PXVMS; do
		wait_until 60 "proxy on $vm has a writer" e8_has_writer "$vm" ||
			{ rec "proxy log on $vm" vm_docker "$vm" logs --tail 30 "$PX"; return 1; }
		rec "proxy status on $vm" e8_status "$vm"
	done
	rec "published ports on the proxy VMs" vm_sh dbcm-lab-m2 "sudo ss -ltn | grep -E ':3306|:3307' || true"
}

phase_e8vip() {
	section "Phase e8vip (gate A): keepalived (VRRP unicast) VIP $VIP over both proxy VMs"
	hostload
	local vm iface self peer prio=150
	for vm in $PXVMS; do
		peer=$(uv2_ip "$(e8_other_px "$vm")")
		self=$(uv2_ip "$vm")
		iface=$(vm_args "$vm" 'ip -4 -o addr show | grep -F " inet $1" | awk "{print \$2}" | head -1' "$USERV2_PREFIX")
		say "$vm: interface $iface, address $self, peer $peer, priority $prio"
		rec "install keepalived on $vm" vm_sh "$vm" 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq keepalived >/dev/null 2>&1; keepalived --version 2>&1 | head -1'
		rec "keepalived check script and configuration on $vm" vm_root "$vm" '
			set -e
			cat >/usr/local/bin/dbcm-e8-proxycheck <<EOF
#!/bin/sh
# healthy = the local proxy container reports a writer
docker exec $6 /e8/e5client status | grep -Eq "has_writer\"?: *true"
EOF
			chmod 0755 /usr/local/bin/dbcm-e8-proxycheck
			cat >/etc/keepalived/keepalived.conf <<EOF
global_defs {
  script_user root
  router_id $(hostname)
}
vrrp_script chk_proxy {
  script "/usr/local/bin/dbcm-e8-proxycheck"
  interval 1
  timeout 2
  fall 2
  rise 2
}
vrrp_instance DBCM_E8 {
  state BACKUP
  interface $1
  virtual_router_id 61
  priority $4
  nopreempt
  advert_int 1
  unicast_src_ip $2
  unicast_peer {
    $3
  }
  virtual_ipaddress {
    $5/24 dev $1
  }
  track_script {
    chk_proxy
  }
}
EOF
			cat /etc/keepalived/keepalived.conf
			systemctl enable --now keepalived >/dev/null 2>&1
			systemctl restart keepalived
			sleep 1; systemctl is-active keepalived' "$iface" "$self" "$peer" "$prio" "$VIP" "$PX"
		prio=100
	done
	wait_until 60 "exactly one VM holds the VIP" e8_vip_single ||
		{ for vm in $PXVMS; do rec "keepalived journal on $vm" vm_sh "$vm" 'sudo journalctl -u keepalived --no-pager | tail -20'; done; return 1; }
	say "VIP holder: $(e8_vip_holder)"
	for vm in $PXVMS; do rec "keepalived journal on $vm" vm_sh "$vm" 'sudo journalctl -u keepalived --no-pager | tail -12 | cut -c1-200'; done
	rec "client on m1 through the VIP ($VIP:3306)" e8c query -addr "$VIP:3306" $E8_APP "SELECT @@hostname, CURRENT_USER()"
	rec "ARP entry for the VIP on m1" vm_sh "$M1" "ip neigh show $VIP"
	rec "round trips through the VIP vs direct proxy" sh -c "limactl shell --workdir / $M1 -- sudo $E8/e5client bench -addr $VIP:3306 $E8_APP -n 300"
}

phase_e8baseline() {
	section "Phase e8baseline (gate A): writer agreement of two independent proxies, resources at rest"
	hostload
	e8_measure "gate A baseline: cluster idle, two proxies, VIP"
	local vm
	for vm in $PXVMS; do rec "proxy status on $vm (single-writer-stable)" e8_brief "$vm"; done
	local ip2 ip3
	ip2=$(uv2_ip dbcm-lab-m2)
	ip3=$(uv2_ip dbcm-lab-m3)
	rec "20 s concurrent writes straight into each proxy (stable policy; do they agree on one writer?)" sh -c \
		"limactl shell --workdir / $M1 -- sudo sh -c '$E8/e5client load -addr $ip2:3306 $E8_APP -mode per-connection -duration 20s -interval 100ms -label e8-base-p2 | tail -1 & $E8/e5client load -addr $ip3:3306 $E8_APP -mode per-connection -duration 20s -interval 100ms -label e8-base-p3 | tail -1 & wait'"
	rec "writer nodes per label (stable policy)" rsql 1 "SELECT label, node, COUNT(*) FROM e5.load WHERE label LIKE 'e8-base-%' GROUP BY label, node ORDER BY label, node"
	local k
	k=$(e8_node_on "$M1")
	say "Galera node on m1 (never stopped in gate A): n$k; pinning both proxies to it (prefer-node)"
	e8_set_policy prefer-node "n$k"
	wait_until 60 "both proxies report writer n$k" e8_both_writer "n$k" || true
	for vm in $PXVMS; do rec "proxy status on $vm (prefer-node n$k)" e8_brief "$vm"; done
	rec "20 s concurrent writes into each proxy (prefer-node n$k)" sh -c \
		"limactl shell --workdir / $M1 -- sudo sh -c '$E8/e5client load -addr $ip2:3306 $E8_APP -mode per-connection -duration 20s -interval 100ms -label e8-pin-p2 | tail -1 & $E8/e5client load -addr $ip3:3306 $E8_APP -mode per-connection -duration 20s -interval 100ms -label e8-pin-p3 | tail -1 & wait'"
	rec "writer nodes per label (prefer-node)" rsql 1 "SELECT label, node, COUNT(*) FROM e5.load WHERE label LIKE 'e8-pin-%' GROUP BY label, node ORDER BY label, node"
}

# e8_clients_start TAG DURATION HOLDER-VM — four clients on m1: through the
# VIP (per-connection and persistent) and with a client-side failover list
# that names the holder first (per-connection and persistent).
e8_clients_start() {
	local tag=$1 dur=$2 holder=$3 other list
	other=$(e8_other_px "$holder")
	list="$(uv2_ip "$holder"):3306,$(uv2_ip "$other"):3306"
	local spec lab addr mode
	for spec in "vip-pc|$VIP:3306|per-connection" "vip-ps|$VIP:3306|persistent" "cs-pc|$list|per-connection" "cs-ps|$list|persistent"; do
		lab="e8-$tag-${spec%%|*}"
		addr=$(echo "$spec" | cut -d'|' -f2)
		mode=$(echo "$spec" | cut -d'|' -f3)
		vm_root "$M1" '
			systemd-run -q --unit "$1" --collect -p StandardOutput=file:"$5/out/$1.out" -p StandardError=file:"$5/out/$1.out" \
				"$5/e5client" load -addr "$2" -user e5app -password-file "$5/app.pw" -mode "$3" -duration "$4" -interval 100ms -label "$1"' \
			"$lab" "$addr" "$mode" "$dur" "$E8"
	done
	say "clients started ($tag, $dur): vip-pc vip-ps cs-pc cs-ps; failover list $list"
}
e8_clients_done() { [ "$(vm_root "$M1" 'systemctl list-units "e8-*" --no-legend | wc -l')" = 0 ]; }
e8_clients_report() { # TAG
	local l
	for l in vip-pc vip-ps cs-pc cs-ps; do
		rec "client e8-$1-$l: transitions and summary" vm_root "$M1" 'cut -c1-420 "$1/out/e8-$2-$3.out" | grep -v "^SUMMARY" ; grep "^SUMMARY" "$1/out/e8-$2-$3.out" | sed "s/^SUMMARY //" | jq -c "{label, ok, errors, max_gap_without_success, max_gap_from, reconnects, per_node, error_examples}"' "$E8" "$1" "$l"
	done
}
e8_keepalived_journal() { # VM SINCE
	vm_sh "$1" "sudo journalctl -u keepalived --no-pager --since '$2' | grep -iE 'state|script|VIP|master|backup|fault' | cut -c1-170 | tail -14"
}

# e8_proxykill TAG RESTART(yes|no) DURATION — kill the VIP holder's proxy
# container under load. With RESTART=yes Docker's restart policy brings it
# back (crash-restart); with no, the policy is disabled for the test so the
# keepalived check script has to move the VIP.
e8_proxykill() {
	local tag=$1 restart=$2 dur=$3 holder other since t0 t1 i
	e8_vip_single || { result "no single VIP holder before $tag"; return 1; }
	holder=$(e8_vip_holder)
	other=$(e8_other_px "$holder")
	say "$tag: VIP holder $holder; proxy container killed there (restart policy kept: $restart); other proxy VM $other"
	[ "$restart" = yes ] || rec "disable the restart policy of $PX on $holder for this test" vm_docker "$holder" update --restart=no "$PX"
	since=$(vm_sh "$M1" 'date "+%F %T"')
	e8_clients_start "$tag" "$dur" "$holder"
	sleep 5
	e8_measure "gate $tag: under load (4 clients, 40 writes/s), before the kill"
	sleep 10
	t0=$(e8_now)
	rec "kill the proxy container on $holder at $t0" vm_docker "$holder" kill "$PX"
	t1=$(e8_now)
	say "container kill returned at $t1 (sent at $t0)"
	for i in 1 2 3 4 5 6; do
		sleep 2
		say "t+$((i * 2))s VIP holders: $(e8_vip_holder | tr '\n' ' ')"
	done
	wait_until 120 "clients finished" e8_clients_done || true
	e8_clients_report "$tag"
	rec "keepalived journal on $holder (kill at $t0)" e8_keepalived_journal "$holder" "$since"
	rec "keepalived journal on $other" e8_keepalived_journal "$other" "$since"
	rec "VIP holder(s) after the kill" e8_vip_holder
	rec "proxy on $other" e8_brief "$other"
	rec "container state after the kill (does the restart policy bring a docker-killed container back?)" vm_docker "$holder" ps -a --filter "name=$PX" --format '{{.Names}} {{.Status}}'
	rec "restore the restart policy and start the container on $holder" sh -c "limactl shell --workdir / $holder -- sudo sh -c 'docker update --restart=unless-stopped $PX && (docker start $PX || true)'"
	wait_until 60 "proxy on $holder has a writer" e8_has_writer "$holder" || true
	sleep 10
	rec "VIP holder(s) after the proxy came back (nopreempt: no second move; exactly one holder)" e8_vip_holder
	rec "proxy status on $holder" e8_brief "$holder"
}

phase_e8proxyloss() {
	section "Phase e8proxyloss (gate A1): the VIP holder's proxy container is killed under load"
	hostload
	e8_proxykill A1a yes 45s
	e8_proxykill A1b no 60s
}

phase_e8vmloss() {
	section "Phase e8vmloss (gate A2): the VIP holder's whole VM is hard-stopped under load"
	hostload
	e8_vip_single || { result "no single VIP holder before the test"; return 1; }
	local holder other since t0 k
	holder=$(e8_vip_holder)
	other=$(e8_other_px "$holder")
	k=$(e8_node_on "$holder" || echo "?")
	say "VIP holder: $holder (also hosts Galera node n$k); other proxy VM: $other"
	since=$(vm_sh "$M1" 'date "+%F %T"')
	e8_clients_start A2 100s "$holder"
	sleep 15
	e8_measure "gate A2: under load, before the VM power loss"
	t0=$(e8_now)
	e8_vm_stop_force "$holder"
	say "limactl stop --force returned; sent at $t0 (m1 clock), now $(e8_now)"
	local i
	for i in 1 2 3 4 5 6; do
		sleep 3
		say "t+$((i * 3))s VIP holders: $(e8_vip_holder | tr '\n' ' ')"
	done
	e8_measure "gate A2: holder VM down, surviving VMs under load"
	wait_until 150 "clients finished" e8_clients_done || true
	e8_clients_report A2
	rec "keepalived journal on $other (power loss at $t0)" e8_keepalived_journal "$other" "$since"
	rec "VIP holder(s) while $holder is down" e8_vip_holder
	rec "proxy on $other" e8_brief "$other"
	rec "Galera cluster while $holder is down" e8_status3
	rec "manager's view of the cluster" api GET "/api/v1/clusters/$(cluster_id)"
	e8_vm_start "$holder" || { result "VM $holder did not start (see log)"; return 1; }
	wait_until 300 "$holder reachable over ssh" vm_sh "$holder" true || true
	wait_until 300 "$holder is a Ready Swarm node" e8_node_ready "$holder" || true
	wait_until 600 "Galera cluster back to 3 synced nodes" e8_synced3 || true
	say "Galera n$k rejoined $(cat "$WAIT_ELAPSED_FILE") s after the Swarm node was ready"
	rec "Galera cluster after the VM returned" e8_status3
	wait_until 120 "keepalived on $holder running" vm_sh "$holder" 'systemctl is-active keepalived' || true
	wait_until 120 "proxy on $holder has a writer" e8_has_writer "$holder" || true
	sleep 10
	rec "VIP holder(s) after the VM returned (exactly one)" e8_vip_holder
	rec "keepalived journal on $holder after the restart" e8_keepalived_journal "$holder" "$since"
	rec "proxy status on $holder" e8_brief "$holder"
	e8_measure "gate A after the proxy VM returned"
}

phase_e8integrity() {
	section "Phase e8integrity (gate A): acknowledged writes, writer nodes and node consistency"
	hostload
	local l n tag ok rows lost=0 line
	rec "rows per label and node (all gate A labels)" rsql 1 "SELECT label, node, COUNT(*), MIN(at), MAX(at) FROM e5.load WHERE label LIKE 'e8-%' GROUP BY label, node ORDER BY label, node"
	rec "distinct writer nodes per phase (split-brain check: pinned phases must show one node)" rsql 1 "SELECT SUBSTRING_INDEX(SUBSTRING_INDEX(label,'-',2),'-',-1) AS phase, COUNT(DISTINCT node) AS writer_nodes, GROUP_CONCAT(DISTINCT node) FROM e5.load WHERE label LIKE 'e8-%' GROUP BY phase"
	for tag in A1a A1b A2; do
		for l in vip-pc vip-ps cs-pc cs-ps; do
			ok=$(vm_root "$M1" 'grep "^SUMMARY" "$1/out/e8-$2-$3.out" | sed "s/^SUMMARY //" | jq -r .ok' "$E8" "$tag" "$l" 2>/dev/null || echo "")
			[ -n "$ok" ] || {
				say "no summary for e8-$tag-$l"
				continue
			}
			rows=$(rsql 1 "SELECT COUNT(*) FROM e5.load WHERE label='e8-$tag-$l'" | tr -d '[:space:]')
			line="e8-$tag-$l: acknowledged $ok, rows in the database $rows"
			if [ "$rows" -lt "$ok" ]; then
				line="$line  LOST ACKNOWLEDGED WRITES: $((ok - rows))"
				lost=1
			else
				line="$line  (unacknowledged but committed: $((rows - ok)))"
			fi
			say "$line"
			printf '%s\n' "$line" >>"$LOG"
		done
	done
	[ "$lost" = 0 ] && result "no acknowledged write was lost in gate A" || result "ACKNOWLEDGED WRITES LOST in gate A (see lines above)"
	for n in 1 2 3; do
		rec "n$n row count and checksum of e5.load" rsql "$n" "SELECT COUNT(*), MAX(id), SUM(CRC32(CONCAT(id,label,node))) FROM e5.load"
	done
}

phase_e8teardown() {
	section "Phase e8teardown (gate A to B): proxies and keepalived removed on the lab VMs"
	hostload
	local vm
	for vm in $PXVMS; do
		e8_vm_up "$vm" || continue
		rec "remove proxy container and stop keepalived on $vm" vm_sh "$vm" 'sudo systemctl disable --now keepalived >/dev/null 2>&1; sudo docker rm -f '"$PX"' >/dev/null 2>&1; echo "keepalived: $(systemctl is-active keepalived || true); containers: $(sudo docker ps -q --filter label=dbcm.e8=lab | wc -l)"'
	done
	rec "VIP held anywhere (must be none)" e8_vip_holder
	wait_until 600 "Galera cluster at 3 synced nodes" e8_synced3 || true
}

# ---------------------------------------------------------------- gate B
e8_sentinel() { # NODE ID NOTE
	rsql "$1" "REPLACE INTO e8.sentinel (id, note, node) VALUES ($2, '$3', @@hostname)"
}
e8_sentinels_all() { # EXPECTED-IDS — every running node shows exactly these ids
	local n got ok=0
	for n in 1 2 3; do
		got=$(rsql "$n" "SELECT COALESCE(GROUP_CONCAT(id ORDER BY id),'') FROM e8.sentinel" 2>&1 | tr -d '[:space:]') || got="unreachable"
		printf 'n%s sentinel ids: %s (expected %s)\n' "$n" "$got" "$1"
		[ "$got" = "$1" ] || ok=1
	done
	return "$ok"
}

phase_e8node() {
	section "Phase e8node (gate B1): hard power loss of one Galera node VM, restart, rejoin"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	E6_UUID=$(e6_uuid 1)
	rec "sentinel table and first sentinel" rsql 1 "CREATE DATABASE IF NOT EXISTS e8; CREATE TABLE IF NOT EXISTS e8.sentinel (id INT PRIMARY KEY, note VARCHAR(64), node VARCHAR(64), at TIMESTAMP(3) DEFAULT CURRENT_TIMESTAMP(3)); REPLACE INTO e8.sentinel (id, note, node) VALUES (1, 'before node power loss', @@hostname)"
	rec "wsrep status before" e8_status3
	e8_measure "gate B1 before: cluster idle"
	local victim=dbcm-lab-m3 k surv
	k=$(e8_node_on "$victim")
	surv=1
	[ "$k" = 1 ] && surv=2
	say "victim VM: $victim (Galera node n$k); survivor used for writes: n$surv; cluster state UUID $E6_UUID"
	rec "service specs before" e6_specs
	e8_vm_stop_force "$victim"
	say "power loss sent at $(e8_now)"
	sleep 25
	rec "wsrep status of the survivors" e8_status3
	rec "sentinel 2 written on n$surv during the outage" e8_sentinel "$surv" 2 'during node power loss'
	e8_measure "gate B1 during: one VM down"
	sleep 30
	rec "manager view of the cluster after 55 s" api GET "/api/v1/clusters/$CLUSTER_ID"
	rec "manager events" e6_events
	rec "operations (no recovery operation may exist)" e6_ops
	rec "service specs during the outage (no bootstrap flag)" e6_specs
	local t0
	t0=$(date +%s)
	e8_vm_start "$victim" || { result "VM $victim did not start (see log)"; return 1; }
	wait_until 300 "$victim reachable over ssh" vm_sh "$victim" true || true
	say "ssh reachable $(($(date +%s) - t0)) s after limactl start began"
	wait_until 300 "$victim is a Ready Swarm node" e8_node_ready "$victim" || true
	wait_until 600 "Galera cluster back to 3 synced nodes" e8_synced3 || true
	say "all three Synced $(($(date +%s) - t0)) s after limactl start began"
	rec "wsrep status after" e8_status3
	e8_measure "gate B1 after: cluster rejoined"
	rec "n$k join lines (IST or SST?)" sh -c "limactl shell --workdir / $victim -- sudo docker logs --since 10m \$(limactl shell --workdir / $victim -- sudo docker ps -q -f label=com.docker.swarm.service.name=$(svc "$k") | head -1) 2>&1 | grep -E 'IST received|SST|State transfer|Synchronized|Quorum results|safe_to_bootstrap|Recovered position|crash recovery|InnoDB.*recover' | cut -c1-190 | head -14"
	rec "sentinels on every node (1 and 2 expected)" e8_sentinels_all "1,2"
	if [ "$(e6_uuid 1)" = "$E6_UUID" ]; then result "B1: node power loss kept the cluster state UUID, rejoined without any bootstrap"; else result "B1: cluster state UUID CHANGED"; fi
	rec "service specs after (no bootstrap flag)" e6_specs
	rec "operations after (still no recovery operation)" e6_ops
	rec "manager view after" api GET "/api/v1/clusters/$CLUSTER_ID"
	rec "events after" e6_events
	rec "agents enrolled again" api GET /api/v1/hosts
}

e8_primary_any() { # lists any node that reports a Primary component
	local n s any=0
	for n in 1 2 3; do
		s=$(rsql "$n" "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_status','wsrep_cluster_size')" 2>/dev/null | tr '\n\t' '  ') || continue
		[ -n "$s" ] || continue
		echo "n$n: $s"
		case "$s" in *"wsrep_cluster_status Primary"*) any=1 ;; esac
	done
	[ "$any" = 0 ] && echo "no node reports a Primary component"
	return 0
}

phase_e8allloss() {
	section "Phase e8allloss (gate B2): hard power loss of all three VMs, restart, guided recovery"
	hostload
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	E6_UUID=$(e6_uuid 1)
	e6_hosts
	rec "sentinel 3 before the full power loss" e8_sentinel 1 3 'before full power loss'
	rec "sentinels on every node (1,2,3)" e8_sentinels_all "1,2,3"
	rec "wsrep positions before" e6_positions
	e8_measure "gate B2 before: cluster idle"
	local vm t0 pids=""
	t0=$(date +%s)
	for vm in $VMS; do
		limactl stop --force "$vm" >>"$LOG" 2>&1 &
		pids="$pids $!"
	done
	# shellcheck disable=SC2086
	wait $pids || true
	say "all three VMs force-stopped in $(($(date +%s) - t0)) s: $(limactl list --format '{{.Name}}={{.Status}}' | grep '^dbcm-lab' | tr '\n' ' ')"
	sleep 20
	t0=$(date +%s)
	# m1 first: it holds the only Swarm manager and the manager's data.
	for vm in $VMS; do
		e8_vm_start "$vm" || { result "VM $vm did not start (see log)"; return 1; }
		wait_until 300 "$vm reachable over ssh" vm_sh "$vm" true || true
	done
	say "all three VMs reachable $(($(date +%s) - t0)) s after the first limactl start"
	e8_measure "gate B2: VMs booted, Swarm and database services starting"
	rec "ground truth after the power loss: grastate.dat per node (read before anything restarts them again)" e6_grastate
	rec "Swarm after the boot" vm_docker "$M1" node ls
	rec "manager unit after the boot (lab: transient systemd unit, not a product service)" vm_sh "$M1" "systemctl is-active $UNIT || true"
	rec "restart the lab manager unit (same config and data directory)" vm_args "$M1" 'sudo systemd-run --unit "$1" -p Restart=no --collect /usr/local/bin/dbcm-e4-manager -listen "127.0.0.1:$2" -config /etc/dbcm-e4/manager.json' "$UNIT" "$API_PORT"
	wait_until 60 "manager health" vm_sh "$M1" "curl -fsS http://127.0.0.1:$API_PORT/api/v1/health" || true
	wait_until 180 "manager leads (API not 503)" api GET /api/v1/setup/status || true
	local before after i
	before=$(e6_specs)
	for i in 1 2 3 4 5 6 7 8 9 10; do
		sleep 15
		say "t+$((i * 15))s running db tasks: $(e6_running_tasks); cluster: $(apij GET "/api/v1/clusters/$CLUSTER_ID" 2>/dev/null | jq -c '{state, nodes: [.nodes[] | "\(.id) \(.state)"]}' 2>/dev/null || echo unavailable)"
	done
	rec "any node Primary after 150 s? (automatic bootstrap would show here)" e8_primary_any
	after=$(e6_specs)
	rec "service specs before the observation window" printf '%s\n' "$before"
	rec "service specs after the observation window" printf '%s\n' "$after"
	if [ "$before" = "$after" ] && ! printf '%s' "$after" | grep -q 'bootstrap=yes'; then
		result "B2 hands off: no service spec changed and no bootstrap flag appeared in 150 s after the power loss"
	else
		result "B2 service specs CHANGED or bootstrap flag present (see specs above)"
	fi
	rec "database task history (restart loop?)" vm_docker "$M1" service ps --format '{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' $(e6_services)
	rec "ground truth again: grastate.dat per node" e6_grastate
	rec "cluster via API" api GET "/api/v1/clusters/$CLUSTER_ID"
	rec "events" e6_events
	rec "operations (no recovery operation yet)" e6_ops
	e6_diagnose "outage diagnosis (GET, read-only)"
	rec "diagnosis with position recovery (POST)" api POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' || true
	local cand
	cand=$(apij GET "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" | jq -r '.candidate // ""')
	if [ -z "$cand" ]; then
		say "no candidate yet; operator step: scale the crash-looping services to 0 (the manager has no stop command), then retry the position recovery"
		rec "operator: scale all three to 0" vm_docker "$M1" service scale -d $(for s in $(e6_services); do printf '%s=0 ' "$s"; done)
		wait_until 180 "no database task running" e6_none_running || true
		rec "diagnosis with position recovery (POST) after the operator stopped the services" api POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' || true
		cand=$(apij POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' | jq -r '.candidate // ""')
	fi
	say "diagnosis candidate: ${cand:-none}"
	[ -n "$cand" ] || { result "B2: no candidate after the power loss even with recovered positions"; return 1; }
	e6_recover "$cand" || true
	e6_verify_recovered "power-loss" 104
	rec "sentinels on every node after recovery (1,2,3 expected)" e8_sentinels_all "1,2,3"
	e8_measure "gate B2 after: cluster recovered"
}
