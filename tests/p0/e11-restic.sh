#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E11: restic host backups against real restic and real
# repositories (X14, ADR 0022). Two lab VMs (dbcm-lab-m1, dbcm-lab-m2, Debian
# 12 arm64, user-v2 network) are created through bin/lab.
#
#   m1  the "agent host": the real agent.Executor + internal/restic wrappers
#       (cmd/resticlab) run as root against a pinned, checksum-verified restic
#       and the local, SFTP, rest-server (TLS, htpasswd) and S3 (MinIO, TLS)
#       backends; plus the negative cases (wrong password, interrupted backup,
#       stale lock, allow-list refusals, secret hygiene).
#   m2  the backend host (sshd, rest-server, MinIO) and, separately, the
#       "external host" that belongs to no cluster: the real SSH-plan client
#       (HostSourceSSHClient, internal/manager test E11) backs it up from m1.
#
# Usage:  tests/p0/e11-restic.sh
# Environment (optional): E11_KEEP_VMS=1 skips the scoped cleanup (dev only).
#
# restic and rest-server are pinned and verified with SHA-256 (restic: read
# from deploy/Dockerfile, the same pins the agent image uses). MinIO's binary
# downloads are gone (HTTP 410), so MinIO is built from its pinned Go module
# version on the host (cross-compile, no host install) and its SHA-256 is
# recorded. The host's Docker, package manager and Lima instances other than
# dbcm-lab-* are never touched.
#
# Output: sanitized log at tests/p0/results/e11-restic.log.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG="$REPO_ROOT/tests/p0/results/e11-restic.log"
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
: >"$LOG"
# shellcheck source=tests/p0/common.sh
. "$REPO_ROOT/tests/p0/common.sh"

M1=dbcm-lab-m1
M2=dbcm-lab-m2
M2H=lima-dbcm-lab-m2.internal
VMS="$M1 $M2"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/dbcm-e11.XXXXXX")
MINIO_MODULE=github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c
RESTSERVER_VERSION=0.14.0
RESTSERVER_SHA256=cef139cbe8b27b16bda731d17f093b0aa466b8c60b136c12d78b6f2bff3daf22
RESTIC_VERSION=$(sed -n 's/^ARG RESTIC_VERSION=//p' "$REPO_ROOT/deploy/Dockerfile")
RESTIC_SHA256=$(sed -n 's/^ARG RESTIC_SHA256_ARM64=//p' "$REPO_ROOT/deploy/Dockerfile")
rc=0

# Synthetic credentials, generated per run. They reach the VMs through stdin
# only and are never part of an argv of a host or guest process we start.
RESTPW=$(openssl rand -hex 12)
S3KEY=e11key$(openssl rand -hex 4)
S3SECRET=$(openssl rand -hex 16)
EXTPW=$(openssl rand -hex 12)

finish() {
	local code=$?
	if [ "${E11_KEEP_VMS:-}" = 1 ]; then
		say "E11_KEEP_VMS=1: VMs kept; run 'bin/lab cleanup --apply' yourself"
	else
		lab_cleanup || code=1
	fi
	rm -rf "$TMP"
	# The generated secrets must not appear anywhere in the kept log.
	for s in "$RESTPW" "$S3SECRET" "$EXTPW"; do
		if grep -qF -- "$s" "$LOG"; then
			echo "SECRET FOUND IN LOG" >>"$LOG"
			code=1
		fi
	done
	sanitize_log "$LOG"
	exit "$code"
}
trap finish EXIT

# m1x CMD / m2x CMD — run a sh script as root in the VM.
m1x() { limactl shell --workdir / "$M1" -- sudo -H sh -c "$1"; }
m2x() { limactl shell --workdir / "$M2" -- sudo -H sh -c "$1"; }

