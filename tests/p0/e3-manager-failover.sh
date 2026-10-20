#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# `A && conclude ... || conclude ...` is intended: conclude always returns 0.
# shellcheck disable=SC2015
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E3: three-manager metadata failover on three dbcm-lab VMs
# (ADR 0003). One `metad` test-harness process per VM (linux/arm64 binary
# cross-compiled on the host and copied into the VM) forms a Raft group over
# the Lima user-v2 network. The script plays the manager controller: it reads
# the leader's term, commits operation commands through the leader's loopback
# HTTP harness and sends steps to a LAB-ONLY simulated agent
# (tests/p0/e3agent, internal/fencing guard) on dbcm-lab-m3.
#
# Scenarios: (a) leader process killed mid-operation, (b) leader VM
# force-stopped, (c) leader VM partitioned with iptables inside the VM,
# (d) all three processes restarted from their data directories,
# (e) majority loss. An unrelated nginx sentinel container runs on m3 the
# whole time and must stay untouched.
#
# Usage (from anywhere):  tests/p0/e3-manager-failover.sh
#
# Environment (all optional, explicit, default off):
#   E3_KEEP_VMS=1   do not run the scoped cleanup at the end (development only;
#                   run `bin/lab cleanup -apply` yourself afterwards)
#   E3_REUSE_VMS=1  skip `bin/lab apply`; expects the VMs of a previous
#                   E3_KEEP_VMS=1 run and resets the E3 state inside them
#
# Disruptions are applied only inside dbcm-lab-* VMs: systemd units named
# dbcm-e3-*, files under /usr/local/bin/dbcm-e3-* and /var/lib/dbcm-e3,
# an iptables chain DBCM-E3 (plus a 600 s heal timer), `limactl stop --force`
# / `limactl start` of a lab VM. The host Docker daemon is never used.
#
# Output: raw sanitized log at tests/p0/results/e3-manager-failover.log.
# Requirements: Go toolchain, jq, python3, limactl on PATH.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG="$REPO_ROOT/tests/p0/results/e3-manager-failover.log"
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
: >"$LOG"
# shellcheck source=tests/p0/common.sh
. "$REPO_ROOT/tests/p0/common.sh"

VMS="dbcm-lab-m1 dbcm-lab-m2 dbcm-lab-m3"
KEEP=3 # node index that hosts the agent and the sentinel; never force-stopped
USERV2_PREFIX=192.168.104.
RAFT_PORT=7101
UNIT=dbcm-e3-metad
METAD=/usr/local/bin/dbcm-e3-metad
AGENT=/usr/local/bin/dbcm-e3agent
DATA=/var/lib/dbcm-e3/metad
AGENT_STATE=/var/lib/dbcm-e3/agent
CHAIN=DBCM-E3
SENTINEL=dbcm-e3-sentinel
SENTINEL_IMAGE=nginx:1.27-alpine
SENTINEL_PORT=18080
CONCLUSIONS=()
BUILD=$(mktemp -d "${TMPDIR:-/tmp}/dbcm-e3-build.XXXXXX")

finish() {
	local rc=$?
	trap - EXIT INT TERM
	{
		printf '\n## Conclusions recorded by the script\n\n'
		local c
		for c in "${CONCLUSIONS[@]+"${CONCLUSIONS[@]}"}"; do printf -- '- %s\n' "$c"; done
		printf '\n'
	} >>"$LOG"
	rm -rf "$BUILD"
	if [ "${E3_KEEP_VMS:-0}" = 1 ]; then
		say "E3_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		# Units, rules, timers and files live inside the lab VMs only and
		# disappear with them.
		lab_cleanup || rc=1
		rec "Lima user-v2 usernet daemons after cleanup (recorded, not touched)" \
			sh -c 'ps -axo pid,command | grep "[l]imactl usernet" | sed "s/ -p .*//" || echo none' || true
		rec "shared Lima user-v2 state directory after cleanup (recorded, not touched)" \
			sh -c 'ls "$HOME/.lima/_networks/user-v2" 2>/dev/null || echo absent' || true
	fi
	sanitize_log "$LOG"
	# The host build directory lives under /var/folders (not covered by
	# sanitize_log, which only knows /private/var/folders).
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" "$LOG"
	say "E3 finished with exit $rc (log: tests/p0/results/e3-manager-failover.log)"
	exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

conclude() { # conclude TEXT — record a result line (log and summary)
	CONCLUSIONS+=("$*")
	printf '**Result:** %s\n\n' "$*" >>"$LOG"
	say "RESULT: $*"
}

note() { printf '%s\n\n' "$*" >>"$LOG"; }
# hostload — record the host load average (unrelated host load skews timings;
# a failed run under load average ~800 is described in the results).
hostload() { note "host load average (1/5/15 min): $(sysctl -n vm.loadavg)"; }

# ---------------------------------------------------------------- helpers
vmn() { echo "dbcm-lab-m$1"; }
hport() { echo "1810$1"; } # metad HTTP port of node N (loopback inside mN)
now_ns() { python3 -c 'import time; print(time.time_ns())'; }
uv2_ip() { # uv2_ip VM — the VM's address on the Lima user-v2 network
	vm_args "$1" 'ip -4 -o addr show | awk "{print \$4}" | cut -d/ -f1 | grep -F "$1" | head -1' "$USERV2_PREFIX"
}

# vm_args VM SCRIPT ARGS... — run sh SCRIPT with positional ARGS inside VM.
vm_args() {
	require_lab_vm "$1"
	local vm=$1 script=$2
	shift 2
	limactl shell --workdir / "$vm" -- sh -c "$script" sh "$@"
}

st() { # st N [QUERY] — GET /state on node N (inside its VM)
	vm_exec "$(vmn "$1")" curl -s -m 8 "http://127.0.0.1:$(hport "$1")/state${2:-}"
}
st_rec() { # st_rec TITLE N [QUERY] — record /state with the scenario ops only
	local out
	out=$(st "$2" "${3:-}" 2>&1) || true
	printf '### %s\n\n```\n$ curl 127.0.0.1:%s/state%s   (inside %s)\n%s\n```\n\n' "$1" "$(hport "$2")" "${3:-}" "$(vmn "$2")" \
		"$(printf '%s' "$out" | jq -c '{id,leader,leader_id,term,barrier,unfinished:[.unfinished[]|select(.ID|startswith("op-"))|{ID,Owner,Steps:[.Steps[]|"\(.ID)=\(.State)/\(.Attempts)"]}]}' 2>/dev/null || printf '%s' "$out")" >>"$LOG"
}
leader_among() { # leader_among N... — print the node that reports leader=true
	local n
	for n in "$@"; do
		if st "$n" 2>/dev/null | jq -e '.leader == true' >/dev/null 2>&1; then
			echo "$n"
			return 0
		fi
	done
	return 1
}
has_leader() { leader_among "$@" >/dev/null; }
term_of() { st "$1" | jq -r .term; }
sig() { st "$1" | jq -cS '{leader_id, term, u: .unfinished}'; } # convergence signature
converged() { # converged N... — same leader, term and unfinished list everywhere
	local first="" n s
	for n in "$@"; do
		s=$(sig "$n" 2>/dev/null) || return 1
		[ -n "$s" ] && [ "$(printf '%s' "$s" | jq -r .leader_id)" != "" ] || return 1
		if [ -z "$first" ]; then first=$s; elif [ "$s" != "$first" ]; then return 1; fi
	done
}
opstate() { # opstate N OP — compact step states of OP from node N's /state?barrier=1
	st "$1" "?barrier=1" | jq -r --arg op "$2" '.unfinished[] | select(.ID==$op) | "owner=\(.Owner) " + ([.Steps[] | "\(.ID)=\(.State)/\(.Attempts)"] | join(" "))'
}

# post N JSON — POST /apply on node N; prints the body and " HTTP<code>".
post() {
	vm_exec "$(vmn "$1")" curl -s -m 15 -w ' HTTP%{http_code}' -X POST \
		-H 'Content-Type: application/json' --data-binary "$2" "http://127.0.0.1:$(hport "$1")/apply"
}
LAST_POST=""
# ctl LABEL N EXPECT JSON — controller command to node N; records it and
# returns 0 if the HTTP code equals EXPECT.
ctl() {
	local label=$1 n=$2 expect=$3 json=$4 out t0 t1
	t0=$(now_ns)
	out=$(post "$n" "$json" 2>&1) || true
	t1=$(now_ns)
	LAST_POST=$out
	printf '### %s\n\n```\n$ POST 127.0.0.1:%s/apply on m%s: %s\n%s\n(host-observed %s ms incl. limactl shell)\n```\n\n' \
		"$label" "$(hport "$n")" "$n" "$json" "$out" "$(((t1 - t0) / 1000000))" >>"$LOG"
	say "$label -> ${out##* }"
	case "$out" in *" HTTP$expect") return 0 ;; esac
	return 1
}
jbegin() { jq -nc --arg op "$1" --argjson t "$2" --argjson s "$3" '{Kind:"begin",OpID:$op,OpKind:"e3",Steps:$s,Term:$t}'; }
jcmd() { # jcmd KIND OP TERM [STEP [RESULT]]
	jq -nc --arg k "$1" --arg op "$2" --argjson t "$3" --arg s "${4:-}" --arg r "${5:-}" \
		'{Kind:$k,OpID:$op,Term:$t} + (if $s != "" then {StepID:$s} else {} end) + (if $r != "" then {Result:$r} else {} end)'
}

