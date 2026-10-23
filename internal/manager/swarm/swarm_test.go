// SPDX-License-Identifier: AGPL-3.0-only

package swarm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dockerclient"
)

func TestDefaultMatrixSeedsOnlyLabVersions(t *testing.T) {
	m := DefaultMatrix()
	if len(m.Docker) != 1 || m.Docker[0].Versions != "29." || m.Docker[0].Status != "tested" {
		t.Fatalf("matrix %+v", m)
	}
}

func TestMatrixCheck(t *testing.T) {
	m := DefaultMatrix()
	m.Docker = append(m.Docker,
		MatrixEntry{Versions: "29.1.", Status: "known-issue", Note: "x"},
		MatrixEntry{Versions: "20.", Status: "unsupported", Note: "too old"})
	cases := map[string]Level{
		"29.9.1":   LevelOK,
		"v29.0.0":  LevelOK,
		"29.1.3":   LevelWarning, // longer prefix wins
		"28.5.0":   LevelWarning, // not in matrix: warning, not error
		"30.0.0-1": LevelWarning,
		"20.10.7":  LevelError,
		"":         LevelWarning,
		"banana":   LevelWarning,
	}
	for v, want := range cases {
		if f := m.Check("n", v); f.Level != want {
			t.Errorf("%q: %s (%s), want %s", v, f.Level, f.Message, want)
		}
	}
}

func TestParseMatrixRejectsBadEntries(t *testing.T) {
	for _, doc := range []string{`{"format":2}`, `{"format":1,"docker":[{"versions":"1.","status":"maybe"}]}`,
		`{"format":1,"docker":[{"status":"tested"}]}`, `{`} {
		if _, err := ParseMatrix([]byte(doc)); err == nil {
			t.Errorf("accepted %s", doc)
		}
	}
}

type fakeConn struct {
	nodes    []dockerclient.Node
	labels   map[string]map[string]string
	services []agent.Object
	created  []agent.ServiceSpec
	secrets  map[string][]byte
	failNode string
}

func (f *fakeConn) CreateSecret(_ context.Context, name string, _ map[string]string, data []byte) (string, error) {
	if f.secrets == nil {
		f.secrets = map[string][]byte{}
	}
	f.secrets[name] = data
	return "sec-" + name, nil
}

func newFakeConn() *fakeConn {
	return &fakeConn{labels: map[string]map[string]string{}, nodes: []dockerclient.Node{
		{ID: "n1", Hostname: "m1", Role: "manager", Availability: "active", State: "ready", EngineVersion: "29.9.1"},
		{ID: "n2", Hostname: "w1", Role: "worker", Availability: "active", State: "ready", EngineVersion: "27.3.1"},
	}}
}

func (f *fakeConn) Version(context.Context) (dockerclient.VersionInfo, error) {
	return dockerclient.VersionInfo{Version: "29.9.1", APIVersion: "1.52"}, nil
}
func (f *fakeConn) Nodes(context.Context) ([]dockerclient.Node, error) { return f.nodes, nil }
func (f *fakeConn) SetNodeLabel(_ context.Context, id, key, value string) error {
	if id == f.failNode {
		return errors.New("update out of sequence")
	}
	if f.labels[id] == nil {
		f.labels[id] = map[string]string{}
	}
	if value == "" {
		delete(f.labels[id], key)
	} else {
		f.labels[id][key] = value
	}
	return nil
}
func (f *fakeConn) CreateGlobalService(_ context.Context, spec agent.ServiceSpec) (string, error) {
	f.created = append(f.created, spec)
	id := "svc-agent"
	f.services = append(f.services, agent.Object{Kind: agent.KindService, ID: id, Name: spec.Name, Labels: spec.Labels})
	return id, nil
}
func (f *fakeConn) ListByLabel(_ context.Context, _ agent.ObjectKind, key, value string) ([]agent.Object, error) {
	var out []agent.Object
	for _, s := range f.services {
		if s.Labels[key] == value {
			out = append(out, s)
		}
	}
	return out, nil
}

func TestObserveChecksEveryEngine(t *testing.T) {
	inv, err := Observe(context.Background(), newFakeConn(), DefaultMatrix())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Nodes) != 2 || len(inv.Findings) != 2 || inv.Findings[0].Level != LevelOK || inv.Findings[1].Level != LevelWarning {
		t.Fatalf("inventory %+v", inv)
	}
}

