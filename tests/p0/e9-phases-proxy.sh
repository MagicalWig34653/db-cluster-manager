# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2016
#
# E9 parts B and C (sourced by e9-phases.sh). Layout of part B: two cmd/proxy
# containers (proxy-a on m2, proxy-b on m3) with the coordinated writer choice
# of ADR 0017, a keepalived VIP, a TLS front for the manager API on every VM
# (socat, 8443 -> loopback 8080; the proxy requires HTTPS to a non-loopback
# manager), clients on m1 and status/VIP samplers at 100 ms.

E8_APP="-user e5app -password-file $E8/app.pw"
PXID_m2=proxy-a
PXID_m3=proxy-b
E9_A1="" E9_A2="" E9_A3=""

# e9_addrs sets the VM addresses (also when pxprep did not run in this process).
e9_addrs() {
	[ -n "$E9_A1" ] || E9_A1=$(uv2_ip dbcm-lab-m1) E9_A2=$(uv2_ip dbcm-lab-m2) E9_A3=$(uv2_ip dbcm-lab-m3)
}

px_id() { case "$1" in dbcm-lab-m2) echo proxy-a ;; dbcm-lab-m3) echo proxy-b ;; esac; }
px_lbl() { case "$1" in dbcm-lab-m2) echo pa ;; dbcm-lab-m3) echo pb ;; esac; }
px_vm() { case "$1" in proxy-a) echo dbcm-lab-m2 ;; proxy-b) echo dbcm-lab-m3 ;; esac; }
# e9_rsql SQL — SQL on the Galera node that runs on m1 (m1 is never stopped
# in part B).
e9_rsql() { rsql "$(e8_node_on "$M1")" "$1"; }

# ------------------------------------------------------------- proxy setup
e9_proxy_config() { # PROXY-ID
	cat <<EOF
{
  "write_listen": "0.0.0.0:3306",
  "control_listen": "127.0.0.1:9306",
  "nodes": [
    {"id": "n1", "address": "$(svc 1):3306", "role": "writer-eligible"},
    {"id": "n2", "address": "$(svc 2):3306", "role": "writer-eligible"},
    {"id": "n3", "address": "$(svc 3):3306", "role": "writer-eligible"}
  ],
  "policy": {"mode": "single-writer-stable", "stale_after": "5s", "drain_deadline": "10s"},
  "probe": {"liveness": "mysql-greeting", "sync": "galera", "interval": "1s", "timeout": "1s",
            "monitor": {"user": "dbcm_e5mon", "password_file": "monitor.pw", "tls": "disabled"}},
  "limits": {"max_connections": 200, "idle_timeout": "10m", "dial_timeout": "2s", "handshake_timeout": "5s"},
  "coordination": {"cluster_id": "$CLUSTER_NAME", "proxy_id": "$1",
                   "manager_urls": ["https://$E9_A1:8443", "https://$E9_A2:8443", "https://$E9_A3:8443"],
                   "token_file": "/e8/$1.token", "timeout": "750ms"}
}
EOF
}

e9_start_proxy() { # VM
	rec "start $PX on $1 (published on host port 3306, overlay network $NET, trust file for the manager front)" vm_args "$1" '
		sudo docker rm -f "$1" >/dev/null 2>&1 || true
		sudo docker run -d --name "$1" --hostname "$1" --network "$2" -p 3306:3306 --restart unless-stopped --label dbcm.e8=lab \
			-e SSL_CERT_FILE=/e8/tls/ca.pem -v "$3:/e8:ro" --entrypoint /e8/proxy "$4" -config /e8/proxy.json' "$PX" "$NET" "$E8" "$MARIADB_IMAGE"
}