section "Build"
(cd "$REPO_ROOT" && rec "build bin/lab" go build -o bin/lab ./cmd/lab) || die "build lab"
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 rec "cross-build resticlab linux/arm64" go build -trimpath -o "$TMP/resticlab" ./cmd/resticlab) || die "build resticlab"
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 rec "cross-build manager E11 test binary" go test -c -o "$TMP/manager.test" ./internal/manager) || die "build manager test"
rec "MinIO ($MINIO_MODULE) cross-build" sh -c "GOOS=linux GOARCH=arm64 go install $MINIO_MODULE"
MINIO_BIN="$(go env GOPATH)/bin/linux_arm64/minio"
rec "MinIO binary sha256" shasum -a 256 "$MINIO_BIN"
rec "pins" sh -c "echo restic $RESTIC_VERSION arm64 sha256 $RESTIC_SHA256; echo rest-server $RESTSERVER_VERSION arm64 sha256 $RESTSERVER_SHA256"

section "Lab VMs"
rec "host free disk before apply" host_free_disk
(cd "$REPO_ROOT" && rec "bin/lab status before" "$LAB" status)
(cd "$REPO_ROOT" && rec "bin/lab apply -vms 2 -templates debian-12,debian-12 -network lima:user-v2" \
	"$LAB" apply -vms 2 -cpus 2 -ram 3 -disk 15 -templates debian-12,debian-12 -network lima:user-v2 --apply) || die "lab apply"
rec "limactl list" limactl list
for vm in $VMS; do
	rec "facts $vm" vm_sh "$vm" 'hostname; uname -m; grep PRETTY_NAME /etc/os-release; nproc; free -m | sed -n 2p; df -h / | tail -1; ip -4 -o addr show | awk "{print \$2, \$4}"'
done
rec "VM-to-VM name resolution m1 -> m2" vm_sh "$M1" "getent hosts $M2H; ping -c1 -W2 $M2H | tail -1"

section "Install restic ($RESTIC_VERSION) with checksum verification"
for vm in $VMS; do
	rec "packages on $vm" vm_sh "$vm" 'sudo apt-get update -qq >/dev/null 2>&1; sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq bzip2 openssl curl ca-certificates openssh-client openssh-server >/dev/null 2>&1; which bunzip2 openssl curl ssh sshd'
	rec "download restic on $vm" vm_sh "$vm" "cd /tmp && curl -fsSL -o restic.bz2 https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_arm64.bz2 && ls -l restic.bz2"
	# The Dockerfile uses exactly this check; a wrong checksum must stop the install.
	if vm_sh "$vm" "cd /tmp && echo '0000000000000000000000000000000000000000000000000000000000000000  restic.bz2' | sha256sum -c -" >>"$LOG" 2>&1; then
		say "FAILED: a wrong checksum was accepted on $vm"
		rc=1
	else
		say "VERIFIED: a wrong checksum is rejected on $vm"
	fi
	rec "verify and install restic on $vm" vm_sh "$vm" "cd /tmp && echo '${RESTIC_SHA256}  restic.bz2' | sha256sum -c - && bunzip2 -c restic.bz2 | sudo tee /usr/local/bin/restic >/dev/null && sudo chmod 0755 /usr/local/bin/restic && restic version"
done

section "Backend host (m2): users, TLS, rest-server, MinIO"
rec "copy MinIO to m2" limactl cp "$MINIO_BIN" "$M2:/tmp/minio"
rec "install MinIO on m2" m2x 'install -m 0755 /tmp/minio /usr/local/bin/minio && /usr/local/bin/minio --version | head -1'
rec "rest-server on m2 (checksum verified)" m2x "cd /tmp && curl -fsSL -o rs.tgz https://github.com/restic/rest-server/releases/download/v${RESTSERVER_VERSION}/rest-server_${RESTSERVER_VERSION}_linux_arm64.tar.gz && echo '${RESTSERVER_SHA256}  rs.tgz' | sha256sum -c - && tar -xzf rs.tgz --strip-components=1 -C /tmp rest-server_${RESTSERVER_VERSION}_linux_arm64/rest-server && install -m 0755 /tmp/rest-server /usr/local/bin/rest-server && rest-server --version"
rec "users and directories on m2" m2x '
	set -e
	for u in e11backup e11ext; do id $u >/dev/null 2>&1 || useradd -m -s /bin/bash $u; install -d -m 700 -o $u -g $u /home/$u/.ssh; done
	install -d -o e11backup -g e11backup /srv/e11-sftp
	install -d -o e11ext -g e11ext /srv/e11ext /srv/e11ext/data /srv/e11ext/data/skip /srv/e11ext/repo
	install -d /srv/e11-rest /srv/e11-minio /etc/e11-tls /etc/e11-tls/minio/certs
	id e11backup e11ext'
