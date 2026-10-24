// SPDX-License-Identifier: AGPL-3.0-only
package runner

import (
	"context"
	"encoding/json"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
	"os"
	"path/filepath"
	"testing"
)

// Real planner, conversion and executor; Docker is an in-memory fake only.
func TestServiceConfigPlansRunAgainstExecutor(t *testing.T) {
	base := service.StorageBase{Image: "example/server:1", Placement: service.Placement{HostID: "h1"}}
	cases := []struct {
		ty   service.Type
		spec any
	}{
		{service.Traefik{}, service.TraefikSpec{Image: "traefik:v3.1", EntryPoints: []service.EntryPoint{{Name: "web", Port: 80}}, Services: []service.Backend{{Name: "app", Servers: []string{"http://app:80"}}}}},
		{service.SFTP{}, service.SFTPSpec{StorageBase: base, Users: []service.StorageUser{{Name: "alice", UID: 1001, PasswordSecret: "password"}}}},
		{service.FTP{}, service.FTPSpec{StorageBase: base, Passive: service.PassiveRange{Min: 30000, Max: 30001}, PassiveAddress: "10.0.0.1", CertSecret: "cert", KeySecret: "key", Users: []service.StorageUser{{Name: "alice", UID: 1001, PasswordSecret: "password"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.ty.Name(), func(t *testing.T) {
			root := t.TempDir()
			volumeBase := filepath.Join(root, "volumes")
			if err := os.Mkdir(volumeBase, 0700); err != nil {
				t.Fatal(err)
			}
			d := &memDocker{objects: map[string]agent.Object{}, specs: map[string]agent.ServiceSpec{}}
			for _, name := range []string{"password", "cert", "key"} {
				d.objects[name] = agent.Object{Kind: agent.KindSecret, ID: name, Name: name, Labels: map[string]string{agent.LabelInstallation: "inst"}}
			}
			guard, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(root, "term")))
			if err != nil {
				t.Fatal(err)
			}
			e, err := agent.NewExecutor(agent.Config{InstallationID: "inst", StatePath: filepath.Join(root, "state.json"), VolumeBase: volumeBase, AllowedIDs: []string{"password", "cert", "key"}, Guard: guard, Docker: d})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			in := service.PlanInput{Context: service.Context{InstallationID: "inst", ServiceID: "svc", Name: "files"}, Spec: raw}
			p, err := tc.ty.Plan(in)
			if err != nil {
				t.Fatal(err)
			}
			b := ServiceBindings{OpID: "create", InstallationID: "inst", VolumeBase: volumeBase, ServiceHost: "h1", TokenIDs: map[string]string{service.VolumeName("files", "data"): "token"}}
			run := func(p service.Plan) {
				t.Helper()
				op, err := FromServicePlan(p, b)
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range op.Steps {
					res, err := e.Handle(context.Background(), agent.Command{Term: 1, OpID: b.OpID, StepID: s.ID, Kind: s.Command, Args: s.Args})
					if err != nil || res.Status != agent.StatusSucceeded {
						t.Fatalf("%s: %+v %v", s.ID, res, err)
					}
				}
			}
			run(p)
			if len(d.specs) != 1 {
				t.Fatal(d.specs)
			}
			for _, s := range d.specs {
				if len(s.Configs) != 1 {
					t.Fatal(s)
				}
				if o, ok := d.objects[s.Configs[0].Config]; !ok || o.Kind != agent.KindConfig {
					t.Fatal("config was not pinned to created ID")
				}
			}
			p, err = tc.ty.DeletePlan(in)
			if err != nil {
				t.Fatal(err)
			}
			b.OpID = "delete"
			run(p)
			for _, o := range d.objects {
				if o.Kind == agent.KindConfig || o.Kind == agent.KindService {
					t.Fatal("managed object remains", o)
				}
			}
		})
	}
}
