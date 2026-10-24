// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Pure domain tests of the storage protocol server types. Nothing here ran
// against a Swarm, Docker or a storage server image.

const digestImage = "example/server@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func base() StorageBase {
	return StorageBase{
		Image:     "example/server:1.2.3",
		Placement: Placement{HostID: "h1"},
		Networks:  []string{"edge"},
		Resources: Resources{Limits: ResourceAmount{MilliCPU: 500, MemoryBytes: 512 << 20}},
	}
}

func goodSFTP() SFTPSpec {
	return SFTPSpec{
		StorageBase:   base(),
		PublishedPort: 2222,
		Users: []StorageUser{
			{Name: "alice", UID: 1001, Home: "/alice", AuthorizedKeysSecret: "alice-keys"},
			{Name: "bob", UID: 1002, PasswordSecret: "bob-pw"},
		},
		HostKeySecret: "sftp-hostkey",
		UsersConfig:   "sftp-users",
	}
}

func goodFTP() FTPSpec {
	return FTPSpec{
		StorageBase:    base(),
		Passive:        PassiveRange{Min: 30000, Max: 30009},
		PassiveAddress: "files.example.org",
		CertSecret:     "ftp-cert", KeySecret: "ftp-key",
		Users: []StorageUser{{Name: "carol", UID: 2001, PasswordSecret: "carol-pw"}},
	}
}

func goodS3() S3Spec {
	b := base()
	return S3Spec{
		StorageBase:    b,
		RootUserSecret: "s3-user", RootPasswordSecret: "s3-pass",
		Console:          true,
		APIPublishedPort: 0,
		APIRoute:         &StorageRoute{Hosts: []string{"s3.example.org"}, EntryPoints: []string{"websecure"}, TLS: true, CertResolver: "le"},
		ConsoleRoute:     &StorageRoute{Hosts: []string{"console.example.org"}, EntryPoints: []string{"websecure"}, TLS: true},
		RouteNetwork:     "edge",
	}
}

func goodNFS() NFSSpec {
	return NFSSpec{
		StorageBase: base(),
		Exports: []NFSExport{
			{Path: "/shared", Clients: []string{"10.0.0.0/24", "192.168.1.5"}},
			{Path: "/ro", Clients: []string{"10.0.0.0/24"}, ReadOnly: true, Squash: NFSAllSquash},
		},
	}
}

func planOf(t *testing.T, ty Type, spec any) Plan {
	t.Helper()
	p, err := ty.Plan(PlanInput{Context: ctx(), Spec: raw(t, spec)})
	if err != nil {
		t.Fatalf("%s plan: %v", ty.Name(), err)
	}
	return p
}

func svcOf(t *testing.T, p Plan) *ServiceSpec {
	t.Helper()
	for _, s := range p.Steps {
		if s.Kind == StepCreateService {
			return s.Spec
		}
	}
	t.Fatal("no create-service step")
	return nil
}

func artifact(p Plan, name string) (string, bool) {
	for _, a := range p.Artifacts {
		if a.Name == name {
			return a.Content, true
		}
	}
	return "", false
}

