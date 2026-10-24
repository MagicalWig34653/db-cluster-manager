package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
	m "github.com/MagicalWig34653/db-cluster-manager/internal/service/k8s"
)

// recorderTransport runs HTTP handlers through httptest without opening a
// listener, so contract tests also run in a socket-restricted sandbox.
type recorderTransport struct{ handler http.Handler }

func (rt recorderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	rt.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

type recorderServer struct {
	URL     string
	handler http.Handler
}

func recordedServer(h http.Handler) recorderServer {
	return recorderServer{URL: "https://kube.example.test", handler: h}
}
func (s recorderServer) Close() {}
func (s recorderServer) Client() *http.Client {
	return &http.Client{Transport: recorderTransport{s.handler}}
}

func object(name, uid, installation string) m.RawObject {
	return m.RawObject{APIVersion: "apps/v1", Kind: "Deployment", Metadata: m.Metadata{Name: name, Namespace: "sandbox", UID: uid, ResourceVersion: "7", Labels: map[string]string{m.LabelInstallation: installation}}, Spec: json.RawMessage(`{"replicas":1}`)}
}
func TestRESTApplyListDeleteOwnership(t *testing.T) {
	ctx := context.Background()
	ref := Ref{Deployments, "sandbox", "web"}
	owned := object("web", "uid-1", "install")
	foreign := object("other", "uid-2", "foreign")
	var methods []string
	server := recordedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Header.Get("Authorization") != "Bearer token-test" {
			t.Error("missing auth")
		}
		if !strings.HasPrefix(r.URL.Path, "/apis/apps/v1/namespaces/sandbox/deployments") {
			t.Error(r.URL.Path)
		}
		switch r.Method {
		case "GET":
			if r.URL.Query().Has("labelSelector") {
				if r.URL.Query().Get("labelSelector") != "dbcm.installation=install" {
					t.Error(r.URL.RawQuery)
				}
				json.NewEncoder(w).Encode(map[string]any{"items": []m.RawObject{foreign, owned, object("spoofed", "uid-3", "install")}})
			} else {
				json.NewEncoder(w).Encode(owned)
			}
		case "PATCH":
			if r.Header.Get("Content-Type") != "application/apply-patch+yaml" || r.URL.Query().Get("fieldManager") != "dbcm-groundwork" || r.URL.Query().Get("force") != "false" {
				t.Error("wrong SSA request")
			}
			var got m.RawObject
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			if got.Metadata.UID != "uid-1" || got.Metadata.ResourceVersion != "7" {
				t.Error("missing CAS preconditions")
			}
			json.NewEncoder(w).Encode(owned)
		case "DELETE":
			var opts struct {
				Preconditions struct {
					UID             string `json:"uid"`
					ResourceVersion string `json:"resourceVersion"`
				} `json:"preconditions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
				t.Error(err)
			}
			if opts.Preconditions.UID != "uid-1" || opts.Preconditions.ResourceVersion != "7" {
				t.Error("unfenced delete")
			}
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	c, err := NewREST(true, server.URL, "install", "sandbox", registry.NewSecret("token-test"), server.Client(), map[Ref]string{ref: "uid-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Apply(ctx, owned); err != nil {
		t.Fatal(err)
	}
	list, err := c.List(ctx, Deployments)
	if err != nil || len(list) != 1 || list[0].Metadata.Name != "web" {
		t.Fatal(list, err)
	}
	before := len(methods)
	if err = c.Delete(ctx, Ref{Deployments, "sandbox", "other"}); !errors.Is(err, ErrNotOwned) || len(methods) != before {
		t.Fatal("unowned delete made a request", err)
	}
	if err = c.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "GET,PATCH,GET,GET,DELETE" {
		t.Fatal(methods)
	}
}
func TestRESTRejectsReplacedOrRelabelledObject(t *testing.T) {
	for _, old := range []m.RawObject{object("web", "replacement", "install"), object("web", "uid-1", "foreign")} {
		t.Run(old.Metadata.UID+old.Metadata.Labels[m.LabelInstallation], func(t *testing.T) {
			calls := 0
			s := recordedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" {
					t.Error("mutation of foreign object")
				}
				json.NewEncoder(w).Encode(old)
			}))
			defer s.Close()
			ref := Ref{Deployments, "sandbox", "web"}
			c, _ := NewREST(true, s.URL, "install", "sandbox", registry.Secret{}, s.Client(), map[Ref]string{ref: "uid-1"})
			if err := c.Delete(context.Background(), ref); !errors.Is(err, ErrNotOwned) {
				t.Fatal(err)
			}
			if err := c.Apply(context.Background(), object("web", "", "install")); !errors.Is(err, ErrNotOwned) {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatal(calls)
			}
		})
	}
}
func TestRESTCreateRaceConflictAndSanitizedErrors(t *testing.T) {
	for _, status := range []int{409, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			methods := []string{}
			s := recordedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				methods = append(methods, r.Method)
				if r.Method == "GET" {
					w.WriteHeader(404)
					return
				}
				w.WriteHeader(status)
				fmt.Fprint(w, "secret-token-and-kubeconfig")
			}))
			defer s.Close()
			c, _ := NewREST(true, s.URL, "install", "sandbox", registry.Secret{}, s.Client(), map[Ref]string{{Deployments, "sandbox", "web"}: ""})
			err := c.Apply(context.Background(), object("web", "", "install"))
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatal(err)
			}
			if status == 409 && !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
			if strings.Join(methods, ",") != "GET,POST" {
				t.Fatal(methods)
			}
		})
	}
}
func TestRESTNewObjectUsesAtomicCreateBeforeSSA(t *testing.T) {
	methods := []string{}
	s := recordedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(object("web", "new-uid", "install"))
	}))
	defer s.Close()
	c, _ := NewREST(true, s.URL, "install", "sandbox", registry.Secret{}, s.Client(), map[Ref]string{{Deployments, "sandbox", "web"}: ""})
	if err := c.Apply(context.Background(), object("web", "", "install")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "GET,POST,PATCH" {
		t.Fatal(methods)
	}
}
func TestRESTFeatureGateAndOriginValidation(t *testing.T) {
	if _, err := NewREST(false, "https://cluster.test", "i", "sandbox", registry.Secret{}, nil, nil); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://cluster.test", "https://token@cluster.test", "https://cluster.test/?token=secret", "https://cluster.test/path"} {
		if _, err := NewREST(true, endpoint, "i", "sandbox", registry.Secret{}, nil, nil); err == nil {
			t.Fatal(endpoint)
		}
	}
}
func TestRESTDoesNotForwardTokenOnRedirect(t *testing.T) {
	hit := false
	target := recordedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer target.Close()
	redirectRequests := 0
	source := recordedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectRequests++; http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	c, _ := NewREST(true, source.URL, "i", "sandbox", registry.NewSecret("secret"), source.Client(), nil)
	if _, err := c.List(context.Background(), Deployments); err == nil || hit || redirectRequests != 1 {
		t.Fatal("redirect followed", err)
	}
}
func TestFakeOwnershipAndCopies(t *testing.T) {
	ctx := context.Background()
	ref := Ref{Deployments, "sandbox", "web"}
	f, err := NewFake(true, "install", "sandbox", map[Ref]string{ref: ""})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Apply(ctx, object("web", "", "install")); err != nil {
		t.Fatal(err)
	}
	list, _ := f.List(ctx, Deployments)
	if len(list) != 1 {
		t.Fatal(list)
	}
	list[0].Metadata.Labels[m.LabelInstallation] = "foreign"
	list[0].Spec[0] = 'x'
	list, _ = f.List(ctx, Deployments)
	if list[0].Metadata.Labels[m.LabelInstallation] != "install" || !json.Valid(list[0].Spec) {
		t.Fatal("fake leaked mutable state")
	}
	if err = f.Delete(ctx, Ref{Deployments, "other", "web"}); !errors.Is(err, ErrNotOwned) {
		t.Fatal(err)
	}
	if err = f.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err = f.Apply(ctx, object("web", "", "install")); !errors.Is(err, ErrNotOwned) {
		t.Fatal("delete retained allow-list", err)
	}
}

func TestFakeRefusesRelabelledAndReplacedInventory(t *testing.T) {
	ctx := context.Background()
	ref := Ref{Deployments, "sandbox", "web"}
	for _, obj := range []m.RawObject{object("web", "old-uid", "foreign"), object("web", "new-uid", "install")} {
		f, err := NewFake(true, "install", "sandbox", map[Ref]string{ref: "old-uid"})
		if err != nil {
			t.Fatal(err)
		}
		f.objects[ref.key()] = obj
		got, err := f.List(ctx, Deployments)
		if err != nil || len(got) != 0 {
			t.Fatal("foreign object listed", got, err)
		}
		if err = f.Delete(ctx, ref); !errors.Is(err, ErrNotOwned) {
			t.Fatal("foreign object deleted", err)
		}
		if err = f.Apply(ctx, object("web", "", "install")); !errors.Is(err, ErrNotOwned) {
			t.Fatal("foreign object updated", err)
		}
	}
}