func TestDeployAgentsIsIdempotentAndConstrained(t *testing.T) {
	c := newFakeConn()
	d := AgentDeployment{InstallationID: "inst-1", Image: "agent@sha256:abc", VolumeBase: "/srv/dbcm", Enrolled: []string{"n1"}, Revoked: []string{"n2"}}
	c.labels["n2"] = map[string]string{AgentLabel("inst-1"): "true", "zone": "b"}
	res, err := DeployAgents(context.Background(), c, d)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.ServiceID != "svc-agent" || !slices.Equal(res.Labeled, []string{"n1"}) || !slices.Equal(res.Unlabeled, []string{"n2"}) {
		t.Fatalf("result %+v", res)
	}
	if c.labels["n1"][AgentLabel("inst-1")] != "true" || c.labels["n2"]["zone"] != "b" || c.labels["n2"][AgentLabel("inst-1")] != "" {
		t.Fatalf("labels %+v", c.labels)
	}
	spec := c.created[0]
	if len(spec.Constraints) != 1 || spec.Constraints[0] != "node.labels.dbcm.agent.inst-1.enrolled==true" ||
		spec.Labels[agent.LabelInstallation] != "inst-1" || !strings.Contains(strings.Join(spec.Args, " "), "-installation inst-1") {
		t.Fatalf("spec %+v", spec)
	}
	again, err := DeployAgents(context.Background(), c, d)
	if err != nil || again.Created || len(c.created) != 1 || again.ServiceID != "svc-agent" {
		t.Fatalf("second deploy %+v %v", again, err)
	}
	c.failNode = "n1"
	partial, err := DeployAgents(context.Background(), c, d)
	if err != nil || partial.Failed["n1"] == "" {
		t.Fatalf("label failure not reported: %+v %v", partial, err)
	}
}

func TestDeployAgentsValidates(t *testing.T) {
	c := newFakeConn()
	for _, d := range []AgentDeployment{
		{InstallationID: "Bad_ID", Image: "a", VolumeBase: "/v"},
		{InstallationID: "ok", VolumeBase: "/v"},
		{InstallationID: "ok", Image: "a", VolumeBase: "/v", Enrolled: []string{"n1"}, Revoked: []string{"n1"}},
	} {
		if _, err := DeployAgents(context.Background(), c, d); !errors.Is(err, ErrInvalidDeployment) {
			t.Errorf("%+v: %v", d, err)
		}
	}
	if len(c.created) != 0 {
		t.Fatal("invalid deployment created a service")
	}
}

