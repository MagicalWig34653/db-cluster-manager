// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/dbadmin"
)

// Ownership labels from ADR 0004 item 1. The values equal the agent's label
// keys (internal/agent/docker.go); a test checks this.
const (
	LabelInstallation = "dbcm.installation"
	LabelCluster      = "dbcm.cluster"
	LabelNode         = "dbcm.node"
)

// Container paths. The node's volume is mounted once, at its root. The
// identity marker lives at the volume root and the datadir is a
// subdirectory: E1 showed that SST deletes files inside the datadir, so a
// marker there does not survive.
//
// The unix socket is the node's monitoring and administration endpoint (ADR
// 0011): it lives in its own directory on the volume, so the agent reaches it
// on the host at <volume root>/run/mysqld.sock without any published port.
// It is neither in the datadir (SST empties it) nor at the volume root (only
// root may write there, which keeps the marker out of the database's reach).
const (
	VolumeTarget   = "/dbcm"
	DataDirName    = "data"
	DataDir        = VolumeTarget + "/" + DataDirName
	RunDirName     = "run"
	RunDir         = VolumeTarget + "/" + RunDirName
	SocketRelPath  = RunDirName + "/mysqld.sock" // same as agent.DefaultSocket
	SocketPath     = VolumeTarget + "/" + SocketRelPath
	MarkerFileName = "dbcm-identity.json" // same as agent.MarkerFileName
	MarkerPath     = VolumeTarget + "/" + MarkerFileName
)

// Numeric owner of the datadir and the socket directory. The official
// mariadb image creates the mysql user with "useradd -r" (no fixed ID in the
// Dockerfile); in mariadb:11.4.13 (index digest sha256:250e1609...) its
// /etc/passwd entry is mysql:x:999:999 (read from the image layer, ADR 0011).
const (
	DefaultDBUID = 999
	DefaultDBGID = 999
)

// Default least-privilege account names (ADR 0008, ADR 0011).
const (
	DefaultMonitorUser = "dbcm_monitor"
	DefaultBackupUser  = "dbcm_backup"
	// DefaultMonitorHost: the agent connects through the unix socket, which
	// MariaDB reports as host "localhost".
	DefaultMonitorHost = "localhost"
	// DefaultBackupHost: backup jobs connect over the cluster overlay
	// network from addresses that are not known in advance.
	DefaultBackupHost = "%"
)

// BootstrapFlag makes mariadbd form a new cluster. It belongs only in the
// spec of the bootstrap step; a spec that keeps it would bootstrap a second
// cluster on the next restart (E1 observation).
const BootstrapFlag = "--wsrep-new-cluster"

// Defaults taken from the lab experiments E1/E2.
const (
	DefaultGcacheSize      = "128M"
	DefaultStopGracePeriod = 60 * time.Second
	galeraProvider         = "/usr/lib/galera/libgalera_smm.so"
	sstMethod              = "mariabackup"
	// Grants the image gives 'mysql'@'localhost' (unix_socket) so the donor's
	// wsrep_sst_mariabackup can connect; without them SST failed in E1.
	sstLocalhostGrants = "RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR"
)

// ErrInvalidSpec reports an input that cannot produce a safe service spec.
var ErrInvalidSpec = errors.New("galera: invalid service spec input")

// ErrBootstrapPersisted reports a spec that still carries BootstrapFlag where
// a steady-state spec is required.
var ErrBootstrapPersisted = errors.New("galera: bootstrap flag in a steady-state spec")

