#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# Lab experiment E15: point-in-time recovery (ADR 0031) on three dbcm-lab VMs.
#
# A three-node Galera cluster runs with the binary log options of
# galera.BuildServiceSpec (binlog: true). The REAL job helper (cmd/dbcm-job,
# cross-built for linux/arm64) runs as the product's agent would start it, in
# the mariadb image: physical backup of galera-2, archive of the closed binlog
# files, physical restore into a new volume, an inspection-style instance
# (the product's init file with the replay account) and the binlog replay up to
# a recorded time. Rows, sums and CHECKSUM TABLE at that time are compared.
#
# NOT exercised: the manager, the agent and the UI (their path is covered by
# fakes and in-process tests); the jobs are started with `docker run` here.
#
# Usage:  tests/p0/e15-pitr.sh
# Environment: E15_KEEP_VMS=1 keeps the VMs (then run `bin/lab cleanup -apply`).
# Guards: refuses to start with 1-minute load >= 40 or < 40 GiB free host disk.
# Output: tests/p0/results/e15-pitr.log (sanitized).
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG="$REPO_ROOT/tests/p0/results/e15-pitr.log"
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
: >"$LOG"
# shellcheck source=tests/p0/common.sh
. "$REPO_ROOT/tests/p0/common.sh"

VMS="dbcm-lab-m1 dbcm-lab-m2 dbcm-lab-m3"
M1=dbcm-lab-m1
M2=dbcm-lab-m2
NET=dbcm-e15-galera
SECRET=dbcm-e15-root-pw
MARIADB_IMAGE=mariadb:11.4
ONESHOT_IMAGE=busybox:1.37
SENTINEL_IMAGE=nginx:1.27-alpine # only pulled by step4_galera for the digest record
INSTALLATION=e15-lab
CLUSTER=e15-galera
GALERA_NAME=dbcm-e15
BASE_ID=b-e15-full
BASE_OBJECT="$CLUSTER-$BASE_ID.xbs.dbcms"
SRC_SLOT=2 # backup source: galera-2 on dbcm-lab-m2
E15=/var/lib/dbcm-e15 # job helper, secrets and the dir backup target on m2
TARGET=/var/lib/dbcm/pitr-target
INSPECT=dbcm-inspect-e15
WORK=$(mktemp -d "${TMPDIR:-/tmp}/dbcm-e15.XXXXXX")