const fp = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func TestDeployAgentsMTLSSpec(t *testing.T) {
	c := newFakeConn()
	d := AgentDeployment{InstallationID: "inst-1", Image: "agent@sha256:abc", VolumeBase: "/srv/dbcm", Enrolled: []string{"n1"},
		MTLS: &AgentMTLS{ManagerURL: "https://10.0.0.4:7444", CAFingerprint: fp, TokenSecret: "dbcm-agent-enroll-inst-1-t1",
			Token: []byte("dbcm-et1.t1.secret"), MonitorUser: "dbcm_monitor", MonitorSecret: "dbcm-monitor-pw"}}
	res, err := DeployAgents(context.Background(), c, d)
	if err != nil || !res.Created || res.TokenSecret != d.MTLS.TokenSecret {
		t.Fatalf("deploy %+v %v", res, err)
	}
	if string(c.secrets[d.MTLS.TokenSecret]) != "dbcm-et1.t1.secret" {
		t.Fatalf("token secret %q", c.secrets)
	}
	spec := c.created[0]
	args := strings.Join(spec.Args, " ")
	for _, want := range []string{"-listen 0.0.0.0:7443", "-manager-url https://10.0.0.4:7444", "-ca-fingerprint " + fp,
		"-enroll-token-file /run/secrets/dbcm-agent-enroll-inst-1-t1", "-monitor-user dbcm_monitor",
		"-monitor-password-file /run/secrets/dbcm-monitor-pw", "-installation inst-1"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if strings.Contains(args, "-insecure-lab") || strings.Contains(args, "secret\"") || strings.Contains(args, "dbcm-et1") {
		t.Fatalf("args %q", args)
	}
	if !slices.Equal(spec.Networks, []string{"host"}) || !slices.Equal(spec.Secrets, []string{"dbcm-agent-enroll-inst-1-t1", "dbcm-monitor-pw"}) ||
		!slices.Equal(spec.Env, []string{"DBCM_HOST_ID={{.Node.ID}}"}) {
		t.Fatalf("spec %+v", spec)
	}
	// Existing service: no second secret.
	delete(c.secrets, d.MTLS.TokenSecret)
	if _, err := DeployAgents(context.Background(), c, d); err != nil || len(c.secrets) != 0 {
		t.Fatalf("redeploy created a secret: %v %v", c.secrets, err)
	}
}

func TestDeployAgentsLabSpecIsExplicit(t *testing.T) {
	spec := AgentSpec(AgentDeployment{InstallationID: "i", Image: "a", VolumeBase: "/v"})
	if !strings.HasPrefix(strings.Join(spec.Args, " "), "-insecure-lab -listen 127.0.0.1:7070") || len(spec.Networks) != 0 || len(spec.Secrets) != 0 {
		t.Fatalf("lab spec %+v", spec)
	}
}

func TestDeployAgentsMTLSValidates(t *testing.T) {
	good := AgentMTLS{ManagerURL: "https://m:7444", CAFingerprint: fp, TokenSecret: "dbcm-t", Token: []byte("x")}
	for name, mod := range map[string]func(*AgentMTLS){
		"url":         func(m *AgentMTLS) { m.ManagerURL = "http://m:7444" },
		"fingerprint": func(m *AgentMTLS) { m.CAFingerprint = "abc" },
		"secret name": func(m *AgentMTLS) { m.TokenSecret = "token" },
		"token":       func(m *AgentMTLS) { m.Token = nil }, // service does not exist yet
		"port":        func(m *AgentMTLS) { m.Port = 70000 },
		"monitor":     func(m *AgentMTLS) { m.MonitorUser = "u" },
	} {
		m := good
		mod(&m)
		c := newFakeConn()
		if _, err := DeployAgents(context.Background(), c, AgentDeployment{InstallationID: "i", Image: "a", VolumeBase: "/v", MTLS: &m}); !errors.Is(err, ErrInvalidDeployment) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestDeployAgentsMTLSDBSecrets: further database secrets (cluster root and
// backup passwords for exec-sql, lab E4) are mounted once each.
func TestDeployAgentsMTLSDBSecrets(t *testing.T) {
	c := newFakeConn()
	d := AgentDeployment{InstallationID: "inst-1", Image: "agent@sha256:abc", VolumeBase: "/srv/dbcm", Enrolled: []string{"n1"},
		MTLS: &AgentMTLS{ManagerURL: "https://10.0.0.4:7444", CAFingerprint: fp, TokenSecret: "dbcm-agent-enroll-inst-1-t1",
			Token: []byte("dbcm-et1.t1.secret"), MonitorUser: "dbcm_monitor", MonitorSecret: "dbcm-monitor-pw",
			DBSecrets: []string{"dbcm-root-pw", "dbcm-monitor-pw", "dbcm-backup-pw"}}}
	if _, err := DeployAgents(context.Background(), c, d); err != nil {
		t.Fatal(err)
	}
	want := []string{"dbcm-agent-enroll-inst-1-t1", "dbcm-monitor-pw", "dbcm-root-pw", "dbcm-backup-pw"}
	if got := c.created[0].Secrets; !slices.Equal(got, want) {
		t.Fatalf("secrets %v, want %v", got, want)
	}
	bad := *d.MTLS
	bad.DBSecrets = []string{"../etc"}
	if _, err := DeployAgents(context.Background(), newFakeConn(), AgentDeployment{InstallationID: "i", Image: "a", VolumeBase: "/v", MTLS: &bad}); !errors.Is(err, ErrInvalidDeployment) {
		t.Fatalf("path-like secret name accepted: %v", err)
	}
}

// TestAgentSpecCommandAndUser: lab E4 ran the agent service from the
// product image, whose entrypoint is the manager (it exited on the agent
// flags) and whose user is the distroless nonroot user; the spec names the
// agent binary and root.
func TestAgentSpecCommandAndUser(t *testing.T) {
	for _, d := range []AgentDeployment{
		{InstallationID: "i", Image: "a", VolumeBase: "/v"},
		{InstallationID: "i", Image: "a", VolumeBase: "/v", MTLS: &AgentMTLS{ManagerURL: "https://m:7444", CAFingerprint: fp, TokenSecret: "dbcm-t", Token: []byte("x")}},
	} {
		spec := AgentSpec(d)
		if !slices.Equal(spec.Command, []string{"/usr/local/bin/agent"}) || spec.User != "0:0" {
			t.Fatalf("command %v user %q", spec.Command, spec.User)
		}
	}
}