# agent ARGS... — run the lab agent on the KEEP VM; prints its line.
agent() { vm_exec "$(vmn $KEEP)" sudo "$AGENT" "$@" -state "$AGENT_STATE"; }
agent_rec() { # agent_rec LABEL EXPECT_EXIT ARGS...
	local label=$1 expect=$2 out rc=0
	shift 2
	out=$(agent "$@" 2>&1) || rc=$?
	printf '### agent: %s\n\n```\n$ dbcm-e3agent %s   (inside %s)\n%s\n(exit %s)\n```\n\n' "$label" "$*" "$(vmn $KEEP)" "$out" "$rc" >>"$LOG"
	say "agent $label: $out (exit $rc)"
	[ "$rc" = "$expect" ]
}

# run_step LABEL N TERM OP STEP — controller runs one step through node N:
# start (commit), agent exec, finish succeeded (commit).
run_step() {
	ctl "$1: start $5" "$2" 200 "$(jcmd start "$4" "$3" "$5")" &&
		agent_rec "$1: exec $5" 0 exec -term "$3" -op "$4" -step "$5" &&
		ctl "$1: finish $5" "$2" 200 "$(jcmd finish "$4" "$3" "$5" succeeded)"
}

start_metad() { # start_metad N — transient unit, same flags on every start
	local n=$1
	vm_sh "$(vmn "$n")" "sudo systemctl reset-failed $UNIT >/dev/null 2>&1; sudo mkdir -p $DATA && sudo systemd-run --quiet --unit=$UNIT --collect -p Restart=no \
		$METAD -id m$n -bind $(eval echo "\$IP$n"):$RAFT_PORT -data $DATA -http 127.0.0.1:$(hport "$n") -peers $PEERS -bootstrap && echo started m$n"
}
stop_metad() { vm_sh "$(vmn "$1")" "sudo systemctl stop $UNIT; echo \"m$1 unit: \$(systemctl is-active $UNIT || true)\""; }
kill_metad() { vm_sh "$(vmn "$1")" "date +%s%N; sudo systemctl kill -s KILL $UNIT"; }
metad_up() { st "$1" >/dev/null 2>&1; }