rec "TLS certificate on m2 (self-signed, SAN $M2H)" m2x "
	set -e
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 -subj /CN=$M2H \
		-addext subjectAltName=DNS:$M2H -keyout /etc/e11-tls/key.pem -out /etc/e11-tls/cert.pem 2>/dev/null
	cp /etc/e11-tls/cert.pem /etc/e11-tls/minio/certs/public.crt; cp /etc/e11-tls/key.pem /etc/e11-tls/minio/certs/private.key
	openssl x509 -in /etc/e11-tls/cert.pem -noout -subject -ext subjectAltName -enddate"
# htpasswd (bcrypt; rest-server 0.14 rejected the apr1 hash in a first attempt
# with "Invalid htpasswd entry"); the password travels on stdin.
rec "apache2-utils on m2 (htpasswd)" vm_sh "$M2" 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq apache2-utils >/dev/null 2>&1; which htpasswd'
printf '%s\n' "$RESTPW" | limactl shell --workdir / "$M2" -- sudo sh -c 'htpasswd -B -i -c /etc/e11-tls/htpasswd e11rest 2>/dev/null; chmod 644 /etc/e11-tls/htpasswd'
rec "start rest-server (TLS, htpasswd) on m2" m2x "systemd-run --unit=e11-rest rest-server --path /srv/e11-rest --listen :8000 --tls --tls-cert /etc/e11-tls/cert.pem --tls-key /etc/e11-tls/key.pem --htpasswd-file /etc/e11-tls/htpasswd >/dev/null 2>&1; sleep 2; systemctl is-active e11-rest"
printf 'MINIO_ROOT_USER=%s\nMINIO_ROOT_PASSWORD=%s\n' "$S3KEY" "$S3SECRET" | limactl shell --workdir / "$M2" -- sudo sh -c 'cat >/etc/e11-tls/minio.env; chmod 600 /etc/e11-tls/minio.env'
rec "start MinIO (TLS) on m2" m2x 'cat >/etc/systemd/system/e11-minio.service <<EOF
[Service]
EnvironmentFile=/etc/e11-tls/minio.env
ExecStart=/usr/local/bin/minio server /srv/e11-minio --certs-dir /etc/e11-tls/minio/certs --address :9000 --console-address :9001
EOF
systemctl daemon-reload; systemctl start e11-minio; sleep 5; systemctl is-active e11-minio'
rec "TLS health of rest-server and MinIO from m2" m2x "curl -sS --cacert /etc/e11-tls/cert.pem -o /dev/null -w 'rest %{http_code}\n' https://$M2H:8000/ ; curl -sS --cacert /etc/e11-tls/cert.pem -o /dev/null -w 'minio %{http_code}\n' https://$M2H:9000/minio/health/live"

section "Keys and trust (m1 -> m2)"
rec "m1 keys" m1x 'set -e; install -d -m 700 /root/.ssh; [ -f /root/.ssh/id_ed25519 ] || ssh-keygen -q -t ed25519 -N "" -f /root/.ssh/id_ed25519; [ -f /root/e11ext_key ] || ssh-keygen -q -t ed25519 -N "" -f /root/e11ext_key; ssh-keyscan -t ed25519 '"$M2H"' >/root/.ssh/known_hosts 2>/dev/null; cp /root/.ssh/known_hosts /root/e11_known_hosts; wc -l /root/.ssh/known_hosts'
SFTPKEY=$(m1x 'cat /root/.ssh/id_ed25519.pub')
EXTKEY=$(m1x 'cat /root/e11ext_key.pub')
rec "authorize keys on m2" m2x "set -e
	printf '%s\n' '$SFTPKEY' >/home/e11backup/.ssh/authorized_keys; chown e11backup:e11backup /home/e11backup/.ssh/authorized_keys; chmod 600 /home/e11backup/.ssh/authorized_keys
	printf '%s\n' '$EXTKEY' >/home/e11ext/.ssh/authorized_keys; chown e11ext:e11ext /home/e11ext/.ssh/authorized_keys; chmod 600 /home/e11ext/.ssh/authorized_keys
	echo ok"