func TestStorageTypesRegistered(t *testing.T) {
	r := DefaultRegistry()
	for _, n := range []string{TypeSFTP, TypeFTP, TypeS3, TypeNFS} {
		if _, err := r.Get(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

// Every type: pinned image rule, host pinning, one data volume with the
// identity steps, ownership labels, one replica, no secret values.
func TestStoragePlansShareTheContainerPlan(t *testing.T) {
	cases := []struct {
		ty   Type
		spec any
	}{{SFTP{}, goodSFTP()}, {FTP{}, goodFTP()}, {S3{}, goodS3()}, {NFS{}, goodNFS()}}
	for _, c := range cases {
		t.Run(c.ty.Name(), func(t *testing.T) {
			p := planOf(t, c.ty, c.spec)
			kinds := []StepKind{}
			for _, s := range p.Steps {
				kinds = append(kinds, s.Kind)
			}
			want := []StepKind{StepCreateVolume, StepInitIdentity, StepCreateService}
			if c.ty.Name() == TypeFTP {
				want = []StepKind{StepCreateVolume, StepInitIdentity, StepCreateConfig, StepCreateService}
			}
			if !slices.Equal(kinds, want) {
				t.Fatalf("steps %v", kinds)
			}
			if p.Steps[0].HostID != "h1" || p.Steps[0].Volume != VolumeName("web", "data") {
				t.Errorf("volume step %+v", p.Steps[0])
			}
			svc := svcOf(t, p)
			if svc.Replicas != 1 || !slices.Equal(svc.Constraints, []string{"node.id==h1"}) {
				t.Errorf("not pinned to one task on h1: %+v", svc)
			}
			if svc.Labels[LabelCluster] != "svc-abc" || svc.Labels[LabelServiceType] != c.ty.Name() {
				t.Errorf("labels %v", svc.Labels)
			}
			if len(svc.Mounts) != 1 || svc.Mounts[0].Source != VolumeName("web", "data") {
				t.Errorf("mounts %+v", svc.Mounts)
			}
			if !slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, "tag only") }) {
				t.Errorf("tag-only image gave no warning: %v", p.Warnings)
			}
			if !slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, "backup") }) {
				t.Errorf("no backup warning: %v", p.Warnings)
			}
			// The derived container passes the generic container rules
			// (no literal secret env, clean targets).
			if err := derivedContainer(c.spec).validate(); err != nil {
				t.Errorf("derived container: %v", err)
			}
			// Delete plan: service first, volume kept unless asked.
			dp, err := c.ty.DeletePlan(PlanInput{Context: ctx(), Spec: raw(t, c.spec)})
			if err != nil || len(dp.Steps) != 1+configCount(c.ty.Name()) || dp.Steps[0].Kind != StepRemoveService || len(dp.Warnings) != 1 {
				t.Errorf("delete plan %+v err %v", dp, err)
			}
			dp, _ = c.ty.DeletePlan(PlanInput{Context: ctx(), Spec: raw(t, c.spec), RemoveVolumes: true})
			if len(dp.Steps) != 2+configCount(c.ty.Name()) || dp.Steps[1].Kind != StepRemoveVolume || dp.Steps[1].HostID != "h1" {
				t.Errorf("delete plan with volumes %+v", dp.Steps)
			}
			// Health is unknown without an observation.
			h, err := c.ty.Health(HealthInput{Context: ctx(), Spec: raw(t, c.spec)})
			if err != nil || h.State != HealthUnknown {
				t.Errorf("health %+v %v", h, err)
			}
		})
	}
}

func derivedContainer(spec any) ContainerSpec {
	switch s := spec.(type) {
	case SFTPSpec:
		return s.container()
	case FTPSpec:
		return s.container()
	case S3Spec:
		return s.container()
	case NFSSpec:
		return s.container()
	}
	panic("unknown spec")
}

func TestStorageCommonRefusals(t *testing.T) {
	type mut func(*StorageBase)
	cases := []struct {
		name string
		mut  mut
		want string
	}{
		{"unpinned image", func(b *StorageBase) { b.Image = "example/server" }, "pinned"},
		{"latest", func(b *StorageBase) { b.Image = "example/server:latest" }, "latest"},
		{"digest required", func(b *StorageBase) { b.RequireDigest = true }, "by digest"},
		{"no host", func(b *StorageBase) { b.Placement.HostID = "" }, "placement.hostId"},
		{"bad role", func(b *StorageBase) { b.Placement.Role = "boss" }, "role"},
		{"bad network", func(b *StorageBase) { b.Networks = []string{"a b"} }, "network"},
		{"bad restart", func(b *StorageBase) { b.Restart = "sometimes" }, "restart"},
		{"negative resources", func(b *StorageBase) { b.Resources.Limits.MemoryBytes = -1 }, "negative"},
	}
	for _, c := range cases {
		for name, build := range map[string]func(StorageBase) (Type, any){
			"sftp": func(b StorageBase) (Type, any) { s := goodSFTP(); s.StorageBase = b; return SFTP{}, s },
			"ftp":  func(b StorageBase) (Type, any) { s := goodFTP(); s.StorageBase = b; return FTP{}, s },
			"s3":   func(b StorageBase) (Type, any) { s := goodS3(); s.StorageBase = b; return S3{}, s },
			"nfs":  func(b StorageBase) (Type, any) { s := goodNFS(); s.StorageBase = b; return NFS{}, s },
		} {
			b := base()
			c.mut(&b)
			ty, spec := build(b)
			err := ty.Validate(raw(t, spec))
			if !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s/%s: err %v, want %q", name, c.name, err, c.want)
			}
		}
	}
}