finish() {
	local rc=$?
	trap - EXIT INT TERM
	rm -rf "$WORK"
	if [ "${E15_KEEP_VMS:-0}" = 1 ]; then
		say "E15_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	say "E15 finished with exit $rc (log: tests/p0/results/e15-pitr.log)"
	exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

# The Galera options of galera.BuildServiceSpec with Binlog set. The lab
# mounts the volume's data/ at /var/lib/mysql (the product: volume at /dbcm,
# datadir /dbcm/data); the job sees both as /dbcm/data.
galera_args() {
	local a="--wsrep_on=ON --wsrep_provider=/usr/lib/galera/libgalera_smm.so"
	a="$a --wsrep_cluster_name=$GALERA_NAME --wsrep_cluster_address=gcomm://galera-1,galera-2,galera-3"
	a="$a --wsrep_sst_method=mariabackup --binlog_format=ROW --innodb_autoinc_lock_mode=2"
	a="$a --datadir=/dbcm/data --socket=/dbcm/run/mysqld.sock"
	a="$a --log_bin=/dbcm/data/dbcm-bin --log_slave_updates=ON --binlog_expire_logs_seconds=1209600"
	a="$a --wsrep_provider_options=gcache.size=128M"
	[ "$1" = 1 ] && a="$a --wsrep-new-cluster"
	echo "$a"
}

# The product mounts the volume at /dbcm (datadir /dbcm/data, socket /dbcm/run),
# so the job sees the same paths as the server. Override the E1 helpers.
eval "orig_$(declare -f write_marker)"
write_marker() {
	orig_write_marker "$1"
	vm_sh "$(vm_of "$1")" "sudo mkdir -p /var/lib/dbcm/galera-$1/run && sudo chown 999:999 /var/lib/dbcm/galera-$1/run"
}
galera_create() { # galera_create N BOOTSTRAP
	local n=$1 boot=$2 nid
	nid=$(node_id "$n")
	# shellcheck disable=SC2046
	rec "create service galera-$n pinned to node $nid (bootstrap=$boot)" \
		vm_docker "$M1" service create --detach --name "galera-$n" --replicas 1 \
		--constraint "node.id==$nid" --network "$NET" --endpoint-mode dnsrr \
		--hostname "galera-$n" --restart-condition any --stop-grace-period 60s \
		--label dbcm.installation="$INSTALLATION" --label dbcm.cluster="$CLUSTER" --label dbcm.node="galera-$n" \
		--secret "$SECRET" --env "MARIADB_ROOT_PASSWORD_FILE=/run/secrets/$SECRET" \
		--env MARIADB_MYSQL_LOCALHOST_USER=1 \
		--env "MARIADB_MYSQL_LOCALHOST_GRANTS=RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR" \
		--mount "type=bind,src=/var/lib/dbcm/galera-$n,dst=/dbcm" \
		"$MARIADB_IMAGE" $(galera_args "$boot")
}

sql() { # as in common.sh, with the product's socket path
	local cid
	cid=$(galera_cid "$1")
	[ -n "$cid" ] || return 1
	printf '%s\n' "$2" | limactl shell --workdir / "$(vm_of "$1")" -- sudo docker exec -i "$cid" \
		sh -c 'exec mariadb --socket=/dbcm/run/mysqld.sock -uroot -p"$(cat /run/secrets/'"$SECRET"')" -N -B'
}

guard_host() {
	local load free
	load=$(sysctl -n vm.loadavg | awk '{print $2}')
	free=$(df -g "$HOME/.lima" | tail -1 | awk '{print $4}')
	say "host load (1 min): $load, free disk: ${free} GiB"
	awk -v l="$load" 'BEGIN{exit !(l < 40)}' || die "host load $load >= 40"
	[ "$free" -ge 40 ] || die "free disk ${free} GiB < 40"
}

# m2 helpers -----------------------------------------------------------------
m2() { vm_sh "$M2" "$1"; }
node_uuid() { vm_sh "$(vm_of "$1")" "sudo sed -n 's/.*\"node_uuid\":\"\\([^\"]*\\)\".*/\\1/p' /var/lib/dbcm/galera-$1/dbcm-identity.json"; }

# job NAME VOLUME_MOUNT... -- ARGS...: the job helper in the mariadb image on m2,
# on the cluster network, with the secrets directory and the backup target.
job() {
	local name=$1 mounts=() args=()
	shift
	while [ "$1" != -- ]; do mounts+=(-v "$1"); shift; done
	shift
	args=("$@")
	vm_docker "$M2" run --rm --name "$name" --network "$NET" --entrypoint /usr/local/bin/dbcm-job \
		-v "$E15/dbcm-job:/usr/local/bin/dbcm-job:ro" -v "$E15/secrets:/run/secrets:ro" -v "$E15/backup:/backup" \
		${mounts[@]+"${mounts[@]}"} "$MARIADB_IMAGE" "${args[@]}"
}

backup_args() { # backup_args JOB TOOL
	echo "backup --job=$1 --tool=$2 --backup-id=$BASE_ID --object=$BASE_OBJECT --cluster-id=$CLUSTER --node-uuid=$SRC_UUID
	--db-host=galera-$SRC_SLOT --db-option-file=/run/secrets/db-cred --backend-config=/run/secrets/backend-cfg
	--recipient=$PUBKEY --datadir=/dbcm/data"
}

dbsql() { sql "$1" "$2"; }

state() { # state N -> "checksum count sum(amount) sum(id)" of pitr.orders after a cluster sync
	sql "$1" "SET SESSION wsrep_sync_wait=1; SELECT CONCAT_WS(' ', (SELECT COUNT(*) FROM pitr.orders), (SELECT SUM(amount) FROM pitr.orders), (SELECT SUM(id) FROM pitr.orders), (SELECT GROUP_CONCAT(DISTINCT batch ORDER BY batch) FROM pitr.orders)); CHECKSUM TABLE pitr.orders" |
		awk 'NR==1{rows=$0} NR==2{print rows, "checksum=" $2}'
}

restored_state() { # same from the restored instance via the inspection account
	vm_docker "$M2" exec "$INSPECT" mariadb -N -B -h127.0.0.1 -P3307 -udbcm_inspect -p"$INSPECT_PW" -e \
		"SELECT CONCAT_WS(' ', (SELECT COUNT(*) FROM pitr.orders), (SELECT SUM(amount) FROM pitr.orders), (SELECT SUM(id) FROM pitr.orders), (SELECT GROUP_CONCAT(DISTINCT batch ORDER BY batch) FROM pitr.orders)); CHECKSUM TABLE pitr.orders" |
		awk 'NR==1{rows=$0} NR==2{print rows, "checksum=" $2}'
}

# ---------------------------------------------------------------- steps
step_prepare() {
	section "Step 0: guards and host-side build"
	rec "uptime" uptime
	guard_host
	rec "go build dbcm-job linux/arm64" sh -c "cd '$REPO_ROOT' && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o '$WORK/dbcm-job' ./cmd/dbcm-job"
	rec "go build pitrlab (host)" sh -c "cd '$REPO_ROOT' && go build -o '$WORK/pitrlab' ./cmd/pitrlab"
	PUBKEY=$("$WORK/pitrlab" keygen "$WORK/recovery.key")
	say "recovery key generated (public half $PUBKEY)"
	INSPECT_PW=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
	REPLAY_PW=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
	BACKUP_PW=$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
}

step_lab() {
	step1_vms
	prepare_guests
	record_guest_facts
	step2_swarm
	section "Step 2b: Galera with the binary log (binlog: true)"
	step4_galera
	rec "log_bin on galera-2" sql 2 "SHOW VARIABLES WHERE Variable_name IN ('log_bin','log_bin_basename','log_slave_updates','binlog_format','binlog_expire_logs_seconds','server_id')"
	rec "binary logs on galera-2" sql 2 "SHOW BINARY LOGS"
}

step_stage() {
	section "Step 3: stage the job helper, secrets and the backup target on m2"
	rec "copy dbcm-job to m2" limactl copy "$WORK/dbcm-job" "$M2:/tmp/dbcm-job"
	rec "copy recovery key to m2" limactl copy "$WORK/recovery.key" "$M2:/tmp/recovery.key"
	rec "stage $E15" vm_sh "$M2" "
		set -e
		sudo install -d -m 0755 $E15 $E15/backup
		sudo install -d -m 0755 $E15/secrets
		sudo install -m 0755 /tmp/dbcm-job $E15/dbcm-job
		sudo install -m 0644 /tmp/recovery.key $E15/secrets/restore-key
		printf '%s' '{\"type\":\"dir\",\"dir\":\"/backup\"}' | sudo tee $E15/secrets/backend-cfg >/dev/null
		printf '[client]\nuser=dbcm_backup\npassword=$BACKUP_PW\n' | sudo tee $E15/secrets/db-cred >/dev/null
		rm -f /tmp/recovery.key /tmp/dbcm-job
		ls -la $E15 $E15/secrets"
	SRC_UUID=$(node_uuid "$SRC_SLOT")
	say "source node UUID: $SRC_UUID"
	# The product's backup account (dbadmin backupPhysicalPrivileges).
	rec "create the backup account" sql 1 "CREATE USER 'dbcm_backup'@'%' IDENTIFIED BY '$BACKUP_PW';
		GRANT RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR ON *.* TO 'dbcm_backup'@'%'"
}

step_data_before_backup() {
	section "Step 4: data before the backup (written on galera-1)"
	rec "create pitr.orders and 2000 rows (batch O)" sql 1 "CREATE DATABASE pitr;
		CREATE TABLE pitr.orders (id INT PRIMARY KEY, batch CHAR(1) NOT NULL, val VARCHAR(40) NOT NULL, amount INT NOT NULL) ENGINE=InnoDB;
		USE pitr;
		INSERT INTO pitr.orders SELECT seq, 'O', CONCAT('row-', seq), seq % 97 FROM seq_1_to_2000"
	STATE_BACKUP=$(state 2)
	say "state at backup time (read on galera-2): $STATE_BACKUP"
}

step_backup() {
	section "Step 5: physical backup of galera-$SRC_SLOT with the real job helper"
	local t0
	t0=$(date +%s)
	# shellcheck disable=SC2046
	rec "dbcm-job backup (mariabackup) of galera-$SRC_SLOT" job dbcm-job-e15-backup "/var/lib/dbcm/galera-$SRC_SLOT:/dbcm:ro" -- $(backup_args dbcm-job-e15-backup mariabackup)
	say "backup took $(($(date +%s) - t0)) s"
	rec "backup target" vm_sh "$M2" "sudo ls -la $E15/backup"
	BASE_MANIFEST=$(vm_sh "$M2" "sudo cat $E15/backup/$BASE_OBJECT.manifest.json")
	printf '### base manifest (binlog_base, finished, tool)\n\n```\n%s\n```\n\n' \
		"$(printf '%s' "$BASE_MANIFEST" | python3 -c 'import json,sys; m=json.load(sys.stdin); print(json.dumps({k:m.get(k) for k in ("backup_id","method","finished","binlog_base","galera","tool","warnings")}, indent=1))')" >>"$LOG"
	BASE_OFFSET=$(printf '%s' "$BASE_MANIFEST" | python3 -c 'import json,sys; print(json.load(sys.stdin)["binlog_base"]["offset"])') ||
		die "the backup manifest has no binlog_base: the binary log was not recorded"
	say "backup binlog position offset: $BASE_OFFSET"
}

step_writes() {
	section "Step 6: writes after the backup, the time T, writes after T"
	# Phase A: written on galera-1 and galera-3 (replicated into galera-2's binlog through log_slave_updates).
	rec "phase A on galera-1: 1000 inserts (batch A)" sql 1 "USE pitr; INSERT INTO pitr.orders SELECT seq, 'A', CONCAT('row-', seq), seq % 89 FROM seq_2001_to_3000"
	rec "phase A on galera-3: updates of batch O" sql 3 "UPDATE pitr.orders SET amount = amount + 1000 WHERE batch = 'O' AND id % 10 = 0"
	rec "phase A on galera-1: deletes of batch O" sql 1 "DELETE FROM pitr.orders WHERE batch = 'O' AND id % 50 = 1"
	STATE_T=$(state 1)
	T=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	say "T = $T; state at T (galera-1, after wsrep_sync_wait): $STATE_T"
	sleep 3
	# Phase B: after T, including a write on the source node itself.
	rec "phase B on galera-3: 1000 inserts (batch B)" sql 3 "USE pitr; INSERT INTO pitr.orders SELECT seq, 'B', CONCAT('row-', seq), seq % 83 FROM seq_3001_to_4000"
	rec "phase B on galera-1: updates of batch A" sql 1 "UPDATE pitr.orders SET amount = amount + 5 WHERE batch = 'A'"
	rec "phase B on galera-2 (the source node): deletes of batch A" sql 2 "DELETE FROM pitr.orders WHERE batch = 'A' AND id % 7 = 0"
	STATE_FINAL=$(state 2)
	say "state after phase B: $STATE_FINAL"
	[ "$STATE_T" != "$STATE_FINAL" ] || die "phase B did not change the data"
}

step_archive() {
	section "Step 7: archive the closed binlog files (real job helper)"
	local t0
	t0=$(date +%s)
	# shellcheck disable=SC2046
	rec "dbcm-job backup --tool=binlog-archive" job dbcm-job-e15-archive-1 "/var/lib/dbcm/galera-$SRC_SLOT:/dbcm:ro" -- $(backup_args dbcm-job-e15-archive-1 binlog-archive)
	say "archive run took $(($(date +%s) - t0)) s"
	rec "binary logs on galera-2 after the rotation" sql 2 "SHOW BINARY LOGS"
	rec "archive objects" vm_sh "$M2" "sudo ls -la $E15/backup | grep -- '-l0'"
	ARCHIVE_COUNT_1=$(vm_sh "$M2" "sudo ls $E15/backup | grep -c 'binlog.dbcms.manifest.json'")
	# A second run with nothing new is a no-op.
	# shellcheck disable=SC2046
	rec "second archive run (nothing new)" job dbcm-job-e15-archive-2 "/var/lib/dbcm/galera-$SRC_SLOT:/dbcm:ro" -- $(backup_args dbcm-job-e15-archive-2 binlog-archive)
	local n2
	n2=$(vm_sh "$M2" "sudo ls $E15/backup | grep -c 'binlog.dbcms.manifest.json'")
	say "archive manifests: $ARCHIVE_COUNT_1 after the first run, $n2 after the second"
	# The second run rotates once more and archives that (now closed) file, which
	# holds no event of ours but its own rotation/format events: one more file.
	[ "$n2" -ge "$ARCHIVE_COUNT_1" ] || die "archives disappeared"
	rec "fetch the backup target to the host" sh -c "limactl shell --workdir / $M2 -- sudo tar -C $E15 -cf - backup | tar -C '$WORK' -xf -"
}

step_propose() {
	section "Step 8: coverage over the real archive (library code via pitrlab)"
	rec "propose T (must be covered)" "$WORK/pitrlab" propose "$WORK/backup" "$CLUSTER" "$BASE_OBJECT" "$T"
	PROPOSAL=$("$WORK/pitrlab" propose "$WORK/backup" "$CLUSTER" "$BASE_OBJECT" "$T")
	printf '%s' "$PROPOSAL" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["covered"] else 1)' || die "T is not covered by the archive"
	SEGMENTS=$(printf '%s' "$PROPOSAL" | python3 -c 'import json,sys; print(json.load(sys.stdin)["segments"])')
	local later
	later=$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ)
	rec "propose a time after the archive (must not be covered)" "$WORK/pitrlab" propose "$WORK/backup" "$CLUSTER" "$BASE_OBJECT" "$later"
	rec "propose a time before the backup (must not be covered)" "$WORK/pitrlab" propose "$WORK/backup" "$CLUSTER" "$BASE_OBJECT" "2020-01-01T00:00:00Z"
	STOP=$("$WORK/pitrlab" stop-datetime "$T")
	SEG_LIST=$(ls "$WORK/backup" | grep "^$CLUSTER-$BASE_ID-l[0-9]*\.binlog\.dbcms$" | sort | head -n "$SEGMENTS" | paste -sd, -)
	say "segments to replay ($SEGMENTS): $SEG_LIST; stop-datetime $STOP"
}