rec "ssh m1 -> m2 as e11backup" m1x "ssh -o BatchMode=yes e11backup@$M2H 'id -un'"
m2x 'cat /etc/e11-tls/cert.pem' | m1x 'cat >/root/e11-ca.pem'
rec "CA copied to m1" m1x 'openssl x509 -in /root/e11-ca.pem -noout -subject'

section "resticlab on m1 (agent executor, real restic, real backends)"
rec "copy resticlab to m1" limactl cp "$TMP/resticlab" "$M1:/tmp/resticlab"
rec "install resticlab on m1" m1x 'install -m 0755 /tmp/resticlab /usr/local/bin/resticlab'
printf 'export E11_SFTP_REPO=sftp:e11backup@%s:/srv/e11-sftp/repo\nexport E11_REST_REPO=rest:https://%s:8000/e11\nexport E11_REST_USER=e11rest\nexport E11_REST_PASS=%s\nexport E11_S3_REPO=s3:https://%s:9000/e11bucket/repo\nexport E11_S3_KEY=%s\nexport E11_S3_SECRET=%s\nexport SSL_CERT_FILE=/root/e11-ca.pem\n' \
	"$M2H" "$M2H" "$RESTPW" "$M2H" "$S3KEY" "$S3SECRET" | m1x 'umask 077; cat >/root/e11.env'
{
	printf '### resticlab run\n\n```\n'
} >>"$LOG"
# A background sampler on m2 records every process command line during the whole run.
m2x 'rm -f /tmp/e11-ps.txt; nohup sh -c "while :; do ps -eo args >>/tmp/e11-ps.txt; sleep 0.05; done" >/dev/null 2>&1 &'
set +e
limactl shell --workdir / "$M1" -- sudo -H sh -c '. /root/e11.env; exec /usr/local/bin/resticlab' 2>&1 | tee -a "$LOG"
vrc=${PIPESTATUS[0]}
set -e
printf '(exit %s)\n```\n\n' "$vrc" >>"$LOG"
say "resticlab exit $vrc"
[ "$vrc" -eq 0 ] || rc=1
rec "restic child processes and open repositories on m1 after the run" m1x 'pgrep -a restic || echo "no restic process left"; df -h / | tail -1'
LEAKS=$(printf '%s\n%s\n' "$RESTPW" "$S3SECRET" | limactl shell --workdir / "$M2" -- sudo sh -c 'grep -cF -f - /tmp/e11-ps.txt || true')
say "m2 process-list lines containing rest/S3 secrets: $LEAKS (sampled every 50 ms during the run)"
[ "${LEAKS:-0}" = 0 ] || rc=1

section "External host (m2 as host in no cluster): SSH plan via HostSourceSSHClient"
rec "copy manager test binary to m1" limactl cp "$TMP/manager.test" "$M1:/tmp/manager.test"
rec "install manager test binary on m1" m1x 'install -m 0755 /tmp/manager.test /usr/local/bin/manager.test'
printf '%s' "$EXTPW" | m2x 'umask 077; cat >/srv/e11ext/pw; chown e11ext:e11ext /srv/e11ext/pw'
rec "prepare external host data and repository on m2" m2x "
	set -e
	i=0; while [ \$i -lt 3000 ]; do echo \"file \$i\" >/srv/e11ext/data/f\$i.txt; i=\$((i+1)); done
	head -c 3000000 /dev/urandom >/srv/e11ext/data/blob.bin
	echo skipped >/srv/e11ext/data/skip/s.txt
	chown -R e11ext:e11ext /srv/e11ext
	su e11ext -c 'RESTIC_PASSWORD_FILE=/srv/e11ext/pw restic -r /srv/e11ext/repo init' | head -2
	ls /srv/e11ext/data | wc -l"