e9_pxprep() {
	section "Phase pxprep: TLS front for the manager API on every VM, accounts, tokens, two coordinated proxy containers"
	hostload
	local vm
	E9_A1=$(uv2_ip dbcm-lab-m1) E9_A2=$(uv2_ip dbcm-lab-m2) E9_A3=$(uv2_ip dbcm-lab-m3)
	rec "TLS certificate of the manager front (self-signed, SAN = the three VM addresses, 2 days)" vm_root "$M1" '
		set -e
		d="$1/front"; rm -rf "$d"; install -d -m 0700 "$d"
		openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj "/CN=e9-manager-front" \
			-addext "subjectAltName=IP:$2,IP:$3,IP:$4" -keyout "$d/key.pem" -out "$d/ca.pem" 2>/dev/null
		openssl x509 -in "$d/ca.pem" -noout -subject -ext subjectAltName' "$STATE" "$E9_A1" "$E9_A2" "$E9_A3"
	rec "generate the lab database passwords on m1 (values never logged)" vm_root "$M1" '
		for n in monitor app; do [ -f "$1/$n.pw" ] || head -c 18 /dev/urandom | base64 | tr -d "/+\n" >"$1/$n.pw"; done
		chmod 0600 "$1"/*.pw; ls -l "$1"/*.pw' "$E8"
	for vm in $VMS; do
		rec "front certificate on $vm" sh -c \
			"limactl shell --workdir / $M1 -- sudo tar -C $STATE -cf - front | limactl shell --workdir / $vm -- sudo sh -c 'install -d -m 0700 $E8/tls && tar -C $E8/tls --strip-components=1 -xf - && ls $E8/tls'"
		rec "TLS front (socat, :8443 on the VM address -> loopback :$API_PORT) on $vm" vm_root "$vm" '
			set -e
			a=$(ip -4 -o addr show | awk "{print \$4}" | cut -d/ -f1 | grep -F "$3" | head -1)
			systemctl stop e9front 2>/dev/null || true
			systemd-run -q --unit e9front --collect -p Restart=always socat "OPENSSL-LISTEN:8443,bind=$a,reuseaddr,fork,cert=$1/tls/ca.pem,key=$1/tls/key.pem,verify=0" "TCP:127.0.0.1:$2"
			sleep 1; systemctl is-active e9front; ss -ltn | grep :8443' "$E8" "$API_PORT" "$USERV2_PREFIX"
	done
	for vm in $PXVMS; do
		rec "tokens and monitor password for $(px_id "$vm") on $vm" sh -c \
			"limactl shell --workdir / $M1 -- sudo tar -cf - -C $STATE/tokens $(px_id "$vm").token -C $E8 monitor.pw | limactl shell --workdir / $vm -- sudo sh -c 'tar -C $E8 -xf - && chmod 0600 $E8/*.token $E8/monitor.pw; ls -l $E8'"
	done
	rec "the administrator token on m1 only" vm_root "$M1" 'install -m 0600 "$1/tokens/admin.token" "$2/admin.token"; ls -l "$2/admin.token"' "$STATE" "$E8"
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
		e9_proxy_config "$(px_id "$vm")" | vm_root "$vm" 'cat >"$1/proxy.json"' "$E8"
		e9_start_proxy "$vm"
	done
	for vm in $PXVMS; do
		wait_until 90 "proxy on $vm has a writer from the manager" e8_has_writer "$vm" ||
			{ rec "proxy log on $vm" vm_docker "$vm" logs --tail 30 "$PX"; rec "front test from m1" vm_sh "$M1" "curl -sS -m 5 --cacert $E8/tls/ca.pem -o /dev/null -w '%{http_code}\n' https://$E9_A2:8443/api/v1/health"; assert_fail pxprep "proxy on $vm never got a writer"; return 1; }
		rec "proxy status on $vm" e8_status "$vm"
	done
	rec "the manager's choice read through the TLS front with the proxy-a token" e9_choice
}

e9_pxvip() {
	phase_e8vip
	for vm in $PXVMS; do
		vm_root "$vm" 'cat >/usr/local/bin/dbcm-e9-vipsample <<"EOF"
#!/bin/sh
# usage: dbcm-e9-vipsample SECONDS VIP — 1 when this host holds the VIP, every 100 ms
end=$(( $(date +%s) + $1 ))
while [ "$(date +%s)" -lt "$end" ]; do
  if ip -4 -o addr show | grep -q " $2/"; then h=1; else h=0; fi
  echo "$(date -u +%T.%3N) $h"
  sleep 0.1
done
EOF
chmod 0755 /usr/local/bin/dbcm-e9-vipsample'
	done
}

# ----------------------------------------------------------- load and sampling
e9_units_done() {
	local v
	[ "$(vm_root "$M1" 'systemctl list-units "e9-*" --no-legend | wc -l')" = 0 ] || return 1
	for v in $PXVMS; do
		e8_vm_up "$v" || continue
		[ "$(vm_root "$v" 'systemctl list-units "e9px-*" "e9vip-*" --no-legend | wc -l')" = 0 ] || return 1
	done
}
e8_clients_done() { e9_units_done; }

# e9_clients_start TAG DURATION HOLDER — four clients on m1 (VIP and client
# failover list, per-connection and persistent; one write every 50 ms each, a
# 1 s connect/read/write timeout) plus status samplers of both proxies and the
# VIP at 100 ms.
e9_clients_start() {
	local tag=$1 dur=$2 holder=${3:-dbcm-lab-m2} other list secs v spec lab addr mode
	secs=${dur%s}
	other=$(e8_other_px "$holder")
	list="$(uv2_ip "$holder"):3306,$(uv2_ip "$other"):3306"
	for v in $PXVMS; do
		e8_vm_up "$v" || continue
		vm_root "$v" 'rm -f "$1/out/$2-$3.smp" "$1/out/$2-vip-$3.smp"
			systemd-run -q --unit "e9px-$2-$3" --collect -p StandardOutput=file:"$1/out/$2-$3.smp" -p StandardError=file:"$1/out/$2-$3.err" /usr/local/bin/dbcm-e9-pxsample "$4" "$3"
			systemd-run -q --unit "e9vip-$2-$3" --collect -p StandardOutput=file:"$1/out/$2-vip-$3.smp" -p StandardError=file:"$1/out/$2-vip-$3.err" /usr/local/bin/dbcm-e9-vipsample "$4" "$5"' "$E8" "$tag" "$(px_lbl "$v")" "$((secs + 5))" "$VIP"
	done
	for spec in "vip-pc|$VIP:3306|per-connection" "vip-ps|$VIP:3306|persistent" "cs-pc|$list|per-connection" "cs-ps|$list|persistent"; do
		lab="e9-$tag-${spec%%|*}"
		addr=$(echo "$spec" | cut -d'|' -f2)
		mode=$(echo "$spec" | cut -d'|' -f3)
		vm_root "$M1" '
			systemd-run -q --unit "$1" --collect -p StandardOutput=file:"$5/out/$1.out" -p StandardError=file:"$5/out/$1.out" \
				"$5/e5client" load -addr "$2" -user e5app -password-file "$5/app.pw" -mode "$3" -duration "$4" -interval 50ms -timeout 1s -label "$1"' \
			"$lab" "$addr" "$mode" "$dur" "$E8"
	done
	say "clients and samplers started ($tag, $dur): vip-pc vip-ps cs-pc cs-ps; failover list $list"
}
e8_clients_start() { e9_clients_start "$@"; }

e9_clients_report() { # TAG
	local l
	for l in vip-pc vip-ps cs-pc cs-ps; do
		rec "client e9-$1-$l: transitions and summary" vm_root "$M1" 'cut -c1-300 "$1/out/e9-$2-$3.out" | grep -v "^SUMMARY" | head -14; grep "^SUMMARY" "$1/out/e9-$2-$3.out" | sed "s/^SUMMARY //" | jq -c "{label, ok, errors, max_gap_without_success, max_gap_from, reconnects, per_node, error_examples}"' "$E8" "$1" "$l"
	done
}
e8_clients_report() { e9_clients_report "$@"; }

# e9_analyze TAG — fetch samples and rows, run the analyzer; output into the log.
e9_collect() { # TAG
	local tag=$1 dir v l
	dir="$SAMPLES_DIR/$tag"
	rm -rf "$dir"
	mkdir -p "$dir"
	for v in $PXVMS; do
		l=$(px_lbl "$v")
		e8_vm_up "$v" || continue
		limactl shell --workdir / "$v" -- sudo cat "$E8/out/$tag-$l.smp" >"$dir/$l.smp" 2>/dev/null || true
		limactl shell --workdir / "$v" -- sudo cat "$E8/out/$tag-vip-$l.smp" >"$dir/vip-$l.smp" 2>/dev/null || true
	done
	e9_rsql "SELECT DATE_FORMAT(at, '%H:%i:%s.%f'), node, label FROM e5.load WHERE label LIKE 'e9-$tag-%' ORDER BY at, id" >"$dir/rows.tsv" 2>/dev/null || true
	{
		printf '### analysis of scenario %s (samples in tests/p0/results/e9-samples/%s)\n\n```\n' "$tag" "$tag"
		python3 "$REPO_ROOT/tests/p0/e9-analyze.py" "$dir"
		printf '```\n\n'
	} >>"$LOG"
	say "analysis of $tag written to the log ($(wc -l <"$dir/rows.tsv" | tr -d ' ') rows, $(wc -l <"$dir/pa.smp" | tr -d ' ')/$(wc -l <"$dir/pb.smp" | tr -d ' ') proxy samples)"
}

# ----------------------------------------------------------- switch helpers
# e9_admin_switch WRITER — POST the controlled switch to every manager URL
# until one (the leader) accepts; followers answer 503.
e9_admin_switch() {
	e9_addrs
	vm_args "$M1" '
		for a in "$3" "$4" "$5"; do
			code=$(sudo curl -s -m 5 -o /tmp/e9sw.out -w "%{http_code}" --cacert "$2/tls/ca.pem" -H "Authorization: Bearer $(sudo cat "$2/admin.token")" -H "Content-Type: application/json" -d "{\"writer\":\"$1\"}" "https://$a:8443/api/v1/proxy-writers/$6/switch")
			echo "$(date -u +%T.%3N) switch to $1 via $a: HTTP $code $(sudo cat /tmp/e9sw.out | tr -d "\n" | cut -c1-200)"
			[ "$code" = 200 ] && exit 0
		done
		exit 1' "$1" "$E8" "$E9_A1" "$E9_A2" "$E9_A3" "$CLUSTER_NAME"
}
# e9_choice — the manager's current choice, read with the proxy-a token.
e9_choice() {
	e9_addrs
	vm_args "$M1" '
		for a in "$2" "$3" "$4"; do
			out=$(sudo curl -s -m 5 --cacert "$1/tls/ca.pem" -H "Authorization: Bearer $(sudo cat "$6/tokens/proxy-a.token")" -H "X-Proxy-ID: proxy-a" "https://$a:8443/api/v1/proxy-writers/$5" -w " HTTP %{http_code}")
			case "$out" in *"HTTP 200") echo "via $a: $out"; exit 0 ;; esac
		done
		echo "no manager answered 200"; exit 1' "$E8" "$E9_A1" "$E9_A2" "$E9_A3" "$CLUSTER_NAME" "$STATE"
}
e9_both_writer() { # NODE
	local v
	for v in $PXVMS; do [ "$(e8_status "$v" | jq -r .writer)" = "$1" ] || return 1; done
}
e9_both_settled() { # both proxies have a writer and no pending target
	local v
	for v in $PXVMS; do e8_status "$v" | jq -e '.has_writer == true and ((.coordination.pending // "") == "")' >/dev/null || return 1; done
}
e9_px_coord() { for v in $PXVMS; do echo "$(px_id "$v"): $(e8_status "$v" | jq -c '{writer, has_writer, epoch: .coordination.epoch, pending: .coordination.pending}')"; done; }

# ----------------------------------------------------------------- scenarios
e9_pxagree() {
	section "Phase pxagree (B): both proxies pick the same writer (E8: two stable proxies picked n2 and n3)"
	hostload
	local v k1 ip2 ip3
	k1=$(e8_node_on "$M1")
	wait_until 60 "both proxies settled with a writer" e9_both_settled || true
	rec "coordination state of both proxies" e9_px_coord
	rec "the manager's choice (leader answers; followers 503)" e9_choice
	for v in $PXVMS; do rec "proxy status on $v" e8_brief "$v"; done
	if [ "$k1" != 1 ]; then
		say "initial writer is n1, but the node on m1 is n$k1; controlled switch to n$k1 so that m1's node is the writer"
		rec "controlled switch to n$k1" e9_admin_switch "n$k1"
		wait_until 60 "both proxies report writer n$k1" e9_both_writer "n$k1" || assert_fail pxagree "switch to n$k1 did not complete"
	fi
	e9_both_settled && ok pxagree "both proxies settled" || assert_fail pxagree "proxies not settled"
	local w1 w2
	w1=$(e8_status dbcm-lab-m2 | jq -r .writer)
	w2=$(e8_status dbcm-lab-m3 | jq -r .writer)
	[ "$w1" = "$w2" ] && ok pxagree "both proxies report writer $w1 (epochs: $(e9_px_coord | tr '\n' ' '))" || assert_fail pxagree "writers differ: $w1 vs $w2"
	ip2=$(uv2_ip dbcm-lab-m2)
	ip3=$(uv2_ip dbcm-lab-m3)
	e9_clients_start_direct agree 20s "$ip2" "$ip3"
	wait_until 60 "direct clients finished" e9_units_done || true
	e9_collect agree
	rec "writer nodes per label" e9_rsql "SELECT label, node, COUNT(*) FROM e5.load WHERE label LIKE 'e9-agree-%' GROUP BY label, node ORDER BY label, node"
	rec "distinct writer nodes in the test" e9_rsql "SELECT COUNT(DISTINCT node), GROUP_CONCAT(DISTINCT node) FROM e5.load WHERE label LIKE 'e9-agree-%'"
	[ "$(e9_rsql "SELECT COUNT(DISTINCT node) FROM e5.load WHERE label LIKE 'e9-agree-%'" | tr -d '[:space:]')" = 1 ] &&
		ok pxagree "one writer node for all direct writes through both proxies" || assert_fail pxagree "writes reached more than one node"
}

# e9_clients_start_direct TAG DURATION IP-A IP-B — one writer straight into
# each proxy (no VIP, no failover list), with the samplers.
e9_clients_start_direct() {
	local tag=$1 dur=$2 v
	for v in $PXVMS; do
		e8_vm_up "$v" || continue
		vm_root "$v" 'systemd-run -q --unit "e9px-$2-$3" --collect -p StandardOutput=file:"$1/out/$2-$3.smp" -p StandardError=file:"$1/out/$2-$3.err" /usr/local/bin/dbcm-e9-pxsample "$4" "$3"' "$E8" "$tag" "$(px_lbl "$v")" "$((${dur%s} + 5))"
	done
	vm_root "$M1" '
		for x in "pa|$2" "pb|$3"; do
			l=${x%%|*}; a=${x#*|}
			systemd-run -q --unit "e9-$5-$l" --collect -p StandardOutput=file:"$6/out/e9-$5-$l.out" -p StandardError=file:"$6/out/e9-$5-$l.out" \
				"$6/e5client" load -addr "$a:3306" -user e5app -password-file "$6/app.pw" -mode per-connection -duration "$4" -interval 50ms -timeout 1s -label "e9-$5-$l"
		done' x "$3" "$4" "$dur" "$tag" "$E8"
}

e9_pxswitch() {
	section "Phase pxswitch (B): controlled switches under load (freeze, acknowledgement by both proxies, publication)"
	hostload
	e9_both_settled || { assert_fail pxswitch "proxies not settled before the test"; return 1; }
	local k1 k2 k3 w0 holder
	k1=$(e8_node_on "$M1")
	w0=$(e8_status dbcm-lab-m2 | jq -r .writer)
	k2=$(for n in 1 2 3; do [ "n$n" != "$w0" ] && echo "n$n"; done | sed -n 1p)
	k3=$(for n in 1 2 3; do [ "n$n" != "$w0" ] && echo "n$n"; done | sed -n 2p)
	holder=$(e8_vip_holder | head -1)
	[ -n "$holder" ] || holder=dbcm-lab-m2
	say "writer now $w0; switches: $w0 -> $k2 -> $k3 -> $w0; VIP holder $holder"
	e8_measure "pxswitch: before"
	e9_clients_start sw 80s "$holder"
	local tgt
	sleep 12
	for tgt in "$k2" "$k3" "$w0"; do
		rec "switch to $tgt" e9_admin_switch "$tgt"
		local i
		for i in 1 2 3 4 5 6; do
			sleep 1
			say "t+${i}s: $(e9_px_coord | tr '\n' ' ')"
		done
		wait_until 30 "both proxies settled on $tgt" e9_both_writer "$tgt" || assert_fail pxswitch "switch to $tgt did not complete in 36 s"
		rec "the manager's choice after the switch" e9_choice
		sleep 10
	done
	wait_until 120 "clients and samplers finished" e9_units_done || true
	e9_clients_report sw
	e9_collect sw
	rec "audit of the switch requests as the manager recorded them (manager log lines)" sh -c "limactl shell --workdir / $M1 -- sudo docker service logs --no-trunc --timestamps ${STACK}_manager-1 ${STACK}_manager-2 ${STACK}_manager-3 2>&1 | grep -iE 'writer|switch|proxy' | cut -c1-240 | tail -15"
}

e9_set_restart() { vm_docker "$1" update --restart="$2" "$PX"; }

e9_pxdown() {
	section "Phase pxdown (B): an unreachable proxy blocks the switch; no second writer appears"
	hostload
	e9_both_settled || { assert_fail pxdown "proxies not settled"; return 1; }
	local w0 tgt holder
	w0=$(e8_status dbcm-lab-m2 | jq -r .writer)
	tgt=$(for n in 1 2 3; do [ "n$n" != "$w0" ] && echo "n$n"; done | sed -n 1p)
	holder=$(e8_vip_holder | head -1)
	[ -n "$holder" ] || holder=dbcm-lab-m2
	# (i) proxy-b alive but cut off from the manager front (port 8443)
	say "(i) proxy-b partitioned from the managers; writer $w0 -> $tgt requested meanwhile"
	e9_clients_start down1 70s "$holder"
	sleep 10
	rec "block proxy-b -> manager front (8443) on m3: DOCKER-USER (forwarded) and INPUT (to the VM's own address)" vm_root dbcm-lab-m3 '
		iptables -I DOCKER-USER -p tcp --dport 8443 -j DROP -m comment --comment e9-partition
		iptables -I INPUT -i docker_gwbridge -p tcp --dport 8443 -j DROP -m comment --comment e9-partition
		iptables -S DOCKER-USER | grep e9-partition; iptables -S INPUT | grep e9-partition'
	sleep 5
	rec "switch to $tgt while proxy-b cannot reach the managers" e9_admin_switch "$tgt"
	local i
	for i in 1 2 3 4 5 6 7 8; do
		sleep 3
		say "partition t+$((i * 3))s: $(e9_px_coord | tr '\n' ' ')"
	done
	rec "manager's choice while the switch is pending" e9_choice
	rec "heal the partition on m3" vm_root dbcm-lab-m3 'iptables -D DOCKER-USER -p tcp --dport 8443 -j DROP -m comment --comment e9-partition; iptables -D INPUT -i docker_gwbridge -p tcp --dport 8443 -j DROP -m comment --comment e9-partition; iptables -S | grep -c e9-partition || true'
	wait_until 60 "both proxies settled on $tgt after the heal" e9_both_writer "$tgt" || assert_fail pxdown "switch did not complete after the heal"
	rec "coordination state after the heal" e9_px_coord
	wait_until 120 "clients and samplers finished" e9_units_done || true
	e9_clients_report down1
	e9_collect down1

	# (ii) proxy-b container stopped
	holder=$(e8_vip_holder | head -1)
	[ -n "$holder" ] || holder=dbcm-lab-m2
	say "(ii) proxy-b container stopped; switch $tgt -> $w0 requested meanwhile"
	e9_clients_start down2 80s "$holder"
	sleep 10
	rec "stop proxy-b on m3 (restart policy off)" sh -c "limactl shell --workdir / dbcm-lab-m3 -- sudo sh -c 'docker update --restart=no $PX && docker stop -t 2 $PX'"
	sleep 5
	rec "switch to $w0 while proxy-b is down" e9_admin_switch "$w0"
	for i in 1 2 3 4 5 6 7 8; do
		sleep 3
		say "proxy-b down t+$((i * 3))s: proxy-a $(e8_status dbcm-lab-m2 | jq -c '{writer, has_writer, epoch: .coordination.epoch, pending: .coordination.pending}'); VIP holder: $(e8_vip_holder | tr '\n' ' ')"
	done
	local ts
	ts=$(date +%s)
	rec "start proxy-b again, restart policy restored" sh -c "limactl shell --workdir / dbcm-lab-m3 -- sudo sh -c 'docker update --restart=unless-stopped $PX && docker start $PX'"
	wait_until 60 "both proxies settled on $w0" e9_both_writer "$w0" && say "proxy-b start to completed switch: $(($(date +%s) - ts)) s" || assert_fail pxdown "switch did not complete after proxy-b returned"
	rec "coordination state after proxy-b returned" e9_px_coord
	wait_until 150 "clients and samplers finished" e9_units_done || true
	e9_clients_report down2
	e9_collect down2
}

e9_mgr_scale() { # N — all three manager services
	vm_docker "$M1" service scale -d "${STACK}_manager-1=$1" "${STACK}_manager-2=$1" "${STACK}_manager-3=$1"
}
e9_mgrs_stopped() { [ "$(vm_docker "$M1" service ps --filter desired-state=running --format '{{.CurrentState}}' ${STACK}_manager-1 ${STACK}_manager-2 ${STACK}_manager-3 | grep -c '^Running')" = 0 ]; }

e9_pxmgrdown() {
	section "Phase pxmgrdown (B): all manager replicas stopped under load (proxies keep their last safe choice); a proxy restarted meanwhile has no writer"
	hostload
	e9_both_settled || { assert_fail pxmgrdown "proxies not settled"; return 1; }
	local w0 holder epoch0 ts
	w0=$(e8_status dbcm-lab-m2 | jq -r .writer)
	epoch0=$(e8_status dbcm-lab-m2 | jq -r .coordination.epoch)
	holder=$(e8_vip_holder | head -1)
	[ -n "$holder" ] || holder=dbcm-lab-m2
	say "writer $w0, epoch $epoch0, VIP holder $holder"
	e9_clients_start mgr 100s "$holder"
	sleep 10
	rec "scale all three manager services to 0" e9_mgr_scale 0
	wait_until 90 "no manager task running" e9_mgrs_stopped || true
	for i in 1 2 3 4; do
		sleep 3
		say "managers down t+$((i * 3))s: $(e9_px_coord | tr '\n' ' ')"
	done
	rec "proxy status with the managers down (last safe choice kept?)" e9_px_coord
	rec "proxy log lines about the manager (last 6)" sh -c "limactl shell --workdir / dbcm-lab-m2 -- sudo docker logs --tail 6 $PX 2>&1 | cut -c1-240"
	local pbvm=dbcm-lab-m3
	rec "restart proxy-b while no manager is reachable (a new proxy must not pick a writer on its own)" vm_docker "$pbvm" restart -t 2 "$PX"
	for i in 1 2 3 4 5 6; do
		sleep 3
		say "after restart of proxy-b t+$((i * 3))s: proxy-b $(e8_status "$pbvm" | jq -c '{writer, has_writer, epoch: .coordination.epoch}' 2>/dev/null || echo unreachable); proxy-a $(e8_status dbcm-lab-m2 | jq -c '{writer, has_writer}'); VIP holder: $(e8_vip_holder | tr '\n' ' ')"
	done
	ts=$(date +%s)
	rec "scale all three manager services back to 1" e9_mgr_scale 1
	wait_until 240 "three manager tasks running" ha_all_running 3 || true
	wait_until 120 "a replica leads" ha_have_leader || true
	say "managers back: leader after $(($(date +%s) - ts)) s"
	wait_until 90 "both proxies settled with a writer" e9_both_settled && say "managers restart to both proxies settled: $(($(date +%s) - ts)) s" || assert_fail pxmgrdown "proxies not settled after the managers returned"
	rec "coordination state after the managers returned (same writer, same epoch?)" e9_px_coord
	[ "$(e8_status dbcm-lab-m2 | jq -r .writer)" = "$w0" ] && [ "$(e8_status dbcm-lab-m2 | jq -r .coordination.epoch)" = "$epoch0" ] &&
		ok pxmgrdown "manager outage kept writer $w0 and epoch $epoch0" || result "pxmgrdown: writer/epoch changed across the manager outage"
	wait_until 150 "clients and samplers finished" e9_units_done || true
	e9_clients_report mgr
	e9_collect mgr
	for i in $HA_IDS; do rec "replica $i after the outage" ha_probe "$(vm_of "$i")"; done
	rec "session after the manager outage" api GET /api/v1/auth/session || e8_login
}

e9_pxkill() {
	section "Phase pxkill (B): the VIP holder's proxy container is killed under load (coordinated writer, restart reads the choice again)"
	hostload
	e9_both_settled || { assert_fail pxkill "proxies not settled"; return 1; }
	e8_proxykill K1 no 60s
	e9_collect K1
}

e9_pxvmloss() {
	section "Phase pxvmloss (B): the VM of proxy-a (also a Galera node and a manager replica) is hard-stopped while it hosts the writer"
	hostload
	e9_both_settled || { assert_fail pxvmloss "proxies not settled"; return 1; }
	local vm=dbcm-lab-m2 kn k1 w0 holder t0 ts
	kn="n$(e8_node_on "$vm")"
	k1="n$(e8_node_on "$M1")"
	w0=$(e8_status "$vm" | jq -r .writer)
	if [ "$w0" != "$kn" ]; then
		rec "controlled switch to $kn (the node on $vm) so that the lost VM hosts the writer" e9_admin_switch "$kn"
		wait_until 60 "both proxies report writer $kn" e9_both_writer "$kn" || assert_fail pxvmloss "switch to $kn did not complete"
	fi
	holder=dbcm-lab-m3
	say "writer $kn on $vm; $vm hosts proxy-a; clients start with proxy-b's VM $holder first in the failover list; planned restore writer $k1"
	e9_clients_start vml 200s "$holder"
	sleep 15
	t0=$(e8_now)
	e8_vm_stop_force "$vm"
	say "power loss of $vm sent at $t0"
	sleep 15
	rec "proxy-b right after the loss (writer node gone: no writer, no local replacement)" e9_brief dbcm-lab-m3
	rec "wsrep status of the survivors" e8_status3
	sleep 10
	rec "controlled switch to $k1 while proxy-a is unreachable (must stay pending)" e9_admin_switch "$k1"
	local i
	for i in 1 2 3 4 5 6; do
		sleep 5
		say "vm down t+$((i * 5))s: proxy-b $(e8_status dbcm-lab-m3 | jq -c '{writer, has_writer, epoch: .coordination.epoch, pending: .coordination.pending}'); VIP holder: $(e8_vip_holder | tr '\n' ' ')"
	done
	rec "manager's choice while pending" e9_choice
	ts=$(date +%s)
	e8_vm_start "$vm" || { result "VM $vm did not start (see log)"; return 1; }
	say "limactl start returned after $(($(date +%s) - ts)) s"
	wait_until 300 "$vm reachable over ssh" vm_sh "$vm" true || true
	wait_until 300 "$vm is a Ready Swarm node" e8_node_ready "$vm" || true
	wait_until 120 "proxy on $vm running and settled on $k1" sh -c "limactl shell --workdir / $vm -- sudo docker exec $PX /e8/e5client status | grep -q has_writer" || true
	wait_until 180 "both proxies settled on $k1" e9_both_writer "$k1" && say "VM start to writes served again via the completed switch: $(($(date +%s) - ts)) s" || assert_fail pxvmloss "switch to $k1 did not complete after the VM returned"
	wait_until 600 "Galera cluster back to 3 synced nodes" e8_synced3 || true
	rec "wsrep status after" e8_status3
	rec "coordination state at the end" e9_px_coord
	wait_until 250 "clients and samplers finished" e9_units_done || true
	e9_clients_report vml
	e9_collect vml
	rec "keepalived/VIP holder(s) after the return (exactly one)" e8_vip_holder
	for i in $HA_IDS; do rec "replica $i after the VM loss" ha_probe "$(vm_of "$i")"; done
	wait_until 240 "three manager tasks running" ha_all_running 3 || true
}

e9_pxintegrity() {
	section "Phase pxintegrity (B): acknowledged writes versus committed rows, and cross-node consistency"
	hostload
	local f l ok rows lost=0 tag line
	for tag in sw down1 down2 mgr K1 vml; do
		for l in vip-pc vip-ps cs-pc cs-ps; do
			ok=$(vm_root "$M1" 'grep "^SUMMARY" "$1/out/e9-$2-$3.out" 2>/dev/null | sed "s/^SUMMARY //" | jq -r .ok' "$E8" "$tag" "$l" 2>/dev/null || echo "")
			[ -n "$ok" ] || { say "no summary for e9-$tag-$l"; continue; }
			rows=$(e9_rsql "SELECT COUNT(*) FROM e5.load WHERE label='e9-$tag-$l'" | tr -d '[:space:]')
			line="e9-$tag-$l: acknowledged $ok, rows in the database $rows"
			if [ "$rows" -lt "$ok" ]; then line="$line  LOST ACKNOWLEDGED WRITES: $((ok - rows))"; lost=1; else line="$line  (unacknowledged but committed: $((rows - ok)))"; fi
			printf '%s\n' "$line" >>"$LOG"
			say "$line"
		done
	done
	[ "$lost" = 0 ] && result "no acknowledged write was lost in part B" || result "ACKNOWLEDGED WRITES LOST in part B (see lines above)"
	rec "distinct writer nodes per scenario (rows)" e9_rsql "SELECT SUBSTRING_INDEX(SUBSTRING_INDEX(label,'-',2),'-',-1) AS scenario, COUNT(DISTINCT node), GROUP_CONCAT(DISTINCT node), COUNT(*) FROM e5.load WHERE label LIKE 'e9-%' GROUP BY scenario"
	local n
	for n in 1 2 3; do
		rec "n$n row count and checksum of e5.load" rsql "$n" "SELECT COUNT(*), MAX(id), SUM(CRC32(CONCAT(id,label,node))) FROM e5.load"
	done
	e8_measure "end of part B"
}

# ------------------------------------------------------------------ Part C
e9_pwrb1() {
	section "Phase pwrb1 (C): E8 B1 first (one node VM hard-stopped), as in E8, because the E8 history preceded B2"
	e9_pxteardown
	phase_e8node
}

# Proxies and keepalived are removed before the power-loss scenarios (as E8's
# teardown did between gate A and gate B).
e9_pxteardown() {
	local vm
	for vm in $PXVMS; do
		e8_vm_up "$vm" || continue
		rec "remove proxy container and stop keepalived on $vm" vm_sh "$vm" 'sudo systemctl disable --now keepalived >/dev/null 2>&1; sudo docker rm -f '"$PX"' >/dev/null 2>&1; echo "keepalived: $(systemctl is-active keepalived || true); containers: $(sudo docker ps -q --filter label=dbcm.e8=lab | wc -l)"'
	done
	wait_until 600 "Galera cluster at 3 synced nodes" e8_synced3 || true
}

# Replaces e8_status3: also records wsrep_cluster_state_uuid (E9 finding: a
# primary component restored from gvwstate.dat after a power loss has the zero UUID).
e8_status3() {
	local n
	for n in 1 2 3; do echo "n$n: $(rsql "$n" "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_cluster_status','wsrep_local_state_comment','wsrep_last_committed','wsrep_cluster_state_uuid')" 2>&1 | tr '\n\t' '  ')"; done
}

# Replaces e8_slot_logs: `docker service logs` hung for good in E8 run 2 and in
# E9 (a slot whose tasks keep failing), so the logs are read per container on
# the slot's own VM (the newest two containers, including exited ones).
e8_slot_logs() {
	local n vm c
	for n in 1 2 3; do
		vm=$(node_vm "$n")
		echo "== $(svc "$n") on $vm"
		for c in $(vm_docker "$vm" ps -aq --no-trunc --filter "label=com.docker.swarm.service.name=$(svc "$n")" | head -2); do
			echo "-- container ${c:0:12}: $(vm_docker "$vm" inspect -f 'status={{.State.Status}} exit={{.State.ExitCode}} finished={{.State.FinishedAt}}' "$c")"
			vm_docker "$vm" logs "$c" 2>&1 | grep -E 'ERROR|Aborting|aborted|\[Warning\].*WSREP|Found saved state|safe_to_bootstrap|State transfer|IST|SST failed|failed to open|restore pc|Quorum results|group UUID|view\(' | cut -c1-210 | tail -14
		done
	done
}

# e9_slot_diag WHY — everything that can explain a slot that does not reach
# Synced: Swarm task history with errors, exited task containers (exit code,
# OOM, last log lines), the data directory state, kernel OOM messages.
e9_slot_diag() {
	local e n vm s c
	printf '### slot diagnostics (%s), %s\n\n```\n' "$1" "$(date -u +%FT%TZ)" >>"$LOG"
	{
		for e in $E6_HOSTS; do
			n=${e%%:*} vm=${e#*:}
			s=$(svc "$n")
			echo "======== $s on $vm"
			e8_vm_up "$vm" || { echo "VM not running"; continue; }
			vm_docker "$M1" service ps --no-trunc --format '{{.ID}} {{.DesiredState}} {{.CurrentState}} err={{.Error}}' "$s" 2>&1 | head -10
			echo "-- containers of the slot on $vm (newest first):"
			vm_docker "$vm" ps -a --filter "label=com.docker.swarm.service.name=$s" --format '{{.ID}} {{.Status}} {{.CreatedAt}}' 2>&1 | head -6
			for c in $(vm_docker "$vm" ps -aq --no-trunc --filter "label=com.docker.swarm.service.name=$s" 2>/dev/null | head -3); do
				echo "-- inspect ${c:0:12}:"
				vm_docker "$vm" inspect -f 'status={{.State.Status}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}} error={{.State.Error}} started={{.State.StartedAt}} finished={{.State.FinishedAt}}' "$c" 2>&1
				echo "-- last log lines of ${c:0:12}:"
				vm_docker "$vm" logs --tail 45 "$c" 2>&1 | cut -c1-230
			done
			echo "-- data directory:"
			vm_root "$vm" 'for d in "$1"/dbcm-'"$CLUSTER_NAME"'-n'"$n"'-*/data; do ls -l --time-style=+%T "$d" | grep -E "grastate|gvwstate|galera.cache|ib_|\.err|\.pid|mysql-bin|aria_log" | cut -c1-100; echo "-- grastate.dat:"; cat "$d/grastate.dat" 2>&1; echo "-- gvwstate.dat:"; cat "$d/gvwstate.dat" 2>&1 | head -12; done' "$VOLBASE" 2>&1
			echo "-- kernel messages about OOM/kills:"
			vm_root "$vm" 'dmesg 2>/dev/null | grep -iE "out of memory|oom-kill|killed process" | tail -4; df -h / | tail -1; free -m | sed -n 2p'
			echo "-- agent log (tail):"
			vm_docker "$vm" logs --tail 15 "$(vm_docker "$vm" ps -q --filter label=com.docker.swarm.service.name=dbcm-agent-$INST | head -1)" 2>&1 | cut -c1-230
		done
	} >>"$LOG" 2>&1
	printf '```\n\n' >>"$LOG"
}

