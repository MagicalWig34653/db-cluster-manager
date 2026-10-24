// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

// addConfigObjects adds Swarm configs to the fixture:
//
//	cfg-1        owned config "sftp-users", allow-listed
//	cfg-foreign  config "other-cfg" of another installation, allow-listed
//	cfg-nolabel  config "nolabel-cfg" without labels, allow-listed
//	cfg-unlisted owned config "unlisted-cfg", not allow-listed
func addConfigObjects(f *fixture) {
	f.t.Helper()
	own := map[string]string{LabelInstallation: testInstallation}
	for _, o := range []Object{
		{Kind: KindConfig, ID: "cfg-1", Name: "sftp-users", Labels: own},
		{Kind: KindConfig, ID: "cfg-foreign", Name: "other-cfg", Labels: map[string]string{LabelInstallation: "other"}},
		{Kind: KindConfig, ID: "cfg-nolabel", Name: "nolabel-cfg"},
		{Kind: KindConfig, ID: "cfg-unlisted", Name: "unlisted-cfg", Labels: own},
	} {
		f.docker.objects[o.ID] = o
	}
	wantStatus(f.t, f.mustDo(1, "setup", "allow-cfg", KindAllowObjects,
		AllowObjectsArgs{IDs: []string{"cfg-1", "cfg-foreign", "cfg-nolabel"}}), StatusSucceeded, "")
}

func TestCreateServiceKeepsPortsAndPinsConfigs(t *testing.T) {
	f := newFixture(t)
	addConfigObjects(f)
	s := spec("sftp-1")
	s.Ports = []PortSpec{{Target: 22, Published: 2222, Mode: PortModeHost}, {Target: 53, Published: 5353, Protocol: PortProtocolUDP}}
	s.Configs = []ConfigRef{{Config: "sftp-users", Target: "/etc/dbcm/sftp-users.json", Mode: 0o440}} // by name
	r := f.mustDo(1, "op", "create", KindCreateService, s)
	wantStatus(t, r, StatusSucceeded, "")
	var out struct{ ID string }
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	got := f.docker.specs[out.ID]
	if !reflect.DeepEqual(got.Ports, s.Ports) {
		t.Fatalf("ports %+v, want %+v", got.Ports, s.Ports)
	}
	want := []ConfigRef{{Config: "cfg-1", Target: "/etc/dbcm/sftp-users.json", Mode: 0o440}}
	if !reflect.DeepEqual(got.Configs, want) {
		t.Fatalf("configs not pinned to the checked ID: %+v, want %+v", got.Configs, want)
	}
}

func TestCreateServiceRefusesUnownedConfigs(t *testing.T) {
	cases := []struct {
		name   string
		config string
		status Status
		code   Code
	}{
		{"foreign config", "other-cfg", StatusRefused, CodeNotOwned},
		{"foreign config by ID", "cfg-foreign", StatusRefused, CodeNotOwned},
		{"unlabelled config", "nolabel-cfg", StatusRefused, CodeNotOwned},
		{"config not in allow-list", "unlisted-cfg", StatusRefused, CodeNotOwned},
		{"secret referenced as config", "sec-1", StatusRefused, CodeNotOwned},
		{"missing config", "nope", StatusFailed, CodeNotFound},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			addRefObjects(f)
			addConfigObjects(f)
			s := spec("sftp-1")
			s.Configs = []ConfigRef{{Config: c.config, Target: "/etc/x"}}
			wantStatus(t, f.mustDo(1, "op", "create-"+string(rune('a'+i)), KindCreateService, s), c.status, c.code)
			if n := countPrefix(f.docker.Calls(), "create-service"); n != 0 {
				t.Fatalf("service created despite %s", c.name)
			}
		})
	}
}

