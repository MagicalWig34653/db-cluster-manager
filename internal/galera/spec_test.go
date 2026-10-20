// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
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
		"--wsrep_on=ON",
		"--wsrep_provider=/usr/lib/galera/libgalera_smm.so",
		"--wsrep_cluster_name=shop",
		"--wsrep_cluster_address=gcomm://dbcm-shop-n1,dbcm-shop-n2,dbcm-shop-n3",
		"--wsrep_sst_method=mariabackup",
		"--binlog_format=ROW",
		"--innodb_autoinc_lock_mode=2",
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
	for _, bad := range []string{BootstrapFlag, "--wsrep_cluster_address=gcomm://", "--datadir=/tmp", "--wsrep_on=OFF"} {
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