# e9_wait_synced DEADLINE — like wait_until e8_synced3, but every 45 s the
# state of the cluster and the slots' tasks is recorded, to see the restart
# cadence of a slot that does not rejoin.
e9_wait_synced() {
	local deadline=$1 start now last=0
	start=$(date +%s)
	while :; do
		if e8_synced3 >/dev/null 2>&1; then say "wait ok after $(($(date +%s) - start))s: Galera cluster back to 3 synced nodes"; return 0; fi
		now=$(date +%s)
		[ $((now - start)) -lt "$deadline" ] || { say "wait TIMEOUT after ${deadline}s: Galera cluster back to 3 synced nodes"; return 1; }
		if [ $((now - last)) -ge 45 ]; then
			last=$now
			say "t+$((now - start))s tasks: $(vm_docker "$M1" service ps --filter desired-state=running --format '{{.Name}}:{{.CurrentState}}' $(e6_services) 2>/dev/null | tr '\n' ' ')"
			printf 't+%ss wsrep: %s\n' "$((now - start))" "$(e8_status3 2>&1 | tr '\n' ' ' | cut -c1-600)" >>"$LOG"
			printf 't+%ss failed tasks: %s\n' "$((now - start))" "$(vm_docker "$M1" service ps --format '{{.Name}} {{.CurrentState}}' $(e6_services) 2>/dev/null | grep -c 'Failed')" >>"$LOG"
		fi
		sleep 5
	done
}