func TestCreateServiceRejectsInvalidPortsAndConfigs(t *testing.T) {
	f := newFixture(t)
	mk := func(name string, mut func(*ServiceSpec)) ServiceSpec {
		s := spec(name)
		mut(&s)
		return s
	}
	bad := map[string]ServiceSpec{
		"a": mk("a", func(s *ServiceSpec) { s.Ports = []PortSpec{{Target: 0, Published: 80}} }),
		"b": mk("b", func(s *ServiceSpec) { s.Ports = []PortSpec{{Target: 80, Published: 0}} }),
		"c": mk("c", func(s *ServiceSpec) { s.Ports = []PortSpec{{Target: 80, Published: 80, Protocol: "sctp"}} }),
		"d": mk("d", func(s *ServiceSpec) { s.Ports = []PortSpec{{Target: 80, Published: 80, Mode: "bridge"}} }),
		"e": mk("e", func(s *ServiceSpec) { s.Ports = []PortSpec{{Target: 80, Published: 80}, {Target: 81, Published: 80}} }),
		"f": mk("f", func(s *ServiceSpec) { s.Configs = []ConfigRef{{Config: "", Target: "/x"}} }),
		"g": mk("g", func(s *ServiceSpec) { s.Configs = []ConfigRef{{Config: "c", Target: "relative"}} }),
		"h": mk("h", func(s *ServiceSpec) { s.Configs = []ConfigRef{{Config: "c", Target: "/run/secrets/x"}} }),
		"i": mk("i", func(s *ServiceSpec) { s.Configs = []ConfigRef{{Config: "c", Target: "/a/../b"}} }),
		"j": mk("j", func(s *ServiceSpec) {
			s.Configs = []ConfigRef{{Config: "c", Target: "/a"}, {Config: "d", Target: "/a"}}
		}),
		"k": mk("k", func(s *ServiceSpec) { s.Configs = []ConfigRef{{Config: "c", Target: "/a", Mode: 0o1777}} }),
	}
	for name, s := range bad {
		if _, err := f.do(1, "op", "bad-"+name, KindCreateService, s); err == nil {
			t.Errorf("spec %s accepted", name)
		}
	}
	if len(f.docker.Calls()) != 0 {
		t.Fatalf("docker called for invalid specs: %v", f.docker.Calls())
	}
}

func TestUpdateServiceChecksConfigOwnership(t *testing.T) {
	f, s, id := updateFixture(t)
	addConfigObjects(f)
	s.Ports = []PortSpec{{Target: 22, Published: 2222}}
	s.Configs = []ConfigRef{{Config: "other-cfg", Target: "/etc/x"}}
	wantStatus(t, f.mustDo(1, "op2", "upd-bad", KindUpdateService, UpdateServiceArgs{ServiceName: s.Name, Spec: s}), StatusRefused, CodeNotOwned)
	s.Configs = []ConfigRef{{Config: "sftp-users", Target: "/etc/x"}}
	wantStatus(t, f.mustDo(1, "op2", "upd-ok", KindUpdateService, UpdateServiceArgs{ServiceName: s.Name, Spec: s}), StatusSucceeded, "")
	got := f.docker.specs[id]
	if !reflect.DeepEqual(got.Configs, []ConfigRef{{Config: "cfg-1", Target: "/etc/x"}}) || !reflect.DeepEqual(got.Ports, s.Ports) {
		t.Fatalf("update lost ports or config pin: %+v", got)
	}
}

// TestServiceSpecOptionalFieldsOmitted pins that the new fields encode away
// when unused (the stable-encoding test covers the digests).
func TestServiceSpecOptionalFieldsOmitted(t *testing.T) {
	b, err := json.Marshal(ServiceSpec{Name: "n", Image: "i"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"name":"n","image":"i","replicas":0}` {
		t.Fatalf("encoding %s", b)
	}
	b, _ = json.Marshal(ServiceSpec{Name: "n", Image: "i", Ports: []PortSpec{{Target: 1, Published: 2}}, Configs: []ConfigRef{{Config: "c", Target: "/t"}}})
	if string(b) != `{"name":"n","image":"i","replicas":0,"ports":[{"target":1,"published":2}],"configs":[{"config":"c","target":"/t"}]}` {
		t.Fatalf("encoding %s", b)
	}
}
