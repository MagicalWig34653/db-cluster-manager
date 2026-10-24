package k8s

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

func testRenderer() Renderer {
	return Renderer{Enabled: true, InstallationID: "install", Namespace: "sandbox"}
}
func input(spec string) service.PlanInput {
	return service.PlanInput{Context: service.Context{InstallationID: "install", ServiceID: "service-1", Name: "web"}, Spec: json.RawMessage(spec)}
}
func TestContainerDefinitionDeterministicAndTyped(t *testing.T) {
	r := testRenderer()
	in := input(`{"image":"nginx:1.27","ports":[{"target":80}],"secrets":["password"],"env":[{"name":"PASSWORD_FILE","value":"/run/secrets/password"}]}`)
	a, err := r.Definition(service.Container{}, in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Definition(service.Container{}, in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Plan.Digest() != b.Plan.Digest() {
		t.Fatal("unstable digest")
	}
	if len(a.Objects) != 2 {
		t.Fatalf("objects: %d", len(a.Objects))
	}
	var deployment Manifest[WorkloadSpec]
	for _, o := range a.Objects {
		if d, ok := o.(Manifest[WorkloadSpec]); ok {
			deployment = d
		}
	}
	if deployment.Kind != "Deployment" || deployment.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("wrong workload")
	}
	pod := deployment.Spec.Template.Spec
	if pod.Volumes[0].Secret.SecretName != "password" || pod.Containers[0].VolumeMounts[0].MountPath != "/run/secrets/password" {
		t.Fatal("secret not referenced as a file")
	}
	if deployment.Metadata.Labels[service.LabelCluster] != "service-1" || deployment.Metadata.Labels[LabelInstallation] != "install" {
		t.Fatal("missing ownership")
	}
	y, err := a.YAML()
	if err != nil || !strings.HasPrefix(y, "---\n{") {
		t.Fatal(y, err)
	}
	for _, doc := range strings.Split(strings.TrimPrefix(y, "---\n"), "---\n") {
		if !json.Valid([]byte(doc)) {
			t.Fatal("export is not a JSON-style YAML document")
		}
	}
	plan, _ := service.Container{}.Plan(in)
	sw, err := (service.Swarm{}).Render(plan)
	if err != nil || sw.Source.ServiceID != a.Plan.Source.ServiceID {
		t.Fatal("different source definition")
	}
	in.Spec = json.RawMessage(`{"image":"nginx:1.28"}`)
	changed, err := r.Definition(service.Container{}, in)
	if err != nil || changed.Plan.Digest() == a.Plan.Digest() {
		t.Fatal("digest did not bind change", err)
	}
	r.Enabled = false
	if _, err := r.Definition(service.Container{}, in); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
}
func TestLocalPVRetainsDataAndPinsNode(t *testing.T) {
	r := testRenderer()
	in := input(`{"image":"nginx:1.27","volumes":[{"name":"data","target":"/data"}],"placement":{"hostId":"swarm-host"}}`)
	a, err := r.Definition(service.Container{}, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Plan.Text(), "local storage pinning") {
		t.Fatal(a.Plan.Text())
	}
	r.Local = map[string]LocalBinding{"dbcm-svc-web-data": {Node: "kube-worker", Path: "/var/lib/dbcm/data", Bytes: 1024}}
	a, err = r.Definition(service.Container{}, in)
	if err != nil {
		t.Fatal(err)
	}
	foundPV, foundPVC, foundStateful := false, false, false
	for _, o := range a.Objects {
		switch m := o.(type) {
		case Manifest[PVSpec]:
			foundPV = true
			if m.Metadata.Namespace != "" || m.Spec.PersistentVolumeReclaimPolicy != "Retain" || m.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values[0] != "kube-worker" || m.Spec.ClaimRef.Namespace != "sandbox" {
				t.Fatal(m)
			}
		case Manifest[PVCSpec]:
			foundPVC = true
			if m.Spec.VolumeName == "" || m.Spec.StorageClassName != "" {
				t.Fatal(m)
			}
		case Manifest[WorkloadSpec]:
			foundStateful = m.Kind == "StatefulSet"
			if m.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "kube-worker" {
				t.Fatal(m)
			}
		}
	}
	if !foundPV || !foundPVC || !foundStateful || !strings.Contains(a.Plan.Text(), "identity marker") {
		t.Fatal(a.Plan.Text())
	}
}
func TestUnsupportedSemanticsRemainBlocked(t *testing.T) {
	spec := service.ServiceSpec{Name: "web", Image: "nginx:1.27", Replicas: 1, Networks: []string{"swarm"}, User: "123", Ports: []service.Port{{Target: 80, Published: 8080}}, UpdateConfig: &service.SwarmUpdateConfig{Order: "start-first"}}
	p := service.Plan{Kind: service.PlanCreate, Type: "container", Name: "web", ServiceID: "id", Steps: []service.Step{{Spec: &spec}}, Blockers: []string{"source blocker"}}
	a, err := testRenderer().Preview(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"source blocker", "NetworkPolicy", "securityContext", "exposure decision", "rollout gates", "execution is disabled"} {
		if !strings.Contains(a.Plan.Text(), want) {
			t.Fatal(want)
		}
	}
}
func TestGaleraNeverExportsBootstrapAndKeepsRecoveryBlocker(t *testing.T) {
	s := galera.ServiceSpec{Name: "dbcm-db-n1", Image: "mariadb:11.4", Args: []string{"--wsrep-new-cluster", "--other"}, Replicas: 1}
	gp := galera.Plan{Steps: []galera.Step{{Spec: &s}, {Spec: &s}}}
	p := service.Plan{Kind: service.PlanCreate, Type: "galera", ServiceID: "id", Name: "db", Galera: &gp}
	a, err := testRenderer().Preview(p)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := a.YAML()
	if strings.Contains(y, "--wsrep-new-cluster") || !strings.Contains(a.Plan.Text(), "safe_to_bootstrap") {
		t.Fatal(y)
	}
	if len(a.Objects) != 2 {
		t.Fatal("duplicate node renders", len(a.Objects))
	}
	for _, o := range a.Objects {
		if w, ok := o.(Manifest[WorkloadSpec]); ok && w.Kind != "StatefulSet" {
			t.Fatal(w)
		}
	}
}
func TestConfigMapAndIngressRouteFromTraefikDefinition(t *testing.T) {
	in := input(`{"image":"traefik:v3.3","entryPoints":[{"name":"web","port":80}],"routers":[{"name":"site","match":{"hosts":["example.test"]},"entryPoints":["web"],"service":"site"}],"services":[{"name":"site","servers":["http://backend:8080"]}]}`)
	a, err := testRenderer().Definition(service.Traefik{}, in)
	if err != nil {
		t.Fatal(err)
	}
	config, route := false, false
	for _, o := range a.Objects {
		switch m := o.(type) {
		case ConfigMap:
			config = true
			if m.Data["content"] == "" {
				t.Fatal(m)
			}
		case Manifest[IngressRouteSpec]:
			route = true
			if m.Spec.Routes[0].Services[0].Name != "backend" || m.Spec.Routes[0].Services[0].Port != 8080 {
				t.Fatal(m)
			}
		}
	}
	if !config || !route || !strings.Contains(a.Plan.Text(), "CRD/controller") {
		t.Fatal(a.Plan.Text())
	}
}

func TestGaleraDefinitionUsesDeterministicPreviewIdentities(t *testing.T) {
	in := input(`{"size":3,"image":"mariadb:11.4","network":"dbcm-test","rootPasswordSecret":"root-password","resources":{"limits":{"milli_cpu":1000,"memory_bytes":1073741824},"reservations":{"milli_cpu":500,"memory_bytes":536870912}},"placement":{}}`)
	for _, id := range []string{"host-1", "host-2", "host-3"} {
		in.Hosts = append(in.Hosts, service.Host{ID: id, Ready: true, Active: true, Capacity: service.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 << 30}})
	}
	r := testRenderer()
	a, err := r.Definition(service.Galera{}, in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Definition(service.Galera{}, in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Plan.Digest() != b.Plan.Digest() {
		t.Fatal("random identities changed the preview digest")
	}
	y, _ := a.YAML()
	if strings.Contains(y, "--wsrep-new-cluster") {
		t.Fatal("bootstrap command persisted in manifest")
	}
	count := 0
	for _, o := range a.Objects {
		if w, ok := o.(Manifest[WorkloadSpec]); ok {
			count++
			if w.Kind != "StatefulSet" {
				t.Fatal(w)
			}
		}
	}
	if count != 3 {
		t.Fatal("expected three reviewed node shapes", count)
	}
}
