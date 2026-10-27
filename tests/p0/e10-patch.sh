#!/usr/bin/env bash
# Remote snippets are single-quoted on purpose (they expand inside the VM).
# shellcheck disable=SC2016
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment E10: OS patch management on two real guest operating systems
# (X15, ADR 0023). Two lab VMs (dbcm-lab-m1 Debian 12 for apt, dbcm-lab-m2
# AlmaLinux 9 for dnf, both arm64) are created through bin/lab, the native
# agent is installed from a reviewed plan, and cmd/patchlab exercises report,
# security classification, report-only guarantees, approval, apply,
# verification, reboot-required detection, the reboot policy, the Galera
# guard and agent removal on each VM.
#
# Usage (from anywhere):  tests/p0/e10-patch.sh
#
# Environment (optional, explicit, default off):
#   E10_KEEP_VMS=1   do not run the scoped cleanup at the end (development
#                    only; run `bin/lab cleanup --apply` yourself afterwards)
#
# What it never does: touch the host's package manager, Docker or any
# non-dbcm-lab Lima instance. All package manager commands run inside the lab
# VMs. The SSH transport and enrollment/mTLS are NOT part of this run (see
# cmd/patchlab/main.go); limactl shell stands in for SSH.
#
# Output: sanitized log at tests/p0/results/e10-patch.log.
set -eu -o pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG="$REPO_ROOT/tests/p0/results/e10-patch.log"
export REPO_ROOT LOG
mkdir -p "$(dirname "$LOG")"
: >"$LOG"
# shellcheck source=tests/p0/common.sh
. "$REPO_ROOT/tests/p0/common.sh"

VMS="dbcm-lab-m1 dbcm-lab-m2"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/dbcm-e10.XXXXXX")
AGENT="$TMP/dbcm-agent"
rc=0

finish() {
	local code=$?
	if [ "${E10_KEEP_VMS:-}" = 1 ]; then
		say "E10_KEEP_VMS=1: VMs kept; run 'bin/lab cleanup --apply' yourself"
	else
		lab_cleanup || code=1
	fi
	rm -rf "$TMP"
	sanitize_log "$LOG"
	exit "$code"
}
trap finish EXIT

section "Build"
(cd "$REPO_ROOT" && rec "build bin/lab" go build -o bin/lab ./cmd/lab) || die "build lab"
(cd "$REPO_ROOT" && rec "build patchlab" go build -o "$TMP/patchlab" ./cmd/patchlab) || die "build patchlab"
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 rec "cross-build agent linux/arm64" go build -trimpath -o "$AGENT" ./cmd/agent) || die "build agent"
rec "agent binary" sh -c "ls -l '$AGENT' && shasum -a 256 '$AGENT'"

section "Lab VMs"
rec "host free disk before apply" host_free_disk
(cd "$REPO_ROOT" && rec "bin/lab status before" "$LAB" status)
(cd "$REPO_ROOT" && rec "bin/lab apply -vms 2 -templates debian-12,almalinux-9" \
	"$LAB" apply -vms 2 -cpus 2 -ram 3 -disk 15 -templates debian-12,almalinux-9 --apply) || die "lab apply"
rec "limactl list" limactl list

for vm in $VMS; do
	section "Patch lab on $vm"
	{
		printf '### patchlab %s\n\n```\n' "$vm"
	} >>"$LOG"
	vrc=0
	"$TMP/patchlab" -vm "$vm" -binary "$AGENT" -apply-packages 2 >>"$LOG" 2>&1 || vrc=$?
	printf '(exit %s)\n```\n\n' "$vrc" >>"$LOG"
	say "patchlab on $vm exit $vrc"
	[ "$vrc" -eq 0 ] || rc=1
done

section "Result"
say "patchlab exit status overall: $rc (0 = no FAILED checks; see VERIFIED / NOT VERIFIED lines)"
exit "$rc"