# Clock offsets (VM clock minus host clock, ns) estimated over a persistent
# limactl shell session: the sample with the smallest round trip wins.
clock_offsets() { # clock_offsets N... — sets OFF<N> and RTT<N>
	local n line
	for n in "$@"; do require_lab_vm "$(vmn "$n")"; done
	python3 - "$@" >"$BUILD/clk" <<'PY'
import subprocess, sys, time
for n in sys.argv[1:]:
    p = subprocess.Popen(["limactl", "shell", "--workdir", "/", "dbcm-lab-m" + n, "--",
                          "sh", "-c", "while read x; do date +%s%N; done"],
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
    best = None
    for _ in range(40):
        h0 = time.time_ns(); p.stdin.write("x\n"); p.stdin.flush()
        v = int(p.stdout.readline()); h1 = time.time_ns()
        if best is None or h1 - h0 < best[0]:
            best = (h1 - h0, v - (h0 + h1) // 2)
    p.stdin.close(); p.wait()
    print(n, best[1], best[0])
PY
	while read -r line; do
		# shellcheck disable=SC2086
		set -- $line
		eval "OFF$1=$2 RTT$1=$3"
		printf 'clock offset m%s: %s us (round trip %s us)\n' "$1" "$(($2 / 1000))" "$(($3 / 1000))" >>"$LOG"
	done <"$BUILD/clk"
}

# watch_bg N MODE OLD SECS — start the in-VM state watcher on node N in the
# background (host side); its output goes to $BUILD/watch-N.
WATCH_PIDS=""
watch_bg() {
	vm_exec "$(vmn "$1")" /usr/local/bin/dbcm-e3-watch "$(hport "$1")" "$2" "$3" "$4" >"$BUILD/watch-$1" 2>&1 &
	WATCH_PIDS="$WATCH_PIDS $!"
}
watch_wait() {
	local p
	for p in $WATCH_PIDS; do wait "$p" || true; done
	WATCH_PIDS=""
}
# watch_hit N — host time (ns) of the watcher's final (matching) line on N,
# or empty if the watcher timed out.
watch_hit() {
	local f="$BUILD/watch-$1" t off
	grep -q '^MATCH' "$f" || return 0
	t=$(grep -v '^MATCH' "$f" | tail -1 | cut -d' ' -f1)
	off=$(eval echo "\$OFF$1")
	echo $((t - off))
}
watch_rec() { # watch_rec TITLE N...
	local title=$1 n
	shift
	{
		printf '### %s\n\n```\n' "$title"
		for n in "$@"; do
			printf -- '-- watcher on m%s (VM clock ns, leader leader_id term):\n' "$n"
			cat "$BUILD/watch-$n"
		done
		printf '```\n\n'
	} >>"$LOG"
}
ms_between() { echo $((($2 - $1) / 1000000)); }

res_sample() { # res_sample TITLE SECS N... — metad CPU/RAM per VM, in parallel
	local title=$1 secs=$2 n pids="" p
	shift 2
	for n in "$@"; do
		vm_exec "$(vmn "$n")" sudo /usr/local/bin/dbcm-e3-res "$secs" >"$BUILD/res-$n" 2>&1 &
		pids="$pids $!"
	done
	for p in $pids; do wait "$p" || true; done
	{
		printf '### %s\n\n```\n' "$title"
		for n in "$@"; do printf 'm%s: %s\n' "$n" "$(cat "$BUILD/res-$n")"; done
		printf '```\n\n'
	} >>"$LOG"
}

sentinel_snapshot() { # container ID, image, start time, restarts, response hash
	vm_sh "$(vmn $KEEP)" "sudo docker inspect --format '{{.Id}} {{.Image}} {{.State.StartedAt}} restarts={{.RestartCount}} running={{.State.Running}}' $SENTINEL; curl -fsS http://127.0.0.1:$SENTINEL_PORT/ | sha256sum | cut -d' ' -f1" | tr '\n' ' '
}

ensure_leader_not_keep() { # move leadership off node KEEP (graceful restart)
	local l
	for _ in 1 2 3 4; do
		l=$(leader_among 1 2 3) || l=""
		if [ -n "$l" ] && [ "$l" != "$KEEP" ]; then
			LEADER=$l
			return 0
		fi
		say "leader is m$KEEP (or unknown); restarting its metad to move leadership"
		rec "stop metad on m$KEEP (SIGTERM) to move leadership" stop_metad $KEEP
		wait_until 30 "leader among m1/m2" has_leader 1 2 || true
		rec "start metad on m$KEEP" start_metad $KEEP
		wait_until 30 "m$KEEP converged" converged 1 2 3 || true
	done
	die "could not move leadership off m$KEEP"
}

# ---------------------------------------------------------------- step 0
step0_build() {
	section "Step 0: build (host)"
	rec "go version" go version
	(cd "$REPO_ROOT" && rec "build bin/lab" go build -o bin/lab ./cmd/lab)
	(cd "$REPO_ROOT" && rec "cross-compile metad linux/arm64" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BUILD/metad" ./cmd/metad)
	(cd "$REPO_ROOT" && rec "cross-compile e3agent linux/arm64 (lab only)" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BUILD/e3agent" ./tests/p0/e3agent)
	rec "binaries" sh -c "cd '$BUILD' && file metad e3agent | sed 's/, BuildID[^,]*//' && shasum -a 256 metad e3agent && ls -l metad e3agent | awk '{print \$5, \$9}'"
	(cd "$REPO_ROOT" && rec "git revision" git rev-parse --short HEAD)

	# In-VM helpers, copied as files (no quoting through limactl shell).
	cat >"$BUILD/watch.sh" <<'EOF'
#!/bin/sh
# dbcm-e3-watch PORT MODE OLD SECS — poll the local metad /state every 50 ms
# and print "<ns> <leader> <leader_id> <term>" on every change.
# MODE newleader: stop when a leader other than OLD is known.
# MODE stepdown:  stop when this node no longer reports leader=true.
port=$1 mode=$2 old=$3 secs=$4
end=$(( $(date +%s) + secs )); last=""
while [ "$(date +%s)" -lt "$end" ]; do
	s=$(curl -s -m 1 "http://127.0.0.1:$port/state" 2>/dev/null)
	t=$(date +%s%N)
	v=$(printf '%s' "$s" | sed -n 's/.*"leader":\([a-z]*\),"leader_id":"\([^"]*\)","leader_address":"[^"]*","term":\([0-9]*\).*/\1 id=\2 \3/p')
	# leader_id is printed as "id=<value>" so an empty ID keeps its field.
	[ -n "$v" ] || v="unreachable id= -"
	if [ "$v" != "$last" ]; then echo "$t $v"; last=$v; fi
	set -- $v
	case $mode in
	newleader) if [ "$2" != "id=" ] && [ "$2" != "id=$old" ]; then echo MATCH; exit 0; fi ;;
	stepdown) if [ "$1" = false ]; then echo MATCH; exit 0; fi ;;
	esac
	sleep 0.05
done
echo TIMEOUT
EOF
	cat >"$BUILD/lat.sh" <<'EOF'
#!/bin/sh
# dbcm-e3-lat PORT TERM COUNT PREFIX — COUNT operations, each begin + start +
# finish (3 sequential commits) on the local leader; prints commit latency.
port=$1 term=$2 n=$3 p=$4 i=0 f=/tmp/dbcm-e3-lat
: >$f
post() { curl -s -o /dev/null -w '%{http_code} %{time_total}\n' -X POST -H 'Content-Type: application/json' --data-binary "$1" "http://127.0.0.1:$port/apply" >>$f; }
start=$(date +%s%N)
while [ $i -lt "$n" ]; do
	post "{\"Kind\":\"begin\",\"OpID\":\"$p-$i\",\"OpKind\":\"e3-latency\",\"Steps\":[\"x\"],\"Term\":$term}"
	post "{\"Kind\":\"start\",\"OpID\":\"$p-$i\",\"StepID\":\"x\",\"Term\":$term}"
	post "{\"Kind\":\"finish\",\"OpID\":\"$p-$i\",\"StepID\":\"x\",\"Result\":\"succeeded\",\"Term\":$term}"
	i=$((i+1))
done
end=$(date +%s%N)
awk '{c[$1]++} END{for (k in c) printf "http %s: %d\n", k, c[k]}' $f
awk '$1==200{print $2*1000}' $f | sort -n | awk -v wall=$(( (end-start)/1000000 )) '{a[NR]=$1} END{ if (NR==0) {print "no successful commits"; exit}
	printf "commits=%d wall=%dms throughput=%.0f/s min=%.2fms p50=%.2fms p90=%.2fms p99=%.2fms max=%.2fms (curl time_total, loopback, sequential)\n", NR, wall, NR*1000/wall, a[1], a[int(NR*0.5+0.5)], a[int(NR*0.9+0.5)], a[int(NR*0.99+0.5)], a[NR]}'
EOF
	cat >"$BUILD/res.sh" <<'EOF'
#!/bin/sh
# dbcm-e3-res SECS — CPU and memory of the metad process over SECS seconds.
pid=$(systemctl show -p MainPID --value dbcm-e3-metad)
[ -n "$pid" ] && [ "$pid" -gt 0 ] || { echo "metad not running"; exit 0; }
hz=$(getconf CLK_TCK)
t0=$(awk '{print $14+$15}' /proc/$pid/stat); s0=$(date +%s%N)
sleep "$1"
t1=$(awk '{print $14+$15}' /proc/$pid/stat); s1=$(date +%s%N)
awk -v pid=$pid -v d=$((t1-t0)) -v tot=$t1 -v hz=$hz -v ns=$((s1-s0)) -v secs=$1 \
	-v rss="$(awk '/^VmRSS/{print $2}' /proc/$pid/status)" -v hwm="$(awk '/^VmHWM/{print $2}' /proc/$pid/status)" \
	-v thr="$(awk '/^Threads/{print $2}' /proc/$pid/status)" -v data="$(du -sk /var/lib/dbcm-e3/metad | cut -f1)" \
	'BEGIN{printf "pid=%s cpu=%.2f%% of one vCPU over %ss, cpu_total=%.2fs, rss=%.1fMiB, peak_rss=%.1fMiB, threads=%s, data_dir=%dKiB\n", pid, 100*d/hz/(ns/1e9), secs, tot/hz, rss/1024, hwm/1024, thr, data}'
EOF
}

# ---------------------------------------------------------------- step 1
step1_vms() {
	section "Step 1: lab VMs"
	rec "host free disk before apply" host_free_disk
	(cd "$REPO_ROOT" && rec "bin/lab status before" "$LAB" status)
	rec "shared Lima user-v2 state before (recorded, not touched)" \
		sh -c 'ls "$HOME/.lima/_networks/user-v2" 2>/dev/null || echo absent; ps -axo pid,command | grep "[l]imactl usernet" | sed "s/ -p .*//" || echo "no usernet daemon"'
	(cd "$REPO_ROOT" && rec "bin/lab apply -apply -vms 3 -cpus 2 -ram 2 -disk 10 -network lima:user-v2" \
		"$LAB" apply -apply -vms 3 -cpus 2 -ram 2 -disk 10 -network lima:user-v2)
	rec "host free disk after apply" host_free_disk
}

reset_vms() {
	section "Reset previous E3 state (E3_REUSE_VMS)"
	local vm
	for vm in $VMS; do
		rec "reset $vm" vm_sh "$vm" "sudo systemctl stop $UNIT dbcm-e3-heal.timer 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null
			sudo iptables -D INPUT -j $CHAIN 2>/dev/null; sudo iptables -D OUTPUT -j $CHAIN 2>/dev/null; sudo iptables -F $CHAIN 2>/dev/null; sudo iptables -X $CHAIN 2>/dev/null
			sudo docker rm -f $SENTINEL >/dev/null 2>&1; sudo rm -rf /var/lib/dbcm-e3; echo reset"
	done
}

PEERS=""
IP1="" IP2="" IP3=""
step1b_guests() {
	section "Step 1b: guest facts and binaries"
	rec "limactl list" limactl list
	local n vm
	for n in 1 2 3; do
		vm=$(vmn "$n")
		rec "facts $vm" vm_sh "$vm" '
			echo "hostname: $(hostname)"; echo "arch: $(uname -m) kernel: $(uname -r)"
			grep PRETTY_NAME /etc/os-release; echo "nproc: $(nproc)"; free -m | head -2; df -h / | tail -1
			ip -4 -o addr show | awk "{print \$2, \$4}"
			echo "time sync: $(timedatectl show -p NTPSynchronized --value 2>/dev/null) $(systemctl is-active chrony systemd-timesyncd 2>/dev/null | tr "\n" " ")"
			echo "iptables: $(sudo iptables -V)"; echo "curl: $(curl -V | head -1)"'
		eval "IP$n=$(uv2_ip "$vm")"
		[ -n "$(eval echo "\$IP$n")" ] || die "$vm has no user-v2 address ($USERV2_PREFIX*)"
	done
	say "user-v2 addresses: m1=$IP1 m2=$IP2 m3=$IP3"
	PEERS="m1=$IP1:$RAFT_PORT,m2=$IP2:$RAFT_PORT,m3=$IP3:$RAFT_PORT"
	note "Raft peers: \`$PEERS\`"
	rec "VM-to-VM reachability over user-v2 (from m1)" vm_sh dbcm-lab-m1 "for a in $IP2 $IP3; do ping -c2 -W2 \$a | tail -1; done"
	for n in 1 2 3; do
		vm=$(vmn "$n")
		rec "copy binaries into $vm" sh -c "limactl copy '$BUILD/metad' '$vm:/tmp/dbcm-e3-metad' && limactl copy '$BUILD/e3agent' '$vm:/tmp/dbcm-e3agent' && limactl copy '$BUILD/watch.sh' '$vm:/tmp/dbcm-e3-watch' && limactl copy '$BUILD/lat.sh' '$vm:/tmp/dbcm-e3-lat' && limactl copy '$BUILD/res.sh' '$vm:/tmp/dbcm-e3-res'"
		rec "install binaries in $vm" vm_sh "$vm" "for f in dbcm-e3-metad dbcm-e3agent dbcm-e3-watch dbcm-e3-lat dbcm-e3-res; do sudo install -m 0755 /tmp/\$f /usr/local/bin/\$f; done; sha256sum $METAD $AGENT | cut -c1-16"
	done
	rec "host loopback listeners on metad/sentinel ports before start" sh -c "lsof -nP -iTCP:18101-18103 -iTCP:$SENTINEL_PORT -sTCP:LISTEN || echo none"
}

# ---------------------------------------------------------------- step 2
LEADER=""
step2_cluster() {
	section "Step 2: three metad processes form a Raft group"
	hostload
	local n
	for n in 1 2 3; do rec "start metad on m$n" start_metad "$n"; done
	wait_until 60 "a leader is elected" has_leader 1 2 3 || die "no leader"
	note "initial election: $(cat "$WAIT_ELAPSED_FILE") s (2 s polling resolution, includes process start)"
	wait_until 30 "all nodes agree on leader and term" converged 1 2 3 || die "no convergence"
	LEADER=$(leader_among 1 2 3)
	for n in 1 2 3; do st_rec "state m$n" "$n"; done
	conclude "initial cluster: leader m$LEADER, term $(term_of "$LEADER"), all three nodes agree"
	rec "metad unit and listeners (m1)" vm_sh dbcm-lab-m1 "systemctl status --no-pager $UNIT | head -8; sudo ss -ltnp | grep -E 'dbcm-e3|:$RAFT_PORT|:1810' || true"
	rec "follower rejects writes (m$(((LEADER % 3) + 1)))" post $(((LEADER % 3) + 1)) "$(jbegin op-probe 1 '["x"]')" || true
	rec "host loopback listeners on metad ports while running (Lima port forwarding)" sh -c "lsof -nP -iTCP:18101-18103 -sTCP:LISTEN || echo none"
}

step2b_sentinel() {
	section "Step 2b: unrelated sentinel (nginx on m$KEEP) and agent"
	rec "enable rootful docker on m$KEEP (template default is rootless)" vm_sh "$(vmn $KEEP)" '
		sudo systemctl unmask containerd.service docker.service docker.socket >/dev/null 2>&1
		sudo systemctl enable --now containerd.service docker.socket docker.service >/dev/null 2>&1
		echo "rootful docker: $(systemctl is-active docker.service)"; sudo docker version --format "server {{.Server.Version}}"'
	rec "start sentinel container" vm_sh "$(vmn $KEEP)" "sudo docker run -d --name $SENTINEL --label dbcm.test=sentinel --restart=no -p 127.0.0.1:$SENTINEL_PORT:80 $SENTINEL_IMAGE"
	wait_until 120 "sentinel answers" vm_sh "$(vmn $KEEP)" "curl -fsS http://127.0.0.1:$SENTINEL_PORT/ >/dev/null"
	rec "sentinel image" vm_sh "$(vmn $KEEP)" "sudo docker image inspect --format '{{.RepoTags}} {{.RepoDigests}} {{.Architecture}}' $SENTINEL_IMAGE"
	SENTINEL_BEFORE=$(sentinel_snapshot)
	printf '### sentinel snapshot before\n\n```\n%s\n```\n\n' "$SENTINEL_BEFORE" >>"$LOG"
	agent_rec "initial highest term" 0 highest
}

# ---------------------------------------------------------------- step 3
step3_measure() {
	section "Step 3: commit latency and resource use (3 nodes)"
	hostload
	LEADER=$(leader_among 1 2 3)
	res_sample "metad idle (30 s window)" 30 1 2 3
	local t
	t=$(term_of "$LEADER")
	vm_exec "$(vmn "$LEADER")" /usr/local/bin/dbcm-e3-lat "$(hport "$LEADER")" "$t" 100 lat3 >"$BUILD/lat" 2>&1 &
	local lp=$!
	res_sample "metad during the latency run (10 s window)" 10 1 2 3
	wait "$lp" || true
	printf '### commit latency, 3 nodes, leader m%s term %s\n\n```\n%s\n```\n\n' "$LEADER" "$t" "$(cat "$BUILD/lat")" >>"$LOG"
	conclude "commit latency 3 nodes: $(tail -1 "$BUILD/lat")"
	rec "VM memory after load" sh -c "for n in 1 2 3; do limactl shell --workdir / dbcm-lab-m\$n -- sh -c 'echo \$(hostname): \$(free -m | awk \"/Mem/{print \\\$3\\\" MiB used of \\\"\\\$2}\")'; done"
}

# ---------------------------------------------------------------- step 4 (a)
step4_kill_process() {
	section "Step 4 (a): leader process killed (SIGKILL) mid-operation"
	hostload
	local L T survivors n L2 T2 tkill hit best=""
	L=$(leader_among 1 2 3)
	T=$(term_of "$L")
	survivors=$(for n in 1 2 3; do [ "$n" = "$L" ] || printf '%s ' "$n"; done)
	note "leader m$L, term $T; survivors: $survivors"
	ctl "a: begin op-a" "$L" 200 "$(jbegin op-a "$T" '["s1","s2","s3","hold"]')"
	run_step a "$L" "$T" op-a s1
	ctl "a: start s2 (then the leader dies before the agent call)" "$L" 200 "$(jcmd start op-a "$T" s2)"
	# shellcheck disable=SC2086
	clock_offsets $L $survivors
	for n in $survivors; do watch_bg "$n" newleader "m$L" 30; done
	sleep 1.5
	tkill=$(kill_metad "$L" | head -1)
	tkill=$((tkill - $(eval echo "\$OFF$L")))
	watch_wait
	# shellcheck disable=SC2086
	watch_rec "survivor watchers after SIGKILL of m$L" $survivors
	for n in $survivors; do
		hit=$(watch_hit "$n")
		[ -n "$hit" ] || continue
		if [ -z "$best" ] || [ "$hit" -lt "$best" ]; then best=$hit; fi
	done
	[ -n "$best" ] || die "no new leader observed"
	ELECT_A=$(ms_between "$tkill" "$best")
	# shellcheck disable=SC2086
	L2=$(leader_among $survivors)
	T2=$(term_of "$L2")
	conclude "(a) SIGKILL of leader m$L (term $T): new leader m$L2 term $T2 known on a survivor ${ELECT_A} ms after the kill (VM clocks corrected by measured offsets)"
	st_rec "new leader journal after barrier (before adopt)" "$L2" "?barrier=1"
	ctl "a: new leader adopts op-a" "$L2" 200 "$(jcmd adopt op-a "$T2")"
	local s
	s=$(opstate "$L2" op-a)
	note "op-a after adopt: \`$s\`"
	case "$s" in *"s1=succeeded/1 s2=unknown/1 s3=pending/0"*) conclude "(a) after adopt the interrupted step is unknown: $s" ;; *) conclude "(a) UNEXPECTED op-a state after adopt: $s" ;; esac
	agent_rec "a: reconcile s2 (query live state)" 0 query -op op-a -step s2
	run_step "a (retry by m$L2)" "$L2" "$T2" op-a s2
	run_step "a" "$L2" "$T2" op-a s3
	s=$(opstate "$L2" op-a)
	note "op-a after retry: \`$s\`"
	case "$s" in *"s1=succeeded/1 s2=succeeded/2 s3=succeeded/1 hold=pending/0"*) conclude "(a) s2 retried by the new leader (attempt 2) and the operation continued: $s" ;; *) conclude "(a) UNEXPECTED op-a state: $s" ;; esac
	ctl "a: old term $T rejected by the journal" "$L2" 409 "$(jcmd start op-a "$T" hold)" &&
		conclude "(a) command with the old term $T on the new leader: 409 not_owner" || conclude "(a) UNEXPECTED: old-term journal command answered ${LAST_POST##* }"
	agent_rec "a: old term $T rejected by the agent" 3 exec -term "$T" -op op-a -step hold &&
		conclude "(a) agent rejected old term $T after admitting $T2" || conclude "(a) UNEXPECTED: agent admitted the old term"
	rec "restart killed metad on m$L" start_metad "$L"
	wait_until 60 "m$L converged after restart" converged 1 2 3 &&
		conclude "(a) restarted m$L converged (same leader, term and unfinished journal after $(cat "$WAIT_ELAPSED_FILE") s)" ||
		conclude "(a) m$L did NOT converge"
	st_rec "m$L after restart" "$L"
}

