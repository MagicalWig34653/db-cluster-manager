// SPDX-License-Identifier: AGPL-3.0-only

package registry

// Fakes only: an httptest server imitates the OCI distribution API. Nothing
// here talked to ghcr.io, Docker Hub or a real registry:2.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte // "repo@digest" -> body
	mtypes    map[string]string
	tags      map[string]string // "repo:tag" -> digest
	uploads   int
	// auth: "" none, "basic", "bearer"
	auth      string
	user, pw  string
	realm     string // overrides the token realm (default own /token)
	tokenHits int
	logs      []string // every request line and Authorization header
	pageSize  int
}

func newFake(auth string) *fakeRegistry {
	return &fakeRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}, mtypes: map[string]string{},
		tags: map[string]string{}, auth: auth, user: "bot", pw: "pw-very-secret", pageSize: 2}
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, r.Method+" "+r.URL.String()+" auth="+r.Header.Get("Authorization"))
	if r.URL.Path == "/token" {
		f.tokenHits++
		u, p, ok := r.BasicAuth()
		if !ok || u != f.user || p != f.pw {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-" + r.URL.Query().Get("scope")})
		return
	}
	switch f.auth {
	case "basic":
		if u, p, ok := r.BasicAuth(); !ok || u != f.user || p != f.pw {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.WriteHeader(401)
			return
		}
	case "bearer":
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
			realm := f.realm
			if realm == "" {
				realm = "http://" + r.Host + "/token"
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="fake"`, realm))
			w.WriteHeader(401)
			return
		}
	}
	p := r.URL.Path
	if p == "/v2/" {
		w.WriteHeader(200)
		return
	}
	rest, ok := strings.CutPrefix(p, "/v2/")
	if !ok {
		w.WriteHeader(404)
		return
	}
	switch {
	case strings.Contains(rest, "/blobs/uploads/") && r.Method == "POST":
		f.uploads++
		repo := rest[:strings.Index(rest, "/blobs/")]
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/u%d", repo, f.uploads))
		w.WriteHeader(202)
	case strings.Contains(rest, "/blobs/uploads/") && r.Method == "PUT":
		b, _ := io.ReadAll(r.Body)
		d := r.URL.Query().Get("digest")
		if DigestOf(b) != d {
			w.WriteHeader(400)
			return
		}
		f.blobs[d] = b
		w.WriteHeader(201)
	case strings.Contains(rest, "/blobs/"):
		d := rest[strings.Index(rest, "/blobs/")+7:]
		if _, ok := f.blobs[d]; ok {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(404)
		}
	case strings.Contains(rest, "/manifests/"):
		i := strings.Index(rest, "/manifests/")
		repo, ref := rest[:i], rest[i+11:]
		f.manifest(w, r, repo, ref)
	case strings.HasSuffix(rest, "/tags/list"):
		repo := strings.TrimSuffix(rest, "/tags/list")
		var all []string
		for k := range f.tags {
			if r2, t, ok := strings.Cut(k, ":"); ok && r2 == repo {
				all = append(all, t)
			}
		}
		sortStrings(all)
		last := r.URL.Query().Get("last")
		var page []string
		for _, t := range all {
			if t > last {
				page = append(page, t)
			}
		}
		more := len(page) > f.pageSize
		if more {
			page = page[:f.pageSize]
			w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?n=100&last=%s>; rel="next"`, repo, page[len(page)-1]))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": page})
	default:
		w.WriteHeader(404)
	}
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func (f *fakeRegistry) manifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	key := repo + "@" + ref
	switch r.Method {
	case "PUT":
		b, _ := io.ReadAll(r.Body)
		d := DigestOf(b)
		f.manifests[repo+"@"+d] = b
		f.mtypes[repo+"@"+d] = r.Header.Get("Content-Type")
		if !strings.HasPrefix(ref, "sha256:") {
			f.tags[repo+":"+ref] = d
		}
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(201)
	case "GET", "HEAD":
		d := ref
		if !strings.HasPrefix(ref, "sha256:") {
			var ok bool
			if d, ok = f.tags[repo+":"+ref]; !ok {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"nope"}]}`))
				return
			}
		}
		b, ok := f.manifests[repo+"@"+d]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`))
			return
		}
		w.Header().Set("Docker-Content-Digest", d)
		w.Header().Set("Content-Type", f.mtypes[repo+"@"+d])
		if r.Method == "GET" {
			_, _ = w.Write(b)
		}
	case "DELETE":
		if _, ok := f.manifests[key]; !ok {
			w.WriteHeader(404)
			return
		}
		delete(f.manifests, key)
		for k, v := range f.tags {
			if strings.HasPrefix(k, repo+":") && v == ref {
				delete(f.tags, k)
			}
		}
		w.WriteHeader(202)
	}
}

