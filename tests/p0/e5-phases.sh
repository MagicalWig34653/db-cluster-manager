# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2016
#
# E5 phases, sourced by tests/p0/e5-e6-proxy-outage.sh after the E4 library.
#
# Layout: the proxy (cmd/proxy, host cross-compiled) runs as a plain
# container on m1, attached to the cluster's attachable overlay network, so
# it reaches the nodes by their service names (dnsrr: the name resolves to
# the running task). The pinned MariaDB image is only the carrier (it has
# the mariadb CLI for the TLS checks); the proxy binary and e5client are
# bind-mounted read-only. Clients run inside the same container
# (docker exec), so the write endpoint never leaves m1.

E5_STEPS="proxyprep e5select e5failover e5drain e5tls e5probes"

# The plaintext proxy: single-writer-stable, read endpoint excluding the
# writer, Galera sync state through a monitoring account over TCP.
e5_proxy_config() { # e5_proxy_config MODE PREFERRED TLS(0|1)
	local tls=""
	[ "$3" = 1 ] && tls=',"tls": {"cert_file": "cert.pem", "key_file": "key.pem"}'
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
  "limits": {"max_connections": 200, "idle_timeout": "10m", "dial_timeout": "2s", "handshake_timeout": "5s"}$tls
}
EOF
}

# px CONTAINER ARGS... — run e5client inside a proxy container.
px() {
	local c=$1
	shift
	vm_docker "$M1" exec "$c" /e5/e5client "$@"
}
pstatus() { px "${1:-$PROXY}" status | tail -n +2; }
pbrief() { pstatus "${1:-$PROXY}" | jq -c '{writer, writer_changes, readers, nodes: [.nodes[] | "\(.id) \(.health)/\(.sync) conns=\(.connections) drain=\(.draining) \(.last_error // "" | .[0:60])"], conns: .connections}'; }
proxy_has_writer() { pstatus | jq -e .has_writer >/dev/null; }
APPARGS='-user e5app -password-file /e5/app.pw'

