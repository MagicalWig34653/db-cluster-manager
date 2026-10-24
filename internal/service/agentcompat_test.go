// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/orchestrate"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// Compatibility-drift tests in the style of internal/galera/agentcompat_test.go.
// The service package does not import the agent. These checks keep the
// mirrored constants and step kinds in step with internal/agent and make
// sure orchestrate.ServiceToAgentSpec carries every domain value. Fakes
// only: no Docker or Swarm is involved.

var specFieldMap = []struct {
	domain, agent string
	conv          func(any) any
}{
	{"Name", "Name", nil},
	{"Image", "Image", nil},
	{"Labels", "Labels", nil},
	{"Env", "Env", nil},
	{"Command", "Command", nil},
	{"Args", "Args", nil},
	{"User", "User", nil},
	{"Mounts[].Type", "Mounts[].Type", nil},
	{"Mounts[].Source", "Mounts[].Source", nil},
	{"Mounts[].Target", "Mounts[].Target", nil},
	{"Mounts[].ReadOnly", "Mounts[].ReadOnly", nil},
	{"Ports[].Target", "Ports[].Target", nil},
	{"Ports[].Published", "Ports[].Published", nil},
	{"Ports[].Protocol", "Ports[].Protocol", nil},
	{"Ports[].Mode", "Ports[].Mode", nil},
	{"Configs[].Config", "Configs[].Config", nil},
	{"Configs[].Target", "Configs[].Target", nil},
	{"Configs[].Mode", "Configs[].Mode", nil},
	{"Constraints", "Constraints", nil},
	{"Replicas", "Replicas", nil},
	{"Networks", "Networks", nil},
	{"Secrets", "Secrets", nil},
	{"RestartCondition", "RestartPolicy.Condition", nil},
	{"StopGracePeriod", "StopGracePeriodNS", func(v any) any { return int64(v.(time.Duration)) }},
	{"Resources.Limits.MilliCPU", "Resources.Limits.NanoCPUs", milliToNano},
	{"Resources.Limits.MemoryBytes", "Resources.Limits.MemoryBytes", nil},
	{"Resources.Reservations.MilliCPU", "Resources.Reservations.NanoCPUs", milliToNano},
	{"Resources.Reservations.MemoryBytes", "Resources.Reservations.MemoryBytes", nil},
}

// domainOnly are domain leaves with no agent counterpart. Unpublished ports
// are not converted (only the published ones are), but every leaf of a
// port is carried.
var domainOnly = map[string]bool{}

// agentUnused are agent leaves the service framework does not set.
var agentUnused = map[string]bool{
	"Hostname":                  true,
	"EndpointMode":              true,
	"RestartPolicy.DelayNS":     true,
	"RestartPolicy.MaxAttempts": true,
	"RestartPolicy.WindowNS":    true,
	"Resources.Limits.Pids":     true,
}

func milliToNano(v any) any { return v.(int64) * 1_000_000 }

func TestAgentCompatibility(t *testing.T) {
	pairs := [][2]string{
		{service.LabelCluster, agent.LabelCluster},
		{service.LabelNode, agent.LabelNode},
		{service.MarkerFileName, agent.MarkerFileName},
		{string(service.StepCreateVolume), string(agent.KindCreateVolume)},
		{string(service.StepInitIdentity), string(agent.KindInitIdentity)},
		{string(service.StepCreateService), string(agent.KindCreateService)},
		{string(service.StepRemoveService), string(agent.KindRemoveClusterService)},
		{string(service.StepRemoveVolume), string(agent.KindRemoveVolume)},
	}
	for _, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("constant %q differs from agent %q", p[0], p[1])
		}
	}
	if !reflect.DeepEqual(fieldNames(reflect.TypeFor[service.Mount]()), fieldNames(reflect.TypeFor[agent.Mount]())) {
		t.Error("service.Mount differs from agent.Mount")
	}
}

func TestConversionDropsNothing(t *testing.T) {
	var d service.ServiceSpec
	seed := 0
	fill(reflect.ValueOf(&d).Elem(), &seed)
	a, err := orchestrate.ServiceToAgentSpec(d)
	if err != nil {
		t.Fatal(err)
	}
	dl, al := map[string]reflect.Value{}, map[string]reflect.Value{}
	leaves(reflect.ValueOf(d), "", dl)
	leaves(reflect.ValueOf(a), "", al)
	mapped, targets := map[string]bool{}, map[string]bool{}
	for _, m := range specFieldMap {
		mapped[m.domain], targets[m.agent] = true, true
		gv, ok := dl[m.domain]
		if !ok {
			t.Errorf("mapping names unknown domain field %s", m.domain)
			continue
		}
		av, ok := al[m.agent]
		if !ok {
			t.Errorf("mapping names unknown agent field %s", m.agent)
			continue
		}
		if gv.IsZero() {
			t.Fatalf("fill left domain field %s zero", m.domain)
		}
		want := gv.Interface()
		if m.conv != nil {
			want = m.conv(want)
		}
		if !reflect.DeepEqual(av.Interface(), want) {
			t.Errorf("domain %s = %v converts to agent %s = %v, want %v", m.domain, gv.Interface(), m.agent, av.Interface(), want)
		}
	}
	for _, p := range sortedKeys(dl) {
		if !mapped[p] && !domainOnly[p] {
			t.Errorf("domain field %s is not converted (add it to orchestrate.ServiceToAgentSpec and specFieldMap, or declare it domain-only)", p)
		}
	}
	for p := range domainOnly {
		if _, ok := dl[p]; !ok {
			t.Errorf("domain-only field %s no longer exists", p)
		}
	}
	for _, p := range sortedKeys(al) {
		switch {
		case targets[p]:
		case agentUnused[p]:
			if !al[p].IsZero() {
				t.Errorf("agent field %s declared unused but set to %v", p, al[p].Interface())
			}
		default:
			t.Errorf("agent field %s is neither filled from the service spec nor declared unused", p)
		}
	}
}

