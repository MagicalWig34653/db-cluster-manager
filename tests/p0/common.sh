#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Shared helpers for the P0 lab experiments (tests/p0/*.sh).
#
# Rules enforced here:
# - Only Lima VMs named dbcm-lab-* are addressed; every docker command runs
#   inside such a VM via `limactl shell` (the host Docker daemon and host
#   Docker contexts are never used).
# - VMs are created and removed only through bin/lab (apply/cleanup).
# - Every wait loop has an explicit deadline.
#
# Source this file from bash; it expects REPO_ROOT and LOG to be set by the
# caller (LOG: raw run log, sanitized by sanitize_log before it is kept).

: "${REPO_ROOT:?REPO_ROOT must be set}"
: "${LOG:?LOG must be set}"

LAB="$REPO_ROOT/bin/lab"
LAB_PREFIX="dbcm-lab-"

# say MESSAGE... — print a progress line to stdout and the log.
say() {
	printf '%s %s\n' "$(date -u +%H:%M:%SZ)" "$*" | tee -a "$LOG"
}

# section TITLE — start a log section.
section() {
	printf '\n## %s\n\n' "$*" | tee -a "$LOG"
}

die() {
	say "FATAL: $*"
	exit 1
}

# require_lab_vm NAME — refuse anything that is not a lab VM name.
require_lab_vm() {
	case "$1" in
	"$LAB_PREFIX"[a-z0-9]*) ;;
	*) die "refusing to address non-lab VM '$1'" ;;
	esac
}

# vm_exec NAME CMD... — run CMD (argv) inside lab VM NAME.
vm_exec() {
	require_lab_vm "$1"
	local vm=$1
	shift
	limactl shell --workdir / "$vm" -- "$@"
}

# vm_sh NAME SCRIPT — run a POSIX sh script string inside lab VM NAME.
vm_sh() {
	require_lab_vm "$1"
	limactl shell --workdir / "$1" -- sh -c "$2"
}

# vm_docker NAME ARGS... — rootful docker inside lab VM NAME.
vm_docker() {
	require_lab_vm "$1"
	local vm=$1
	shift
	limactl shell --workdir / "$vm" -- sudo docker "$@"
}

# rec TITLE CMD... — run CMD, record "$ CMD" and its output in the log.
# The command's exit status is returned.
rec() {
	local title=$1
	shift
	{
		printf '### %s\n\n```\n$ %s\n' "$title" "$*"
	} >>"$LOG"
	local rc=0
	"$@" >>"$LOG" 2>&1 || rc=$?
	printf '(exit %s)\n```\n\n' "$rc" >>"$LOG"
	printf '%s %s (exit %s)\n' "$(date -u +%H:%M:%SZ)" "$title" "$rc"
	return "$rc"
}

# wait_until DEADLINE_SECONDS DESCRIPTION CMD... — poll CMD every 2 s until it
# succeeds or the deadline passes. Prints the elapsed seconds on success
# (to stdout) and returns 1 on timeout.
wait_until() {
	local deadline=$1 desc=$2
	shift 2
	local start now
	start=$(date +%s)
	while :; do
		if "$@" >/dev/null 2>&1; then
			now=$(date +%s)
			say "wait ok after $((now - start))s: $desc"
			echo "$((now - start))" >"$WAIT_ELAPSED_FILE"
			return 0
		fi
		now=$(date +%s)
		if [ $((now - start)) -ge "$deadline" ]; then
			say "wait TIMEOUT after ${deadline}s: $desc"
			return 1
		fi
		sleep 2
	done
}
WAIT_ELAPSED_FILE="${WAIT_ELAPSED_FILE:-$(mktemp "${TMPDIR:-/tmp}/dbcm-p0-wait.XXXXXX")}"

# host_free_disk — free space of the filesystem holding the Lima VM disks.
host_free_disk() {
	df -h "$HOME/.lima" | tail -1 | awk '{print "host free disk (" $NF "): " $4 " of " $2}'
}

# lab_status_unrelated — print only the "unrelated (never touched)" block of
# `bin/lab status`.
lab_status_unrelated() {
	"$LAB" status | awk '/^unrelated/{p=1} p'
}

# lab_cleanup — scoped teardown through bin/lab plus verification. Never
# aborts the caller; returns non-zero if cleanup reported a failure.
lab_cleanup() {
	local rc=0
	section "Scoped cleanup"
	(cd "$REPO_ROOT" && rec "bin/lab cleanup -apply" "$LAB" cleanup -apply) || rc=$?
	(cd "$REPO_ROOT" && rec "bin/lab status after cleanup" "$LAB" status) || rc=$?
	rec "limactl list after cleanup" limactl list || true
	rec "host free disk after cleanup" host_free_disk || true
	return "$rc"
}

# sanitize_log FILE — remove private host paths and Swarm join tokens.
sanitize_log() {
	local f=$1 tmp
	tmp=$(mktemp "${TMPDIR:-/tmp}/dbcm-p0-log.XXXXXX")
	sed -e "s#$REPO_ROOT#<repo>#g" \
		-e "s#$HOME#~#g" \
		-e "s#$(id -un)#<user>#g" \
		-e 's#SWMTKN-[A-Za-z0-9-]*#SWMTKN-<redacted>#g' \
		-e "s#/Users/[A-Za-z0-9._-]*#~#g" \
		-e "s#/private/var/folders/[^ ]*#<tmp>#g" \
		"$f" >"$tmp"
	mv "$tmp" "$f"
}