printf 'export E11_EXT_ADDR=%s\nexport E11_EXT_USER=e11ext\nexport E11_EXT_KEY=/root/e11ext_key\nexport E11_EXT_KNOWN_HOSTS=/root/e11_known_hosts\nexport E11_EXT_REPO=/srv/e11ext/repo\nexport E11_EXT_DATA=/srv/e11ext/data\nexport E11_EXT_RESTORE=/srv/e11ext/restore\nexport E11_EXT_PASSWORD=%s\nexport E11_EXT_PHASE_HOOK="echo changed-between-snapshots >/srv/e11ext/data/changed.txt"\n' \
	"$M2H" "$EXTPW" | m1x 'umask 077; cat >/root/e11ext.env'
m2x 'rm -f /tmp/e11-ps2.txt; nohup sh -c "while :; do ps -eo args >>/tmp/e11-ps2.txt; sleep 0.05; done" >/dev/null 2>&1 &'
{
	printf '### external host test (go test, real SSH)\n\n```\n'
} >>"$LOG"
set +e
limactl shell --workdir / "$M1" -- sudo -H sh -c '. /root/e11ext.env; exec /usr/local/bin/manager.test -test.run TestE11ExternalHostSSHPlanReal -test.v -test.count=1' 2>&1 | tee -a "$LOG"
erc=${PIPESTATUS[0]}
set -e
printf '(exit %s)\n```\n\n' "$erc" >>"$LOG"
say "external host test exit $erc"
[ "$erc" -eq 0 ] || rc=1
rec "m2 remote restic stderr (lab-only capture; the product discards it)" m2x 'cat /tmp/e11-restic-stderr.txt 2>/dev/null || echo "(none)"'
rec "m2 verification of external host results" m2x '
	echo "snapshots in repository:"; su e11ext -c "RESTIC_PASSWORD_FILE=/srv/e11ext/pw restic -r /srv/e11ext/repo snapshots --compact" | tail -4
	echo "restore diff (expect only the excluded skip/ directory to differ):"
	diff -r -x skip /srv/e11ext/data /srv/e11ext/restore/srv/e11ext/data && echo "restored tree identical (excluding skip/)"
	ls /srv/e11ext/restore/srv/e11ext/data | wc -l
	stat -c "restore root mode %a owner %U" /srv/e11ext/restore
	ls /srv/e11ext/restore/srv/e11ext/data/skip 2>&1 | head -1
	echo "locks:"; su e11ext -c "RESTIC_PASSWORD_FILE=/srv/e11ext/pw restic -r /srv/e11ext/repo list locks" | wc -l'
XLEAKS=$(printf '%s\n' "$EXTPW" | limactl shell --workdir / "$M2" -- sudo sh -c 'grep -cF -f - /tmp/e11-ps2.txt || true')
say "m2 process-list lines containing the external-host restic password: $XLEAKS (sampled every 50 ms)"
[ "${XLEAKS:-0}" = 0 ] || rc=1
JLEAKS=$(printf '%s\n' "$EXTPW" | limactl shell --workdir / "$M2" -- sudo sh -c 'journalctl --no-pager 2>/dev/null | grep -cF -f - || true')
say "m2 journal lines containing the external-host restic password: $JLEAKS"
[ "${JLEAKS:-0}" = 0 ] || rc=1
m2x 'pkill -f "ps -eo args" ; pkill -f "sleep 0.05"; true' >/dev/null 2>&1 || true

section "Result"
say "overall: $rc (0 = no FAILED checks; see VERIFIED / NOT VERIFIED lines)"
exit "$rc"
