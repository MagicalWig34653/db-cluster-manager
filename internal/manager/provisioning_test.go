package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

type fakeProvisionCommands struct {
	calls   int
	cluster bool
}

func (f *fakeProvisionCommands) Run(_ context.Context, _ provision.Host, _ provision.Credential, cmd, _ string) (string, error) {
	f.calls++
	if strings.Contains(cmd, ". /etc/os-release") {
		return "os=ubuntu\ndisk=20000000000\ntime=yes\nports=free\ncluster=none\ndocker=inactive\n", nil
	}
	if strings.Contains(cmd, ".Swarm.Cluster.ID") {
		return "synthetic-cluster-id\n", nil
	}
	return "", nil
}
func TestProvisionCredentialsSealedAndOwnedStepReplayed(t *testing.T) {
	c := newCluster(t, 1, nil)
	svc := c.leader()
	store := svc.Store()
	commands := &fakeProvisionCommands{}
	h := provision.Host{ID: "a", Address: "a.example.test", User: "admin", Role: "manager"}
	p, err := provision.Build(provision.Request{ID: "lab", Platform: provision.Swarm, Hosts: []provision.Host{h}}, []provision.Check{{HostID: "a", OS: "linux", PortsAvailable: true, TimeSynchronized: true, DiskBytes: 20 << 30}}, false)
	if err != nil {
		t.Fatal(err)
	}
	draft := ProvisionDraft{Plan: p, Credentials: map[string]provision.Credential{"a": {PrivateKey: "synthetic-private-key", KnownHosts: "a.example.test ssh-ed25519 synthetic"}}}
	if err := PutSealed(store, testKey, CollProvisionPlans, p.ID, draft); err != nil {
		t.Fatal(err)
	}
	raw, ok, err := store.Raw(CollProvisionPlans, p.ID)
	if err != nil || !ok || bytes.Contains(raw.Value, []byte("synthetic-private-key")) {
		t.Fatal("unsealed key", err)
	}
	args, _ := json.Marshal(map[string]string{"planId": p.ID, "stepId": p.Steps[0].ID})
	client := ProvisionAgentClient{Store: store, Keys: testKey, Commands: commands}
	cmd := agent.Command{OpID: "op", StepID: "host-01", Kind: provisionCommand, Args: args}
	result, err := client.Send(context.Background(), "provision/lab/a", cmd)
	if err != nil || result.Status != agent.StatusSucceeded {
		t.Fatal(result, err)
	}
	owned, ok, err := Get[ProvisionOwnership](store, CollProvisionOwnership, p.ID)
	if err != nil || !ok || owned.Value.ClusterID != "synthetic-cluster-id" || !owned.Value.Hosts["a"] {
		t.Fatal(owned, err)
	}
	before := commands.calls
	result, err = client.Send(context.Background(), "provision/lab/a", cmd)
	if err != nil || result.Status != agent.StatusSucceeded || commands.calls != before+1 {
		t.Fatal("replay mutated host", result, err)
	}
}
