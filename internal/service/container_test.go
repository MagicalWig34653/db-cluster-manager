// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// These tests use only the pure domain code. Nothing here ran against a
// Swarm or Docker.

func ctx() Context { return Context{InstallationID: "inst-1", ServiceID: "svc-abc", Name: "web"} }

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func goodContainer() ContainerSpec {
	return ContainerSpec{
		Image: "nginx:1.27.2",
		Env:   []EnvVar{{Name: "TZ", Value: "UTC"}, {Name: "APP_MODE", Value: "prod"}},
		Ports: []Port{{Target: 8080}},
		Volumes: []Volume{
			{Name: "data", Target: "/var/lib/web"},
			{Name: "cache", Target: "/var/cache/web", ReadOnly: false},
		},
		Networks:  []string{"frontend"},
		Secrets:   []string{"web-key", "web-cert"},
		Resources: Resources{Limits: ResourceAmount{MilliCPU: 500, MemoryBytes: 256 << 20}},
		Placement: Placement{HostID: "h1", NodeLabels: map[string]string{"zone": "a"}},
		Restart:   RestartOnFailure, StopGraceSeconds: 20,
	}
}

func TestContainerValidateRefusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ContainerSpec)
		want string
	}{
		{"unpinned image", func(c *ContainerSpec) { c.Image = "nginx" }, "pinned"},
		{"latest tag", func(c *ContainerSpec) { c.Image = "nginx:latest" }, "latest"},
		{"registry port is not a tag", func(c *ContainerSpec) { c.Image = "registry.example:5000/nginx" }, "pinned"},
		{"secret env literal", func(c *ContainerSpec) { c.Env = []EnvVar{{Name: "DB_PASSWORD", Value: "x"}} }, "looks like a secret"},
		{"url password in ordinary env", func(c *ContainerSpec) {
			c.Env = []EnvVar{{Name: "DATABASE_URL", Value: "mysql://user:password@example.test/db"}}
		}, "secret reference"},
		{"token in ordinary env", func(c *ContainerSpec) {
			c.Env = []EnvVar{{Name: "APP_OPTION", Value: "Bearer abcdefghijklmnopqrstuvwxyz"}}
		}, "secret reference"},
		{"token in args", func(c *ContainerSpec) { c.Args = []string{"--token=ghp_abcdefghijklmnopqrstuvwxyz123456"} }, "secret reference"},
		{"password in command", func(c *ContainerSpec) { c.Command = []string{"--password=hunter2"} }, "secret reference"},
		{"duplicate env", func(c *ContainerSpec) { c.Env = []EnvVar{{Name: "A", Value: "1"}, {Name: "A", Value: "2"}} }, "twice"},
		{"bad port", func(c *ContainerSpec) { c.Ports = []Port{{Target: 70000}} }, "out of range"},
		{"duplicate published port", func(c *ContainerSpec) { c.Ports = []Port{{Target: 80, Published: 80}, {Target: 81, Published: 80}} }, "used twice"},
		{"volume without host", func(c *ContainerSpec) { c.Placement.HostID = "" }, "pinned to a host"},
		{"volume with two replicas", func(c *ContainerSpec) { c.Replicas = 2 }, "exactly one replica"},
		{"volume in secrets dir", func(c *ContainerSpec) { c.Volumes[0].Target = "/run/secrets/x" }, "inside /run/secrets"},
		{"relative volume target", func(c *ContainerSpec) { c.Volumes[0].Target = "data" }, "absolute"},
		{"duplicate volume target", func(c *ContainerSpec) { c.Volumes[1].Target = c.Volumes[0].Target }, "used twice"},
		{"bad placement role", func(c *ContainerSpec) { c.Placement.Role = "root" }, "role"},
		{"reservation above limit", func(c *ContainerSpec) { c.Resources.Reservations.MemoryBytes = 1 << 30 }, "reservation"},
		{"bad restart", func(c *ContainerSpec) { c.Restart = "always" }, "restart"},
		{"bad user", func(c *ContainerSpec) { c.User = "root" }, "uid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := goodContainer()
			tc.mut(&c)
			err := Container{}.Validate(raw(t, c))
			if !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an ErrInvalidSpec mentioning %q", err, tc.want)
			}
		})
	}
	if err := (Container{}).Validate(raw(t, goodContainer())); err != nil {
		t.Fatalf("good spec: %v", err)
	}
}

func TestContainerCredentialIsAbsentFromErrorsAndPlans(t *testing.T) {
	secret := "mysql://alice:veryprivatepassword@example.test/db"
	c := goodContainer()
	c.Env = []EnvVar{{Name: "DATABASE_URL", Value: secret}}
	in := PlanInput{Context: ctx(), Spec: raw(t, c)}
	_, err := (Container{}).Plan(in)
	if !errors.Is(err, ErrInvalidSpec) || strings.Contains(err.Error(), secret) {
		t.Fatalf("secret reached plan or error: %v", err)
	}
}