phase_proxyprep() {
	section "Phase proxyprep (E5): accounts, certificates, proxy container on m1"
	hostload
	limactl copy "$BIN/proxy" "$BIN/e5client" "$M1:/tmp/"
	rec "install proxy and e5client in $E5" vm_root "$M1" '
		set -e
		install -d -m 0700 "$1" "$1/out"
		install -m 0755 /tmp/proxy /tmp/e5client "$1/"
		rm -f /tmp/proxy /tmp/e5client
		for n in monitor app; do [ -f "$1/$n.pw" ] || head -c 18 /dev/urandom | base64 | tr -d "/+\n" >"$1/$n.pw"; done
		chmod 0600 "$1"/*.pw
		"$1/e5client" gencert -dir "$1" -name dbcm-e5-proxy
		ls -l "$1"' "$E5"
	local mpw apw
	mpw=$(vm_sh "$M1" "sudo cat $E5/monitor.pw")
	apw=$(vm_sh "$M1" "sudo cat $E5/app.pw")
	# Operator step: an account for the proxy host. The product's monitoring
	# account dbcm_monitor@localhost only works over the node socket.
	rsql 1 "CREATE USER IF NOT EXISTS 'dbcm_e5mon'@'%' IDENTIFIED BY '$mpw';
		CREATE USER IF NOT EXISTS 'e5app'@'%' IDENTIFIED BY '$apw';
		CREATE DATABASE IF NOT EXISTS e5;
		CREATE TABLE IF NOT EXISTS e5.load (id BIGINT AUTO_INCREMENT PRIMARY KEY, node VARCHAR(64), label VARCHAR(64), at TIMESTAMP(3) DEFAULT CURRENT_TIMESTAMP(3));
		GRANT ALL ON e5.* TO 'e5app'@'%';" >/dev/null
	mpw="" apw=""
	rec "proxy accounts (n2, replicated)" rsql 2 "SHOW GRANTS FOR 'dbcm_e5mon'@'%'; SHOW GRANTS FOR 'e5app'@'%'"
	rec "node settings relevant to a proxy" rsql 1 "SELECT @@version, @@skip_name_resolve, @@max_connect_errors, @@max_connections, @@have_ssl, @@require_secure_transport"
	e5_proxy_config single-writer-stable "" 0 | vm_root "$M1" 'cat >"$1/proxy.json"' "$E5"
	e5_proxy_config prefer-node n2 1 | vm_root "$M1" 'cat >"$1/proxy-tls.json"' "$E5"
	rec "proxy configuration" vm_root "$M1" 'cat "$1/proxy.json"' "$E5"
	local c
	for c in "$PROXY:proxy.json" "$PROXY_TLS:proxy-tls.json"; do
		rec "start ${c%%:*}" vm_args "$M1" '
			sudo docker rm -f "$1" >/dev/null 2>&1 || true
			sudo docker run -d --name "$1" --hostname "$1" --network "$2" --label dbcm.e5=lab \
				-v "$3:/e5:ro" -v "$3/out:/e5out" --entrypoint /e5/proxy "$4" -config "/e5/$5"' \
			"${c%%:*}" "$NET" "$E5" "$MARIADB_IMAGE" "${c#*:}"
	done
	wait_until 60 "proxy has a writer" proxy_has_writer ||
		{ rec "proxy log" vm_docker "$M1" logs --tail 40 "$PROXY"; return 1; }
	rec "proxy status" pstatus
	rec "proxy log" vm_docker "$M1" logs --tail 20 "$PROXY"
}

phase_e5select() {
	section "Phase e5select (E5): writer selection, read endpoint, latency through the proxy"
	hostload
	rec "status (stable mode)" pbrief
	rec "status (prefer-node n2, TLS proxy)" pbrief "$PROXY_TLS"
	local n
	for n in 1 2 3; do
		rec "direct round trips to n$n (from the proxy container)" px "$PROXY" bench -addr "$(svc "$n"):3306" $APPARGS -n 300
	done
	rec "round trips through the write endpoint" px "$PROXY" bench -addr 127.0.0.1:3306 $APPARGS -n 300
	rec "round trips through the read endpoint" px "$PROXY" bench -addr 127.0.0.1:3307 $APPARGS -n 300
	rec "10 s per-connection writes on the write endpoint" px "$PROXY" load -addr 127.0.0.1:3306 $APPARGS -mode per-connection -duration 10s -interval 100ms -label select-w
	rec "10 s per-connection writes on the read endpoint (round-robin over non-writers)" px "$PROXY" load -addr 127.0.0.1:3307 $APPARGS -mode per-connection -duration 10s -interval 100ms -label select-r
}

# e5_watch SECONDS FILE-LABEL — poll the proxy status once per second.
e5_watch() {
	local i
	for i in $(seq 1 "$1"); do
		say "t+${i}s $(pbrief 2>&1 | head -c 600)"
		sleep 1
	done
}

phase_e5failover() {
	section "Phase e5failover (E5): kill the writer's container under load; hysteresis when it returns"
	hostload
	local w wn vm c
	w=$(pstatus | jq -r .writer)
	wn=${w#n}
	vm=$(node_vm "$wn")
	c=$(cid "$wn")
	say "writer before: $w on $vm"
	vm_docker "$M1" exec -d "$PROXY" sh -c '/e5/e5client load -addr 127.0.0.1:3306 '"$APPARGS"' -mode persistent -duration 150s -interval 100ms -label fo-persistent >/e5out/fo-persistent.out 2>&1'
	vm_docker "$M1" exec -d "$PROXY" sh -c '/e5/e5client load -addr 127.0.0.1:3306 '"$APPARGS"' -mode per-connection -duration 150s -interval 100ms -label fo-perconn >/e5out/fo-perconn.out 2>&1'
	sleep 10
	rec "kill the writer's container ($w) on $vm" vm_docker "$vm" kill "$c"
	say "kill sent at $(date -u +%H:%M:%S)"
	e5_watch 125
	sleep 20
	rec "persistent client (one connection, replaced after errors)" vm_root "$M1" 'cat "$1/out/fo-persistent.out"' "$E5"
	rec "per-connection client" vm_root "$M1" 'cat "$1/out/fo-perconn.out"' "$E5"
	rec "status after the old writer returned" pbrief
	rec "writer killed: Swarm tasks" vm_docker "$M1" service ps --format '{{.Name}} {{.Node}} {{.CurrentState}} {{.Error}}' "$(svc "$wn")"
	rec "proxy log (writer changes)" sh -c "limactl shell --workdir / $M1 -- sudo docker logs $PROXY 2>&1 | grep -iE 'writer|drain|reload' | tail -20"
	rec "rows per node and label (n1)" rsql 1 "SELECT label, node, COUNT(*) FROM e5.load GROUP BY label, node ORDER BY label, node"
}

phase_e5drain() {
	section "Phase e5drain (E5): planned writer switch by reload (prefer-node), existing connection drained at the deadline"
	hostload
	local w target
	w=$(pstatus | jq -r .writer)
	target=n1
	[ "$w" = n1 ] && target=n3
	say "writer $w; reload to prefer-node $target"
	vm_docker "$M1" exec -d "$PROXY" sh -c '/e5/e5client load -addr 127.0.0.1:3306 '"$APPARGS"' -mode persistent -duration 45s -interval 200ms -label drain-persistent >/e5out/drain-persistent.out 2>&1'
	sleep 5
	e5_proxy_config prefer-node "$target" 0 | vm_root "$M1" 'cat >"$1/proxy.json"' "$E5"
	rec "SIGHUP the proxy" vm_docker "$M1" kill -s HUP "$PROXY"
	say "reload sent at $(date -u +%H:%M:%S)"
	e5_watch 20
	sleep 25
	rec "persistent client across the planned switch" vm_root "$M1" 'cat "$1/out/drain-persistent.out"' "$E5"
	rec "status after the switch" pbrief
	rec "proxy log (reload, drain)" sh -c "limactl shell --workdir / $M1 -- sudo docker logs $PROXY 2>&1 | grep -iE 'writer|drain|reload' | tail -10"
	# back to the stable policy for the following phases
	e5_proxy_config single-writer-stable "" 0 | vm_root "$M1" 'cat >"$1/proxy.json"' "$E5"
	vm_docker "$M1" kill -s HUP "$PROXY" >/dev/null
}

phase_e5tls() {
	section "Phase e5tls (E5): client-facing TLS termination (prefer-node n2 proxy with TLS)"
	hostload
	rec "status TLS proxy" pbrief "$PROXY_TLS"
	rec "go-sql-driver with the lab CA (TLS verified)" px "$PROXY_TLS" query -addr 127.0.0.1:3306 $APPARGS -tls /e5/ca.pem -server-name dbcm-e5-proxy \
		"SELECT @@hostname, CURRENT_USER(), @@version" "SHOW SESSION STATUS LIKE 'Ssl_cipher'"
	rec "go-sql-driver without TLS (must be refused)" px "$PROXY_TLS" query -addr 127.0.0.1:3306 $APPARGS "SELECT 1" || true
	rec "go-sql-driver with a wrong server name (must fail verification)" px "$PROXY_TLS" query -addr 127.0.0.1:3306 $APPARGS -tls /e5/ca.pem -server-name wrong.example "SELECT 1" || true
	rec "mariadb CLI with --ssl-ca and server-cert verification" vm_docker "$M1" exec "$PROXY_TLS" sh -c \
		'MYSQL_PWD=$(cat /e5/app.pw) mariadb -h 127.0.0.1 -P3306 -ue5app --ssl-ca=/e5/ca.pem --ssl-verify-server-cert -e "status" | grep -iE "SSL|Server version|Connection:"'
	rec "mariadb CLI with TLS disabled (must be refused)" vm_docker "$M1" exec "$PROXY_TLS" sh -c \
		'MYSQL_PWD=$(cat /e5/app.pw) mariadb -h 127.0.0.1 -P3306 -ue5app --skip-ssl -e "SELECT 1"' || true
	rec "mariadb CLI default settings, no CA (11.4 client verifies by default)" vm_docker "$M1" exec "$PROXY_TLS" sh -c \
		'MYSQL_PWD=$(cat /e5/app.pw) mariadb -h 127.0.0.1 -P3306 -ue5app -e "SELECT @@hostname"' || true
	rec "mariadb CLI through the plaintext proxy (no termination; client and node negotiate)" vm_docker "$M1" exec "$PROXY" sh -c \
		'MYSQL_PWD=$(cat /e5/app.pw) mariadb -h 127.0.0.1 -P3306 -ue5app -e "status" | grep -iE "SSL|Server version|Connection:"' || true
	rec "mariadb CLI through the plaintext proxy with --ssl-verify-server-cert and the lab CA (node certificate is not from that CA)" vm_docker "$M1" exec "$PROXY" sh -c \
		'MYSQL_PWD=$(cat /e5/app.pw) mariadb -h 127.0.0.1 -P3306 -ue5app --ssl-ca=/e5/ca.pem --ssl-verify-server-cert -e "SELECT 1"' || true
	rec "TLS proxy: 10 s per-connection writes with verified TLS" px "$PROXY_TLS" load -addr 127.0.0.1:3306 $APPARGS -tls /e5/ca.pem -server-name dbcm-e5-proxy -mode per-connection -duration 10s -interval 100ms -label tls
	rec "TLS round trips" px "$PROXY_TLS" bench -addr 127.0.0.1:3306 $APPARGS -tls /e5/ca.pem -server-name dbcm-e5-proxy -n 300
	rec "TLS proxy log" vm_docker "$M1" logs --tail 15 "$PROXY_TLS"
}

e5_aborted_log() { # N - count the node's log lines about aborted connections
	local vm c
	vm=$(node_vm "$1")
	c=$(cid "$1")
	echo "log lines mentioning 'Aborted connection': $(vm_docker "$vm" logs "$c" 2>&1 | grep -c 'Aborted connection' || true)"
	rsql "$1" "SELECT @@log_warnings, @@performance_schema, @@skip_name_resolve, @@max_connect_errors"
}

phase_e5probes() {
	section "Phase e5probes (E5): side effects of the probes on the nodes"
	hostload
	local n
	for n in 1 2 3; do
		rec "n$n: aborted connects, host cache, connection errors" rsql "$n" "SHOW GLOBAL STATUS WHERE Variable_name IN ('Aborted_connects','Aborted_connects_preauth','Connection_errors_internal','Connections','Threads_connected','Max_used_connections');
			SELECT IP, HOST, SUM_CONNECT_ERRORS, COUNT_HANDSHAKE_ERRORS FROM performance_schema.host_cache" || true
	done
	rec "node log lines about aborted connections (n2)" e5_aborted_log 2
	rec "proxy uptime and probe counts" pbrief
	rec "proxy log warnings" sh -c "limactl shell --workdir / $M1 -- sudo docker logs $PROXY 2>&1 | grep -iE 'level=(WARN|ERROR)' | tail -15"
}
