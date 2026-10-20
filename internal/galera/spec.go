// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
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
const (
	VolumeTarget   = "/dbcm"
	DataDir        = VolumeTarget + "/data"
	MarkerFileName = "dbcm-identity.json" // same as agent.MarkerFileName
	MarkerPath     = VolumeTarget + "/" + MarkerFileName
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

func (a ResourceAmount) fits(in ResourceAmount) bool {
	return a.MilliCPU <= in.MilliCPU && a.MemoryBytes <= in.MemoryBytes
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

// ServiceSpec is a Swarm service definition for one Galera node. It mirrors
// agent.ServiceSpec (Name, Image, Labels, Env, Args, Mounts, Constraints,
// Replicas) and adds the fields the lab services used that the agent's
// DockerAPI does not take yet (hostname, network, endpoint mode, secrets,
// restart policy, stop grace period, resources).
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
)

// ServiceName returns the Swarm service name, hostname and Galera node
// address of a slot.
func ServiceName(clusterName, slot string) string { return "dbcm-" + clusterName + "-" + slot }

// managedArgs are the mariadbd options the builder controls. TLS pass-through
// must not override them.
var managedArgs = []string{
	"datadir", "wsrep_on", "wsrep_provider", "wsrep_cluster_name", "wsrep_cluster_address",
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
// mounts one named volume at its root and points --datadir into it.
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
		"--wsrep_on=ON",
		"--wsrep_provider=" + galeraProvider,
		"--wsrep_cluster_name=" + c.Name,
		"--wsrep_cluster_address=gcomm://" + strings.Join(addrs, ","),
		"--wsrep_sst_method=" + sstMethod,
		"--binlog_format=ROW",
		"--innodb_autoinc_lock_mode=2",
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
