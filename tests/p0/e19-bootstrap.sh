#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E19 part 3: fresh-server bootstrap through the real manager
# HTTP API. Two lab VMs (bin/lab apply -vms 2 -templates debian-12,docker
# -network lima:user-v2 --apply): m1 is the FRESH Debian 12 server (no Docker,
# no agent), m2 runs the real manager (managed mode, auth, mTLS agent
# transport, serverBootstrap enabled), a single-node Swarm and an HTTPS server
# with a lab-own CA for the reviewed native agent binary.
#
# This script only prepares m2 (phase `prepare`); the scenario is driven by
# tests/p0/e19/drive.mjs (`bootstrap-*` commands).
#   E19B_LOG=FILE   log (default tests/p0/results/e19-bootstrap-setup.log)
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${E19B_LOG:-"$REPO_ROOT/tests/p0/results/e19-bootstrap-setup.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
: >"$LOG"
export E4_AUTORESUME=false
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"
M1=dbcm-lab-m1 # fresh Debian 12
M2=dbcm-lab-m2 # manager host
INST=e19boot
PUBPORT=8443

trap 'sanitize_log "$LOG"' EXIT

hostload
rec "host free disk" host_free_disk
for vm in $M1 $M2; do
	rec "facts $vm" vm_sh "$vm" 'grep PRETTY_NAME /etc/os-release; uname -m; nproc; free -m | sed -n 2p; command -v docker || echo "docker: absent"; systemctl is-active ssh || true'
done
rec "rootful docker on m2" vm_sh "$M2" '
	set -e
	systemctl --user disable --now docker.service >/dev/null 2>&1 || true
	sudo systemctl unmask containerd.service docker.service docker.socket >/dev/null 2>&1
	sudo systemctl enable --now containerd.service docker.socket docker.service >/dev/null 2>&1
	echo "docker: $(systemctl is-active docker.service)"'
A2=$(uv2_ip "$M2")
A1=$(uv2_ip "$M1")
say "m1 (fresh) $A1  m2 (manager) $A2"
rec "swarm init on m2" vm_docker "$M2" swarm init --advertise-addr "$A2"
rec "binaries to m2" sh -c "limactl copy '$BIN/manager' '$BIN/agent' '$BIN/e4client' $M2:/tmp/ && limactl shell --workdir / $M2 -- sudo sh -c 'install -m 0755 /tmp/manager /usr/local/bin/dbcm-e4-manager; install -m 0755 /tmp/e4client /usr/local/bin/dbcm-e4-client; install -d /srv/agent; install -m 0755 /tmp/agent /srv/agent/dbcm-agent; sha256sum /srv/agent/dbcm-agent'"
SHA=$(vm_root "$M2" 'sha256sum /srv/agent/dbcm-agent | cut -d" " -f1')
rec "lab CA, server certificate for m2, HTTPS file server for the agent binary (systemd unit)" vm_root "$M2" '
	set -e
	d="$1/pki"; install -d -m 0700 "$d" /srv/agent-tls; cd "$d"
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout ca.key -out ca.pem -days 7 -subj "/CN=dbcm-e19-boot-ca" -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout srv.key -out srv.csr -subj "/CN=m2" 2>/dev/null
	printf "subjectAltName=IP:%s\nextendedKeyUsage=serverAuth\n" "$2" >srv.ext
	openssl x509 -req -in srv.csr -CA ca.pem -CAkey ca.key -CAcreateserial -out srv.crt -days 7 -extfile srv.ext 2>/dev/null
	cp srv.crt srv.key /srv/agent-tls/
	cat >/srv/agent-tls/serve.py <<"PY"
import http.server, ssl, functools
h = functools.partial(http.server.SimpleHTTPRequestHandler, directory="/srv/agent")
s = http.server.ThreadingHTTPServer(("0.0.0.0", '"$PUBPORT"'), h)
c = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); c.load_cert_chain("/srv/agent-tls/srv.crt", "/srv/agent-tls/srv.key")
s.socket = c.wrap_socket(s.socket, server_side=True); s.serve_forever()
PY
	systemd-run --unit e19-agentfile --collect python3 /srv/agent-tls/serve.py
	sleep 2; curl -sS --cacert ca.pem -o /dev/null -w "agent binary over TLS: %{http_code} %{size_download} bytes\n" "https://$2:'"$PUBPORT"'/dbcm-agent"' "$STATE" "$A2"