// TestPublishedPortsConverted checks that published ports reach the agent
// spec (with defaults left to the agent) and unpublished ones stay internal.
func TestPublishedPortsConverted(t *testing.T) {
	a, err := orchestrate.ServiceToAgentSpec(service.ServiceSpec{Name: "x", Image: "i:1",
		Ports: []service.Port{{Target: 80, Published: 8080, Protocol: "udp", Mode: "host"}, {Target: 81}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []agent.PortSpec{{Target: 80, Published: 8080, Protocol: "udp", Mode: "host"}}
	if !reflect.DeepEqual(a.Ports, want) {
		t.Fatalf("ports %+v, want %+v", a.Ports, want)
	}
}

// strictArgs decodes step args into the agent's own argument type with
// unknown fields refused, like the agent's command decoder.
func strictArgs(kind agent.Kind, raw json.RawMessage) error {
	var v any
	switch kind {
	case agent.KindCreateVolume:
		v = &agent.CreateVolumeArgs{}
	case agent.KindInitIdentity:
		v = &agent.InitIdentityArgs{}
	case agent.KindCreateService:
		v = &agent.ServiceSpec{}
	case agent.KindRemoveClusterService:
		v = &agent.RemoveClusterServiceArgs{}
	case agent.KindRemoveVolume:
		v = &agent.RemoveVolumeArgs{}
	default:
		return fmt.Errorf("kind %q is not a service step", kind)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func TestPlanConvertsToAgentCommands(t *testing.T) {
	spec, _ := json.Marshal(service.ContainerSpec{
		Image:     "nginx:1.27.2",
		Volumes:   []service.Volume{{Name: "data", Target: "/data"}},
		Secrets:   []string{"s1"},
		Networks:  []string{"net"},
		Placement: service.Placement{HostID: "h1"},
		Resources: service.Resources{Limits: service.ResourceAmount{MilliCPU: 250, MemoryBytes: 1 << 28}},
	})
	ctx := service.Context{InstallationID: "inst-1", ServiceID: "svc-1", Name: "web"}
	ty, err := service.DefaultRegistry().Get("container")
	if err != nil {
		t.Fatal(err)
	}
	b := runner.ServiceBindings{OpID: "op1", InstallationID: "inst-1", VolumeBase: "/srv/dbcm",
		TokenIDs: map[string]string{"dbcm-svc-web-data": "tok1"}, StepTimeout: time.Second, ServiceHost: "mgr1"}
	for _, kind := range []string{"create", "delete"} {
		var p service.Plan
		if kind == "create" {
			p, err = ty.Plan(service.PlanInput{Context: ctx, Spec: spec})
		} else {
			p, err = ty.DeletePlan(service.PlanInput{Context: ctx, Spec: spec, RemoveVolumes: true})
		}
		if err != nil {
			t.Fatal(err)
		}
		op, err := runner.FromServicePlan(p, b)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if op.Locks[0] != "service/svc-1" {
			t.Errorf("locks %v", op.Locks)
		}
		for _, st := range op.Steps {
			if err := strictArgs(st.Command, st.Args); err != nil {
				t.Errorf("%s step %s: agent argument type refuses the args: %v", kind, st.ID, err)
			}
			if st.Command == agent.KindCreateService || st.Command == agent.KindRemoveClusterService {
				if st.Target != "mgr1" {
					t.Errorf("service command on %s, want the service host", st.Target)
				}
			} else if st.Target != "h1" {
				t.Errorf("volume command on %s, want h1", st.Target)
			}
		}
	}
	// Without a token the init-identity step cannot be built.
	p, _ := ty.Plan(service.PlanInput{Context: ctx, Spec: spec})
	if _, err := runner.FromServicePlan(p, runner.ServiceBindings{OpID: "o", InstallationID: "i", VolumeBase: "/srv/dbcm"}); err == nil {
		t.Error("missing token accepted")
	}
	// Blocked plans never become operations (a deliver-config step has no
	// agent command).
	blocked, _ := json.Marshal(service.TraefikSpec{Image: "traefik:v3.1",
		EntryPoints: []service.EntryPoint{{Name: "web", Port: 80}},
		Services:    []service.Backend{{Name: "b", Servers: []string{"http://web:80"}}},
		Routers:     []service.Router{{Name: "r", Match: service.Match{Hosts: []string{"a.example.org"}}, EntryPoints: []string{"web"}, Service: "b"}}})
	ty, _ = service.DefaultRegistry().Get("traefik")
	bp, _ := ty.Plan(service.PlanInput{Context: ctx, Spec: blocked})
	if _, err := runner.FromServicePlan(bp, b); !errors.Is(err, service.ErrNotExecutable) {
		t.Errorf("blocked plan: %v", err)
	}
}

func fill(v reflect.Value, seed *int) {
	*seed++
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			fill(v.Field(i), seed)
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), seed)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, seed)
		fill(e, seed)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.String:
		v.SetString(fmt.Sprintf("v%d", *seed))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(*seed))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(*seed))
	case reflect.Bool:
		v.SetBool(true)
	default:
		panic("fill: unsupported kind " + v.Kind().String())
	}
}

