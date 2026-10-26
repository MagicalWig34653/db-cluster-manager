#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E9 (W05): the three-service manager HA stack on a 3-VM Swarm,
# the coordinated proxy writer choice (ADR 0017) and the E8 B2/B3 power-loss
# diagnosis.
#
#   Part A  deploy/stack-ha.yaml (three pinned manager services, host network,
#           peer mTLS, replicated keyring): leader election, follower request
#           forwarding, peer TLS evidence and refusal of a replica with a wrong
#           certificate, leader kill, node drain.
#   Part B  two cmd/proxy containers (m2, m3) with the manager-coordinated
#           writer choice, keepalived VIP, client-side failover list: agreement
#           on one writer, controlled switch (freeze/ack), unreachable proxy,
#           unreachable manager, proxy kill, VM loss; client writes sampled at
#           50 ms, proxy status at 100 ms.
#   Part C  E8 B2/B3 (power loss of all three VMs) again, with slot logs and
#           task diagnostics captured to find why node n3 did not rejoin.
#
# The E4 harness (VMs, Swarm, agents, Galera through the manager API), the E6
# helpers and the E8 proxy/VIP helpers are sourced as libraries.
#
# Phases: build vms swarm images prereqs hainit hastack hasetup hafwd hatls
#         hakill hadrain agents cluster
#         pxprep pxvip pxagree pxswitch pxdown pxmgrdown pxkill pxvmloss pxintegrity
#         pwrb1 pwrb2 pwrb3
# Usage:  tests/p0/e9-ha-proxy-writer.sh
#   E9_KEEP_VMS=1   keep the VMs (run `bin/lab cleanup -apply` yourself)
#   E9_REUSE_VMS=1  skip vms/swarm
#   E9_PHASES="..." run only these phases (default: all, in order)
#   E9_LOG=FILE     log file (default tests/p0/results/e9-ha-proxy-writer.log)
#   E9_APPEND_LOG=1 append to the log instead of truncating it
#
# Requirements: Go toolchain, jq, python3, limactl on PATH, bin/lab built.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${E9_LOG:-"$REPO_ROOT/tests/p0/results/e9-ha-proxy-writer.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${E9_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
export E4_AUTORESUME=false
# shellcheck source=tests/p0/e4-manager-galera.sh
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"
# shellcheck source=tests/p0/e6-phases.sh
. "$REPO_ROOT/tests/p0/e6-phases.sh"
# shellcheck source=tests/p0/e8-phases.sh
. "$REPO_ROOT/tests/p0/e8-phases.sh"

# Overrides of the library settings.
INST=e9lab
NET=dbcm-e9lab-net
ROOT_SECRET=dbcm-e9lab-root-pw
MON_SECRET=dbcm-e9lab-monitor-pw
BAK_SECRET=dbcm-e9lab-backup-pw
API_PORT=8080
IMAGE=db-cluster-manager:e9
SRC=/root/dbcm-e9-src
STACK=dbcm
CLUSTER_NAME=e9db
E8=/root/dbcm-e9 # inside every VM (mounted at /e8 in the proxy containers)
PX=dbcm-e9-proxy
RESULTS_DIR="$REPO_ROOT/tests/p0/results"
SAMPLES_DIR="$RESULTS_DIR/e9-samples"

ASSERT_FAILS=""
assert_fail() { # NAME MESSAGE
	ASSERT_FAILS="$ASSERT_FAILS $1"
	result "ASSERTION FAILED ($1): $2"
}
ok() { result "PASS ($1): $2"; }

finish9() {
	local rc=$?
	trap - EXIT INT TERM
	if [ -n "$ASSERT_FAILS" ]; then
		say "failed assertions in:$ASSERT_FAILS"
		[ "$rc" != 0 ] || rc=1
	fi
	if [ "${E9_KEEP_VMS:-0}" = 1 ]; then
		say "E9_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" -e 's#dbcm-et1\.[A-Za-z0-9._-]*#dbcm-et1.<redacted>#g' \
		-e "s#PASSWORD '\*[0-9A-F]*'#PASSWORD '<hash redacted>'#g" \
		-e 's#\(setup token[^:]*: \)[A-Za-z0-9_-]*#\1<redacted>#g' "$LOG"
	say "E9 finished with exit $rc"
	exit "$rc"
}
trap finish9 EXIT
trap 'exit 130' INT TERM

step() { case " ${E9_PHASES:-all} " in *" all "* | *" $1 "*) return 0 ;; esac; return 1; }

# shellcheck source=tests/p0/e9-phases.sh
. "$REPO_ROOT/tests/p0/e9-phases.sh"

ALL_STEPS="build vms swarm images prereqs hainit hastack hasetup hafwd hatls hakill hadrain agents cluster pxprep pxvip pxagree pxswitch pxdown pxmgrdown pxkill pxvmloss pxintegrity pwrb1 pwrb2 pwrb3"
for p in $ALL_STEPS; do
	step "$p" || continue
	case "$p" in vms | swarm) [ "${E9_REUSE_VMS:-0}" = 1 ] && continue ;; esac
	# A phase that returns non-zero is recorded and the run continues: later
	# phases stand on their own where possible, and an interrupted state is
	# cleaned up by the exit trap either way.
	"e9_$p" || say "phase $p returned non-zero"
done
