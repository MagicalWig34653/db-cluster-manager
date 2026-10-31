// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"path"
	"slices"
	"strings"
	"testing"
	"time"
)

func testCluster() Cluster {
	return Cluster{
		InstallationID:     "inst-1",
		ClusterID:          "clu-1",
		Name:               "shop",
		Image:              "mariadb:11.4@sha256:250e1609e93c0e6aecf64bfbab2d84d5e89d2481540fe77447ff6c855c3b1873",
		Network:            "dbcm-shop-galera",
		RootPasswordSecret: "dbcm-shop-root-pw",
		Members:            []string{"n1", "n2", "n3"},
	}
}

func testNode(slot string) Node {
	return Node{
		Slot:     slot,
		NodeUUID: "81def889-15f7-44a3-b9db-3d4196e4b5b3",
		HostID:   "z4zp15cc49dfljfv2rx9q1kpf",
		Volume:   "dbcm-shop-" + slot + "-81def889",
		Resources: Resources{
			Limits:       ResourceAmount{MilliCPU: 2000, MemoryBytes: 2 << 30},
			Reservations: ResourceAmount{MilliCPU: 500, MemoryBytes: 1 << 30},
		},
	}
}

func TestBuildServiceSpecSteadyState(t *testing.T) {
	spec, err := BuildServiceSpec(testCluster(), testNode("n2"), SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "dbcm-shop-n2" || spec.Hostname != "dbcm-shop-n2" {
		t.Errorf("name/hostname = %q/%q", spec.Name, spec.Hostname)
	}
	if spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", spec.Replicas)
	}
	if !slices.Equal(spec.Constraints, []string{"node.id==z4zp15cc49dfljfv2rx9q1kpf"}) {
		t.Errorf("constraints = %v", spec.Constraints)
	}
	wantLabels := map[string]string{
		LabelInstallation: "inst-1",
		LabelCluster:      "clu-1",
		LabelNode:         "81def889-15f7-44a3-b9db-3d4196e4b5b3",
	}
	if len(spec.Labels) != len(wantLabels) {
		t.Errorf("labels = %v", spec.Labels)
	}
	for k, v := range wantLabels {
		if spec.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, spec.Labels[k], v)
		}
	}
	// One mount: the volume root. The marker lives at its root, the datadir
	// is a subdirectory, so SST (which empties the datadir) cannot touch it.
	if len(spec.Mounts) != 1 {
		t.Fatalf("mounts = %v", spec.Mounts)
	}
	m := spec.Mounts[0]
	if m.Type != "volume" || m.Source != "dbcm-shop-n2-81def889" || m.Target != VolumeTarget || m.ReadOnly {
		t.Errorf("mount = %+v", m)
	}
	if !strings.HasPrefix(DataDir, VolumeTarget+"/") || MarkerPath == DataDir || strings.HasPrefix(MarkerPath, DataDir+"/") {
		t.Errorf("marker %q must be outside datadir %q but on the volume %q", MarkerPath, DataDir, VolumeTarget)
	}
	wantArgs := []string{
		"--datadir=/dbcm/data",
		"--socket=/dbcm/run/mysqld.sock",
		"--wsrep_on=ON",
		"--wsrep_provider=/usr/lib/galera/libgalera_smm.so",
		"--wsrep_cluster_name=shop",
		"--wsrep_cluster_address=gcomm://dbcm-shop-n1,dbcm-shop-n2,dbcm-shop-n3",
		"--wsrep_sst_method=mariabackup",
		"--binlog_format=ROW",
		"--innodb_autoinc_lock_mode=2",
		"--skip_name_resolve=ON",
		"--wsrep_provider_options=gcache.size=128M",
	}
	if !slices.Equal(spec.Args, wantArgs) {
		t.Errorf("args =\n%v\nwant\n%v", spec.Args, wantArgs)
	}
	if spec.Bootstrap() {
		t.Error("steady-state spec reports bootstrap")
	}
	for _, a := range spec.Args {
		if strings.Contains(a, "innodb_buffer_pool_size") {
			t.Error("innodb_buffer_pool_size must not be a mariadbd argument (rejected by mariabackup SST in E1)")
		}
	}
	for _, e := range []string{
		"MARIADB_ROOT_PASSWORD_FILE=/run/secrets/dbcm-shop-root-pw",
		"MARIADB_MYSQL_LOCALHOST_USER=1",
		"MARIADB_MYSQL_LOCALHOST_GRANTS=RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR",
	} {
		if !slices.Contains(spec.Env, e) {
			t.Errorf("env lacks %q: %v", e, spec.Env)
		}
	}
	if !slices.Equal(spec.Secrets, []string{"dbcm-shop-root-pw"}) {
		t.Errorf("secrets = %v", spec.Secrets)
	}
	if !slices.Equal(spec.Networks, []string{"dbcm-shop-galera"}) || spec.EndpointMode != "dnsrr" {
		t.Errorf("network = %v endpoint = %q", spec.Networks, spec.EndpointMode)
	}
	if spec.RestartCondition != "any" || spec.StopGracePeriod != 60*time.Second {
		t.Errorf("restart = %q grace = %v", spec.RestartCondition, spec.StopGracePeriod)
	}
	if spec.Resources != testNode("n2").Resources {
		t.Errorf("resources = %+v", spec.Resources)
	}
}

