// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

// Fakes only: an httptest server imitates the OCI distribution API (tag
// list and manifest HEAD) with basic authentication.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

type fakeDist struct {
	mu   sync.Mutex
	logs []string
}

func (f *fakeDist) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.logs = append(f.logs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	f.mu.Unlock()
	if u, p, ok := r.BasicAuth(); !ok || u != "bot" || p != "pw-very-secret" {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/v2/team/web/tags/list":
		_, _ = w.Write([]byte(`{"name":"team/web","tags":["1.0.0","1.0.1","1.1.0"]}`))
	case r.URL.Path == "/v2/team/web/manifests/1.0.1":
		w.Header().Set("Docker-Content-Digest", dgNew)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestRegistryResolverUsesStoredCredential(t *testing.T) {
	f := &fakeDist{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	cfgs := &registry.MemoryConfigStore{}
	secrets := &registry.MemorySecretStore{}
	rs := &registry.Service{Configs: cfgs, Secrets: secrets, Now: time.Now}
	if err := secrets.Put(ctx, "cred-1", registry.NewSecret("pw-very-secret")); err != nil {
		t.Fatal(err)
	}
	if err := cfgs.Put(ctx, registry.Config{ID: "r1", Name: "local", Kind: registry.KindGeneric, Endpoint: srv.URL, Username: "bot", CredentialRef: "cred-1"}); err != nil {
		t.Fatal(err)
	}
	res := &RegistryResolver{Registries: rs}
	ref := mustRef(t, host+"/team/web:1.0.0")

	tags, err := res.Tags(ctx, ref)
	if err != nil || len(tags) != 3 {
		t.Fatalf("%v %v", tags, err)
	}
	d, err := res.Digest(ctx, ref.Pinned("1.0.1", ""))
	if err != nil || d != dgNew {
		t.Fatalf("%q %v", d, err)
	}
	if _, err := res.Digest(ctx, ref.Pinned("9.9.9", "")); err == nil {
		t.Fatal("unknown tag resolved")
	}
	// Without a matching registry configuration an anonymous client is
	// used; the credential-protected registry then refuses it, and the
	// error carries no secret.
	res2 := &RegistryResolver{Registries: &registry.Service{Configs: &registry.MemoryConfigStore{}, Secrets: secrets}}
	if _, err := res2.Tags(ctx, ref); err == nil || strings.Contains(err.Error(), "pw-very-secret") {
		t.Fatalf("anonymous access: %v", err)
	}
}

func TestEndpointHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://ghcr.io": "ghcr.io", "https://registry-1.docker.io": "docker.io", "http://127.0.0.1:5000": "127.0.0.1:5000", "https://index.docker.io": "docker.io",
	} {
		if got := endpointHost(in); got != want {
			t.Fatalf("%s: %s", in, got)
		}
	}
}
