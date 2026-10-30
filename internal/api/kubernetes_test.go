package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/kube"
)

type fakeKubernetesRegistrations struct {
	entries    []kube.Registration
	credential string
	calls      int
}

func (f *fakeKubernetesRegistrations) Register(_ context.Context, in kube.RegistrationInput) (kube.Registration, error) {
	f.calls++
	for _, v := range f.entries {
		if v.ID == in.ID {
			return kube.Registration{}, kube.ErrConflict
		}
	}
	f.credential = in.Token.Reveal() + in.Kubeconfig.Reveal()
	v := in.View()
	f.entries = append(f.entries, v)
	return v, nil
}
func (f *fakeKubernetesRegistrations) Registrations(context.Context) ([]kube.Registration, error) {
	return f.entries, nil
}
func TestKubernetesRegistrationContract(t *testing.T) {
	f := newFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	viewer := f.loggedIn("viewer", authz.RoleViewer, authz.Global)
	backend := &fakeKubernetesRegistrations{}
	f.srv.cfg.Kubernetes = backend
	body := map[string]any{"id": "lab", "name": "Lab", "endpoint": "https://kube.example.test", "namespace": "sandbox", "token": "synthetic-test-token"}
	reg := f.useModules(t, nil)
	wantError(t, admin.do("POST", "/api/v1/kubernetes-clusters", body), http.StatusNotFound, errModuleDisabled)
	if backend.calls != 0 {
		t.Fatal("disabled feature reached backend")
	}
	if err := reg.Set("kubernetes", true, "admin"); err != nil {
		t.Fatal(err)
	}
	wantError(t, viewer.do("GET", "/api/v1/kubernetes-clusters", nil), http.StatusForbidden, errForbidden)
	wantError(t, viewer.do("POST", "/api/v1/kubernetes-clusters", body), http.StatusForbidden, errForbidden)
	r := admin.do("POST", "/api/v1/kubernetes-clusters", body)
	wantStatus(t, r, http.StatusCreated)
	if strings.Contains(r.Body.String(), "synthetic-test-token") || strings.Contains(r.Body.String(), `"token":`) {
		t.Fatal(r.Body.String())
	}
	var view kube.Registration
	r.decode(t, &view)
	if !view.CredentialSet || view.CredentialKind != "token" || backend.credential != "synthetic-test-token" {
		t.Fatal(view)
	}
	wantError(t, admin.do("POST", "/api/v1/kubernetes-clusters", body), http.StatusConflict, errConflict)
	r = admin.do("GET", "/api/v1/kubernetes-clusters", nil)
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(r.Body.String(), "synthetic-test-token") {
		t.Fatal(r.Body.String())
	}
	body["id"] = "config"
	delete(body, "token")
	body["kubeconfig"] = "synthetic-kubeconfig-with-exec-plugin"
	r = admin.do("POST", "/api/v1/kubernetes-clusters", body)
	wantStatus(t, r, http.StatusCreated)
	if strings.Contains(r.Body.String(), "exec-plugin") {
		t.Fatal(r.Body.String())
	}
	body["token"] = "also-present"
	wantError(t, admin.do("POST", "/api/v1/kubernetes-clusters", body), http.StatusBadRequest, errBadRequest)
	delete(body, "kubeconfig")
	body["endpoint"] = "https://secret@kube.example.test"
	wantError(t, admin.do("POST", "/api/v1/kubernetes-clusters", body), http.StatusBadRequest, errBadRequest)
}
func TestKubernetesOpenAPIMetadataHasNoCredentialFields(t *testing.T) {
	b, err := json.Marshal(kube.Registration{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"token"`, `"kubeconfig"`, `"credentialRef"`} {
		if strings.Contains(string(b), name) {
			t.Fatal(string(b))
		}
	}
}

func TestKubernetesOpenAPISchemasMatchTypes(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{"KubernetesRegistration": kube.Registration{}, "KubernetesRegistrationInput": KubernetesRegistrationInput{}} {
		_, body, ok := strings.Cut(string(raw), "    "+name+":\n")
		if !ok {
			t.Fatal("missing schema", name)
		}
		lines := strings.Split(body, "\n")
		end := len(lines)
		for i, line := range lines {
			if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") && strings.HasSuffix(line, ":") {
				end = i
				break
			}
		}
		body = strings.Join(lines[:end], "\n")
		rt := reflect.TypeOf(value)
		for i := range rt.NumField() {
			field, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if !strings.Contains(body, "        "+field+":") {
				t.Error(name, field)
			}
		}
		if name == "KubernetesRegistration" && (strings.Contains(body, "        token:") || strings.Contains(body, "        kubeconfig:")) {
			t.Fatal("response schema exposes credentials")
		}
	}
}