step_restore() {
	section "Step 9: physical restore into a new volume and the inspection-style instance"
	rec "prepare the empty target volume" vm_sh "$M2" "sudo rm -rf $TARGET && sudo install -d -m 0755 $TARGET $TARGET/data"
	local t0
	t0=$(date +%s)
	rec "dbcm-job restore --mode=physical" job dbcm-job-e15-restore "$TARGET:/dbcm" -- \
		restore --job=dbcm-job-e15-restore --mode=physical --backup-id="$BASE_ID" --object="$BASE_OBJECT" --approval-id=appr-e15 \
		--source-cluster-id="$CLUSTER" --backend-config=/run/secrets/backend-cfg --key-file=/run/secrets/restore-key \
		--datadir=/dbcm/data --target-cluster-id=inspect-e15
	say "restore took $(($(date +%s) - t0)) s"
	"$WORK/pitrlab" init-sql "$INSPECT_PW" "$REPLAY_PW" >"$WORK/init.sql"
	"$WORK/pitrlab" replay-opts "$REPLAY_PW" 3307 >"$WORK/replay-cred"
	rec "copy init file and replay option file to m2" limactl copy "$WORK/init.sql" "$WORK/replay-cred" "$M2:/tmp/"
	rec "stage them" vm_sh "$M2" "
		set -e
		sudo install -m 0644 /tmp/init.sql $E15/init.sql
		sudo install -m 0644 /tmp/replay-cred $E15/secrets/replay-cred
		rm -f /tmp/init.sql /tmp/replay-cred
		sudo install -d -m 0750 $TARGET/run
		sudo chown -R 999:999 $TARGET
		sudo ls -la $TARGET/data | head -20"
	# The arguments of galera.InspectionServiceSpec (wsrep off, no provider, no
	# binary log, replication not started, another port, the init file).
	rec "start the instance" vm_docker "$M2" run -d --name "$INSPECT" --hostname "$INSPECT" --network "$NET" \
		-v "$TARGET:/dbcm" -v "$E15/init.sql:/run/secrets/inspect-init:ro" "$MARIADB_IMAGE" \
		--datadir=/dbcm/data --socket=/dbcm/run/mysqld.sock --wsrep-on=OFF --wsrep-provider=none --wsrep-cluster-address= \
		--wsrep-cluster-name=inspect-e15 --port=3307 --skip-slave-start --event-scheduler=OFF --skip-log-bin --init-file=/run/secrets/inspect-init
	wait_until 180 "instance answers" vm_docker "$M2" exec "$INSPECT" mariadb -N -B -h127.0.0.1 -P3307 -udbcm_inspect -p"$INSPECT_PW" -e "SELECT 1" ||
		{ rec "instance logs" vm_docker "$M2" logs --tail 60 "$INSPECT"; return 1; }
	STATE_RESTORED_BASE=$(restored_state)
	say "state of the restored backup before the replay: $STATE_RESTORED_BASE"
	say "state at backup time was:                       $STATE_BACKUP"
}