# ---------------------------------------------------------------- step 5 (b)
start_vm() { # start_vm VM — like E2: tolerate a non-zero exit of a running VM
	local rc=0
	rec "limactl start $1" limactl start --tty=false --timeout 6m "$1" || rc=$?
	if [ "$rc" != 0 ]; then
		vm_sh "$1" true || die "$1 not reachable after limactl start"
		conclude "limactl start of $1 exited $rc, but the VM is reachable"
	fi
}

step5_vm_stop() {
	section "Step 5 (b): leader VM force-stopped mid-operation"
	hostload
	ensure_leader_not_keep
	local L T survivors n L2 T2 tstop hit best="" s ip
	L=$LEADER
	T=$(term_of "$L")
	survivors=$(for n in 1 2 3; do [ "$n" = "$L" ] || printf '%s ' "$n"; done)
	note "leader m$L, term $T; survivors: $survivors"
	ctl "b: begin op-b" "$L" 200 "$(jbegin op-b "$T" '["s1","s2","hold"]')"
	run_step b "$L" "$T" op-b s1
	ctl "b: start s2" "$L" 200 "$(jcmd start op-b "$T" s2)"
	agent_rec "b: exec s2 (the finish never gets committed)" 0 exec -term "$T" -op op-b -step s2
	# shellcheck disable=SC2086
	clock_offsets $survivors
	for n in $survivors; do watch_bg "$n" newleader "m$L" 40; done
	sleep 1.5
	tstop=$(now_ns)
	rec "limactl stop --force $(vmn "$L")" limactl stop --force "$(vmn "$L")"
	watch_wait
	# shellcheck disable=SC2086
	watch_rec "survivor watchers after forced stop of $(vmn "$L")" $survivors
	for n in $survivors; do
		hit=$(watch_hit "$n")
		[ -n "$hit" ] || continue
		if [ -z "$best" ] || [ "$hit" -lt "$best" ]; then best=$hit; fi
	done
	[ -n "$best" ] || die "no new leader observed"
	ELECT_B=$(ms_between "$tstop" "$best")
	# shellcheck disable=SC2086
	L2=$(leader_among $survivors)
	T2=$(term_of "$L2")
	conclude "(b) forced stop of $(vmn "$L") (leader, term $T): new leader m$L2 term $T2 known ${ELECT_B} ms after the stop command was issued (host clock)"
	ctl "b: new leader adopts op-b" "$L2" 200 "$(jcmd adopt op-b "$T2")"
	s=$(opstate "$L2" op-b)
	note "op-b after adopt: \`$s\`"
	case "$s" in *"s1=succeeded/1 s2=unknown/1 hold=pending/0"*) conclude "(b) after adopt the interrupted step is unknown: $s" ;; *) conclude "(b) UNEXPECTED op-b state after adopt: $s" ;; esac
	agent_rec "b: reconcile s2 (query live state)" 0 query -op op-b -step s2
	ctl "b (retry by m$L2): start s2" "$L2" 200 "$(jcmd start op-b "$T2" s2)"
	agent_rec "b (retry by m$L2): exec s2 is idempotent" 0 exec -term "$T2" -op op-b -step s2
	case "$(agent query -op op-b -step s2)" in *"term=$T"*) conclude "(b) s2 executed once (under term $T); the retry under term $T2 did not execute it again" ;; *) conclude "(b) UNEXPECTED agent record for s2" ;; esac
	ctl "b (retry by m$L2): finish s2" "$L2" 200 "$(jcmd finish op-b "$T2" s2 succeeded)"
	s=$(opstate "$L2" op-b)
	note "op-b after retry: \`$s\`"
	agent_rec "b: old term $T rejected by the agent" 3 exec -term "$T" -op op-b -step hold &&
		conclude "(b) agent rejected old term $T" || conclude "(b) UNEXPECTED: agent admitted the old term"
	start_vm "$(vmn "$L")"
	ip=$(uv2_ip "$(vmn "$L")")
	[ "$ip" = "$(eval echo "\$IP$L")" ] && note "user-v2 address of $(vmn "$L") unchanged after restart: $ip" ||
		conclude "(b) user-v2 address of $(vmn "$L") CHANGED after restart: $ip (Raft membership stores the old one)"
	rec "metad unit after VM restart (transient unit, not started at boot)" vm_sh "$(vmn "$L")" "systemctl is-active $UNIT || true; sudo ls -la $DATA"
	rec "start metad on m$L after VM restart" start_metad "$L"
	wait_until 60 "m$L converged after VM restart" converged 1 2 3 &&
		conclude "(b) m$L restarted from its data dir after the forced stop and converged ($(cat "$WAIT_ELAPSED_FILE") s)" ||
		conclude "(b) m$L did NOT converge"
	st_rec "m$L after VM restart" "$L"
}