func TestBuildServiceSpecBinlog(t *testing.T) {
	c := testCluster()
	off, err := BuildServiceSpec(c, testNode("n2"), SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range off.Args {
		if strings.Contains(a, "log_bin") || strings.Contains(a, "log_slave_updates") {
			t.Fatalf("binary log enabled by default: %s", a)
		}
	}
	c.Binlog = true
	on, err := BuildServiceSpec(c, testNode("n2"), SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--log_bin=/dbcm/data/dbcm-bin", "--log_slave_updates=ON", "--binlog_expire_logs_seconds=1209600"} {
		if !slices.Contains(on.Args, want) {
			t.Errorf("args %v lack %s", on.Args, want)
		}
	}
	// The pass-through options cannot override them.
	c.TLS.Args = []string{"--log_bin=/tmp/x"}
	if _, err := BuildServiceSpec(c, testNode("n2"), SpecOptions{}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("log_bin override: %v", err)
	}
}

func TestBuildServiceSpecBootstrapOnlyWhenRequested(t *testing.T) {
	boot, err := BuildServiceSpec(testCluster(), testNode("n1"), SpecOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	if !boot.Bootstrap() || boot.Args[len(boot.Args)-1] != BootstrapFlag {
		t.Fatalf("bootstrap spec args = %v", boot.Args)
	}
	steady := boot.WithoutBootstrap()
	if steady.Bootstrap() {
		t.Fatal("WithoutBootstrap kept the flag")
	}
	if !boot.Bootstrap() {
		t.Fatal("WithoutBootstrap mutated its receiver")
	}
	if err := steady.ValidateSteadyState(); err != nil {
		t.Fatal(err)
	}
	if err := boot.ValidateSteadyState(); !errors.Is(err, ErrBootstrapPersisted) {
		t.Fatalf("ValidateSteadyState(bootstrap) = %v", err)
	}
	plain, _ := BuildServiceSpec(testCluster(), testNode("n1"), SpecOptions{})
	if !slices.Equal(plain.Args, steady.Args) {
		t.Errorf("WithoutBootstrap args %v differ from a steady build %v", steady.Args, plain.Args)
	}
}

func TestBuildServiceSpecDonorHint(t *testing.T) {
	spec, err := BuildServiceSpec(testCluster(), testNode("n3"), SpecOptions{SSTDonor: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	// Trailing comma: fall back to any other donor if the hint is gone.
	if !slices.Contains(spec.Args, "--wsrep_sst_donor=dbcm-shop-n1,") {
		t.Errorf("args lack donor hint: %v", spec.Args)
	}
	if _, err := BuildServiceSpec(testCluster(), testNode("n3"), SpecOptions{SSTDonor: "n3"}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("self donor: err = %v", err)
	}
	if _, err := BuildServiceSpec(testCluster(), testNode("n3"), SpecOptions{SSTDonor: "n9"}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("unknown donor: err = %v", err)
	}
	if _, err := BuildServiceSpec(testCluster(), testNode("n1"), SpecOptions{SSTDonor: "n2", Bootstrap: true}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("bootstrap with donor: err = %v", err)
	}
}

func TestBuildServiceSpecTLSPlaceholder(t *testing.T) {
	c := testCluster()
	c.TLS = TLSOptions{Args: []string{"--ssl_cert=/run/secrets/c"}, Secrets: []string{"c"}}
	spec, err := BuildServiceSpec(c, testNode("n2"), SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Args, "--ssl_cert=/run/secrets/c") || !slices.Contains(spec.Secrets, "c") {
		t.Errorf("TLS not passed through: args=%v secrets=%v", spec.Args, spec.Secrets)
	}
	for _, bad := range []string{BootstrapFlag, "--wsrep_cluster_address=gcomm://", "--datadir=/tmp", "--wsrep_on=OFF", "--socket=/tmp/x.sock"} {
		c.TLS = TLSOptions{Args: []string{bad}}
		if _, err := BuildServiceSpec(c, testNode("n2"), SpecOptions{}); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("TLS arg %q accepted: %v", bad, err)
		}
	}
}

func TestBuildServiceSpecValidation(t *testing.T) {
	cases := map[string]func(*Cluster, *Node){
		"no installation":   func(c *Cluster, _ *Node) { c.InstallationID = "" },
		"no cluster id":     func(c *Cluster, _ *Node) { c.ClusterID = "" },
		"bad name":          func(c *Cluster, _ *Node) { c.Name = "Shop_DB" },
		"no image":          func(c *Cluster, _ *Node) { c.Image = "" },
		"no network":        func(c *Cluster, _ *Node) { c.Network = "" },
		"no secret":         func(c *Cluster, _ *Node) { c.RootPasswordSecret = "" },
		"slot not member":   func(_ *Cluster, n *Node) { n.Slot = "n7" },
		"duplicate member":  func(c *Cluster, _ *Node) { c.Members = []string{"n1", "n2", "n2"} },
		"bad uuid":          func(_ *Cluster, n *Node) { n.NodeUUID = "not-a-uuid" },
		"no host":           func(_ *Cluster, n *Node) { n.HostID = "" },
		"no volume":         func(_ *Cluster, n *Node) { n.Volume = "" },
		"negative cpu":      func(_ *Cluster, n *Node) { n.Resources.Limits.MilliCPU = -1 },
		"reserve > limit":   func(_ *Cluster, n *Node) { n.Resources.Reservations.MemoryBytes = 3 << 30 },
		"bad gcache":        func(c *Cluster, _ *Node) { c.GcacheSize = "128M;evict" },
		"long service name": func(c *Cluster, _ *Node) { c.Name = strings.Repeat("a", 60) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c, n := testCluster(), testNode("n1")
			mutate(&c, &n)
			if _, err := BuildServiceSpec(c, n, SpecOptions{}); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("err = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

// The monitoring endpoint is the unix socket inside the node volume: the
// agent reaches it on the host at <volume root>/run/mysqld.sock. It must
// not be in the datadir (SST empties the datadir) and not at the volume
// root (only root writes there, so the marker stays protected).
func TestSocketInsideVolumeOutsideDatadir(t *testing.T) {
	if SocketPath != VolumeTarget+"/"+SocketRelPath || RunDir != VolumeTarget+"/"+RunDirName {
		t.Errorf("socket %q / run dir %q not below %q", SocketPath, RunDir, VolumeTarget)
	}
	if strings.HasPrefix(SocketPath, DataDir+"/") || path.Dir(SocketPath) == VolumeTarget {
		t.Errorf("socket %q must be in its own directory below the volume, not in the datadir", SocketPath)
	}
	if DataDir != VolumeTarget+"/"+DataDirName {
		t.Errorf("datadir %q", DataDir)
	}
	if DefaultDBUID != 999 || DefaultDBGID != 999 {
		t.Errorf("default database owner %d:%d, want 999:999 (mysql in mariadb:11.4.13)", DefaultDBUID, DefaultDBGID)
	}
	c := testCluster()
	if u, g := c.DBOwner(); u != DefaultDBUID || g != DefaultDBGID {
		t.Errorf("default owner %d:%d", u, g)
	}
	c.DBUID, c.DBGID = 1001, 1002
	if u, g := c.DBOwner(); u != 1001 || g != 1002 {
		t.Errorf("configured owner %d:%d", u, g)
	}
}

func TestClusterAccountValidation(t *testing.T) {
	ok := testCluster()
	ok.Accounts = Accounts{MonitorSecret: "dbcm-inst-monitor-pw", BackupSecret: "dbcm-inst-backup-pw"}
	if _, err := BuildServiceSpec(ok, testNode("n1"), SpecOptions{}); err != nil {
		t.Fatalf("valid accounts rejected: %v", err)
	}
	cases := map[string]func(*Cluster){
		"root uid":         func(c *Cluster) { c.DBUID = 0; c.DBGID = 5 },
		"negative gid":     func(c *Cluster) { c.DBGID = -1 },
		"huge uid":         func(c *Cluster) { c.DBUID = 1 << 31 },
		"bad monitor user": func(c *Cluster) { c.Accounts.MonitorUser = "dbcm monitor" },
		"bad monitor host": func(c *Cluster) { c.Accounts.MonitorHost = "a'b" },
		"bad backup user":  func(c *Cluster) { c.Accounts.BackupUser = "root;--" },
		"bad secret":       func(c *Cluster) { c.Accounts.BackupSecret = "../etc/shadow" },
		"user without pw":  func(c *Cluster) { c.Accounts = Accounts{MonitorUser: "dbcm_monitor"} },
		"same user twice": func(c *Cluster) {
			c.Accounts.MonitorUser, c.Accounts.BackupUser = "x", "x"
			c.Accounts.MonitorHost, c.Accounts.BackupHost = "%", "%"
		},
		"root as monitor": func(c *Cluster) { c.Accounts.MonitorUser = "root" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := ok
			mutate(&c)
			if _, err := BuildServiceSpec(c, testNode("n1"), SpecOptions{}); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("err = %v, want ErrInvalidSpec", err)
			}
		})
	}
	// Defaults: least-privilege accounts named as in ADR 0008/0011.
	a := Accounts{MonitorSecret: "m", BackupSecret: "b"}.withDefaults()
	if a.MonitorUser != DefaultMonitorUser || a.MonitorHost != "localhost" || a.BackupUser != DefaultBackupUser || a.BackupHost != "%" {
		t.Errorf("defaults %+v", a)
	}
}

func TestBuildServiceSpecIsIndependentCopy(t *testing.T) {
	c := testCluster()
	spec, err := BuildServiceSpec(c, testNode("n1"), SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.Members[0] = "changed"
	spec2 := spec.WithoutBootstrap()
	spec2.Labels[LabelNode] = "x"
	spec2.Args[0] = "x"
	if spec.Labels[LabelNode] == "x" || spec.Args[0] == "x" {
		t.Error("WithoutBootstrap shares maps or slices with its receiver")
	}
}