func testImage(t *testing.T, marker string) Image {
	layer := BlobOf([]byte("layer-" + marker))
	cfg := BlobOf([]byte(`{"marker":"` + marker + `"}`))
	m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "config": map[string]any{"digest": cfg.Digest}, "layers": []any{map[string]any{"digest": layer.Digest}}})
	return Image{MediaType: "application/vnd.oci.image.manifest.v1+json", Manifest: m, Blobs: []Blob{layer, cfg}}
}

func newClient(t *testing.T, f *fakeRegistry, pw string) (*External, *httptest.Server) {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := NewExternal(ExternalConfig{Kind: KindGeneric, Endpoint: srv.URL, Username: "bot", Password: NewSecret(pw)})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestPushResolveTagListDelete(t *testing.T) {
	for _, auth := range []string{"", "basic", "bearer"} {
		t.Run("auth="+auth, func(t *testing.T) {
			ctx := context.Background()
			f := newFake(auth)
			c, _ := newClient(t, f, f.pw)
			if err := c.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			img := testImage(t, "a")
			d, err := c.Push(ctx, "team/web", "v1", img)
			if err != nil || d != DigestOf(img.Manifest) {
				t.Fatalf("push %q %v", d, err)
			}
			if len(f.blobs) != 2 {
				t.Fatalf("blobs %d", len(f.blobs))
			}
			// Second push of the same blobs skips the upload.
			up := f.uploads
			if _, err := c.Push(ctx, "team/web", "v1b", img); err != nil || f.uploads != up {
				t.Fatalf("existing blobs re-uploaded: %v", err)
			}
			if got, err := c.Resolve(ctx, "team/web", "v1"); err != nil || got != d {
				t.Fatalf("resolve %q %v", got, err)
			}
			if got, err := c.Resolve(ctx, "team/web", d); err != nil || got != d {
				t.Fatalf("resolve digest %q %v", got, err)
			}
			if err := c.Tag(ctx, "team/web", "v1", "latest"); err != nil {
				t.Fatal(err)
			}
			if err := c.Tag(ctx, "team/web", d, "stable"); err != nil {
				t.Fatal(err)
			}
			for _, extra := range []string{"v2", "v3"} {
				_ = c.Tag(ctx, "team/web", "v1", extra)
			}
			tags, err := c.List(ctx, "team/web")
			want := "latest,stable,v1,v1b,v2,v3"
			if err != nil || strings.Join(tags, ",") != want {
				t.Fatalf("tags %v %v (pagination page size 2)", tags, err)
			}
			if err := c.Delete(ctx, "team/web", d); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Resolve(ctx, "team/web", "v1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("after delete: %v", err)
			}
		})
	}
}

func TestErrorsCarryNoSecrets(t *testing.T) {
	ctx := context.Background()
	f := newFake("basic")
	c, _ := newClient(t, f, "wrong-password-123")
	err := c.Ping(ctx)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want unauthorized: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-password-123") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error leaks: %v", err)
	}
	f2 := newFake("")
	c2, _ := newClient(t, f2, "x")
	_, err = c2.Resolve(ctx, "a/b", "nope")
	var re *Error
	if !errors.As(err, &re) || re.Status != 404 || !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// Connection failure: no URL in the message.
	c3, _ := NewExternal(ExternalConfig{Kind: KindGeneric, Endpoint: "http://127.0.0.1:1", Username: "u", Password: NewSecret("sekret-pw")})
	err = c3.Ping(ctx)
	if err == nil || strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "sekret-pw") {
		t.Fatalf("%v", err)
	}
}