# Replaces e8_power_scenario: the manager is the HA Swarm stack, which Swarm
# restarts by itself; the lab manager unit of E8 does not exist.
e8_power_scenario() {
	local tag=$1 ids=$2 vm t0 before after i cand
	t0=$(date +%s)
	for vm in $VMS; do
		e8_vm_start "$vm" || { result "VM $vm did not start (see log)"; return 1; }
		wait_until 300 "$vm reachable over ssh" vm_sh "$vm" true || true
	done
	say "all three VMs reachable $(($(date +%s) - t0)) s after the first limactl start"
	e8_measure "gate $tag: VMs booted, Swarm, manager stack and database services starting"
	rec "ground truth after the power loss: grastate.dat per node" e6_grastate
	wait_until 300 "Swarm answers on m1" vm_docker "$M1" node ls || true
	rec "Swarm after the boot" vm_docker "$M1" node ls
	wait_until 300 "three manager tasks running" ha_all_running 3 || true
	wait_until 180 "a manager replica leads" ha_have_leader || true
	say "Swarm and manager stack back $(($(date +%s) - t0)) s after the first limactl start"
	rec "manager tasks after the boot" vm_docker "$M1" service ls --filter "name=${STACK}_manager"
	e8_login
	sync_client "$M1"
	before=$(e6_specs)
	for i in 1 2 3 4 5 6 7 8 9 10; do
		sleep 15
		say "t+$((i * 15))s running db tasks: $(e6_running_tasks); cluster: $(apij GET "/api/v1/clusters/$CLUSTER_ID" 2>/dev/null | jq -c '{state, nodes: [.nodes[] | "\(.id) \(.state)"]}' 2>/dev/null || echo unavailable)"
	done
	rec "any node Primary after 150 s?" e8_primary_any
	after=$(e6_specs)
	rec "service specs before the observation window" printf '%s\n' "$before"
	rec "service specs after the observation window" printf '%s\n' "$after"
	if [ "$before" = "$after" ] && ! printf '%s' "$after" | grep -q 'bootstrap=yes'; then
		result "$tag hands off: no service spec changed and no bootstrap flag appeared in 150 s after the power loss"
	else
		result "$tag service specs CHANGED or bootstrap flag present (see specs above)"
	fi
	rec "database task history (restart loop?)" vm_docker "$M1" service ps --no-trunc --format '{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' $(e6_services)
	rec "ground truth again: grastate.dat per node" e6_grastate
	rec "cluster via API" api GET "/api/v1/clusters/$CLUSTER_ID"
	rec "events" e6_events
	rec "operations (no recovery operation may exist)" e6_ops
	rec "outage diagnosis (GET, read-only; 409 is the expected refusal while a node is Primary)" api GET "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" || true
	rec "slot logs 150 s after the boot (errors, state transfers, restored views)" e8_slot_logs
	e9_slot_diag "$tag 150 s after the boot"
	local prim
	prim=$(e8_primary_any 2>/dev/null || true)
	if printf '%s\n' "$prim" | grep -q 'wsrep_cluster_status Primary'; then
		E8_SELF=1
		if ! e9_wait_synced 600; then
			result "$tag: not all three nodes Synced within 600 s after the Primary component formed (diagnostics follow)"
			rec "wsrep status at the timeout" e8_status3
			rec "slot logs at the timeout" e8_slot_logs
			e9_slot_diag "$tag at the 600 s timeout"
			rec "Swarm tasks at the timeout" vm_docker "$M1" service ps --no-trunc --format '{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' $(e6_services)
		fi
		rec "wsrep status after the self-recovery wait" e8_status3
		if [ "$(e6_uuid 1 2>/dev/null)" = "$E6_UUID" ]; then result "$tag: same cluster state UUID after the power loss, no manager action"; else result "$tag: cluster state UUID not confirmed equal (query on n1 may have failed: n1 can be Donor/Desynced)"; fi
		rec "operations after the wait (still none)" e6_ops
		rec "sentinels on every node ($ids expected)" e8_sentinels_all "$ids"
		rec "write after the self-recovery" rsql 1 "REPLACE INTO e8.sentinel (id, note, node) VALUES (900, 'after $tag self-recovery', @@hostname)"
		e8_measure "gate $tag after"
		return 0
	fi
	E8_SELF=0
	rec "diagnosis with position recovery (POST)" api POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' || true
	cand=$(apij GET "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" | jq -r '.candidate // ""')
	if [ -z "$cand" ]; then
		say "no candidate yet; operator step: scale the crash-looping services to 0, then retry the position recovery"
		rec "operator: scale all three to 0" vm_docker "$M1" service scale -d $(for s in $(e6_services); do printf '%s=0 ' "$s"; done)
		wait_until 180 "no database task running" e6_none_running || true
		rec "diagnosis with position recovery (POST) after the operator stopped the services" api POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' || true
		cand=$(apij POST "/api/v1/clusters/$CLUSTER_ID/outage-diagnosis" '{}' | jq -r '.candidate // ""')
	fi
	say "diagnosis candidate: ${cand:-none}"
	[ -n "$cand" ] || { result "$tag: no candidate after the power loss even with recovered positions"; return 1; }
	E8_CANDIDATE=$cand
	e6_recover "$cand" || true
	e6_verify_recovered "$tag" "$((E8_ROW))"
	rec "sentinels on every node after the guided recovery ($ids expected)" e8_sentinels_all "$ids"
	e8_measure "gate $tag after: cluster recovered by the guided recovery"
}