# ---------------------------------------------------------------- step 6 (c)
step6_partition() {
	section "Step 6 (c): leader VM partitioned (iptables inside the VM)"
	hostload
	ensure_leader_not_keep
	local L T majority n L2 T2 hit best="" tpart s a1 a2 vmL out sshc
	L=$LEADER
	vmL=$(vmn "$L")
	T=$(term_of "$L")
	majority=$(for n in 1 2 3; do [ "$n" = "$L" ] || printf '%s ' "$n"; done)
	note "leader m$L, term $T; majority: $majority"
	ctl "c: begin op-c" "$L" 200 "$(jbegin op-c "$T" '["s1","s2","s3"]')"
	run_step c "$L" "$T" op-c s1
	# shellcheck disable=SC2086
	set -- $majority
	a1=$(eval echo "\$IP$1")
	a2=$(eval echo "\$IP$2")
	sshc=$(vm_sh "$vmL" 'echo "$SSH_CONNECTION"')
	note "SSH_CONNECTION inside $vmL: \`$sshc\`"
	case "$sshc" in *"$a1"* | *"$a2"*) die "limactl shell reaches $vmL via a peer address; refusing to partition" ;; esac
	rec "arm 600 s heal timer inside $vmL" vm_sh "$vmL" "
		printf '%s\n' 'iptables -D INPUT -j $CHAIN 2>/dev/null' 'iptables -D OUTPUT -j $CHAIN 2>/dev/null' \
			'iptables -F $CHAIN 2>/dev/null' 'iptables -X $CHAIN 2>/dev/null' 'exit 0' | sudo tee /run/dbcm-e3-heal.sh >/dev/null
		sudo systemd-run --quiet --unit dbcm-e3-heal --on-active=600 /bin/sh /run/dbcm-e3-heal.sh && echo armed"
	# shellcheck disable=SC2086
	clock_offsets $L $majority
	for n in $majority; do watch_bg "$n" newleader "m$L" 40; done
	watch_bg "$L" stepdown "-" 40
	sleep 1.5
	out=$(vm_sh "$vmL" "date +%s%N; sudo iptables -N $CHAIN && for a in $a1 $a2; do sudo iptables -A $CHAIN -s \$a -j DROP; sudo iptables -A $CHAIN -d \$a -j DROP; done && sudo iptables -I INPUT 1 -j $CHAIN && sudo iptables -I OUTPUT 1 -j $CHAIN && sudo iptables -S | grep $CHAIN")
	tpart=$(printf '%s\n' "$out" | head -1)
	tpart=$((tpart - $(eval echo "\$OFF$L")))
	printf '### partition rules inside %s\n\n```\n%s\n```\n\n' "$vmL" "$(printf '%s\n' "$out" | tail -n +2)" >>"$LOG"
	# The old leader's controller tries to commit the next step with its term.
	ctl "c: isolated old leader tries to commit start s2 (term $T)" "$L" 504 "$(jcmd start op-c "$T" s2)" ||
		note "isolated write answered ${LAST_POST##* } (504 outcome_unknown expected while it still believed it was leader)"
	watch_wait
	# shellcheck disable=SC2086
	watch_rec "watchers after the partition (majority: newleader, m$L: stepdown)" $majority $L
	for n in $majority; do
		hit=$(watch_hit "$n")
		[ -n "$hit" ] || continue
		if [ -z "$best" ] || [ "$hit" -lt "$best" ]; then best=$hit; fi
	done
	[ -n "$best" ] || die "majority elected no leader"
	ELECT_C=$(ms_between "$tpart" "$best")
	hit=$(watch_hit "$L")
	[ -n "$hit" ] && STEPDOWN_C=$(ms_between "$tpart" "$hit") || STEPDOWN_C="not observed"
	# shellcheck disable=SC2086
	L2=$(leader_among $majority)
	T2=$(term_of "$L2")
	conclude "(c) partition of $vmL: majority leader m$L2 term $T2 after ${ELECT_C} ms; isolated m$L stopped reporting leader after ${STEPDOWN_C} ms"
	ctl "c: isolated old leader retries start s2 (term $T)" "$L" 503 "$(jcmd start op-c "$T" s2)" &&
		conclude "(c) isolated old leader refuses commits: ${LAST_POST}" || conclude "(c) isolated write answered: ${LAST_POST}"
	ctl "c: new leader adopts op-c" "$L2" 200 "$(jcmd adopt op-c "$T2")"
	run_step "c (majority m$L2)" "$L2" "$T2" op-c s2
	agent_rec "c: isolated old leader's command (term $T) reaches the agent" 3 exec -term "$T" -op op-c -step s3 &&
		conclude "(c) agent rejected the isolated old leader's command with term $T (highest $T2)" ||
		conclude "(c) UNEXPECTED: agent admitted the isolated old leader's term $T"
	vm_exec "$(vmn "$L2")" /usr/local/bin/dbcm-e3-lat "$(hport "$L2")" "$T2" 50 lat2 >"$BUILD/lat" 2>&1 || true
	printf '### commit latency, 2 of 3 nodes (m%s isolated), leader m%s\n\n```\n%s\n```\n\n' "$L" "$L2" "$(cat "$BUILD/lat")" >>"$LOG"
	conclude "(c) majority keeps committing: $(tail -1 "$BUILD/lat")"
	st_rec "isolated m$L (local view, no barrier)" "$L"
	local tl
	tl=$(term_of "$L")
	note "term of the isolated node after ~$((($(now_ns) - tpart) / 1000000000)) s: $tl (pre-vote keeps it from inflating the term?)"
	conclude "(c) isolated node term while partitioned: $tl (was $T, majority $T2)"
	rec "isolated node log (last raft lines)" vm_sh "$vmL" "sudo journalctl -u $UNIT --no-pager -o cat | grep -iE 'heartbeat timeout|candidate|pre-vote|entering|failed to contact|stepping down|lost leadership|leadership' | tail -15"
	rec "heal partition inside $vmL" vm_sh "$vmL" "sudo sh /run/dbcm-e3-heal.sh; sudo systemctl stop dbcm-e3-heal.timer 2>/dev/null; echo remaining $CHAIN rules: \$(sudo iptables -S | grep -c $CHAIN || true)"
	wait_until 60 "old leader converged after heal" converged 1 2 3 &&
		conclude "(c) healed m$L converged to leader m$L2, term $(term_of "$L2") ($(cat "$WAIT_ELAPSED_FILE") s)" ||
		conclude "(c) m$L did NOT converge after heal"
	s=$(opstate "$L2" op-c)
	note "op-c after heal: \`$s\`"
	case "$s" in *"s1=succeeded/1 s2=succeeded/1 s3=pending/0"*) conclude "(c) the isolated start of s2 never committed (s2 attempts 1, by the majority): $s" ;; *) conclude "(c) UNEXPECTED op-c state: $s" ;; esac
	st_rec "m$L after heal" "$L"
}