// Mount is a service mount. It mirrors agent.Mount.
type Mount struct {
	Type     string `json:"type"` // "volume" or "bind"
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// ResourceAmount is an amount of CPU (thousandths of a CPU) and memory.
type ResourceAmount struct {
	MilliCPU    int64 `json:"milli_cpu,omitempty"`
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
}

func (a ResourceAmount) add(b ResourceAmount) ResourceAmount {
	return ResourceAmount{MilliCPU: a.MilliCPU + b.MilliCPU, MemoryBytes: a.MemoryBytes + b.MemoryBytes}
}

func (a ResourceAmount) sub(b ResourceAmount) ResourceAmount {
	return ResourceAmount{MilliCPU: a.MilliCPU - b.MilliCPU, MemoryBytes: a.MemoryBytes - b.MemoryBytes}
}

// Resources are Swarm task limits and reservations (A04). Zero means unset.
type Resources struct {
	Limits       ResourceAmount `json:"limits"`
	Reservations ResourceAmount `json:"reservations"`
}

// Validate rejects negative values and reservations above a set limit.
func (r Resources) Validate() error {
	for _, v := range []int64{r.Limits.MilliCPU, r.Limits.MemoryBytes, r.Reservations.MilliCPU, r.Reservations.MemoryBytes} {
		if v < 0 {
			return fmt.Errorf("%w: negative resource value", ErrInvalidSpec)
		}
	}
	if r.Limits.MilliCPU > 0 && r.Reservations.MilliCPU > r.Limits.MilliCPU {
		return fmt.Errorf("%w: CPU reservation above limit", ErrInvalidSpec)
	}
	if r.Limits.MemoryBytes > 0 && r.Reservations.MemoryBytes > r.Limits.MemoryBytes {
		return fmt.Errorf("%w: memory reservation above limit", ErrInvalidSpec)
	}
	return nil
}

// Demand is the capacity a node needs on its host: per dimension the larger
// of limit and reservation. A database may use its whole limit, so capacity
// planning counts the limit, not only the reservation.
func (r Resources) Demand() ResourceAmount {
	return ResourceAmount{
		MilliCPU:    max(r.Limits.MilliCPU, r.Reservations.MilliCPU),
		MemoryBytes: max(r.Limits.MemoryBytes, r.Reservations.MemoryBytes),
	}
}

// TLSOptions is a placeholder for database TLS settings. Client-facing TLS
// is designed in a separate package; here the options are only passed
// through as extra mariadbd arguments and Swarm secrets. Arguments that the
// spec builder manages itself are rejected.
type TLSOptions struct {
	Args    []string `json:"args,omitempty"`
	Secrets []string `json:"secrets,omitempty"`
}

// ServiceSpec is a Swarm service definition for one Galera node, in domain
// units (time.Duration, milli-CPUs). internal/orchestrate.ToAgentSpec
// converts it into an agent.ServiceSpec; agentcompat_test.go fails if a
// field is added here without being converted.
type ServiceSpec struct {
	Name             string            `json:"name"`
	Image            string            `json:"image"`
	Labels           map[string]string `json:"labels,omitempty"`
	Env              []string          `json:"env,omitempty"`
	Args             []string          `json:"args,omitempty"`
	Mounts           []Mount           `json:"mounts,omitempty"`
	Constraints      []string          `json:"constraints,omitempty"`
	Replicas         uint64            `json:"replicas"`
	Hostname         string            `json:"hostname,omitempty"`
	Networks         []string          `json:"networks,omitempty"`
	EndpointMode     string            `json:"endpoint_mode,omitempty"`
	Secrets          []string          `json:"secrets,omitempty"`
	RestartCondition string            `json:"restart_condition,omitempty"`
	StopGracePeriod  time.Duration     `json:"stop_grace_period,omitempty"`
	Resources        Resources         `json:"resources"`
}

// Bootstrap reports whether the spec carries BootstrapFlag.
func (s ServiceSpec) Bootstrap() bool { return slices.Contains(s.Args, BootstrapFlag) }

// WithoutBootstrap returns a deep copy without BootstrapFlag.
func (s ServiceSpec) WithoutBootstrap() ServiceSpec {
	c := s.clone()
	c.Args = slices.DeleteFunc(c.Args, func(a string) bool { return a == BootstrapFlag })
	return c
}

// ValidateSteadyState returns ErrBootstrapPersisted if the spec could
// bootstrap a new cluster on restart. Every spec that is stored as a node's
// desired state must pass it.
func (s ServiceSpec) ValidateSteadyState() error {
	if s.Bootstrap() {
		return fmt.Errorf("%w: service %s", ErrBootstrapPersisted, s.Name)
	}
	return nil
}

func (s ServiceSpec) clone() ServiceSpec {
	c := s
	c.Labels = maps.Clone(s.Labels)
	c.Env = slices.Clone(s.Env)
	c.Args = slices.Clone(s.Args)
	c.Mounts = slices.Clone(s.Mounts)
	c.Constraints = slices.Clone(s.Constraints)
	c.Networks = slices.Clone(s.Networks)
	c.Secrets = slices.Clone(s.Secrets)
	return c
}

// Cluster is the cluster-wide input of the spec builder.
type Cluster struct {
	InstallationID string `json:"installation_id"`
	ClusterID      string `json:"cluster_id"`
	// Name is the wsrep_cluster_name and the service name prefix. It must be
	// a lowercase DNS label.
	Name string `json:"name"`
	// Image should be pinned by digest.
	Image              string `json:"image"`
	Network            string `json:"network"`              // attachable overlay network
	RootPasswordSecret string `json:"root_password_secret"` // Swarm secret name
	// Members are the node slots in cluster-address order. Slots are stable
	// names ("n1", "n2", ...); a replaced node keeps its slot and therefore
	// its service and DNS name, but gets a new node UUID and volume.
	Members    []string   `json:"members"`
	GcacheSize string     `json:"gcache_size,omitempty"` // default DefaultGcacheSize
	TLS        TLSOptions `json:"tls"`
	// DBUID and DBGID own the datadir and the socket directory; both zero
	// selects DefaultDBUID/DefaultDBGID.
	DBUID int `json:"db_uid,omitempty"`
	DBGID int `json:"db_gid,omitempty"`
	// Accounts are the manager's database accounts created after the
	// bootstrap node is up.
	Accounts Accounts `json:"accounts,omitzero"`
}

// DBOwner returns the effective numeric owner of the database directories.
func (c Cluster) DBOwner() (uid, gid int) {
	if c.DBUID == 0 && c.DBGID == 0 {
		return DefaultDBUID, DefaultDBGID
	}
	return c.DBUID, c.DBGID
}

// Accounts configures the least-privilege accounts the create plan sets up
// on the bootstrap node (Galera replicates them to the joiners). A secret
// names the Swarm secret holding the account's password; the agent reads it
// from its own secret mount, the password never travels in a command. An
// empty secret omits the account.
type Accounts struct {
	MonitorUser       string `json:"monitor_user,omitempty"`
	MonitorHost       string `json:"monitor_host,omitempty"`
	MonitorSecret     string `json:"monitor_secret,omitempty"`
	MonitorRequireSSL bool   `json:"monitor_require_ssl,omitempty"`
	BackupUser        string `json:"backup_user,omitempty"`
	BackupHost        string `json:"backup_host,omitempty"`
	BackupSecret      string `json:"backup_secret,omitempty"`
	BackupRequireSSL  bool   `json:"backup_require_ssl,omitempty"`
	// BackupLogical also grants the read privileges mariadb-dump needs for
	// the per-database logical export (ADR 0009).
	BackupLogical bool `json:"backup_logical,omitempty"`
}

func (a Accounts) withDefaults() Accounts {
	if a.MonitorUser == "" {
		a.MonitorUser = DefaultMonitorUser
	}
	if a.MonitorHost == "" {
		a.MonitorHost = DefaultMonitorHost
	}
	if a.BackupUser == "" {
		a.BackupUser = DefaultBackupUser
	}
	if a.BackupHost == "" {
		a.BackupHost = DefaultBackupHost
	}
	return a
}

func (a Accounts) validate() error {
	if (a.MonitorUser != "" || a.MonitorHost != "" || a.MonitorRequireSSL) && a.MonitorSecret == "" {
		return fmt.Errorf("%w: monitor account options without monitor_secret", ErrInvalidSpec)
	}
	if (a.BackupUser != "" || a.BackupHost != "" || a.BackupRequireSSL || a.BackupLogical) && a.BackupSecret == "" {
		return fmt.Errorf("%w: backup account options without backup_secret", ErrInvalidSpec)
	}
	d := a.withDefaults()
	check := func(kind, user, host, secret string) error {
		if secret == "" {
			return nil
		}
		if err := dbadmin.ValidateUserName(user); err != nil {
			return fmt.Errorf("%w: %s user: %v", ErrInvalidSpec, kind, err)
		}
		if dbadmin.IsProtectedUser(user) {
			return fmt.Errorf("%w: %s user %q is a system account", ErrInvalidSpec, kind, user)
		}
		if err := dbadmin.ValidateHost(host); err != nil {
			return fmt.Errorf("%w: %s host: %v", ErrInvalidSpec, kind, err)
		}
		if !secretName.MatchString(secret) {
			return fmt.Errorf("%w: %s secret %q", ErrInvalidSpec, kind, secret)
		}
		return nil
	}
	if err := check("monitor", d.MonitorUser, d.MonitorHost, a.MonitorSecret); err != nil {
		return err
	}
	if err := check("backup", d.BackupUser, d.BackupHost, a.BackupSecret); err != nil {
		return err
	}
	if a.MonitorSecret != "" && a.BackupSecret != "" && d.MonitorUser == d.BackupUser && d.MonitorHost == d.BackupHost {
		return fmt.Errorf("%w: monitor and backup accounts must differ", ErrInvalidSpec)
	}
	return nil
}

// Node is one database node identity and placement.
type Node struct {
	Slot      string    `json:"slot"`
	NodeUUID  string    `json:"node_uuid"`
	HostID    string    `json:"host_id"` // Swarm node ID
	Volume    string    `json:"volume"`  // local named volume
	Resources Resources `json:"resources"`
}

// SpecOptions select step-specific variants.
type SpecOptions struct {
	// Bootstrap adds BootstrapFlag. Only the bootstrap step of cluster
	// creation or of an approved full-outage recovery sets it.
	Bootstrap bool
	// SSTDonor is a member slot whose node should donate the state
	// transfer. It is emitted with a trailing comma so Galera may fall back
	// to another donor.
	SSTDonor string
}

var (
	dnsLabel  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	uuidRe    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sizeRe    = regexp.MustCompile(`^[0-9]+[KMGT]?$`)
	dockerRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
	// secretName matches agent's secret reference rule.
	secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ServiceName returns the Swarm service name, hostname and Galera node
// address of a slot.
func ServiceName(clusterName, slot string) string { return "dbcm-" + clusterName + "-" + slot }

// managedArgs are the mariadbd options the builder controls. TLS pass-through
// must not override them.
var managedArgs = []string{
	"datadir", "socket", "wsrep_on", "wsrep_provider", "wsrep_cluster_name", "wsrep_cluster_address",
	"wsrep_sst_method", "wsrep_sst_donor", "wsrep_new_cluster", "wsrep_provider_options",
	"binlog_format", "innodb_autoinc_lock_mode",
}

func argKey(a string) string {
	k, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
	return strings.ReplaceAll(k, "-", "_")
}

func (c Cluster) validate() error {
	switch {
	case c.InstallationID == "", c.ClusterID == "":
		return fmt.Errorf("%w: installation and cluster ID are required", ErrInvalidSpec)
	case !dnsLabel.MatchString(c.Name):
		return fmt.Errorf("%w: cluster name %q is not a lowercase DNS label", ErrInvalidSpec, c.Name)
	case !dockerRef.MatchString(c.Image):
		return fmt.Errorf("%w: image %q", ErrInvalidSpec, c.Image)
	case !dockerRef.MatchString(c.Network):
		return fmt.Errorf("%w: network %q", ErrInvalidSpec, c.Network)
	case !dockerRef.MatchString(c.RootPasswordSecret):
		return fmt.Errorf("%w: root password secret %q", ErrInvalidSpec, c.RootPasswordSecret)
	case c.GcacheSize != "" && !sizeRe.MatchString(c.GcacheSize):
		return fmt.Errorf("%w: gcache size %q", ErrInvalidSpec, c.GcacheSize)
	case len(c.Members) == 0:
		return fmt.Errorf("%w: no members", ErrInvalidSpec)
	case (c.DBUID == 0) != (c.DBGID == 0), c.DBUID < 0, c.DBGID < 0, c.DBUID > math.MaxInt32, c.DBGID > math.MaxInt32:
		return fmt.Errorf("%w: database owner %d:%d (both set to 1..%d, or both zero for the default)", ErrInvalidSpec, c.DBUID, c.DBGID, math.MaxInt32)
	}
	if err := c.Accounts.validate(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range c.Members {
		if !dnsLabel.MatchString(m) || seen[m] {
			return fmt.Errorf("%w: member slot %q invalid or duplicate", ErrInvalidSpec, m)
		}
		seen[m] = true
		if n := ServiceName(c.Name, m); len(n) > 63 {
			return fmt.Errorf("%w: service name %q longer than 63 characters", ErrInvalidSpec, n)
		}
	}
	for _, a := range c.TLS.Args {
		if !strings.HasPrefix(a, "--") || slices.Contains(managedArgs, argKey(a)) {
			return fmt.Errorf("%w: TLS argument %q not allowed", ErrInvalidSpec, a)
		}
	}
	for _, s := range c.TLS.Secrets {
		if !dockerRef.MatchString(s) {
			return fmt.Errorf("%w: TLS secret %q", ErrInvalidSpec, s)
		}
	}
	return nil
}

func (n Node) validate(c Cluster) error {
	switch {
	case !slices.Contains(c.Members, n.Slot):
		return fmt.Errorf("%w: slot %q is not a cluster member", ErrInvalidSpec, n.Slot)
	case !uuidRe.MatchString(n.NodeUUID):
		return fmt.Errorf("%w: node UUID %q", ErrInvalidSpec, n.NodeUUID)
	case !dockerRef.MatchString(n.HostID):
		return fmt.Errorf("%w: host ID %q", ErrInvalidSpec, n.HostID)
	case !dockerRef.MatchString(n.Volume):
		return fmt.Errorf("%w: volume %q", ErrInvalidSpec, n.Volume)
	}
	return n.Resources.Validate()
}

// BuildServiceSpec returns the Swarm service definition of node n in
// cluster c. The options are the ones that worked in E1/E2, except that the
// lab bind-mounted the datadir and the marker separately while this spec
// mounts one named volume at its root and points --datadir into it, and
// that --socket moves the unix socket onto the volume (ADR 0011; not yet run
// in the lab). The volume's data/ and run/ directories must exist with the
// database owner before the service starts (plan step init-volume-dirs).
func BuildServiceSpec(c Cluster, n Node, opt SpecOptions) (ServiceSpec, error) {
	if err := c.validate(); err != nil {
		return ServiceSpec{}, err
	}
	if err := n.validate(c); err != nil {
		return ServiceSpec{}, err
	}
	if opt.SSTDonor != "" && (opt.Bootstrap || opt.SSTDonor == n.Slot || !slices.Contains(c.Members, opt.SSTDonor)) {
		return ServiceSpec{}, fmt.Errorf("%w: SST donor %q for slot %q", ErrInvalidSpec, opt.SSTDonor, n.Slot)
	}
	name := ServiceName(c.Name, n.Slot)
	addrs := make([]string, len(c.Members))
	for i, m := range c.Members {
		addrs[i] = ServiceName(c.Name, m)
	}
	gcache := c.GcacheSize
	if gcache == "" {
		gcache = DefaultGcacheSize
	}
	// No --innodb_buffer_pool_size: wsrep_sst_mariabackup passes mariadbd
	// options on to mariadb-backup, which rejected it in E1.
	args := []string{
		"--datadir=" + DataDir,
		"--socket=" + SocketPath,
		"--wsrep_on=ON",
		"--wsrep_provider=" + galeraProvider,
		"--wsrep_cluster_name=" + c.Name,
		"--wsrep_cluster_address=gcomm://" + strings.Join(addrs, ","),
		"--wsrep_sst_method=" + sstMethod,
		"--binlog_format=ROW",
		"--innodb_autoinc_lock_mode=2",
		// Explicit, not inherited from the image's configuration files: with
		// name resolution on, MariaDB keeps a host cache and blocks a client
		// host after max_connect_errors (default 100) aborted handshakes.
		// The database proxy's liveness probe reads the greeting and
		// disconnects before authenticating (lab E5: about 2 aborted
		// connects per second and node, Aborted_connects_preauth 750 after
		// 6 minutes; the pinned image sets skip-name-resolve itself).
		"--skip_name_resolve=ON",
		"--wsrep_provider_options=gcache.size=" + gcache,
	}
	if opt.SSTDonor != "" {
		args = append(args, "--wsrep_sst_donor="+ServiceName(c.Name, opt.SSTDonor)+",")
	}
	args = append(args, c.TLS.Args...)
	if opt.Bootstrap {
		args = append(args, BootstrapFlag)
	}
	secrets := append([]string{c.RootPasswordSecret}, c.TLS.Secrets...)
	return ServiceSpec{
		Name:  name,
		Image: c.Image,
		Labels: map[string]string{
			LabelInstallation: c.InstallationID,
			LabelCluster:      c.ClusterID,
			LabelNode:         n.NodeUUID,
		},
		Env: []string{
			"MARIADB_ROOT_PASSWORD_FILE=/run/secrets/" + c.RootPasswordSecret,
			"MARIADB_MYSQL_LOCALHOST_USER=1",
			"MARIADB_MYSQL_LOCALHOST_GRANTS=" + sstLocalhostGrants,
		},
		Args:             args,
		Mounts:           []Mount{{Type: "volume", Source: n.Volume, Target: VolumeTarget}},
		Constraints:      []string{"node.id==" + n.HostID},
		Replicas:         1,
		Hostname:         name,
		Networks:         []string{c.Network},
		EndpointMode:     "dnsrr",
		Secrets:          secrets,
		RestartCondition: "any",
		StopGracePeriod:  DefaultStopGracePeriod,
		Resources:        n.Resources,
	}, nil
}