func leaves(v reflect.Value, prefix string, out map[string]reflect.Value) {
	join := func(name string) string {
		if prefix == "" {
			return name
		}
		return prefix + "." + name
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			leaves(reflect.Zero(v.Type().Elem()), prefix, out)
		} else {
			leaves(v.Elem(), prefix, out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			leaves(v.Field(i), join(v.Type().Field(i).Name), out)
		}
	case reflect.Slice:
		if k := v.Type().Elem().Kind(); k == reflect.Struct || k == reflect.Pointer {
			e := reflect.Zero(v.Type().Elem())
			if v.Len() > 0 {
				e = v.Index(0)
			}
			leaves(e, prefix+"[]", out)
			return
		}
		out[prefix] = v
	default:
		out[prefix] = v
	}
}

func sortedKeys(m map[string]reflect.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fieldNames(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		out = append(out, t.Field(i).Name+" "+t.Field(i).Type.String()+" "+string(t.Field(i).Tag))
	}
	return out
}

// TestStoragePlansConvertToAgentCommands carries the storage types through
// the runner into the agent's own argument type: published ports and config
// references must survive, and an NFS plan must never become an operation.
func TestStoragePlansConvertToAgentCommands(t *testing.T) {
	base := service.StorageBase{Image: "example/server:1.2.3", Placement: service.Placement{HostID: "h1"}}
	ctx := service.Context{InstallationID: "inst-1", ServiceID: "svc-1", Name: "files"}
	b := runner.ServiceBindings{OpID: "op1", InstallationID: "inst-1", VolumeBase: "/srv/dbcm",
		TokenIDs: map[string]string{"dbcm-svc-files-data": "tok1"}, StepTimeout: time.Second, ServiceHost: "mgr1"}
	cases := []struct {
		typ         string
		spec        any
		ports       int
		configs     int
		wantBlocked bool
	}{
		{"sftp", service.SFTPSpec{StorageBase: base, PublishedPort: 2222, UsersConfig: "sftp-users",
			Users: []service.StorageUser{{Name: "alice", UID: 1001, PasswordSecret: "alice-pw"}}}, 1, 1, false},
		{"ftp", service.FTPSpec{StorageBase: base, Passive: service.PassiveRange{Min: 30000, Max: 30004}, PassiveAddress: "10.0.0.1",
			CertSecret: "c", KeySecret: "k", Users: []service.StorageUser{{Name: "bob", UID: 1002, PasswordSecret: "bob-pw"}}}, 6, 0, false},
		{"s3", service.S3Spec{StorageBase: base, RootUserSecret: "u", RootPasswordSecret: "p", APIPublishedPort: 9000}, 1, 0, false},
		{"nfs", service.NFSSpec{StorageBase: base, Exports: []service.NFSExport{{Path: "/", Clients: []string{"10.0.0.0/24"}}}}, 1, 0, true},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			raw, _ := json.Marshal(c.spec)
			ty, err := service.DefaultRegistry().Get(c.typ)
			if err != nil {
				t.Fatal(err)
			}
			p, err := ty.Plan(service.PlanInput{Context: ctx, Spec: raw})
			if err != nil {
				t.Fatal(err)
			}
			op, err := runner.FromServicePlan(p, b)
			if c.wantBlocked {
				if !errors.Is(err, service.ErrNotExecutable) {
					t.Fatalf("blocked plan: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, st := range op.Steps {
				if err := strictArgs(st.Command, st.Args); err != nil {
					t.Fatalf("step %s: %v", st.ID, err)
				}
				if st.Command != agent.KindCreateService {
					continue
				}
				var spec agent.ServiceSpec
				if err := json.Unmarshal(st.Args, &spec); err != nil {
					t.Fatal(err)
				}
				if len(spec.Ports) != c.ports || len(spec.Configs) != c.configs {
					t.Errorf("agent spec has %d ports and %d configs, want %d and %d", len(spec.Ports), len(spec.Configs), c.ports, c.configs)
				}
				for _, p := range spec.Ports {
					if p.Published == 0 || p.Target == 0 {
						t.Errorf("port %+v", p)
					}
				}
			}
		})
	}
}