# ---------------------------------------------------------------- step 7 (d)
step7_restart_all() {
	section "Step 7 (d): all three processes restarted from their data directories"
	hostload
	local L n before after tb ok=1
	say "capturing state before the full restart"
	L=$(leader_among 1 2 3)
	before=$(st "$L" "?barrier=1" | jq -cS .unfinished)
	for n in 1 2 3; do eval "TB$n=$(term_of "$n")"; done
	note "before: leader m$L, terms m1=$TB1 m2=$TB2 m3=$TB3, unfinished sha256 $(printf '%s' "$before" | shasum -a 256 | cut -c1-16), $(printf '%s' "$before" | jq length) ops"
	say "state captured"
	for n in 1 2 3; do rec "stop metad on m$n (SIGTERM)" stop_metad "$n"; done
	for n in 1 2 3; do rec "start metad on m$n" start_metad "$n"; done
	for n in 1 2 3; do
		wait_until 20 "m$n answers" metad_up "$n" || true
		tb=$(eval echo "\$TB$n")
		eval "TA$n=$(term_of "$n")"
		[ "$(eval echo "\$TA$n")" -ge "$tb" ] || ok=0
	done
	note "terms read right after restart: m1=$TA1 m2=$TA2 m3=$TA3"
	[ $ok = 1 ] && conclude "(d) after restarting all three, no term decreased (m1 $TB1->$TA1, m2 $TB2->$TA2, m3 $TB3->$TA3)" || conclude "(d) a TERM DECREASED after restart"
	wait_until 60 "leader after full restart" has_leader 1 2 3 || die "no leader after full restart"
	note "election after full restart: $(cat "$WAIT_ELAPSED_FILE") s (2 s polling)"
	L=$(leader_among 1 2 3)
	after=$(st "$L" "?barrier=1" | jq -cS .unfinished)
	[ "$before" = "$after" ] && conclude "(d) journal restored: the leader's unfinished operations after the barrier are identical ($(printf '%s' "$after" | jq length) ops, leader m$L term $(term_of "$L"))" ||
		conclude "(d) JOURNAL DIFFERS after restart"
	wait_until 30 "all converged after full restart" converged 1 2 3 || true
	st_rec "leader after full restart" "$L"
	rec "data directories" sh -c "for n in 1 2 3; do limactl shell --workdir / dbcm-lab-m\$n -- sudo sh -c 'echo \$(hostname): \$(du -sh $DATA | cut -f1); ls -la $DATA $DATA/snapshots | grep -v total'; done"
	res_sample "metad after full restart (10 s window)" 10 1 2 3
}

