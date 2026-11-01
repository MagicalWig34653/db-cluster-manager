#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E19, infrastructure part: the E4 layout (real manager as a
# process in m1, mTLS agents, Swarm) with TWO MinIO targets over TLS with a
# private CA (m2 = main target, m3 = copy target, different credentials) and a
# manager configuration with `manager.backup.targets` and
# `settings.backup.targetSecrets`. The scenario itself (administrator through
# the UI, 3-node Galera with binlog, copy, restore of the copy through the
# UI, archive schedule, PITR across files, gap) is driven from the host by
# tests/p0/e19/drive.mjs (Playwright, virtual WebAuthn authenticator).
#
# Usage:   tests/p0/e19-copy-archive.sh
#   E19_KEEP_VMS=1       keep the VMs (default for development; run `bin/lab cleanup --apply` afterwards)
#   E19_PHASES="..."     default: guard build vms swarm images prereqs minio manager
#   E19_TARGET_SECRETS=0 write the manager config WITHOUT targetSecrets (phase `manager` only)
#   E19_REUSE_VMS=1      skip vms and swarm
#   E19_LOG=FILE         log (default tests/p0/results/e19-infra.log)
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${E19_LOG:-"$REPO_ROOT/tests/p0/results/e19-infra.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${E19_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
export E4_AUTORESUME=false
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"
INST=e19lab
NET=dbcm-e19-net
CLUSTER_NAME=e19db
MINIO_BIN="$(go env GOPATH)/bin/linux_arm64/minio"
M2=dbcm-lab-m2
M3=dbcm-lab-m3
MINIO_PORT=9000

finish19() {
	local rc=$?
	trap - EXIT INT TERM
	if [ "${E19_KEEP_VMS:-1}" = 1 ]; then
		say "E19_KEEP_VMS=1: VMs kept; run bin/lab cleanup --apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	say "E19 infrastructure part finished with exit $rc"
	exit "$rc"
}
trap finish19 EXIT
trap 'exit 130' INT TERM

phase_guard() {
	section "Phase guard: host load below 40 and at least 40 GiB free"
	local load free
	load=$(sysctl -n vm.loadavg | awk '{print $2}')
	free=$(df -g "$HOME/.lima" | tail -1 | awk '{print $4}')
	say "1-minute load $load, free disk ${free} GiB"
	hostload
	rec "host free disk" host_free_disk
	[ "${load%.*}" -lt 40 ] || die "host load $load is not below 40"
	[ "$free" -ge 40 ] || die "free disk ${free} GiB is below 40 GiB"
}

phase_build() {
	section "Phase build: cross-compile linux/arm64 binaries on the host"
	mkdir -p "$BIN"
	local c
	for c in manager agent dbcm-job; do
		(cd "$REPO_ROOT" && rec "go build ./cmd/$c" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/$c" "./cmd/$c")
	done
	(cd "$REPO_ROOT" && rec "go build ./tests/p0/e4client" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/e4client" ./tests/p0/e4client)
	rec "MinIO binary sha256 (pinned module build from E11)" shasum -a 256 "$MINIO_BIN"
}

# Private CA and two server certificates (IP SANs) are made in m1 with
# openssl; the CA key never leaves m1's root-only state directory.
phase_minio() {
	section "Phase minio: private CA, two MinIO servers with TLS (m2 main, m3 copy target), backend files and Swarm secrets"
	hostload
	local a2 a3
	a2=$(uv2_ip "$M2")
	a3=$(uv2_ip "$M3")
	say "m2 $a2  m3 $a3"
	rec "CA and server certificates (made in m1)" vm_root "$M1" '
		set -e
		d="$1/pki"; install -d -m 0700 "$d"; cd "$d"
		if [ ! -f ca.pem ]; then
			openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout ca.key -out ca.pem -days 30 -subj "/CN=dbcm-e19-lab-ca" 2>/dev/null
			for n in m2:"$2" m3:"$3"; do
				name=${n%%:*}; ip=${n#*:}
				openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$name.key" -out "$name.csr" -subj "/CN=$name" 2>/dev/null
				printf "subjectAltName=IP:%s\nextendedKeyUsage=serverAuth\n" "$ip" >"$name.ext"
				openssl x509 -req -in "$name.csr" -CA ca.pem -CAkey ca.key -CAcreateserial -out "$name.crt" -days 30 -extfile "$name.ext" 2>/dev/null
			done
		fi
		ls -l; openssl x509 -in ca.pem -noout -subject -enddate; openssl x509 -in m2.crt -noout -subject -issuer -ext subjectAltName' "$STATE" "$a2" "$a3"
	local vm name
	for vm in $M2 $M3; do
		name=m${vm##*m}
		rec "copy MinIO binary and the certificate of $vm" sh -c "
			limactl cp '$MINIO_BIN' '$vm:/tmp/minio'
			limactl shell --workdir / $M1 -- sudo tar -c -C $STATE/pki ca.pem $name.crt $name.key | limactl shell --workdir / $vm -- sudo sh -c 'mkdir -p /etc/e19-minio/certs && tar -x -C /etc/e19-minio/certs && cd /etc/e19-minio/certs && mv $name.crt public.crt && mv $name.key private.key && mkdir -p CAs && cp ca.pem CAs/ && chmod 600 private.key && ls -l'"
		rec "MinIO on $vm (TLS, own credentials, data in /srv/e19-minio)" vm_root "$vm" '
			set -e
			install -m 0755 /tmp/minio /usr/local/bin/minio; rm -f /tmp/minio
			install -d /srv/e19-minio/dbcm-backups
			ak=e19$(head -c 6 /dev/urandom | od -An -tx1 | tr -d " \n"); sk=$(head -c 18 /dev/urandom | base64 | tr -d "\n=/+")
			printf "MINIO_ROOT_USER=%s\nMINIO_ROOT_PASSWORD=%s\n" "$ak" "$sk" >/etc/e19-minio/minio.env; chmod 600 /etc/e19-minio/minio.env
			cat >/etc/systemd/system/e19-minio.service <<EOF
[Service]
EnvironmentFile=/etc/e19-minio/minio.env
ExecStart=/usr/local/bin/minio server /srv/e19-minio --certs-dir /etc/e19-minio/certs --address :'"$MINIO_PORT"' --console-address :9001
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF
			systemctl daemon-reload; systemctl enable --now e19-minio; sleep 5; systemctl is-active e19-minio
			ls /srv/e19-minio'
	done
	rec "TLS check of both servers from m1 with the private CA" vm_root "$M1" '
		for ip in "$2" "$3"; do curl -sS --cacert "$1/pki/ca.pem" -o /dev/null -w "$ip %{http_code}\n" "https://$ip:'"$MINIO_PORT"'/minio/health/live"; done
		curl -sS -o /dev/null -w "without CA (must fail): %{http_code}\n" "https://$2:'"$MINIO_PORT"'/minio/health/live" || true' "$STATE" "$a2" "$a3"
	# Backend files. The credentials are read from the MinIO unit environment
	# of each VM and written into root-only files in m1; they are never logged.
	local k2 k3
	k2=$(vm_root "$M2" 'cat /etc/e19-minio/minio.env')
	k3=$(vm_root "$M3" 'cat /etc/e19-minio/minio.env')
	printf '%s\n%s\n' "$k2" "$k3" | vm_root "$M1" '
		set -e
		cd "$1"
		cat >env.tmp; chmod 0600 env.tmp
		python3 - "$2" "$3" "$4" <<"PY"
import json, sys
env = [l.strip().split("=", 1) for l in open("env.tmp").read().splitlines() if "=" in l]
(u2, p2), (u3, p3) = [(env[0][1], env[1][1]), (env[2][1], env[3][1])]
ca = open("pki/ca.pem").read()
def cfg(ip, u, p, prefix):
    return {"type": "s3", "s3": {"endpoint": "https://%s:%s" % (ip, sys.argv[3]), "region": "us-east-1", "bucket": "dbcm-backups",
            "prefix": prefix, "addressing_style": "path", "access_key": u, "secret_key": p, "ca_pem": ca}}
json.dump(cfg(sys.argv[1], u2, p2, "e19-"), open("backend.json", "w"))
json.dump(cfg(sys.argv[2], u3, p3, "e19c-"), open("backend-copy.json", "w"))
PY
		rm -f env.tmp; chmod 0600 backend.json backend-copy.json
		for x in dbcm-e19-backend:backend.json dbcm-e19-backend-copy:backend-copy.json; do
			n=${x%%:*}; f=${x#*:}
			if docker secret inspect "$n" >/dev/null 2>&1; then echo "secret $n exists"; else docker secret create --label "dbcm.installation=$5" "$n" "$f" >/dev/null && echo "secret $n created"; fi
		done
		[ -f recipient.pub ] || /usr/local/bin/dbcm-e4-client -state "$1/client.json" keygen "$1/recovery.key" >recipient.pub
		cat recipient.pub
		n=dbcm-e19-recovery-key
		if docker secret inspect "$n" >/dev/null 2>&1; then echo "secret $n exists"; else docker secret create --label "dbcm.installation=$5" "$n" "$1/recovery.key" >/dev/null && echo "secret $n created"; fi' "$STATE" "$a2" "$a3" "$MINIO_PORT" "$INST" 2>&1 | tee -a "$LOG"
	rec "credentials differ between the two backend files (hashes only)" vm_root "$M1" 'cd "$1"; python3 -c "
import json,hashlib
a=json.load(open(\"backend.json\"))[\"s3\"]; b=json.load(open(\"backend-copy.json\"))[\"s3\"]
print(\"access keys differ:\", a[\"access_key\"]!=b[\"access_key\"], \" secret keys differ:\", a[\"secret_key\"]!=b[\"secret_key\"], \" endpoints:\", a[\"endpoint\"], b[\"endpoint\"], \" prefixes:\", a[\"prefix\"], b[\"prefix\"])"' "$STATE"
}

phase_manager() {
	section "Phase manager: start bin/manager with targets (copy2) and targetSecrets=${E19_TARGET_SECRETS:-1}"
	hostload
	local a1 pub
	a1=$(uv2_ip "$M1")
	pub=$(vm_sh "$M1" "sudo cat $STATE/recipient.pub")
	rec "stop a running manager unit" vm_root "$M1" 'systemctl stop "$1" 2>/dev/null || true; systemctl reset-failed "$1" 2>/dev/null || true; echo stopped' "$UNIT"
	rec "manager config" vm_root "$M1" '
		set -e
		install -d -m 0700 /etc/dbcm-e4 /var/lib/dbcm-e4
		python3 - "$@" <<"PY"
import json, sys
a1, api, meta, enroll, mon, root, bak, inst, state, pub, ts = sys.argv[1:12]
cfg = {
  "auth": {"enabled": True, "rpId": "localhost", "rpDisplayName": "E19 lab", "origins": ["http://localhost:" + api]},
  "manager": {
    "nodeId": "manager-1", "dataDir": "/var/lib/dbcm-e4", "bind": "127.0.0.1:" + meta, "bootstrap": True, "autoResume": False,
    "agentTransport": {"enrollListen": a1 + ":" + enroll, "managerUrl": "https://" + a1 + ":" + enroll, "agentPort": 7443,
                       "monitorUser": "dbcm_monitor", "monitorSecret": mon, "dbSecrets": [root, bak]},
    "backup": {"backendConfigFile": state + "/backend.json", "scheduleIntervalSeconds": 30,
               "targets": {"copy2": state + "/backend-copy.json"}},
    "settings": {
      "installationId": inst, "volumeBase": "/var/lib/dbcm/volumes", "agentImage": "dbcm-agent:e4",
      "accounts": {"monitorUser": "dbcm_monitor", "monitorSecret": mon, "backupSecret": bak},
      "backup": {"jobImage": "dbcm-job:e4", "backendSecret": "dbcm-e19-backend", "dbCredentialSecret": "dbcm-e4-backup-cred",
                 "recipientKey": pub, "keySecret": "dbcm-e19-recovery-key", "inspectionNetwork": "dbcm-e4-inspect"}
    }
  }
}
if ts == "1":
    cfg["manager"]["settings"]["backup"]["targetSecrets"] = {"copy2": "dbcm-e19-backend-copy"}
json.dump(cfg, open("/etc/dbcm-e4/manager.json", "w"), indent=1)
d = json.loads(json.dumps(cfg))
print(json.dumps(d["manager"]["settings"]["backup"], indent=1)); print(json.dumps(d["manager"]["backup"]))
PY' "$a1" "$API_PORT" "$META_PORT" "$ENROLL_PORT" "$MON_SECRET" "$ROOT_SECRET" "$BAK_SECRET" "$INST" "$STATE" "$pub" "${E19_TARGET_SECRETS:-1}"
	rec "start manager unit" vm_args "$M1" 'sudo systemd-run --unit "$1" -p Restart=no --collect /usr/local/bin/dbcm-e4-manager -listen "127.0.0.1:$2" -config /etc/dbcm-e4/manager.json' "$UNIT" "$API_PORT"
	wait_until 60 "manager health" vm_sh "$M1" "curl -fsS http://127.0.0.1:$API_PORT/api/v1/health"
	rec "manager log (setup token line removed)" vm_args "$M1" 'sudo journalctl -u "$1" --no-pager | grep -v "setup token" | tail -15' "$UNIT"
}

E19_PHASES=${E19_PHASES:-"guard build vms swarm images prereqs minio manager"}
for p in $E19_PHASES; do
	case "$p" in vms | swarm) [ "${E19_REUSE_VMS:-0}" = 1 ] && continue ;; esac
	"phase_$p"
done