func TestDigestImageHasNoTagWarning(t *testing.T) {
	s := goodSFTP()
	s.Image, s.RequireDigest = digestImage, true
	p := planOf(t, SFTP{}, s)
	if slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, "tag only") }) {
		t.Fatalf("digest image warned: %v", p.Warnings)
	}
}

func TestUnknownFieldsAreRefused(t *testing.T) {
	for _, ty := range []Type{SFTP{}, FTP{}, S3{}, NFS{}} {
		if err := ty.Validate([]byte(`{"image":"x:1","privileged":true}`)); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: %v", ty.Name(), err)
		}
	}
}

// --- SFTP ---

func TestSFTPPlan(t *testing.T) {
	p := planOf(t, SFTP{}, goodSFTP())
	svc := svcOf(t, p)
	if len(svc.Ports) != 1 || svc.Ports[0] != (Port{Target: 22, Published: 2222, Protocol: "tcp", Mode: "host"}) {
		t.Fatalf("ports %+v", svc.Ports)
	}
	if !slices.Equal(svc.Secrets, []string{"alice-keys", "bob-pw", "sftp-hostkey"}) {
		t.Fatalf("secrets %v", svc.Secrets)
	}
	if !slices.Equal(svc.Configs, []ConfigRef{{Config: "sftp-users", Target: SFTPUsersFile}}) {
		t.Fatalf("configs %+v", svc.Configs)
	}
	if !p.Executable() {
		t.Fatalf("blockers %v", p.Blockers)
	}
	users, ok := artifact(p, "sftp-users.json")
	if !ok || !strings.Contains(users, "/run/secrets/alice-keys") || !strings.Contains(users, `"name": "bob"`) {
		t.Fatalf("users artifact:\n%s", users)
	}
	if _, ok := artifact(p, "published-ports.json"); !ok {
		t.Fatal("no ports artifact")
	}
	w := strings.Join(p.Warnings, "\n")
	if !strings.Contains(w, "password only") {
		t.Errorf("no password-only warning:\n%s", w)
	}
	if strings.Contains(w, "usersConfig") || strings.Contains(w, "host key changes") {
		t.Errorf("unexpected warnings:\n%s", w)
	}
}

func TestSFTPWarnings(t *testing.T) {
	s := goodSFTP()
	s.HostKeySecret, s.UsersConfig, s.PublishedPort = "", "", 22
	w := strings.Join(planOf(t, SFTP{}, s).Warnings, "\n")
	for _, want := range []string{"host key changes", "port 22"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing warning %q in:\n%s", want, w)
		}
	}
	s.PublishedPort = 0
	if !strings.Contains(strings.Join(planOf(t, SFTP{}, s).Warnings, "\n"), "no published port") {
		t.Error("internal service gave no warning")
	}
}

func TestSFTPRefusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*SFTPSpec)
		want string
	}{
		{"no users", func(s *SFTPSpec) { s.Users = nil }, "at least one user"},
		{"root uid", func(s *SFTPSpec) { s.Users[0].UID = 0 }, "uid"},
		{"duplicate uid", func(s *SFTPSpec) { s.Users[1].UID = s.Users[0].UID }, "uid"},
		{"duplicate name", func(s *SFTPSpec) { s.Users[1].Name = "alice" }, "twice"},
		{"bad name", func(s *SFTPSpec) { s.Users[0].Name = "Al ice" }, "user name"},
		{"no credential", func(s *SFTPSpec) { s.Users[1].PasswordSecret = "" }, "no credential"},
		{"relative home", func(s *SFTPSpec) { s.Users[0].Home = "alice" }, "home"},
		{"home dotdot", func(s *SFTPSpec) { s.Users[0].Home = "/a/../b" }, "home"},
		{"bad secret name", func(s *SFTPSpec) { s.Users[1].PasswordSecret = "../x" }, "secret name"},
		{"same secret", func(s *SFTPSpec) { s.Users[0].PasswordSecret = s.Users[0].AuthorizedKeysSecret }, "same secret"},
		{"port range", func(s *SFTPSpec) { s.PublishedPort = 70000 }, "out of range"},
		{"mode without port", func(s *SFTPSpec) { s.PublishedPort, s.PortMode = 0, "host" }, "portMode"},
		{"bad mode", func(s *SFTPSpec) { s.PortMode = "bridge" }, "port mode"},
		{"bad host key", func(s *SFTPSpec) { s.HostKeySecret = "a b" }, "hostKeySecret"},
		{"bad users config", func(s *SFTPSpec) { s.UsersConfig = "a b" }, "usersConfig"},
	}
	for _, c := range cases {
		s := goodSFTP()
		s.Users = slices.Clone(s.Users)
		c.mut(&s)
		if err := (SFTP{}).Validate(raw(t, s)); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

// --- FTP ---

func TestFTPPlanPassivePortsAndTLS(t *testing.T) {
	p := planOf(t, FTP{}, goodFTP())
	svc := svcOf(t, p)
	pub := svc.PublishedPorts()
	if len(pub) != 11 {
		t.Fatalf("%d published ports, want control + 10 passive", len(pub))
	}
	for _, pt := range pub {
		if pt.Published != pt.Target || pt.Mode != "host" || pt.Protocol != "tcp" {
			t.Errorf("port %+v: passive FTP needs published == target in host mode", pt)
		}
	}
	if pub[0].Published != 21 && !slices.ContainsFunc(pub, func(pt Port) bool { return pt.Published == 21 }) {
		t.Errorf("control port 21 missing: %+v", pub)
	}
	env := strings.Join(svc.Env, "\n")
	for _, want := range []string{"DBCM_FTP_MODE=ftps-explicit", "DBCM_FTP_PASSIVE_MIN=30000", "DBCM_FTP_PASSIVE_MAX=30009",
		"DBCM_FTP_PASSIVE_ADDRESS=files.example.org", "DBCM_FTP_TLS_CERT_FILE=/run/secrets/ftp-cert", "DBCM_FTP_TLS_KEY_FILE=/run/secrets/ftp-key"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s:\n%s", want, env)
		}
	}
	if !slices.Equal(svc.Secrets, []string{"carol-pw", "ftp-cert", "ftp-key"}) {
		t.Fatalf("secrets %v", svc.Secrets)
	}
	if !p.Executable() {
		t.Fatalf("blockers %v", p.Blockers)
	}
	w := strings.Join(p.Warnings, "\n")
	if !strings.Contains(w, "30000-30009") || !strings.Contains(w, "AUTH TLS") {
		t.Errorf("warnings:\n%s", w)
	}
}

func TestFTPImplicitDefaultsToPort990AndPlainNeedsConsent(t *testing.T) {
	f := goodFTP()
	f.Mode = FTPImplicitTLS
	svc := svcOf(t, planOf(t, FTP{}, f))
	if !slices.ContainsFunc(svc.PublishedPorts(), func(pt Port) bool { return pt.Published == 990 }) {
		t.Fatalf("ports %+v", svc.Ports)
	}
	f = goodFTP()
	f.Mode, f.CertSecret, f.KeySecret = FTPPlain, "", ""
	if err := (FTP{}).Validate(raw(t, f)); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), "allowPlaintext") {
		t.Fatalf("plain without consent: %v", err)
	}
	f.AllowPlaintext = true
	p := planOf(t, FTP{}, f)
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "unencrypted") {
		t.Errorf("no plain-text warning: %v", p.Warnings)
	}
}