func TestSecretNeverFormatted(t *testing.T) {
	s := NewSecret("topsecret")
	var buf bytes.Buffer
	h := slog.New(slog.NewJSONHandler(&buf, nil))
	h.Info("x", "pw", s, "cfg", ExternalConfig{Password: s})
	out := fmt.Sprintf("%v %+v %#v %s", s, s, s, s)
	b, _ := json.Marshal(struct{ P Secret }{s})
	if strings.Contains(out+buf.String()+string(b), "topsecret") {
		t.Fatalf("leak: %s %s %s", out, buf.String(), b)
	}
}

func TestCredentialsNotSentToForeignRealm(t *testing.T) {
	ctx := context.Background()
	evil := newFake("")
	evilSrv := httptest.NewServer(evil)
	t.Cleanup(evilSrv.Close)
	f := newFake("bearer")
	f.realm = evilSrv.URL + "/token"
	c, _ := newClient(t, f, f.pw)
	err := c.Ping(ctx)
	if err == nil {
		t.Fatal("foreign token realm accepted")
	}
	for _, l := range evil.logs {
		if strings.Contains(l, "Basic") {
			t.Fatalf("credentials reached foreign host: %v", evil.logs)
		}
	}
	if len(evil.logs) != 0 {
		t.Fatalf("foreign host was contacted: %v", evil.logs)
	}
}

func TestUploadLocationOnOtherHostRefused(t *testing.T) {
	ctx := context.Background()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached foreign host: %s", r.URL)
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "HEAD":
			w.WriteHeader(404)
		case r.Method == "POST":
			w.Header().Set("Location", other.URL+"/up")
			w.WriteHeader(202)
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := NewExternal(ExternalConfig{Kind: KindGeneric, Endpoint: srv.URL, Username: "u", Password: NewSecret("p")})
	if _, err := c.Push(ctx, "a/b", "t", testImage(t, "z")); err == nil {
		t.Fatal("cross-host upload location followed")
	}
}

func TestPushDetectsBadInput(t *testing.T) {
	ctx := context.Background()
	f := newFake("")
	c, _ := newClient(t, f, "")
	img := testImage(t, "q")
	img.Blobs[0].Data = []byte("tampered")
	if _, err := c.Push(ctx, "a/b", "t", img); !errors.Is(err, ErrDigestMismatch) {
		t.Fatal(err)
	}
	for _, repo := range []string{"", "UPPER", "a//b", "../x", "a b"} {
		if _, err := c.Resolve(ctx, repo, "t"); !errors.Is(err, ErrInvalidName) {
			t.Errorf("repo %q accepted", repo)
		}
	}
	if _, err := c.Resolve(ctx, "a/b", "bad tag!"); !errors.Is(err, ErrInvalidName) {
		t.Fatal("bad reference accepted")
	}
	if err := c.Delete(ctx, "a/b", "latest"); !errors.Is(err, ErrInvalidName) {
		t.Fatal("delete by tag accepted")
	}
}

