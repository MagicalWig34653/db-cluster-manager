#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Release-candidate demo: builds the R1 stack (three Lima VMs, Docker Swarm,
# the three-replica HA manager stack from deploy/stack-ha.yaml with peer mTLS)
# in one command and stops BEFORE the first administrator exists, so that the
# setup token is still unused and you create the administrator in your own
# browser. It prints the URL, the setup token and how to reach the UI.
#
# STATUS: NOT EXECUTED. This wrapper reuses the E12 harness phases build, vms,
# swarm, images, prereqs, hainit and hastack, which ran in E12/E9; the
# token extraction and port forward below have never run. See
# tests/p0/results/e13-not-run.md.
#
# Usage:   tests/p0/rc-demo.sh up        build everything, keep the VMs
#          tests/p0/rc-demo.sh info      print URL, token and forward command
#          tests/p0/rc-demo.sh forward   forward the UI of manager-1 to localhost:8080
#          tests/p0/rc-demo.sh down      scoped teardown (bin/lab cleanup --apply)
# Needs: Go, jq, python3, limactl, bin/lab (make build), about 12 GiB RAM,
# 3 vCPUs x 2, 60 GiB thin disk and at least 15 GiB free host disk.
set -eu -o pipefail
REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO_ROOT"
VM=dbcm-lab-m1
PORT=${RC_DEMO_PORT:-8080}

token() {
	local i out
	for i in 1 2 3; do
		out=$(limactl shell --workdir / "$VM" -- sudo docker service logs "dbcm_manager-$i" 2>&1 |
			grep -o 'setup token[^:]*: [^ ]*' | tail -1 | awk '{print $NF}' || true)
		[ -z "$out" ] || { printf '%s\n' "$out"; return 0; }
	done
	return 1
}

info() {
	cat <<EOF
UI (after "forward"):  http://localhost:$PORT   (WebAuthn accepts http://localhost; rpId is "localhost")
Setup token:           $(token || echo "<not found: the administrator may already exist, or the stack is not up>")
Forward the UI:        tests/p0/rc-demo.sh forward   (ssh tunnel to the manager on $VM)
Teardown:              tests/p0/rc-demo.sh down
Known gap:             the other two replicas cannot lead until their data-key recipients are registered
                       by an authenticated administrator (docs/guides/en/11-operator-runbook.md, section 5);
                       the E12 harness does this with its lab client, this demo leaves it to you.
EOF
}

case "${1:-}" in
up)
	[ -x bin/lab ] || make build
	E12_KEEP_VMS=1 E12_LOG="$REPO_ROOT/tests/p0/results/rc-demo.log" \
		E12_PHASES="build vms swarm images prereqs hainit hastack" \
		tests/p0/e12-recovery-autofailover-rolling.sh
	info
	;;
info) info ;;
forward)
	# Manager-1 listens on 127.0.0.1:8080 of the VM (host network).
	exec ssh -F "$HOME/.lima/$VM/ssh.config" -N -L "$PORT:127.0.0.1:8080" "lima-$VM"
	;;
down) bin/lab cleanup --apply ;;
*)
	sed -n 2,22p "$0"
	exit 2
	;;
esac