step_replay() {
	section "Step 10: replay up to T with the real job helper"
	local t0
	# Refusals first: a production host, a target after the archive.
	rec "replay against a production service name (must be refused)" sh -c "! limactl shell --workdir / $M2 -- sudo docker run --rm --network $NET --entrypoint /usr/local/bin/dbcm-job -v $E15/dbcm-job:/usr/local/bin/dbcm-job:ro -v $E15/secrets:/run/secrets:ro -v $E15/backup:/backup $MARIADB_IMAGE restore --job=dbcm-job-e15-bad --mode=binlog --backup-id=$BASE_ID --object=$BASE_OBJECT --approval-id=a --source-cluster-id=$CLUSTER --backend-config=/run/secrets/backend-cfg --key-file=/run/secrets/restore-key --db-host=galera-1 --db-option-file=/run/secrets/replay-cred --segments=$SEG_LIST --start-offset=$BASE_OFFSET --stop-datetime='$STOP'"
	rec "replay beyond the archive (must fail before applying anything)" sh -c "! limactl shell --workdir / $M2 -- sudo docker run --rm --network $NET --entrypoint /usr/local/bin/dbcm-job -v $E15/dbcm-job:/usr/local/bin/dbcm-job:ro -v $E15/secrets:/run/secrets:ro -v $E15/backup:/backup $MARIADB_IMAGE restore --job=dbcm-job-e15-late --mode=binlog --backup-id=$BASE_ID --object=$BASE_OBJECT --approval-id=a --source-cluster-id=$CLUSTER --backend-config=/run/secrets/backend-cfg --key-file=/run/secrets/restore-key --db-host=$INSPECT --db-option-file=/run/secrets/replay-cred --segments=$SEG_LIST --start-offset=$BASE_OFFSET --stop-datetime='2099-01-01 00:00:00'"
	t0=$(date +%s)
	rec "dbcm-job restore --mode=binlog (replay up to $T)" job dbcm-job-e15-pitr -- \
		restore --job=dbcm-job-e15-pitr --mode=binlog --backup-id="$BASE_ID" --object="$BASE_OBJECT" --approval-id=appr-e15 \
		--source-cluster-id="$CLUSTER" --backend-config=/run/secrets/backend-cfg --key-file=/run/secrets/restore-key \
		--db-host="$INSPECT" --db-option-file=/run/secrets/replay-cred --segments="$SEG_LIST" --start-offset="$BASE_OFFSET" --stop-datetime="$STOP"
	say "replay took $(($(date +%s) - t0)) s"
	rec "job status object" vm_sh "$M2" "sudo cat $E15/backup/dbcm-job-e15-pitr.status.json"
}

