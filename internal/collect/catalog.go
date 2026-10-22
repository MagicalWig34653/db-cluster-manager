// SPDX-License-Identifier: AGPL-3.0-only

package collect

import "sort"

// Type is the kind of a metric.
type Type string

// Metric types.
const (
	Gauge   Type = "gauge"
	Counter Type = "counter"
)

// Def documents one metric.
type Def struct {
	Name string
	Type Type
	Unit string
	Help string
	// Source names where the value comes from.
	Source string
}

// Label names used by the catalog.
const (
	LabelSwarm     = "swarm"
	LabelCluster   = "cluster"
	LabelNode      = "node"
	LabelHost      = "host"
	LabelContainer = "container"
	LabelRole      = "role"
	LabelCollector = "collector"
	LabelProxy     = "proxy"
	LabelKind      = "kind"
)

// Metric names.
const (
	MTargetUp = "dcm_target_up"

	MGaleraClusterSize       = "dcm_galera_cluster_size"
	MGaleraSizeDeficit       = "dcm_galera_cluster_size_deficit"
	MGaleraPrimary           = "dcm_galera_cluster_primary"
	MGaleraLocalState        = "dcm_galera_local_state"
	MGaleraReady             = "dcm_galera_ready"
	MGaleraConnected         = "dcm_galera_connected"
	MGaleraFlowControlPaused = "dcm_galera_flow_control_paused_ratio"
	MGaleraFlowControlSent   = "dcm_galera_flow_control_sent_total"
	MGaleraFlowControlRecv   = "dcm_galera_flow_control_recv_total"
	MGaleraRecvQueueAvg      = "dcm_galera_local_recv_queue_avg"
	MGaleraSendQueueAvg      = "dcm_galera_local_send_queue_avg"
	MGaleraLastCommitted     = "dcm_galera_last_committed_total"
	MGaleraReplicatedBytes   = "dcm_galera_replicated_bytes_total"
	MGaleraReceivedBytes     = "dcm_galera_received_bytes_total"
	MDBThreadsConnected      = "dcm_mariadb_threads_connected"
	MDBThreadsRunning        = "dcm_mariadb_threads_running"
	MDBMaxUsedConnections    = "dcm_mariadb_max_used_connections"
	MDBConnections           = "dcm_mariadb_connections_total"
	MDBAbortedConnects       = "dcm_mariadb_aborted_connects_total"
	MDBQueries               = "dcm_mariadb_queries_total"
	MDBQuestions             = "dcm_mariadb_questions_total"
	MDBSlowQueries           = "dcm_mariadb_slow_queries_total"
	MDBBytesReceived         = "dcm_mariadb_bytes_received_total"
	MDBBytesSent             = "dcm_mariadb_bytes_sent_total"
	MDBUptime                = "dcm_mariadb_uptime_seconds"
	MDBBufferPoolHitRatio    = "dcm_mariadb_innodb_buffer_pool_hit_ratio"
	MDBBufferPoolUsedRatio   = "dcm_mariadb_innodb_buffer_pool_used_ratio"
	MDBBufferPoolDirtyRatio  = "dcm_mariadb_innodb_buffer_pool_dirty_ratio"
	MDBReplicationLagSeconds = "dcm_mariadb_replication_lag_seconds"
	MContainerCPURatio       = "dcm_container_cpu_ratio"
	MContainerMemUsed        = "dcm_container_memory_used_bytes"
	MContainerMemLimit       = "dcm_container_memory_limit_bytes"
	MContainerMemRatio       = "dcm_container_memory_used_ratio"
	MContainerNetRx          = "dcm_container_network_receive_bytes_total"
	MContainerNetTx          = "dcm_container_network_transmit_bytes_total"
	MContainerBlockRead      = "dcm_container_block_read_bytes_total"
	MContainerBlockWrite     = "dcm_container_block_write_bytes_total"
	MHostCPURatio            = "dcm_host_cpu_ratio"
	MHostMemRatio            = "dcm_host_memory_used_ratio"
	MHostDiskUsed            = "dcm_host_disk_used_bytes"
	MHostDiskTotal           = "dcm_host_disk_total_bytes"
	MHostDiskRatio           = "dcm_host_disk_used_ratio"
	MProxyHasWriter          = "dcm_proxy_has_writer"
	MProxyWriterChanges      = "dcm_proxy_writer_changes_total"
	MProxyConnections        = "dcm_proxy_connections"
	MProxyNodeHealthy        = "dcm_proxy_node_healthy"
	MProxyNodeConnections    = "dcm_proxy_node_connections"
	MProxyNodeDraining       = "dcm_proxy_node_draining_connections"
	MBackupAgeRatio          = "dcm_backup_age_ratio"
	MCertExpirySeconds       = "dcm_certificate_expiry_seconds"
	MCollectDuration         = "dcm_collector_duration_seconds"
	MCollectSamples          = "dcm_collector_samples"
	MCollectErrors           = "dcm_collector_errors_total"
	MCollectSkipped          = "dcm_collector_skipped_total"
	MCollectRejected         = "dcm_collector_rejected_total"
	MCollectLastSuccess      = "dcm_collector_last_success_timestamp_seconds"
	MCollectBackoff          = "dcm_collector_backoff_seconds"
)

