// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// memDocker is a minimal in-memory agent.DockerAPI for end-to-end tests of
// builder output against the real executor.
type memDocker struct {
	mu      sync.Mutex
	objects map[string]agent.Object
	specs   map[string]agent.ServiceSpec
	n       int
}

func (d *memDocker) CreateService(_ context.Context, s agent.ServiceSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	id := fmt.Sprintf("svc-%d", d.n)
	d.objects[id] = agent.Object{Kind: agent.KindService, ID: id, Name: s.Name, Labels: maps.Clone(s.Labels),
		Mounts: s.Mounts, Constraints: s.Constraints, Replicas: s.Replicas}
	d.specs[id] = s
	return id, nil
}

func (d *memDocker) UpdateService(_ context.Context, id string, s agent.ServiceSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.objects[id]
	if !ok || o.Kind != agent.KindService {
		return agent.ErrObjectNotFound
	}
	o.Labels, o.Mounts, o.Constraints, o.Replicas = maps.Clone(s.Labels), s.Mounts, s.Constraints, s.Replicas
	d.objects[id], d.specs[id] = o, s
	return nil
}

func (d *memDocker) CreateVolume(_ context.Context, v agent.VolumeSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.objects[v.Name] = agent.Object{Kind: agent.KindVolume, ID: v.Name, Name: v.Name, Labels: maps.Clone(v.Labels), Mountpoint: v.Device}
	return nil
}

func (d *memDocker) RemoveService(context.Context, string) error { return agent.ErrObjectNotFound }
func (d *memDocker) ScaleService(context.Context, string, uint64) error {
	return agent.ErrObjectNotFound
}
func (d *memDocker) RemoveVolume(context.Context, string) error { return agent.ErrObjectNotFound }

func (d *memDocker) Inspect(_ context.Context, kind agent.ObjectKind, id string) (agent.Object, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if o, ok := d.objects[id]; ok {
		return o, nil
	}
	for _, o := range d.objects {
		if o.Kind == kind && o.Name == id && (kind == agent.KindNetwork || kind == agent.KindSecret) {
			return o, nil
		}
	}
	return agent.Object{}, agent.ErrObjectNotFound
}

func (d *memDocker) ListByLabel(_ context.Context, kind agent.ObjectKind, key, value string) ([]agent.Object, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []agent.Object
	for _, o := range d.objects {
		if o.Kind == kind && o.Labels[key] == value {
			out = append(out, o)
		}
	}
	return out, nil
}

// TestGaleraCreatePlanRunsAgainstRealExecutor converts a 3-node create plan
// and sends every command step, in plan order, to the real agent.Executor of
// its target host. No step may be rejected or refused, and the bootstrap
// flag must be gone from the final stored spec.
func TestGaleraCreatePlanRunsAgainstRealExecutor(t *testing.T) {
	p := createPlan(t)
	// init-volume-dirs really chowns; an unprivileged test may only give the
	// directories to its own IDs.
	for i := range p.Steps {
		if p.Steps[i].Kind == galera.StepInitVolumeDirs {
			p.Steps[i].Owner = &galera.DirOwner{UID: os.Getuid(), GID: os.Getgid()}
		}
	}
	root := t.TempDir()
	hostBase := map[string]string{}
	execs := map[string]*agent.Executor{}
	dockers := map[string]*memDocker{}
	for _, h := range []string{"h1", "h2", "h3"} {
		base := filepath.Join(root, h, "volumes")
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		hostBase[h] = base
		own := map[string]string{agent.LabelInstallation: "inst-1"}
		d := &memDocker{specs: map[string]agent.ServiceSpec{}, objects: map[string]agent.Object{
			"net-1":    {Kind: agent.KindNetwork, ID: "net-1", Name: "dbcm-shop-galera", Labels: own},
			"secret-1": {Kind: agent.KindSecret, ID: "secret-1", Name: "dbcm-shop-root-pw", Labels: own},
		}}
		g, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(root, h, "term")))
		if err != nil {
			t.Fatal(err)
		}
		e, err := agent.NewExecutor(agent.Config{InstallationID: "inst-1", StatePath: filepath.Join(root, h, "state.json"),
			VolumeBase: base, AllowedIDs: []string{"net-1", "secret-1"}, Guard: g, Docker: d})
		if err != nil {
			t.Fatal(err)
		}
		execs[h], dockers[h] = e, d
	}
	b := createBindings("op-e2e")
	// The builder paths must lie below each agent's volume base; the real
	// bases differ per host here, so build one operation per host base.
	opByHost := map[string]Operation{}
	for h, base := range hostBase {
		b.VolumeBase = base
		op, err := FromGaleraPlan(p, b)
		if err != nil {
			t.Fatal(err)
		}
		opByHost[h] = op
	}
	steps := opByHost["h1"].Steps
	for i, s := range steps {
		if s.Command == "" {
			continue
		}
		hostStep := opByHost[s.Target].Steps[i]
		if s.Command == agent.KindUpdateService {
			var had bool
			for _, sp := range dockers[s.Target].specs {
				had = had || slices.Contains(sp.Args, galera.BootstrapFlag)
			}
			if !had {
				t.Fatal("bootstrap node did not carry the flag before update-service")
			}
		}
		res, err := execs[s.Target].Handle(context.Background(), agent.Command{
			Term: 1, OpID: "op-e2e", StepID: hostStep.ID, Kind: hostStep.Command, Args: hostStep.Args})
		if err != nil {
			t.Fatalf("step %s (%s on %s): rejected: %v", s.ID, s.Command, s.Target, err)
		}
		if res.Status != agent.StatusSucceeded {
			t.Fatalf("step %s (%s on %s): %+v", s.ID, s.Command, s.Target, res)
		}
	}
	// Volumes exist below each host's base with identity markers (written by
	// init-identity, not create-volume), and the services are stored.
	for _, n := range p.Nodes {
		dir := filepath.Join(hostBase[n.HostID], n.Volume)
		if _, err := os.Stat(filepath.Join(dir, agent.MarkerFileName)); err != nil {
			t.Errorf("node %s: no marker: %v", n.Slot, err)
		}
		for _, d := range []string{galera.DataDirName, galera.RunDirName} {
			if fi, err := os.Lstat(filepath.Join(dir, d)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
				t.Errorf("node %s: %s not prepared: %v %v", n.Slot, d, fi, err)
			}
		}
		vol := dockers[n.HostID].objects[n.Volume]
		if vol.Labels[agent.LabelNode] != n.NodeUUID || vol.Labels[agent.LabelCluster] != "clu-1" || vol.Labels[agent.LabelInstallation] != "inst-1" {
			t.Errorf("node %s volume labels %v", n.Slot, vol.Labels)
		}
	}
	var stored int
	for h, d := range dockers {
		for _, s := range d.specs {
			stored++
			if slices.Contains(s.Args, galera.BootstrapFlag) {
				t.Errorf("host %s service %s still has %s: %v", h, s.Name, galera.BootstrapFlag, s.Args)
			}
		}
	}
	if stored != 3 {
		t.Fatalf("%d stored services, want 3", stored)
	}
}