# Diagnosis of a slot that does not rejoin (run after pwrb2 on the kept VMs).
e9_pwrdiag() {
	section "Phase pwrdiag (C): why does a slot not rejoin? Everything the E8 run lacked"
	CLUSTER_ID=${CLUSTER_ID:-$(cluster_id)}
	e6_hosts
	rec "wsrep status of every node (note wsrep_cluster_state_uuid)" e8_status3
	rec "Swarm tasks of the slots" vm_docker "$M1" service ps --no-trunc --format '{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}' $(e6_services)
	e9_slot_diag "pwrdiag"
}

# Guided recovery of the cluster that self-recovered with a zero UUID (the
# operator scales the services to 0, then the manager's approval-gated
# recovery runs); this is the first guided recovery after a VM power loss.
e9_pwrrecover() {
	phase_e8recover
	wait_until 600 "Galera cluster back to 3 synced nodes" e8_synced3 || true
	rec "wsrep status after the guided recovery (state UUID must be non-zero)" e8_status3
	e8_sentinels_all "1,2,3" >/dev/null 2>&1 || true
	rec "sentinels on every node (1,2,3 expected)" e8_sentinels_all "1,2,3"
}

e9_pwrb2() {
	phase_e8allloss
}
e9_pwrb3() {
	phase_e8staggered
}