func TestContainerSpecIsStrictJSON(t *testing.T) {
	for _, s := range []string{
		``, `{"image":"nginx:1","hostPath":"/etc"}`, `{"image":"nginx:1"} {}`, `[]`,
		`{"image":"nginx:1","volumes":[{"name":"a","target":"/a","source":"/etc"}]}`,
	} {
		if err := (Container{}).Validate(json.RawMessage(s)); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestContainerPlanSteps(t *testing.T) {
	p, err := Container{}.Plan(PlanInput{Context: ctx(), Spec: raw(t, goodContainer()),
		Hosts: []Host{{ID: "h1", Ready: true, Active: true}, {ID: "h2", Ready: true, Active: true}}})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []StepKind
	for _, s := range p.Steps {
		kinds = append(kinds, s.Kind)
	}
	want := []StepKind{StepCreateVolume, StepInitIdentity, StepCreateVolume, StepInitIdentity, StepCreateService}
	if !slices.Equal(kinds, want) {
		t.Fatalf("steps %v", kinds)
	}
	// Volumes sorted by name: cache first. Each identity step follows its
	// own create; the service waits for both identities.
	if p.Steps[0].Volume != "dbcm-svc-web-cache" || p.Steps[2].Volume != "dbcm-svc-web-data" {
		t.Fatalf("volumes %q %q", p.Steps[0].Volume, p.Steps[2].Volume)
	}
	if !slices.Equal(p.Steps[1].After, []string{p.Steps[0].ID}) || !slices.Equal(p.Steps[3].After, []string{p.Steps[2].ID}) {
		t.Fatalf("identity steps must follow their volume: %v %v", p.Steps[1].After, p.Steps[3].After)
	}
	svc := p.Steps[4]
	if !slices.Equal(svc.After, []string{p.Steps[1].ID, p.Steps[3].ID}) {
		t.Fatalf("service waits for %v", svc.After)
	}
	s := svc.Spec
	if s.Name != "dbcm-svc-web" || s.Replicas != 1 || s.RestartCondition != RestartOnFailure || s.StopGracePeriod.Seconds() != 20 {
		t.Fatalf("%+v", s)
	}
	if s.Labels[LabelCluster] != "svc-abc" || s.Labels[LabelServiceType] != TypeContainer || s.Labels[LabelServiceName] != "web" {
		t.Fatalf("labels %v", s.Labels)
	}
	if !slices.Equal(s.Constraints, []string{"node.id==h1", "node.labels.zone==a"}) {
		t.Fatalf("constraints %v", s.Constraints)
	}
	if !slices.Equal(s.Env, []string{"APP_MODE=prod", "TZ=UTC"}) || !slices.Equal(s.Secrets, []string{"web-cert", "web-key"}) {
		t.Fatalf("env %v secrets %v", s.Env, s.Secrets)
	}
	if len(s.Mounts) != 2 || s.Mounts[0] != (Mount{Type: "volume", Source: "dbcm-svc-web-cache", Target: "/var/cache/web"}) {
		t.Fatalf("mounts %+v", s.Mounts)
	}
	if !p.Executable() || !p.RequiresApproval || p.Kind != PlanCreate {
		t.Fatalf("%+v", p)
	}
	if len(p.Artifacts) != 1 || p.Artifacts[0].Name != "swarm-service.json" || !strings.Contains(p.Artifacts[0].Content, `"dbcm-svc-web"`) {
		t.Fatalf("artifacts %+v", p.Artifacts)
	}
	txt := p.Text()
	for _, w := range []string{"1. create-volume:", "5. create-service:", "dbcm-svc-web", "identity marker"} {
		if !strings.Contains(txt, w) {
			t.Errorf("plan text lacks %q:\n%s", w, txt)
		}
	}
	// A digest-pinned image and a memory limit leave no warning about them.
	for _, w := range p.Warnings {
		if strings.Contains(w, "memory limit") {
			t.Errorf("unexpected warning %q", w)
		}
	}
	if !slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, "tag only") }) {
		t.Errorf("warnings %v lack the tag-only hint", p.Warnings)
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	a, _ := Container{}.Plan(PlanInput{Context: ctx(), Spec: raw(t, goodContainer())})
	c := goodContainer()
	slices.Reverse(c.Volumes)
	slices.Reverse(c.Secrets)
	slices.Reverse(c.Env)
	b, _ := Container{}.Plan(PlanInput{Context: ctx(), Spec: raw(t, c)})
	if !slices.EqualFunc(a.Steps, b.Steps, func(x, y Step) bool { return string(raw(t, x)) == string(raw(t, y)) }) {
		t.Fatal("plan depends on the order of the input lists")
	}
}

func TestPublishedPortsArePlanned(t *testing.T) {
	c := goodContainer()
	c.Ports = []Port{{Target: 8080, Published: 80}, {Target: 9090}}
	p, err := Container{}.Plan(PlanInput{Context: ctx(), Spec: raw(t, c)})
	if err != nil {
		t.Fatal(err)
	}
	// The agent publishes ports now: a published port is no blocker, and
	// the spec artifact shows it for the approval.
	if !p.Executable() {
		t.Fatalf("blockers %v", p.Blockers)
	}
	var art string
	for _, a := range p.Artifacts {
		if a.Name == "swarm-service.json" {
			art = a.Content
		}
	}
	if !strings.Contains(art, `"published": 80`) {
		t.Fatalf("artifact lacks the published port:\n%s", art)
	}
}