func TestFTPLargePassiveRangeWarnsAndHugeIsRefused(t *testing.T) {
	f := goodFTP()
	f.Passive = PassiveRange{Min: 30000, Max: 30049}
	if !strings.Contains(strings.Join(planOf(t, FTP{}, f).Warnings, "\n"), "50 ports") {
		t.Error("no warning for a 50-port range")
	}
	f.Passive = PassiveRange{Min: 30000, Max: 30100}
	if err := (FTP{}).Validate(raw(t, f)); err == nil || !strings.Contains(err.Error(), "at most 100") {
		t.Fatalf("101 ports: %v", err)
	}
}

func TestFTPRefusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*FTPSpec)
		want string
	}{
		{"no cert", func(f *FTPSpec) { f.CertSecret = "" }, "certSecret"},
		{"same cert and key", func(f *FTPSpec) { f.KeySecret = f.CertSecret }, "different"},
		{"plaintext flag with TLS", func(f *FTPSpec) { f.AllowPlaintext = true }, "ftp only"},
		{"bad mode", func(f *FTPSpec) { f.Mode = "sftp" }, "mode"},
		{"low passive", func(f *FTPSpec) { f.Passive.Min = 80 }, "1024..65535"},
		{"high passive", func(f *FTPSpec) { f.Passive.Max = 70000 }, "1024..65535"},
		{"inverted passive", func(f *FTPSpec) { f.Passive = PassiveRange{Min: 31000, Max: 30000} }, "empty"},
		{"control in passive", func(f *FTPSpec) { f.ControlPort = 30005 }, "inside the passive range"},
		{"control out of range", func(f *FTPSpec) { f.ControlPort = 70000 }, "out of range"},
		{"no passive address", func(f *FTPSpec) { f.PassiveAddress = "" }, "passiveAddress"},
		{"bad passive address", func(f *FTPSpec) { f.PassiveAddress = "Files Example" }, "passiveAddress"},
		{"key auth", func(f *FTPSpec) { f.Users[0].AuthorizedKeysSecret = "k" }, "not supported"},
		{"no users", func(f *FTPSpec) { f.Users = nil }, "at least one user"},
	}
	for _, c := range cases {
		f := goodFTP()
		f.Users = slices.Clone(f.Users)
		c.mut(&f)
		if err := (FTP{}).Validate(raw(t, f)); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

func TestFTPIngressModeWarns(t *testing.T) {
	f := goodFTP()
	f.PortMode = "ingress"
	p := planOf(t, FTP{}, f)
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "routing mesh") {
		t.Fatalf("warnings %v", p.Warnings)
	}
	for _, pt := range svcOf(t, p).PublishedPorts() {
		if pt.Mode != "ingress" {
			t.Fatalf("port %+v", pt)
		}
	}
}

// --- S3 ---

