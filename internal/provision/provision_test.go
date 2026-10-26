package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeCommands struct {
	calls   []string
	cluster bool
	fail    bool
}

func (f *fakeCommands) Run(_ context.Context, h Host, _ Credential, cmd, stdin string) (string, error) {
	f.calls = append(f.calls, h.ID+":"+cmd)
	if strings.Contains(cmd, ". /etc/os-release") {
		cluster := "none"
		if f.cluster {
			cluster = "swarm"
		}
		return "os=ubuntu\ndisk=20000000000\ntime=yes\nports=free\ncluster=" + cluster + "\ndocker=inactive\n", nil
	}
	if strings.Contains(cmd, "join-token -q") {
		return "synthetic-token", nil
	}
	if f.fail {
		return "", errors.New("fake disconnect")
	}
	if strings.Contains(cmd, "swarm join") && stdin != "synthetic-token\n" {
		return "", errors.New("missing token")
	}
	return "", nil
}

func TestPlanDigestScriptsAndForeignHost(t *testing.T) {
	req := Request{ID: "lab", Platform: Swarm, Hosts: []Host{{ID: "a", Address: "a.example.test", User: "admin", Role: "manager"}, {ID: "b", Address: "b.example.test", User: "admin", Role: "worker"}}}
	f := &fakeCommands{}
	var checks []Check
	for _, h := range req.Hosts {
		c, err := Inspect(context.Background(), f, h, Credential{}, Swarm)
		if err != nil {
			t.Fatal(err)
		}
		checks = append(checks, c)
	}
	p, err := Build(req, checks, false)
	if err != nil {
		t.Fatal(err)
	}
	q, err := Build(req, checks, false)
	if err != nil || p.Digest != q.Digest {
		t.Fatal("unstable digest", err)
	}
	if strings.Contains(strings.Join([]string{p.Scripts["a"], p.Scripts["b"]}, ""), "synthetic-token") {
		t.Fatal("token leaked")
	}
	if !strings.Contains(p.Scripts["b"], "JOIN_TOKEN") {
		t.Fatal("join script missing token placeholder")
	}
	checks[1].ClusterID = "foreign"
	if _, err := Build(req, checks, false); !errors.Is(err, ErrExistingCluster) {
		t.Fatal(err)
	}
	checks[1].ClusterID = ""
	f.cluster = true
	if _, err := Inspect(context.Background(), f, req.Hosts[0], Credential{}, Swarm); !errors.Is(err, ErrExistingCluster) {
		t.Fatal(err)
	}
	if _, err := Build(Request{ID: "k", Platform: Kubernetes, Hosts: req.Hosts}, checks, false); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	kube, err := Build(Request{ID: "k", Platform: Kubernetes, Hosts: req.Hosts}, checks, true)
	if err != nil || !strings.Contains(kube.Scripts["a"], "K3S_INSTALL_SCRIPT") {
		t.Fatal("k3s preview", err)
	}
}

func TestApplyStepUsesTokenOnlyInStdinAndStopsOnForeignCluster(t *testing.T) {
	req := Request{ID: "lab", Platform: Swarm, Hosts: []Host{{ID: "a", Address: "a.example.test", User: "admin", Role: "manager"}, {ID: "b", Address: "b.example.test", User: "admin", Role: "worker"}}}
	f := &fakeCommands{}
	checks := []Check{{HostID: "a", OS: "linux", PortsAvailable: true, TimeSynchronized: true, DiskBytes: 20 << 30}, {HostID: "b", OS: "linux", PortsAvailable: true, TimeSynchronized: true, DiskBytes: 20 << 30}}
	p, err := Build(req, checks, false)
	if err != nil {
		t.Fatal(err)
	}
	creds := map[string]Credential{"a": {}, "b": {}}
	if err := ApplyStep(context.Background(), f, p, p.Steps[1], creds); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "synthetic-token") {
			t.Fatal("token in command")
		}
	}
	f.cluster = true
	if err := ApplyStep(context.Background(), f, p, p.Steps[1], creds); !errors.Is(err, ErrExistingCluster) {
		t.Fatal(err)
	}
}