const (
	srcWsrep  = "SHOW GLOBAL STATUS"
	srcDocker = "Docker stats"
	srcProxy  = "proxy /status"
	srcSched  = "scheduler"
)

// Catalog is the documented metric catalog. Metrics marked "external" are
// part of the alert contract but produced by other subsystems (backup,
// certificates); no collector in this package emits them yet.
var Catalog = []Def{
	{MTargetUp, Gauge, "bool", "1 if the last observation of the target succeeded and was fresh, else 0. Labels: collector plus target labels.", "collector"},

	{MGaleraClusterSize, Gauge, "nodes", "wsrep_cluster_size as seen by the node.", srcWsrep},
	{MGaleraSizeDeficit, Gauge, "nodes", "Expected cluster size minus wsrep_cluster_size; emitted only when an expected size is configured.", srcWsrep + " + desired state"},
	{MGaleraPrimary, Gauge, "bool", "1 if wsrep_cluster_status is Primary, else 0.", srcWsrep},
	{MGaleraLocalState, Gauge, "state", "wsrep_local_state (4 = Synced).", srcWsrep},
	{MGaleraReady, Gauge, "bool", "wsrep_ready.", srcWsrep},
	{MGaleraConnected, Gauge, "bool", "wsrep_connected.", srcWsrep},
	{MGaleraFlowControlPaused, Gauge, "ratio", "wsrep_flow_control_paused: fraction of time replication was paused since the last FLUSH STATUS.", srcWsrep},
	{MGaleraFlowControlSent, Counter, "messages", "wsrep_flow_control_sent.", srcWsrep},
	{MGaleraFlowControlRecv, Counter, "messages", "wsrep_flow_control_recv.", srcWsrep},
	{MGaleraRecvQueueAvg, Gauge, "events", "wsrep_local_recv_queue_avg.", srcWsrep},
	{MGaleraSendQueueAvg, Gauge, "events", "wsrep_local_send_queue_avg.", srcWsrep},
	{MGaleraLastCommitted, Counter, "seqno", "wsrep_last_committed.", srcWsrep},
	{MGaleraReplicatedBytes, Counter, "bytes", "wsrep_replicated_bytes.", srcWsrep},
	{MGaleraReceivedBytes, Counter, "bytes", "wsrep_received_bytes.", srcWsrep},
	{MDBThreadsConnected, Gauge, "threads", "Threads_connected.", srcWsrep},
	{MDBThreadsRunning, Gauge, "threads", "Threads_running.", srcWsrep},
	{MDBMaxUsedConnections, Gauge, "connections", "Max_used_connections.", srcWsrep},
	{MDBConnections, Counter, "connections", "Connections (connection attempts).", srcWsrep},
	{MDBAbortedConnects, Counter, "connections", "Aborted_connects.", srcWsrep},
	{MDBQueries, Counter, "queries", "Queries (including statements inside stored programs).", srcWsrep},
	{MDBQuestions, Counter, "queries", "Questions (statements sent by clients).", srcWsrep},
	{MDBSlowQueries, Counter, "queries", "Slow_queries.", srcWsrep},
	{MDBBytesReceived, Counter, "bytes", "Bytes_received.", srcWsrep},
	{MDBBytesSent, Counter, "bytes", "Bytes_sent.", srcWsrep},
	{MDBUptime, Gauge, "seconds", "Uptime.", srcWsrep},
	{MDBBufferPoolHitRatio, Gauge, "ratio", "1 - Innodb_buffer_pool_reads / Innodb_buffer_pool_read_requests since server start; omitted while there were no read requests.", srcWsrep},
	{MDBBufferPoolUsedRatio, Gauge, "ratio", "1 - Innodb_buffer_pool_pages_free / Innodb_buffer_pool_pages_total.", srcWsrep},
	{MDBBufferPoolDirtyRatio, Gauge, "ratio", "Innodb_buffer_pool_pages_dirty / Innodb_buffer_pool_pages_total.", srcWsrep},
	{MDBReplicationLagSeconds, Gauge, "seconds", "Seconds_Behind_Master when the status source supplies it (asynchronous replica); omitted when absent or NULL. Galera nodes have no such lag; see the recv queue instead.", srcWsrep},

	{MContainerCPURatio, Gauge, "ratio", "CPU use as a fraction of one core.", srcDocker},
	{MContainerMemUsed, Gauge, "bytes", "Container memory in use.", srcDocker},
	{MContainerMemLimit, Gauge, "bytes", "Container memory limit (omitted if unlimited).", srcDocker},
	{MContainerMemRatio, Gauge, "ratio", "Memory in use divided by the limit (omitted if unlimited).", srcDocker},
	{MContainerNetRx, Counter, "bytes", "Network bytes received.", srcDocker},
	{MContainerNetTx, Counter, "bytes", "Network bytes transmitted.", srcDocker},
	{MContainerBlockRead, Counter, "bytes", "Block device bytes read.", srcDocker},
	{MContainerBlockWrite, Counter, "bytes", "Block device bytes written.", srcDocker},
	{MHostCPURatio, Gauge, "ratio", "Host CPU busy fraction of total capacity.", srcDocker},
	{MHostMemRatio, Gauge, "ratio", "Host memory in use fraction.", srcDocker},
	{MHostDiskUsed, Gauge, "bytes", "Used bytes of the database volume file system.", srcDocker},
	{MHostDiskTotal, Gauge, "bytes", "Total bytes of the database volume file system.", srcDocker},
	{MHostDiskRatio, Gauge, "ratio", "Used divided by total bytes of the database volume file system.", srcDocker},

	{MProxyHasWriter, Gauge, "bool", "1 if the proxy currently routes writes to a node.", srcProxy},
	{MProxyWriterChanges, Counter, "changes", "Writer changes since the proxy started.", srcProxy},
	{MProxyConnections, Gauge, "connections", "Open client connections per role (write, read).", srcProxy},
	{MProxyNodeHealthy, Gauge, "bool", "1 healthy, 0 unhealthy; omitted when the proxy reports unknown.", srcProxy},
	{MProxyNodeConnections, Gauge, "connections", "Open connections to the node.", srcProxy},
	{MProxyNodeDraining, Gauge, "connections", "Connections draining on the node.", srcProxy},

	{MBackupAgeRatio, Gauge, "ratio", "Age of the newest successful backup divided by the policy's maximum age (external: not emitted yet).", "external"},
	{MCertExpirySeconds, Gauge, "seconds", "Seconds until the certificate expires (external: not emitted yet).", "external"},

	{MCollectDuration, Gauge, "seconds", "Duration of the last run of the collector.", srcSched},
	{MCollectSamples, Gauge, "samples", "Samples produced by the last run.", srcSched},
	{MCollectErrors, Counter, "runs", "Runs that returned an error or timed out.", srcSched},
	{MCollectSkipped, Counter, "runs", "Runs skipped because the previous run was still in flight or the store pushed back.", srcSched},
	{MCollectRejected, Counter, "samples", "Samples rejected by the store.", srcSched},
	{MCollectLastSuccess, Gauge, "seconds", "Unix time of the last run without error.", srcSched},
	{MCollectBackoff, Gauge, "seconds", "Current backoff caused by store pushback (0 = none).", srcSched},
}

var catalogIndex = func() map[string]Def {
	m := make(map[string]Def, len(Catalog))
	for _, d := range Catalog {
		m[d.Name] = d
	}
	return m
}()

// Lookup returns the catalog entry of a metric.
func Lookup(name string) (Def, bool) {
	d, ok := catalogIndex[name]
	return d, ok
}

// Names returns all catalog metric names, sorted.
func Names() []string {
	out := make([]string, 0, len(Catalog))
	for _, d := range Catalog {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}