func TestS3PlanRoutesAndSecrets(t *testing.T) {
	p := planOf(t, S3{}, goodS3())
	svc := svcOf(t, p)
	if !slices.Equal(svc.Secrets, []string{"s3-pass", "s3-user"}) {
		t.Fatalf("secrets %v", svc.Secrets)
	}
	env := strings.Join(svc.Env, "\n")
	if !strings.Contains(env, "MINIO_ROOT_USER_FILE=/run/secrets/s3-user") || !strings.Contains(env, "MINIO_ROOT_PASSWORD_FILE=/run/secrets/s3-pass") {
		t.Fatalf("env %s", env)
	}
	if !slices.Equal(svc.Args, []string{"server", "/data", "--address", ":9000", "--console-address", ":9001"}) {
		t.Fatalf("args %v", svc.Args)
	}
	l := svc.Labels
	want := map[string]string{
		"traefik.enable":                                             "true",
		"traefik.http.routers.web.rule":                              "Host(`s3.example.org`)",
		"traefik.http.routers.web.entrypoints":                       "websecure",
		"traefik.http.routers.web.tls":                               "true",
		"traefik.http.routers.web.tls.certresolver":                  "le",
		"traefik.http.services.web.loadbalancer.server.port":         "9000",
		"traefik.http.routers.web-console.rule":                      "Host(`console.example.org`)",
		"traefik.http.services.web-console.loadbalancer.server.port": "9001",
		"traefik.swarm.network":                                      "edge",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q", k, l[k], v)
		}
	}
	// Ownership labels are not overridden by routing labels.
	if l[LabelCluster] != "svc-abc" {
		t.Errorf("labels %v", l)
	}
	for _, name := range []string{"traefik-route-s3-api.yml", "traefik-route-s3-console.yml", "published-ports.json"} {
		if _, ok := artifact(p, name); !ok {
			t.Errorf("artifact %s missing", name)
		}
	}
	api, _ := artifact(p, "traefik-route-s3-api.yml")
	for _, want := range []string{"Host(`s3.example.org`)", "http://dbcm-svc-web:9000", `certResolver: "le"`} {
		if !strings.Contains(api, want) {
			t.Errorf("route artifact lacks %q:\n%s", want, api)
		}
	}
	if len(svc.PublishedPorts()) != 0 {
		t.Errorf("a routed S3 service publishes nothing directly: %+v", svc.Ports)
	}
	if !p.Executable() {
		t.Fatalf("blockers %v", p.Blockers)
	}
	w := strings.Join(p.Warnings, "\n")
	if !strings.Contains(w, "single-drive") || !strings.Contains(w, "Swarm provider") {
		t.Errorf("warnings:\n%s", w)
	}
}

func TestS3PublishedPortsAndWarnings(t *testing.T) {
	s := goodS3()
	s.APIRoute, s.ConsoleRoute, s.RouteNetwork = nil, nil, ""
	s.APIPublishedPort, s.ConsolePublishedPort, s.PortMode = 9000, 9001, "host"
	p := planOf(t, S3{}, s)
	svc := svcOf(t, p)
	if len(svc.PublishedPorts()) != 2 || svc.PublishedPorts()[0].Mode != "host" {
		t.Fatalf("ports %+v", svc.Ports)
	}
	w := strings.Join(p.Warnings, "\n")
	for _, want := range []string{"console is published directly", "without TLS"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %q:\n%s", want, w)
		}
	}
	for k := range svc.Labels {
		if strings.HasPrefix(k, "traefik.") {
			t.Errorf("unrouted service has label %s", k)
		}
	}
}