step_compare() {
	section "Step 11: comparison"
	STATE_RESTORED=$(restored_state)
	{
		printf '### states (rows, sum(amount), sum(id), batches, CHECKSUM TABLE)\n\n```\n'
		printf 'at backup time   : %s\n' "$STATE_BACKUP"
		printf 'at T (%s): %s\n' "$T" "$STATE_T"
		printf 'after phase B    : %s\n' "$STATE_FINAL"
		printf 'restored backup  : %s\n' "$STATE_RESTORED_BASE"
		printf 'restored to T    : %s\n' "$STATE_RESTORED"
		printf '```\n\n'
	} >>"$LOG"
	rec "accounts in the restored instance (the replay account must be gone)" vm_docker "$M2" exec "$INSPECT" \
		mariadb -N -B -h127.0.0.1 -P3307 -udbcm_inspect -p"$INSPECT_PW" -e "SELECT User, Host, JSON_VALUE(Priv, '\$.account_locked') FROM mysql.global_priv ORDER BY User, Host"
	if [ "$STATE_RESTORED" = "$STATE_T" ]; then
		say "RESULT: restored state equals the state at T"
		echo "RESULT: PASS (restored state equals the state at T)" >>"$LOG"
	else
		say "RESULT: MISMATCH restored=[$STATE_RESTORED] expected=[$STATE_T]"
		echo "RESULT: FAIL restored=[$STATE_RESTORED] expected=[$STATE_T]" >>"$LOG"
		return 1
	fi
}

step_prepare
step_lab
step_stage
step_data_before_backup
step_backup
step_writes
step_archive
step_propose
step_restore
step_replay
step_compare
