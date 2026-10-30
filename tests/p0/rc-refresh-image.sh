#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Rebuild the manager image from the committed tree (git archive HEAD) inside
# m1, copy it to m2/m3 and roll the three HA manager services one at a time
# (RC lab iteration after a fix; same tag, so the stack configuration stays).
# Needs the VMs from tests/p0/rc-demo.sh up. Commit first: untracked and
# uncommitted changes are not built.
set -eu -o pipefail
REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO_ROOT"
IMAGE=${RC_IMAGE:-db-cluster-manager:e12}
SRC=/root/dbcm-e9-src
M1=dbcm-lab-m1
vm() { local v=$1; shift; limactl shell --workdir / "$v" -- "$@"; }
rev=$(git rev-parse --short HEAD)
git archive --format=tar HEAD cmd internal web go.mod go.sum deploy .dockerignore |
	vm "$M1" sudo sh -c "rm -rf $SRC && mkdir -p $SRC && tar -x -C $SRC"
vm "$M1" sudo sh -c "cd $SRC && docker build -q -f deploy/Dockerfile -t $IMAGE --build-arg VERSION=0.0.0-rc --build-arg COMMIT=$rev ."
for v in dbcm-lab-m2 dbcm-lab-m3; do
	vm "$M1" sudo docker save "$IMAGE" | vm "$v" sudo docker load
done
for n in 1 2 3; do
	vm "$M1" sudo docker service update --force --detach=false "dbcm_manager-$n" 2>&1 | tail -2
	sleep 20
done
vm "$M1" sudo docker image prune -f >/dev/null
echo "manager image refreshed to $rev"