func TestS3Refusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*S3Spec)
		want string
	}{
		{"no root user", func(s *S3Spec) { s.RootUserSecret = "" }, "rootUserSecret"},
		{"no root password", func(s *S3Spec) { s.RootPasswordSecret = "" }, "rootPasswordSecret"},
		{"same secret", func(s *S3Spec) { s.RootPasswordSecret = s.RootUserSecret }, "different"},
		{"bad region", func(s *S3Spec) { s.Region = "US East" }, "region"},
		{"bad domain", func(s *S3Spec) { s.Domain = "*.example.org" }, "domain"},
		{"console port without console", func(s *S3Spec) { s.Console, s.ConsolePublishedPort = false, 9001; s.ConsoleRoute = nil }, "needs console"},
		{"console route without console", func(s *S3Spec) { s.Console = false }, "consoleRoute needs console"},
		{"same published port", func(s *S3Spec) { s.APIPublishedPort, s.ConsolePublishedPort = 9000, 9000 }, "same port"},
		{"route without hosts", func(s *S3Spec) { s.APIRoute.Hosts = nil }, "at least one host"},
		{"wildcard host", func(s *S3Spec) { s.APIRoute.Hosts = []string{"*.example.org"} }, "invalid"},
		{"host injection", func(s *S3Spec) { s.APIRoute.Hosts = []string{"a.example.org`) || Host(`b"} }, "invalid"},
		{"route without entry point", func(s *S3Spec) { s.APIRoute.EntryPoints = nil }, "entry point"},
		{"entry point injection", func(s *S3Spec) { s.APIRoute.EntryPoints = []string{"web,admin"} }, "entry point name"},
		{"resolver without tls", func(s *S3Spec) { s.APIRoute.TLS = false }, "needs tls"},
		{"route without network", func(s *S3Spec) { s.RouteNetwork = "" }, "routeNetwork"},
		{"network not attached", func(s *S3Spec) { s.RouteNetwork = "other" }, "one of networks"},
	}
	for _, c := range cases {
		s := goodS3()
		r1, r2 := *s.APIRoute, *s.ConsoleRoute
		s.APIRoute, s.ConsoleRoute = &r1, &r2
		c.mut(&s)
		if err := (S3{}).Validate(raw(t, s)); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

// --- NFS ---

func TestNFSPlanIsBlockedAndRendersExports(t *testing.T) {
	p := planOf(t, NFS{}, goodNFS())
	if p.Executable() || len(p.Blockers) != 2 {
		t.Fatalf("blockers %v", p.Blockers)
	}
	if !strings.Contains(p.Blockers[0], "CAP_SYS_ADMIN") || !strings.Contains(p.Text(), "blocked:") {
		t.Fatalf("blockers %v", p.Blockers)
	}
	exports, ok := artifact(p, "nfs-exports")
	if !ok {
		t.Fatal("no exports artifact")
	}
	want := "/export/ro 10.0.0.0/24(ro,sync,no_subtree_check,all_squash)\n" +
		"/export/shared 10.0.0.0/24(rw,sync,no_subtree_check,root_squash) 192.168.1.5(rw,sync,no_subtree_check,root_squash)\n"
	if !strings.HasSuffix(exports, want) {
		t.Fatalf("exports:\n%s", exports)
	}
	svc := svcOf(t, p)
	if len(svc.Ports) != 1 || svc.Ports[0] != (Port{Target: 2049, Published: 2049, Protocol: "tcp", Mode: "host"}) {
		t.Fatalf("ports %+v", svc.Ports)
	}
	w := strings.Join(p.Warnings, "\n")
	for _, want := range []string{"no encryption", "port 2049", "numeric user"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %q:\n%s", want, w)
		}
	}
}

func TestNFSNoRootSquashWarns(t *testing.T) {
	n := goodNFS()
	n.Exports[0].Squash = NFSNoRootSquash
	if !strings.Contains(strings.Join(planOf(t, NFS{}, n).Warnings, "\n"), "no_root_squash") {
		t.Fatal("no warning")
	}
}

func TestNFSRefusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*NFSSpec)
		want string
	}{
		{"no exports", func(n *NFSSpec) { n.Exports = nil }, "at least one export"},
		{"relative path", func(n *NFSSpec) { n.Exports[0].Path = "shared" }, "export path"},
		{"path with space", func(n *NFSSpec) { n.Exports[0].Path = "/a b" }, "export path"},
		{"path with options", func(n *NFSSpec) { n.Exports[0].Path = "/a(rw)" }, "export path"},
		{"dotdot", func(n *NFSSpec) { n.Exports[0].Path = "/a/../b" }, "export path"},
		{"duplicate", func(n *NFSSpec) { n.Exports[1].Path = n.Exports[0].Path }, "twice"},
		{"no clients", func(n *NFSSpec) { n.Exports[0].Clients = nil }, "at least one client"},
		{"wildcard", func(n *NFSSpec) { n.Exports[0].Clients = []string{"*"} }, "not an IP"},
		{"host name", func(n *NFSSpec) { n.Exports[0].Clients = []string{"client.example.org"} }, "not an IP"},
		{"everyone", func(n *NFSSpec) { n.Exports[0].Clients = []string{"0.0.0.0/0"} }, "every address"},
		{"bad squash", func(n *NFSSpec) { n.Exports[0].Squash = "none" }, "squash"},
		{"bad port", func(n *NFSSpec) { n.PublishedPort = 70000 }, "publishedPort"},
	}
	for _, c := range cases {
		n := goodNFS()
		n.Exports = slices.Clone(n.Exports)
		c.mut(&n)
		if err := (NFS{}).Validate(raw(t, n)); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

// --- shared behaviour ---

// No plan text, artifact or step note may contain a secret value. The
// specs hold secret names only; this pins that the plans show names and
// /run/secrets paths and nothing else by planting recognizable values in
// the one place a user could try (env and labels are not user-settable).
func TestStoragePlansCarryNoSecretValues(t *testing.T) {
	s := goodS3()
	s.RootPasswordSecret = "s3-pass"
	p := planOf(t, S3{}, s)
	all := p.Text()
	for _, a := range p.Artifacts {
		all += a.Content
	}
	for _, st := range p.Steps {
		if st.Spec != nil {
			all += strings.Join(st.Spec.Env, " ")
		}
	}
	for _, bad := range []string{"PASSWORD=", "SECRET=", "ROOT_USER="} {
		if strings.Contains(all, bad) {
			t.Errorf("plan contains %q", bad)
		}
	}
	// A literal credential cannot be passed in the specs at all.
	for _, ty := range []Type{SFTP{}, FTP{}, S3{}, NFS{}} {
		if err := ty.Validate([]byte(`{"image":"x:1","password":"hunter2"}`)); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s accepted a literal password field: %v", ty.Name(), err)
		}
	}
}