rec "host-side secrets and options on m2 (values not logged)" vm_root "$M2" '
	set -e
	install -d -m 0700 "$1" /var/lib/dbcm-agent /var/lib/dbcm/volumes
	for n in root monitor backup; do [ -f "$1/$n.pw" ] || head -c 18 /dev/urandom | base64 | tr -d "\n" >"$1/$n.pw"; done
	for p in "'"$ROOT_SECRET"':root" "'"$MON_SECRET"':monitor" "'"$BAK_SECRET"':backup"; do
		n=${p%%:*}; f=${p#*:}; docker secret inspect "$n" >/dev/null 2>&1 || docker secret create --label "dbcm.installation=$2" "$n" "$1/$f.pw" >/dev/null; echo "secret $n"
	done' "$STATE" "$INST"
rec "manager config (serverBootstrap enabled, agent release pinned by hash)" vm_root "$M2" '
	set -e
	install -d -m 0700 /etc/dbcm-e4 /var/lib/dbcm-e4
	python3 - "$@" <<"PY"
import json, sys
a2, api, meta, enroll, mon, root, bak, inst, sha, pub = sys.argv[1:11]
cfg = {"auth": {"enabled": True, "rpId": "localhost", "rpDisplayName": "E19 boot", "origins": ["http://localhost:" + api]},
  "manager": {"nodeId": "manager-1", "dataDir": "/var/lib/dbcm-e4", "bind": "127.0.0.1:" + meta, "bootstrap": True, "autoResume": False,
    "agentTransport": {"enrollListen": a2 + ":" + enroll, "managerUrl": "https://" + a2 + ":" + enroll, "agentPort": 7443,
                       "monitorUser": "dbcm_monitor", "monitorSecret": mon, "dbSecrets": [root, bak]},
    "serverBootstrap": {"enabled": True, "sweepSeconds": 60, "agentRelease": {"version": "lab-e19", "binaryUrl": "https://" + a2 + ":'"$PUBPORT"'/dbcm-agent", "sha256": sha}},
    "settings": {"installationId": inst, "volumeBase": "/var/lib/dbcm/volumes", "agentImage": "dbcm-agent:e4",
      "accounts": {"monitorUser": "dbcm_monitor", "monitorSecret": mon, "backupSecret": bak}}}}
json.dump(cfg, open("/etc/dbcm-e4/manager.json", "w"), indent=1)
print(json.dumps(cfg["manager"]["serverBootstrap"]))
PY' "$A2" "$API_PORT" "$META_PORT" "$ENROLL_PORT" "$MON_SECRET" "$ROOT_SECRET" "$BAK_SECRET" "$INST" "$SHA" x
rec "start manager unit" vm_args "$M2" 'sudo systemd-run --unit "$1" -p Restart=no --collect /usr/local/bin/dbcm-e4-manager -listen "127.0.0.1:$2" -config /etc/dbcm-e4/manager.json' "$UNIT" "$API_PORT"
wait_until 90 "manager health" vm_sh "$M2" "curl -fsS http://127.0.0.1:$API_PORT/api/v1/health"
rec "manager log (setup token line removed)" vm_args "$M2" 'sudo journalctl -u "$1" --no-pager | grep -v "setup token" | tail -12' "$UNIT"
rec "lab-own SSH key for root on the fresh server m1 and the lab CA in its trust store (prepared by the harness, as in E17)" sh -c "
	limactl shell --workdir / $M2 -- sudo sh -c 'test -f $STATE/boot_key || ssh-keygen -q -t ed25519 -N \"\" -f $STATE/boot_key; cat $STATE/boot_key.pub' | limactl shell --workdir / $M1 -- sudo sh -c 'install -d -m 0700 /root/.ssh; cat >/root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys'
	limactl shell --workdir / $M2 -- sudo cat $STATE/pki/ca.pem | limactl shell --workdir / $M1 -- sudo sh -c 'cat >/usr/local/share/ca-certificates/dbcm-e19-boot-ca.crt; update-ca-certificates 2>&1 | tail -1'
	limactl shell --workdir / $M1 -- sudo sh -c 'grep -E \"^PermitRootLogin|^PasswordAuthentication\" /etc/ssh/sshd_config /etc/ssh/sshd_config.d/* 2>/dev/null || echo \"no explicit PermitRootLogin\"; sshd -T | grep -E \"^permitrootlogin|^passwordauthentication\"'"
echo "E19B_FRESH=$A1 E19B_MANAGER=$A2" >"$REPO_ROOT/.lab/e19-boot.env"
say "done: fresh server $A1, manager API in m2 on 127.0.0.1:$API_PORT"