func TestEndpointValidation(t *testing.T) {
	for _, bad := range []string{"", "ftp://x", "http://example.com", "https://u:p@x.io", "https://x.io/path", "https://x.io?q=1", "x.io"} {
		if _, err := ValidateEndpoint(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, ok := range []string{"https://registry.example.com", "https://r.io:5000/", "http://127.0.0.1:5000", "http://localhost:5000"} {
		if _, err := ValidateEndpoint(ok); err != nil {
			t.Errorf("rejected %q: %v", ok, err)
		}
	}
	if _, err := NewExternal(ExternalConfig{Kind: KindGeneric}); err == nil {
		t.Fatal("generic without endpoint")
	}
	if c, err := NewExternal(ExternalConfig{Kind: KindGHCR}); err != nil || c.base.Host != "ghcr.io" {
		t.Fatalf("ghcr default endpoint: %v", err)
	}
	if c, err := NewExternal(ExternalConfig{Kind: KindDockerHub}); err != nil || c.base.Host != "registry-1.docker.io" {
		t.Fatalf("dockerhub default endpoint: %v", err)
	}
	if _, err := NewExternal(ExternalConfig{Kind: KindManaged}); err == nil {
		t.Fatal("managed accepted as external")
	}
}

// --- service: credentials stored through the secret store ---

func TestServiceAddExternalStoresSecretSeparately(t *testing.T) {
	ctx := context.Background()
	f := newFake("basic")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	secrets := &MemorySecretStore{}
	s := &Service{Configs: &MemoryConfigStore{}, Secrets: secrets, Now: func() time.Time { return time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC) }}
	in := ExternalInput{ID: "r1", Name: "Main", Kind: KindGeneric, Endpoint: srv.URL, Username: "bot", Password: NewSecret("wrong")}
	if _, err := s.AddExternal(ctx, "alice", in); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad credentials accepted: %v", err)
	}
	if l, _ := s.List(ctx); len(l) != 0 {
		t.Fatal("failed registration stored")
	}
	if _, err := secrets.Get(ctx, "registry/r1/credential"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatal("secret stored for failed registration")
	}
	in.Password = NewSecret(f.pw)
	cfg, err := s.AddExternal(ctx, "alice", in)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	if strings.Contains(string(b), f.pw) || cfg.CredentialRef != "registry/r1/credential" || cfg.CreatedBy != "alice" {
		t.Fatalf("config %s", b)
	}
	if got, _ := secrets.Get(ctx, cfg.CredentialRef); got.Reveal() != f.pw {
		t.Fatal("secret not stored")
	}
	if _, err := s.AddExternal(ctx, "alice", in); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	cl, _, err := s.Open(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Push(ctx, "a/b", "t", testImage(t, "svc")); err != nil {
		t.Fatal(err)
	}
	for _, l := range f.logs {
		_ = l // request lines contain only the Authorization header, which the fake owns
	}
	for _, bad := range []ExternalInput{
		{ID: "", Name: "x", Kind: KindGeneric, Endpoint: srv.URL},
		{ID: "r2", Name: "x", Kind: KindGeneric, Endpoint: srv.URL, Username: "u"},
		{ID: "r3", Name: "x", Kind: KindGeneric, Endpoint: "http://example.com"},
		{ID: "r4", Name: "x", Kind: KindManaged},
	} {
		if _, err := s.AddExternal(ctx, "a", bad); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%+v: %v", bad.ID, err)
		}
	}
}

// --- managed registry descriptor and policy ---

func goodManaged() ManagedSpec {
	return ManagedSpec{
		Name: "registry", Image: "registry:2@sha256:" + strings.Repeat("c", 64), Auth: AuthHtpasswd, HtpasswdSecret: "registry-htpasswd",
		TLS: TLSSpec{CertSecret: "registry-cert", KeySecret: "registry-key"}, Storage: StorageSpec{Volume: "registry-data", Host: "node-1"},
	}
}