func TestStoragePlansAreDeterministic(t *testing.T) {
	a, b := goodSFTP(), goodSFTP()
	slices.Reverse(b.Users)
	pa, pb := planOf(t, SFTP{}, a), planOf(t, SFTP{}, b)
	ra, _ := json.Marshal(pa.Artifacts)
	rb, _ := json.Marshal(pb.Artifacts)
	if string(ra) != string(rb) {
		t.Fatal("artifacts depend on the order of users")
	}
	n1, n2 := goodNFS(), goodNFS()
	slices.Reverse(n2.Exports)
	if n1.RenderExports() != n2.RenderExports() {
		t.Fatal("exports depend on the order")
	}
}

func TestStorageHostCheck(t *testing.T) {
	in := PlanInput{Context: ctx(), Spec: raw(t, goodSFTP()), Hosts: []Host{{ID: "h2", Ready: true, Active: true}}}
	if _, err := (SFTP{}).Plan(in); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("unknown host: %v", err)
	}
	in.Hosts = []Host{{ID: "h1", Ready: true, Active: true}}
	if _, err := (SFTP{}).Plan(in); err != nil {
		t.Fatal(err)
	}
}

func TestTraefikManagementArtifact(t *testing.T) {
	spec := TraefikSpec{
		Image:       "traefik:v3.1.2",
		EntryPoints: []EntryPoint{{Name: "web", Port: 80}},
		Services:    []Backend{{Name: "b", Servers: []string{"http://x:80"}}},
		Routers:     []Router{{Name: "r", Match: Match{Hosts: []string{"a.example.org"}}, EntryPoints: []string{"web"}, Service: "b"}},
		Middlewares: []Middleware{{Name: "z", Type: MWCompress}},
	}
	p := planOf(t, Traefik{}, spec)
	content, ok := artifact(p, "traefik-management.json")
	if !ok {
		t.Fatal("no management artifact")
	}
	var m struct {
		Service     string `json:"service"`
		Dashboard   bool   `json:"dashboard"`
		InsecureAPI bool   `json:"insecureApi"`
		DynamicFile string `json:"dynamicFile"`
		Routers     []struct {
			Name  string   `json:"name"`
			Hosts []string `json:"hosts"`
		} `json:"routers"`
		Middlewares []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"middlewares"`
	}
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		t.Fatal(err)
	}
	if m.Service != "dbcm-svc-web" || m.Dashboard || m.InsecureAPI || m.DynamicFile != TraefikDynamicFile ||
		len(m.Routers) != 1 || m.Routers[0].Hosts[0] != "a.example.org" || m.Middlewares[0].Kind != MWCompress {
		t.Fatalf("%+v", m)
	}
}

func configCount(typ string) int {
	if typ == TypeFTP {
		return 1
	}
	return 0
}
