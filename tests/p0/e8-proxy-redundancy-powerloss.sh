#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E8, the two remaining W05 gates that fit arm64 lab VMs.
# It reuses the E4 environment (VMs, Swarm, images, real manager, mTLS agents,
# a 3-node Galera cluster created through the manager API) as a library.
#
#   Gate A (redundant proxies): two cmd/proxy instances on different VMs
#     (m2, m3) in front of the real cluster, a keepalived VIP (VRRP unicast)
#     over both, and a client-side failover list. Measures client write
#     continuity when the proxy container and then the whole proxy VM
#     (limactl stop --force) of the VIP holder is lost, checks that both
#     proxies agree on one writer (no multi-writer split) and that no
#     acknowledged write is lost.
#   Gate B (VM power loss): limactl stop --force of one Galera node VM, then
#     of all three VMs; restart; the manager's guided recovery must stay
#     approval-gated (no automatic bootstrap) and sentinel rows must survive.
#
# Usage:   tests/p0/e8-proxy-redundancy-powerloss.sh
# Environment (optional):
#   E8_KEEP_VMS=1   keep the VMs (development; run bin/lab cleanup -apply)
#   E4_REUSE_VMS=1  skip VM creation and Swarm setup (VMs of a kept run)
#   E8_PHASES="..." run only these phases (default: all, in order)
#   E8_LOG=FILE     log file (default tests/p0/results/e8-proxy-redundancy-powerloss.log)
#   E8_APPEND_LOG=1 append to the log instead of truncating it
#
# Requirements: Go toolchain, jq, limactl on PATH, bin/lab built.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG=${E8_LOG:-"$REPO_ROOT/tests/p0/results/e8-proxy-redundancy-powerloss.log"}
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
[ "${E8_APPEND_LOG:-0}" = 1 ] || : >"$LOG"
# shellcheck source=tests/p0/e4-manager-galera.sh
E4_LIBRARY=1 . "$REPO_ROOT/tests/p0/e4-manager-galera.sh"
# shellcheck source=tests/p0/e6-phases.sh
. "$REPO_ROOT/tests/p0/e6-phases.sh"

finish8() {
	local rc=$?
	trap - EXIT INT TERM
	if [ "${E8_KEEP_VMS:-0}" = 1 ]; then
		say "E8_KEEP_VMS=1: VMs kept; run bin/lab cleanup -apply manually"
	else
		lab_cleanup || rc=1
	fi
	sanitize_log "$LOG"
	sed -i '' -e "s#/var/folders/[^ ']*#<tmp>#g" -e 's#dbcm-et1\.[A-Za-z0-9._-]*#dbcm-et1.<redacted>#g' \
		-e "s#PASSWORD '\*[0-9A-F]*'#PASSWORD '<hash redacted>'#g" "$LOG"
	say "E8 finished with exit $rc"
	exit "$rc"
}
trap finish8 EXIT
trap 'exit 130' INT TERM

step() { case " ${E8_PHASES:-all} " in *" all "* | *" $1 "*) return 0 ;; esac; return 1; }

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

# shellcheck source=tests/p0/e8-phases.sh
. "$REPO_ROOT/tests/p0/e8-phases.sh"

E4_STEPS="vms swarm images prereqs backend manager setup swarmapi cluster verify"
for p in build $E4_STEPS $E8_STEPS; do
	if step "$p"; then
		case "$p" in vms | swarm) [ "${E4_REUSE_VMS:-0}" = 1 ] && continue ;; esac
		"phase_$p"
	fi
done