func TestManagedDescriptor(t *testing.T) {
	d, err := goodManaged().Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 1 || d.Ports[0].Published != 5000 || d.Constraints[0] != "node.hostname == node-1" ||
		d.Env["REGISTRY_AUTH"] != "htpasswd" || d.Env["REGISTRY_STORAGE_DELETE_ENABLED"] != "false" ||
		d.Volumes[0].Target != "/var/lib/registry" || len(d.Secrets) != 3 || d.Secrets[0].Target != "registry_htpasswd" {
		t.Fatalf("%+v", d)
	}
	b, _ := json.Marshal(d)
	d2, _ := goodManaged().Descriptor()
	b2, _ := json.Marshal(d2)
	if !bytes.Equal(b, b2) {
		t.Fatal("descriptor not deterministic")
	}
	tok := goodManaged()
	tok.Auth, tok.HtpasswdSecret = AuthToken, ""
	tok.Token = &TokenAuth{Realm: "https://auth.example.com/token", Service: "reg", Issuer: "issuer", RootCertBundleSecret: "roots"}
	td, err := tok.Descriptor()
	if err != nil || td.Env["REGISTRY_AUTH_TOKEN_REALM"] != "https://auth.example.com/token" {
		t.Fatalf("%v %+v", err, td)
	}
}

func TestManagedValidation(t *testing.T) {
	cases := map[string]func(*ManagedSpec){
		"floating image":  func(m *ManagedSpec) { m.Image = "registry:2" },
		"other image":     func(m *ManagedSpec) { m.Image = "evil/registry:2@sha256:" + strings.Repeat("c", 64) },
		"no tls":          func(m *ManagedSpec) { m.TLS = TLSSpec{} },
		"no auth":         func(m *ManagedSpec) { m.Auth = "" },
		"htpasswd no ref": func(m *ManagedSpec) { m.HtpasswdSecret = "" },
		"no storage host": func(m *ManagedSpec) { m.Storage.Host = "" },
		"bad name":        func(m *ManagedSpec) { m.Name = "Bad Name" },
		"bad port":        func(m *ManagedSpec) { m.PublishedPort = 70000 },
		"token incomplete": func(m *ManagedSpec) {
			m.Auth, m.HtpasswdSecret, m.Token = AuthToken, "", &TokenAuth{Realm: "https://x"}
		},
		"token http realm": func(m *ManagedSpec) {
			m.Auth, m.HtpasswdSecret = AuthToken, ""
			m.Token = &TokenAuth{Realm: "http://x/token", Service: "s", Issuer: "i", RootCertBundleSecret: "r"}
		},
	}
	for n, mut := range cases {
		m := goodManaged()
		mut(&m)
		if err := m.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestDistributionPolicy(t *testing.T) {
	ok := DistributionPolicy{Mode: DistributionExternal, Registry: "ghcr", PullSecrets: map[string]string{"s1": "pull-s1", "s2": "pull-s2"}}
	if err := ok.Validate(KindGHCR, []string{"s1", "s2"}); err != nil {
		t.Fatal(err)
	}
	if err := ok.Validate(KindGHCR, []string{"s1", "s3", "s2"}); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "s3") {
		t.Fatalf("missing pull secret not reported: %v", err)
	}
	if ref, found := ok.PullSecretFor("s1"); ref != "pull-s1" || !found {
		t.Fatal("pull secret lookup")
	}
	if _, found := ok.PullSecretFor("s9"); found {
		t.Fatal("unknown swarm has a secret")
	}
	if err := ok.Validate(KindManaged, []string{"s1"}); err == nil {
		t.Fatal("external mode with managed registry")
	}
	m := DistributionPolicy{Mode: DistributionManaged, Registry: "mine", AnonymousPull: true}
	if err := m.Validate(KindManaged, []string{"s1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(KindGHCR, nil); err == nil {
		t.Fatal("managed mode with external registry")
	}
	air := DistributionPolicy{Mode: DistributionExternal, Registry: "g", AirGapped: true, AnonymousPull: true}
	if err := air.Validate(KindGHCR, nil); err == nil {
		t.Fatal("air-gapped with external registry")
	}
	if err := (DistributionPolicy{Mode: "x", Registry: "r"}).Validate(KindGHCR, nil); err == nil {
		t.Fatal("bad mode")
	}
}