# ---------------------------------------------------------------- step 8 (e)
step8_majority_loss() {
	section "Step 8 (e): majority loss (two VMs force-stopped)"
	hostload
	local n L T s
	L=$(leader_among 1 2 3)
	T=$(term_of "$L")
	note "leader before: m$L term $T; survivor: m$KEEP"
	for n in 1 2 3; do
		[ "$n" = "$KEEP" ] && continue
		rec "limactl stop --force $(vmn "$n")" limactl stop --force "$(vmn "$n")"
	done
	sleep 5
	st_rec "survivor m$KEEP after majority loss (local read)" "$KEEP"
	ctl "e: survivor refuses a new operation" "$KEEP" 503 "$(jbegin op-e "$(term_of $KEEP)" '["x"]')" ||
		[ "${LAST_POST##* }" = HTTP504 ] || true
	conclude "(e) write on the survivor after majority loss: ${LAST_POST}"
	sleep 5
	ctl "e: survivor refuses again (10 s after loss)" "$KEEP" 503 "$(jbegin op-e "$(term_of $KEEP)" '["x"]')" || true
	conclude "(e) second write attempt: ${LAST_POST##* }"
	s=$(st "$KEEP" | jq -c '{leader, leader_id, term, ops: (.unfinished | length)}')
	conclude "(e) local reads still work on the survivor: $s"
	rec "barrier read on the survivor (expected 503)" vm_exec "$(vmn $KEEP)" curl -s -m 10 -w ' HTTP%{http_code}' "http://127.0.0.1:$(hport $KEEP)/state?barrier=1" || true
	agent_rec "e: agent state readable" 0 highest
	rec "survivor raft log (last lines)" vm_sh "$(vmn $KEEP)" "sudo journalctl -u $UNIT --no-pager -o cat | grep -iE 'candidate|pre-vote|entering|failed to contact|election' | tail -8"
	res_sample "survivor metad without quorum (10 s window)" 10 "$KEEP"
}

# ---------------------------------------------------------------- step 9
step9_sentinel() {
	section "Step 9: sentinel comparison"
	local after
	after=$(sentinel_snapshot)
	printf '### sentinel snapshot after\n\n```\nbefore: %s\nafter:  %s\n```\n\n' "$SENTINEL_BEFORE" "$after" >>"$LOG"
	if [ "$after" = "$SENTINEL_BEFORE" ]; then
		conclude "sentinel unchanged (container, image, start time, restarts, response hash)"
	else
		conclude "sentinel CHANGED"
		return 1
	fi
	rec "unrelated entries before cleanup" lab_status_unrelated
}

step0_build
if [ "${E3_REUSE_VMS:-0}" = 1 ]; then
	reset_vms
else
	step1_vms
fi
step1b_guests
step2_cluster
step2b_sentinel
step3_measure
step4_kill_process
step5_vm_stop
step6_partition
step7_restart_all
step8_majority_loss
step9_sentinel
