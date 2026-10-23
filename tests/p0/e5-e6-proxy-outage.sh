#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiments E5 and E6 on the E4 lab (tests/p0/e4-manager-galera.sh is
# sourced as a library for VMs, Swarm, images, manager, agents and the
# 3-node Galera cluster created through the manager API).
#
#   E5: cmd/proxy (internal/proxy, internal/routing, internal/galeraprobe)
#       in front of the real cluster: writer selection, hysteresis, failover
#       when the writer's container is killed, client-facing TLS (MySQL
#       in-protocol upgrade), connection behaviour and probe side effects.
#   E6: full-cluster outage: every node stopped (clean: all services scaled
#       to 0; crash: all containers killed at once with Swarm restarting
#       them). The manager's read-only outage diagnosis
#       (GET /clusters/{id}/outage-diagnosis: read-grastate through the
#       agents + internal/galera) must give the safe bootstrap guidance and
#       must not start anything. Recovery then follows the guided path after
#       explicit approval.
#
# Usage:   tests/p0/e5-e6-proxy-outage.sh
# Environment (optional):
#   E5_KEEP_VMS=1   keep the VMs (development; run bin/lab cleanup -apply)
#   E4_REUSE_VMS=1  skip VM creation and Swarm setup (VMs of a kept run)
#   E5_PHASES="..." run only these phases (default: all, in order)
#   E5_LOG=FILE     log file (default tests/p0/results/e5-e6-proxy-outage.log)
#   E5_APPEND_LOG=1 append to the log instead of truncating it
#
# Requirements: Go toolchain, jq, limactl on PATH.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${E5_LOG:-"$REPO_ROOT/tests/p0/results/e5-e6-proxy-outage.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${E5_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
# shellcheck source=tests/p0/e4-manager-galera.sh
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"

E5=/root/dbcm-e5 # inside m1: proxy binary, configs, certificates, passwords
PROXY=dbcm-e5-proxy
PROXY_TLS=dbcm-e5-proxy-tls

finish5() {
	local rc=$?
	trap - EXIT INT TERM
	if [ "${E5_KEEP_VMS:-0}" = 1 ]; then
		say "E5_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" -e 's#dbcm-et1\.[A-Za-z0-9._-]*#dbcm-et1.<redacted>#g' \
		-e "s#PASSWORD '\*[0-9A-F]*'#PASSWORD '<hash redacted>'#g" "$LOG"
	say "E5/E6 finished with exit $rc"
	exit "$rc"
}
trap finish5 EXIT
trap 'exit 130' INT TERM

step() { case " ${E5_PHASES:-all} " in *" all "* | *" $1 "*) return 0 ;; esac; return 1; }

phase_build() {
	section "Phase build: cross-compile linux/arm64 binaries on the host"
	mkdir -p "$BIN"
	local c
	for c in manager agent dbcm-job proxy; do
		(cd "$REPO_ROOT" && rec "go build ./cmd/$c" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/$c" "./cmd/$c")
	done
	for c in e4client e5client; do
		(cd "$REPO_ROOT" && rec "go build ./tests/p0/$c" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$BIN/$c" "./tests/p0/$c")
	done
	rec "git revision" git -C "$REPO_ROOT" log -1 --format='%h %s'
}

# shellcheck source=tests/p0/e5-phases.sh
. "$REPO_ROOT/tests/p0/e5-phases.sh"
# shellcheck source=tests/p0/e6-phases.sh
. "$REPO_ROOT/tests/p0/e6-phases.sh"

E4_STEPS="vms swarm images prereqs backend manager setup swarmapi cluster verify"
for p in build upgrade $E4_STEPS $E5_STEPS $E6_STEPS; do
	if step "$p"; then
		case "$p" in vms | swarm) [ "${E4_REUSE_VMS:-0}" = 1 ] && continue ;; esac
		"phase_$p"
	fi
done
