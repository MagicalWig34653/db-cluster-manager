#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# P0 experiment helper (E2 step 4): pre-start identity marker guard, a lab
# stand-in for the agent check proposed in ADR 0004 item 3. It is NOT product
# code. It runs before the database process starts and refuses the start
# unless the identity marker on the node's local volume matches the identity
# the manager expects for this node.
#
# Usage:
#   marker-guard.sh                 check only; exit status = decision
#   marker-guard.sh CMD [ARGS...]   check, then exec CMD (container entrypoint
#                                   wrapper, e.g. `marker-guard.sh
#                                   docker-entrypoint.sh --wsrep_on=ON ...`)
#
# Environment:
#   DBCM_EXPECTED_NODE_UUID      node-uuid from manager state (required)
#   DBCM_EXPECTED_INSTALLATION   installation ID (required)
#   DBCM_EXPECTED_CLUSTER        cluster ID (required)
#   DBCM_MARKER   marker path   (default /dbcm/dbcm-identity.json; the marker
#                                lives at the volume root, outside the datadir,
#                                because SST empties the datadir, see E1)
#   DBCM_DATADIR  datadir path  (default /var/lib/mysql)
#
# Decisions (fail closed):
#   exit 0   marker present and installation, cluster and node-uuid match
#   exit 64  expected identity not configured
#   exit 65  marker present but foreign (any field differs or is missing)
#   exit 66  marker missing, datadir not empty (data of unknown identity)
#   exit 67  marker missing, datadir empty (volume lost or replaced); only an
#            explicit manager "new identity" operation, which writes a new
#            marker and changes the expected node-uuid, may make this start
set -u

marker=${DBCM_MARKER:-/dbcm/dbcm-identity.json}
datadir=${DBCM_DATADIR:-/var/lib/mysql}
exp_uuid=${DBCM_EXPECTED_NODE_UUID:-}
exp_inst=${DBCM_EXPECTED_INSTALLATION:-}
exp_cluster=${DBCM_EXPECTED_CLUSTER:-}

log() { echo "marker-guard: $*" >&2; }

refuse() { # refuse CODE REASON
	log "REFUSE ($1): $2"
	log "database start blocked; report to manager (marker=$marker datadir=$datadir)"
	exit "$1"
}

field() { # field KEY FILE -> value of a flat JSON string field
	sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" "$2" | head -1
}

datadir_empty() {
	[ -d "$datadir" ] || return 0
	for e in "$datadir"/* "$datadir"/.[!.]* "$datadir"/..?*; do
		[ -e "$e" ] || [ -L "$e" ] || continue
		[ "${e##*/}" = lost+found ] && continue
		return 1
	done
	return 0
}

if [ -z "$exp_uuid" ] || [ -z "$exp_inst" ] || [ -z "$exp_cluster" ]; then
	refuse 64 "expected identity not configured (DBCM_EXPECTED_* unset)"
fi

if [ -f "$marker" ]; then
	uuid=$(field node_uuid "$marker")
	inst=$(field installation_id "$marker")
	cluster=$(field cluster_id "$marker")
	if [ "$uuid" != "$exp_uuid" ] || [ "$inst" != "$exp_inst" ] || [ "$cluster" != "$exp_cluster" ]; then
		refuse 65 "foreign marker: found node_uuid=$uuid installation=$inst cluster=$cluster, expected node_uuid=$exp_uuid installation=$exp_inst cluster=$exp_cluster"
	fi
	log "ALLOW: marker matches node_uuid=$uuid installation=$inst cluster=$cluster"
elif datadir_empty; then
	refuse 67 "marker missing and datadir empty: volume lost or replaced for existing identity node_uuid=$exp_uuid; needs an explicit manager new-identity operation"
else
	refuse 66 "marker missing but datadir not empty: data of unknown identity"
fi

[ "$#" -eq 0 ] && exit 0
exec "$@"