func TestPlacementHostIsChecked(t *testing.T) {
	in := PlanInput{Context: ctx(), Spec: raw(t, goodContainer())}
	in.Hosts = []Host{{ID: "h2", Ready: true, Active: true}}
	if _, err := (Container{}).Plan(in); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), "not an enrolled host") {
		t.Fatalf("unknown host: %v", err)
	}
	in.Hosts = []Host{{ID: "h1", Ready: true, Active: false}}
	if _, err := (Container{}).Plan(in); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), "ready and active") {
		t.Fatalf("inactive host: %v", err)
	}
}

func TestContextIsChecked(t *testing.T) {
	for _, c := range []Context{{}, {InstallationID: "i", ServiceID: "s", Name: "Bad Name"}, {InstallationID: "i", Name: "web"}} {
		if _, err := (Container{}).Plan(PlanInput{Context: c, Spec: raw(t, goodContainer())}); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestContainerDeletePlan(t *testing.T) {
	spec := raw(t, goodContainer())
	create, err := Container{}.Plan(PlanInput{Context: ctx(), Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := Container{}.DeletePlan(PlanInput{Context: ctx(), Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if len(keep.Steps) != 1 || keep.Steps[0].Kind != StepRemoveService || !keep.Steps[0].Destructive || keep.Steps[0].ServiceID != "svc-abc" {
		t.Fatalf("%+v", keep.Steps)
	}
	if len(keep.Warnings) != 2 || !strings.Contains(keep.Warnings[0], "are kept") {
		t.Fatalf("warnings %v", keep.Warnings)
	}
	all, err := Container{}.DeletePlan(PlanInput{Context: ctx(), Spec: spec, RemoveVolumes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Steps) != 3 || all.Steps[1].Kind != StepRemoveVolume || !all.Steps[1].Destructive || !slices.Equal(all.Steps[1].After, []string{all.Steps[0].ID}) {
		t.Fatalf("%+v", all.Steps)
	}
	// The deletion verifies the identity the create wrote, derived from the
	// service ID alone (a failed create can be discarded without state).
	identity := map[string]string{}
	for _, s := range create.Steps {
		if s.Kind == StepInitIdentity {
			identity[s.Volume] = s.NodeUUID
		}
	}
	for _, s := range all.Steps[1:] {
		if identity[s.Volume] == "" || identity[s.Volume] != s.NodeUUID {
			t.Errorf("volume %s: delete identity %q, create identity %q", s.Volume, s.NodeUUID, identity[s.Volume])
		}
	}
	if !slices.ContainsFunc(all.Warnings, func(w string) bool { return strings.Contains(w, "stays on the host") }) {
		t.Errorf("warnings %v", all.Warnings)
	}
}

func TestVolumeIdentity(t *testing.T) {
	a, b := VolumeIdentity("svc-1", "data"), VolumeIdentity("svc-1", "data")
	if a != b || a == VolumeIdentity("svc-2", "data") || a == VolumeIdentity("svc-1", "logs") {
		t.Fatalf("identity must be stable and depend on service and volume: %s", a)
	}
	if len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Fatalf("not UUID-shaped: %s", a)
	}
}

func TestHealth(t *testing.T) {
	cases := []struct {
		o    Observation
		want HealthState
	}{
		{Observation{}, HealthUnknown},
		{Observation{Known: true, Running: 1, Desired: 1}, HealthHealthy},
		{Observation{Known: true, Running: 1, Desired: 3}, HealthDegraded},
		{Observation{Known: true, Running: 0, Desired: 2, Reason: "no suitable node"}, HealthUnhealthy},
	}
	for _, tc := range cases {
		h, err := Container{}.Health(HealthInput{Context: ctx(), Spec: raw(t, goodContainer()), Observed: tc.o})
		if err != nil || h.State != tc.want {
			t.Errorf("%+v: %v %v", tc.o, h, err)
		}
	}
	if h, _ := (Container{}).Health(HealthInput{Context: ctx(), Spec: raw(t, goodContainer()), Observed: Observation{Known: true, Desired: 1, Reason: "boom"}}); !strings.Contains(h.Reasons[0], "boom") {
		t.Errorf("%v", h)
	}
	if _, err := (Container{}).Health(HealthInput{Spec: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("invalid spec: %v", err)
	}
}

func TestRegistry(t *testing.T) {
	r := DefaultRegistry()
	if got := r.Names(); !slices.Equal(got, []string{"container", "ftp", "galera", "nfs", "s3", "sftp", "traefik"}) {
		t.Fatalf("names %v", got)
	}
	if ty, err := r.Get("traefik"); err != nil || ty.Name() != "traefik" {
		t.Fatalf("%v %v", ty, err)
	}
	if _, err := r.Get("mongodb"); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("unknown type: %v", err)
	}
	if _, err := NewRegistry(Container{}, Container{}); err == nil {
		t.Fatal("duplicate registration accepted")
	}
}
