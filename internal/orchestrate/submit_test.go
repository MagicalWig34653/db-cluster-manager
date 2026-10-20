// SPDX-License-Identifier: AGPL-3.0-only

package orchestrate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dockerclient"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// swarmStub answers the Engine API calls of one create_service command.
// Response shapes follow the Docker Engine API reference v1.47 (abridged).
type swarmStub struct {
	mu      sync.Mutex
	created []byte
	posts   int
}

func (s *swarmStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1.47")
	own := `{"dbcm.installation":"inst-1"}`
	switch {
	case r.Method == http.MethodGet && (path == "/networks/dbcm-shop-galera" || path == "/networks/net0123"):
		_, _ = io.WriteString(w, `{"Name":"dbcm-shop-galera","Id":"net0123","Scope":"swarm","Driver":"overlay","Labels":`+own+`}`)
	case r.Method == http.MethodGet && (path == "/secrets/dbcm-shop-root-pw" || path == "/secrets/sec0001"):
		_, _ = io.WriteString(w, `{"ID":"sec0001","Version":{"Index":3},"Spec":{"Name":"dbcm-shop-root-pw","Labels":`+own+`}}`)
	case r.Method == http.MethodGet && (path == "/secrets/dbcm-shop-tls-key" || path == "/secrets/sec0002"):
		_, _ = io.WriteString(w, `{"ID":"sec0002","Version":{"Index":4},"Spec":{"Name":"dbcm-shop-tls-key","Labels":`+own+`}}`)
	case r.Method == http.MethodPost && path == "/services/create":
		s.posts++
		s.created, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ID":"svc-galera-n2","Warnings":[]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"no such object"}`)
	}
}

func newExecutor(t *testing.T, docker agent.DockerAPI, allowed []string) *agent.Executor {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "volumes")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	g, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(dir, "term")))
	if err != nil {
		t.Fatal(err)
	}
	e, err := agent.NewExecutor(agent.Config{
		InstallationID: "inst-1",
		StatePath:      filepath.Join(dir, "state.json"),
		VolumeBase:     base,
		AllowedIDs:     allowed,
		Guard:          g,
		Docker:         docker,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestGaleraSpecSubmittedThroughAgentAndDockerClient runs a Galera node
// spec through the whole path: galera.BuildServiceSpec, ToAgentSpec, the
// JSON command contract, the agent executor (ownership checks on network
// and secrets) and dockerclient, and checks the Engine API request.
func TestGaleraSpecSubmittedThroughAgentAndDockerClient(t *testing.T) {
	stub := &swarmStub{}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	dc, err := dockerclient.New(dockerclient.Options{Host: "tcp://" + srv.Listener.Addr().String(), APIVersion: "1.47"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dc.Close)

	g, err := galera.BuildServiceSpec(testCluster(), testNode("n2"), galera.SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(ToAgentSpec(g))
	if err != nil {
		t.Fatal(err)
	}
	cmd := agent.Command{Term: 1, OpID: "op-1", StepID: "create-n2", Kind: agent.KindCreateService, Args: args}

	// Without the network and secrets in the allow-list nothing is created.
	res, err := newExecutor(t, dc, nil).Handle(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.StatusRefused || res.Code != agent.CodeNotOwned || stub.posts != 0 {
		t.Fatalf("unlisted references: result %+v, %d creates", res, stub.posts)
	}

	res, err = newExecutor(t, dc, []string{"net0123", "sec0001", "sec0002"}).Handle(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.StatusSucceeded {
		t.Fatalf("result %+v", res)
	}
	var got struct {
		Name         string
		Labels       map[string]string
		TaskTemplate struct {
			ContainerSpec struct {
				Image, Hostname string
				Args, Env       []string
				StopGracePeriod int64
				Secrets         []struct {
					File                 struct{ Name string }
					SecretID, SecretName string
				}
				Mounts []struct{ Type, Source, Target string }
			}
			Networks      []struct{ Target string }
			RestartPolicy struct{ Condition string }
			Resources     struct {
				Limits, Reservations struct{ NanoCPUs, MemoryBytes int64 }
			}
			Placement struct{ Constraints []string }
		}
		Mode         struct{ Replicated struct{ Replicas uint64 } }
		EndpointSpec struct{ Mode string }
	}
	if err := json.Unmarshal(stub.created, &got); err != nil {
		t.Fatalf("create body %s: %v", stub.created, err)
	}
	cs := got.TaskTemplate.ContainerSpec
	tt := got.TaskTemplate
	checks := []struct {
		what string
		ok   bool
	}{
		{"name", got.Name == "dbcm-shop-n2"},
		{"installation and step labels", got.Labels[agent.LabelInstallation] == "inst-1" && got.Labels[agent.LabelStepID] == "create-n2" && got.Labels[agent.LabelNode] == g.Labels[galera.LabelNode]},
		{"image", cs.Image == g.Image},
		{"hostname", cs.Hostname == "dbcm-shop-n2"},
		{"args", len(cs.Args) == len(g.Args) && cs.Args[0] == g.Args[0]},
		{"env", len(cs.Env) == len(g.Env)},
		{"stop grace period", cs.StopGracePeriod == 60_000_000_000},
		{"secrets pinned by ID with names", len(cs.Secrets) == 2 && cs.Secrets[0].SecretID == "sec0001" && cs.Secrets[0].SecretName == "dbcm-shop-root-pw" &&
			cs.Secrets[0].File.Name == "dbcm-shop-root-pw" && cs.Secrets[1].SecretID == "sec0002"},
		{"volume mount", len(cs.Mounts) == 1 && cs.Mounts[0].Type == "volume" && cs.Mounts[0].Source == "dbcm-shop-n2-81def889" && cs.Mounts[0].Target == galera.VolumeTarget},
		{"network pinned by ID", len(tt.Networks) == 1 && tt.Networks[0].Target == "net0123"},
		{"restart policy", tt.RestartPolicy.Condition == "any"},
		{"CPU limit 2 CPUs", tt.Resources.Limits.NanoCPUs == 2_000_000_000},
		{"CPU reservation 0.5 CPUs", tt.Resources.Reservations.NanoCPUs == 500_000_000},
		{"memory", tt.Resources.Limits.MemoryBytes == 2<<30 && tt.Resources.Reservations.MemoryBytes == 1<<30},
		{"placement", len(tt.Placement.Constraints) == 1 && tt.Placement.Constraints[0] == "node.id==z4zp15cc49dfljfv2rx9q1kpf"},
		{"replicas", got.Mode.Replicated.Replicas == 1},
		{"endpoint mode", got.EndpointSpec.Mode == "dnsrr"},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("create request: %s wrong\nbody %s", c.what, stub.created)
		}
	}
}
