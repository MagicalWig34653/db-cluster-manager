// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

// All tests here use fakes only: no Docker, no network, no registry.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

var baseDigest = "sha256:" + strings.Repeat("a", 64)

func goodSpec() WebServerSpec {
	return WebServerSpec{
		Name:  "shop web",
		Nginx: NginxSpec{Version: "1.27", BaseDigest: baseDigest},
		PHP: PHPSpec{Version: "8.3", Extensions: []string{"mysqli", "gd", "opcache"},
			INI: map[string]string{"memory_limit": "256M", "date.timezone": "Europe/Berlin"}},
		ExtraPackages: []string{"tzdata", "curl"},
	}
}

func TestRenderIsDeterministicAndOrderIndependent(t *testing.T) {
	a, err := Render(goodSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s2 := goodSpec()
	s2.PHP.Extensions = []string{"opcache", "mysqli", "gd", "gd"}
	s2.ExtraPackages = []string{"curl", "tzdata"}
	s2.Name = "  shop web "
	b, err := Render(s2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.SpecHash != b.SpecHash || a.ContextHash != b.ContextHash || a.Dockerfile != b.Dockerfile {
		t.Fatal("equivalent specs rendered differently")
	}
	for i := 0; i < 20; i++ { // map iteration must not leak into output
		c, _ := Render(goodSpec(), nil)
		if c.ContextHash != a.ContextHash {
			t.Fatal("render is not stable")
		}
	}
	s3 := goodSpec()
	s3.PHP.INI["memory_limit"] = "512M"
	c, _ := Render(s3, nil)
	if c.SpecHash == a.SpecHash || c.ContextHash == a.ContextHash {
		t.Fatal("different specs share a hash")
	}
	if strings.Contains(a.Dockerfile, time.Now().Format("2006")) && strings.Contains(a.Dockerfile, "20") && false {
		t.Fatal("unreachable")
	}
}

func TestRenderContent(t *testing.T) {
	r, err := Render(goodSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"FROM docker.io/library/nginx:1.27-alpine@" + baseDigest,
		"curl \\\n      php83-fpm \\\n      php83-gd \\\n      php83-mysqli \\\n      php83-opcache \\\n      tzdata\n",
		"COPY php/dbcm.ini /etc/php83/conf.d/99-dbcm.ini",
		"RUN php-fpm83 -t && nginx -t",
	} {
		if !strings.Contains(r.Dockerfile, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, r.Dockerfile)
		}
	}
	var paths []string
	for _, f := range r.Files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "docker-entrypoint.d/40-php-fpm.sh,nginx/default.conf,nginx/nginx.conf,php/dbcm-pool.conf,php/dbcm.ini" {
		t.Fatalf("files not in sorted order or incomplete: %v", paths)
	}
	ini := fileContent(r, "php/dbcm.ini")
	if !strings.Contains(ini, "date.timezone = Europe/Berlin\nmemory_limit = 256M\n") {
		t.Fatalf("ini not sorted: %q", ini)
	}
	if !strings.Contains(fileContent(r, "nginx/nginx.conf"), "server_tokens off;") {
		t.Fatal("server_tokens must default to off")
	}
	if !strings.Contains(fileContent(r, "nginx/default.conf"), "fastcgi_pass 127.0.0.1:9000;") {
		t.Fatal("PHP location missing")
	}
	// Static image: no PHP files, no php packages.
	st := WebServerSpec{Name: "static", Nginx: NginxSpec{Version: "1.26", BaseDigest: baseDigest}}
	sr, err := Render(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sr.Dockerfile, "php") || len(sr.Files) != 2 {
		t.Fatalf("static render contains PHP: %s %v", sr.Dockerfile, sr.Files)
	}
}

func fileContent(r Rendered, p string) string {
	for _, f := range r.Files {
		if f.Path == p {
			return f.Content
		}
	}
	return ""
}

func TestValidationRejections(t *testing.T) {
	cases := map[string]func(*WebServerSpec){
		"floating base (no digest)":  func(s *WebServerSpec) { s.Nginx.BaseDigest = "" },
		"tag as digest":              func(s *WebServerSpec) { s.Nginx.BaseDigest = "latest" },
		"short digest":               func(s *WebServerSpec) { s.Nginx.BaseDigest = "sha256:abc" },
		"unknown nginx":              func(s *WebServerSpec) { s.Nginx.Version = "0.1" },
		"unknown php":                func(s *WebServerSpec) { s.PHP.Version = "5.6" },
		"extension not allow-listed": func(s *WebServerSpec) { s.PHP.Extensions = []string{"xdebug"} },
		"extension unavailable":      func(s *WebServerSpec) { s.PHP.Version = "8.4"; s.PHP.Extensions = []string{"redis"} },
		"ini not allow-listed":       func(s *WebServerSpec) { s.PHP.INI = map[string]string{"auto_prepend_file": "/x"} },
		"ini bad value":              func(s *WebServerSpec) { s.PHP.INI = map[string]string{"memory_limit": "lots"} },
		"ini newline injection":      func(s *WebServerSpec) { s.PHP.INI = map[string]string{"memory_limit": "1M\nauto_prepend_file=/x"} },
		"ini without php": func(s *WebServerSpec) {
			*s = WebServerSpec{Name: "x", Nginx: s.Nginx, PHP: PHPSpec{INI: map[string]string{"memory_limit": "1M"}}}
		},
		"package not allow-listed": func(s *WebServerSpec) { s.ExtraPackages = []string{"netcat-openbsd"} },
		"bad name":                 func(s *WebServerSpec) { s.Name = "x\"; RUN rm -rf /" },
		"bad platform":             func(s *WebServerSpec) { s.Platforms = []string{"windows/amd64"} },
		"doc root escape":          func(s *WebServerSpec) { s.Nginx.Options.DocumentRoot = "/var/www/../etc" },
		"server name injection":    func(s *WebServerSpec) { s.Nginx.Options.ServerName = "a; root /" },
		"body size":                func(s *WebServerSpec) { s.Nginx.Options.ClientMaxBodySize = "10m; x" },
		"workers":                  func(s *WebServerSpec) { s.Nginx.Options.WorkerProcesses = "9000" },
	}
	for name, mut := range cases {
		s := goodSpec()
		mut(&s)
		err := s.Validate(nil)
		if !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: want ErrInvalidSpec, got %v", name, err)
		}
		if _, err := Render(s, nil); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
	if err := goodSpec().Validate(nil); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionAvailabilityPerVersion(t *testing.T) {
	c := DefaultCatalog()
	if !c.PHPExtensionAvailable("8.3", "redis") || c.PHPExtensionAvailable("8.4", "redis") || c.PHPExtensionAvailable("8.2", "gmp") {
		t.Fatal("per-version availability not honored")
	}
	s := goodSpec()
	s.PHP.Version = "8.4"
	s.PHP.Extensions = []string{"redis"}
	err := s.Validate(c)
	if err == nil || !strings.Contains(err.Error(), "not available for PHP 8.4") {
		t.Fatalf("message should name the version: %v", err)
	}
}

func TestSnippetValidation(t *testing.T) {
	ok := []string{
		"",
		"location /health {\n    return 200 'ok';\n}",
		"add_header X-Test \"a;b\";  # a comment with load_module in it",
		"if ($request_method = POST) {\n  return 405;\n}",
		"location /api { limit_except GET { deny all; } }",
	}
	for _, s := range ok {
		if err := ValidateSnippet(s); err != nil {
			t.Errorf("rejected %q: %v", s, err)
		}
	}
	bad := map[string]string{
		"load_module":     "load_module modules/x.so;",
		"include":         "include /etc/passwd;",
		"upper case":      "INCLUDE /etc/passwd;",
		"lua":             "content_by_lua_block { os.execute('x') }",
		"lua prefix":      "lua_package_path '/x';",
		"perl":            "perl_set $x 'sub {}';",
		"njs":             "js_import x.js;",
		"user":            "user root;",
		"log redirect":    "access_log /etc/cron.d/x;",
		"key":             "ssl_certificate_key /etc/ssl/private/x.key;",
		"nested server":   "server { listen 81; }",
		"events":          "events { }",
		"upstream":        "upstream x { server 1.1.1.1; }",
		"alias traversal": "location /x { alias /var/www/../etc/; }",
		"missing semi":    "return 200",
		"unclosed brace":  "location / { return 200;",
		"extra brace":     "return 200; }",
		"unterminated q":  "return 200 'abc;",
		"too long":        strings.Repeat("a", MaxSnippetBytes+1),
		"too many lines":  strings.Repeat("\n", MaxSnippetLines+1),
		"nul":             "return 200;\x00",
		"include in blk":  "location / { include /x; }",
	}
	for name, s := range bad {
		if err := ValidateSnippet(s); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: accepted %q (%v)", name, s, err)
		}
	}
	// A comment must not hide a following directive on the next line.
	if err := ValidateSnippet("# x\ninclude /x;"); err == nil {
		t.Error("directive after comment accepted")
	}
}

func TestSnippetRenderedIntoServerBlock(t *testing.T) {
	s := goodSpec()
	s.Nginx.CustomSnippet = "location /health {\n  return 200 'ok';\n}\n"
	r, err := Render(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fileContent(r, "nginx/default.conf"), "include /etc/nginx/snippets/dbcm-custom.conf;") ||
		!strings.Contains(r.Dockerfile, "COPY nginx/custom.conf /etc/nginx/snippets/dbcm-custom.conf") {
		t.Fatal("snippet not wired in")
	}
}

func TestPlanPreview(t *testing.T) {
	tg := Target{Registry: "ghcr-main", Repository: "team/shop-web", Tag: "1.0"}
	p, err := NewPlan(goodSpec(), tg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nginx 1.27", "PHP-FPM 8.3", "gd, mysqli, opcache", "pinned by digest", "ghcr-main/team/shop-web:1.0"} {
		if !strings.Contains(p.Explanation, want) {
			t.Errorf("explanation lacks %q:\n%s", want, p.Explanation)
		}
	}
	if len(p.Steps) < 4 || p.Dockerfile == "" || len(p.Warnings) == 0 {
		t.Fatalf("plan incomplete: %+v", p)
	}
	p2, _ := NewPlan(goodSpec(), tg, nil)
	if p.Digest != p2.Digest {
		t.Fatal("plan digest not stable")
	}
	tg.Tag = "1.1"
	p3, _ := NewPlan(goodSpec(), tg, nil)
	if p3.Digest == p.Digest {
		t.Fatal("digest must cover the target")
	}
	if err := p.CheckApproval(p3.Digest); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatal("wrong approval accepted")
	}
	if err := p.CheckApproval(""); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatal("empty approval accepted")
	}
	for _, bad := range []Target{{}, {Registry: "r", Repository: "Bad/Upper", Tag: "t"}, {Registry: "r", Repository: "a", Tag: "-x"}, {Registry: "r", Repository: "a b", Tag: "t"}} {
		if _, err := NewPlan(goodSpec(), bad, nil); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("target %+v accepted", bad)
		}
	}
}

// --- docker/BuildKit command construction (never executed) ---

type recordRunner struct {
	cmd        Command
	configSeen string
	err        error
	meta       string
}

func (r *recordRunner) Run(_ context.Context, c Command) error {
	r.cmd = c
	for _, e := range c.Env {
		if d, ok := strings.CutPrefix(e, "DOCKER_CONFIG="); ok {
			b, err := os.ReadFile(filepath.Join(d, "config.json"))
			if err == nil {
				r.configSeen = string(b)
			}
			if st, err := os.Stat(filepath.Join(d, "config.json")); err == nil && st.Mode().Perm() != 0o600 {
				r.configSeen = "BAD MODE"
			}
		}
	}
	if r.err != nil {
		return r.err
	}
	// Emulate buildx writing the metadata file given by --metadata-file.
	for i, a := range c.Args {
		if a == "--metadata-file" {
			return os.WriteFile(c.Args[i+1], []byte(r.meta), 0o600)
		}
	}
	return nil
}

func TestBuildArgs(t *testing.T) {
	r, _ := Render(goodSpec(), nil)
	in := BuildInput{Rendered: r, Platforms: []string{"linux/amd64", "linux/arm64"}, Reference: "ghcr.io/team/shop:1", Push: true}
	args, err := BuildArgs(in, "/ctx", "/meta.json")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	want := "buildx build --platform linux/amd64,linux/arm64 --file /ctx/Dockerfile --tag ghcr.io/team/shop:1 --metadata-file /meta.json --progress plain --push /ctx"
	if got != want {
		t.Fatalf("args\n got %s\nwant %s", got, want)
	}
	in.Push = false
	if _, err := BuildArgs(in, "/ctx", "/m"); !errors.Is(err, ErrInvalidSpec) {
		t.Fatal("multi-platform without push accepted")
	}
	in.Platforms = []string{"linux/amd64"}
	args, _ = BuildArgs(in, "/ctx", "/m")
	if !strings.Contains(strings.Join(args, " "), "--load") || strings.Contains(strings.Join(args, " "), "--push") {
		t.Fatal("single-platform local build should --load")
	}
	for _, ref := range []string{"", "--rm", "a b"} {
		in.Reference = ref
		if _, err := BuildArgs(in, "/ctx", "/m"); err == nil {
			t.Errorf("reference %q accepted", ref)
		}
	}
}

func TestDockerBuilderWithFakeRunner(t *testing.T) {
	r, _ := Render(goodSpec(), nil)
	dig := "sha256:" + strings.Repeat("b", 64)
	rr := &recordRunner{meta: `{"containerimage.digest":"` + dig + `"}`}
	b := &DockerBuilder{Runner: rr, WorkDir: t.TempDir()}
	pw := "hunter2-very-secret"
	in := BuildInput{Rendered: r, Platforms: []string{"linux/amd64"}, Reference: "ghcr.io/team/shop:1", Push: true,
		Auth: &RegistryAuth{Server: "ghcr.io", Username: "bot", Password: registry.NewSecret(pw)}}
	out, err := b.Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out.ImageDigest != dig || out.SBOM.Status != "not_generated" || len(out.BaseDigests) != 1 || out.BaseDigests[0] != baseDigest {
		t.Fatalf("output %+v", out)
	}
	if rr.cmd.Name != "docker" || rr.cmd.Args[0] != "buildx" {
		t.Fatalf("command %v", rr.cmd)
	}
	if strings.Contains(strings.Join(rr.cmd.Args, " "), pw) || strings.Contains(strings.Join(rr.cmd.Env, " "), pw) {
		t.Fatal("password on the command line or in the environment")
	}
	want := base64.StdEncoding.EncodeToString([]byte("bot:" + pw))
	if !strings.Contains(rr.configSeen, want) || strings.Contains(rr.configSeen, pw+"\"") {
		t.Fatalf("docker config during build: %q", rr.configSeen)
	}
	// The temporary root, including the credential file, is gone.
	entries, _ := os.ReadDir(b.WorkDir)
	if len(entries) != 0 {
		t.Fatalf("temporary build directory left behind: %v", entries)
	}
	// The context contained the Dockerfile while building (checked via args path).
	rr2 := &recordRunner{err: errors.New("exit status 1")}
	_, err = (&DockerBuilder{Runner: rr2, WorkDir: t.TempDir()}).Build(context.Background(), in)
	if !errors.Is(err, ErrBuild) {
		t.Fatal("runner error swallowed")
	}
	_, err = (&DockerBuilder{Runner: &recordRunner{meta: "{}"}, WorkDir: t.TempDir()}).Build(context.Background(), in)
	if !errors.Is(err, ErrBuild) {
		t.Fatalf("missing digest accepted: %v", err)
	}
}

func TestWriteContextRejectsUnsafePaths(t *testing.T) {
	r := Rendered{Dockerfile: "FROM x\n", Files: []File{{Path: "../evil", Mode: 0o644, Content: "x"}}}
	if err := WriteContext(t.TempDir(), r); err == nil {
		t.Fatal("path traversal accepted")
	}
	r.Files[0].Path = "/abs"
	if err := WriteContext(t.TempDir(), r); err == nil {
		t.Fatal("absolute path accepted")
	}
}

// --- redaction, jobs, service ---

func TestRedactor(t *testing.T) {
	r := NewRedactor("s3cr3t-value")
	r.AddBasicAuth("bot", "s3cr3t-value")
	basic := base64.StdEncoding.EncodeToString([]byte("bot:s3cr3t-value"))
	in := strings.Join([]string{
		"login with s3cr3t-value ok",
		"Authorization: Bearer abc.def.ghi",
		"password=hunter2 next",
		`{"auth": "` + basic + `"}`,
		"push https://bot:pa55@ghcr.io/v2/",
		"TOKEN: tok123",
		"plain line",
	}, "\n")
	out := r.Redact(in)
	for _, leak := range []string{"s3cr3t-value", "abc.def.ghi", "hunter2", basic, "pa55", "tok123"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "plain line") {
		t.Fatal("over-redaction")
	}
}

func TestLogCaptureRedactsAcrossWritesAndCaps(t *testing.T) {
	lc := &logCapture{r: NewRedactor("topsecret"), max: 60}
	_, _ = lc.Write([]byte("start top"))
	_, _ = lc.Write([]byte("secret end\nsecond line\n"))
	lc.finish()
	if strings.Contains(lc.out.String(), "topsecret") || !strings.Contains(lc.out.String(), "[REDACTED]") {
		t.Fatalf("split secret leaked: %q", lc.out.String())
	}
	for i := 0; i < 20; i++ {
		_, _ = lc.Write([]byte("0123456789 filler filler\n"))
	}
	if !lc.truncated || lc.out.Len() > 60 {
		t.Fatalf("cap not enforced: %d truncated=%v", lc.out.Len(), lc.truncated)
	}
}

type fakeResolver struct {
	cfgs map[string]registry.Config
	sec  registry.Secret
}

func (f fakeResolver) Credential(_ context.Context, id string) (registry.Config, registry.Secret, error) {
	c, ok := f.cfgs[id]
	if !ok {
		return registry.Config{}, registry.Secret{}, registry.ErrNotFound
	}
	return c, f.sec, nil
}

func newService(fb *FakeBuilder) *Service {
	n := 0
	return &Service{
		Builder: fb, Jobs: &MemoryJobStore{},
		Registries: fakeResolver{
			cfgs: map[string]registry.Config{
				"ghcr-main": {ID: "ghcr-main", Kind: registry.KindGHCR, Endpoint: "https://ghcr.io", Username: "bot"},
				"hub":       {ID: "hub", Kind: registry.KindDockerHub, Endpoint: "https://registry-1.docker.io", Username: "bot"},
				"managed":   {ID: "managed", Kind: registry.KindManaged},
			},
			sec: registry.NewSecret("pw-12345-secret"),
		},
		Now:    func() time.Time { n++; return time.Date(2026, 10, 23, 12, 0, n, 0, time.UTC) },
		IDFunc: func() string { n++; return "ib-" + string(rune('a'+n)) },
	}
}

func TestExecutionIDPreventsSecondBuild(t *testing.T) {
	ctx := context.Background()
	fb := &FakeBuilder{}
	s := newService(fb)
	req := Request{Spec: goodSpec(), Target: Target{Registry: "ghcr-main", Repository: "team/shop", Tag: "1"}}
	plan, err := s.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ApprovalDigest, req.ExecutionID = plan.Digest, "vx-test-intent"
	first, err := s.Start(ctx, "admin", req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Start(ctx, "admin", req)
	if err != nil || second.ID != first.ID || len(fb.Calls) != 1 {
		t.Fatalf("duplicate execution: %+v, %+v, calls=%d, err=%v", first, second, len(fb.Calls), err)
	}
}

func TestServiceApprovalAndJobRecord(t *testing.T) {
	ctx := context.Background()
	fb := &FakeBuilder{LogLines: []string{"pushing with pw-12345-secret", "Authorization: Bearer zzz"}}
	s := newService(fb)
	req := Request{Spec: goodSpec(), Target: Target{Registry: "ghcr-main", Repository: "team/shop", Tag: "1"}}
	plan, err := s.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Reference != "ghcr.io/team/shop:1" || len(fb.Calls) != 0 {
		t.Fatalf("plan must not build: %+v calls=%d", plan, len(fb.Calls))
	}
	// No approval, wrong approval: nothing builds, nothing is recorded.
	for _, d := range []string{"", "sha256:" + strings.Repeat("0", 64)} {
		req.ApprovalDigest = d
		if _, err := s.Start(ctx, "alice", req); !errors.Is(err, ErrApprovalMismatch) {
			t.Fatalf("approval %q: %v", d, err)
		}
	}
	if jobs, _ := s.List(ctx); len(jobs) != 0 || len(fb.Calls) != 0 {
		t.Fatal("unapproved request left a trace")
	}
	req.ApprovalDigest = plan.Digest
	job, err := s.Start(ctx, "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != JobSucceeded || job.SpecHash != plan.SpecHash || job.ContextHash != plan.ContextHash ||
		job.ImageDigest == "" || job.SBOM.Status != "not_generated" || len(job.BaseDigests) != 1 || job.RequestedBy != "alice" {
		t.Fatalf("job %+v", job)
	}
	if fb.Calls[0].Auth == nil || fb.Calls[0].Auth.Server != "ghcr.io" || !fb.Calls[0].Push {
		t.Fatalf("builder input %+v", fb.Calls[0])
	}
	if strings.Contains(job.Log, "pw-12345-secret") || strings.Contains(job.Log, "zzz") || !strings.Contains(job.Log, "[REDACTED]") {
		t.Fatalf("log not redacted: %q", job.Log)
	}
	b, _ := json.Marshal(job)
	if strings.Contains(string(b), "pw-12345-secret") {
		t.Fatal("secret in job JSON")
	}
	// Determinism: the same approved request gives the same image digest in the fake.
	job2, _ := s.Start(ctx, "alice", req)
	if job2.ImageDigest != job.ImageDigest || job2.ID == job.ID {
		t.Fatalf("expected equal digest and different job: %+v", job2)
	}
	list, _ := s.List(ctx)
	if len(list) != 2 || list[0].ID != job2.ID || list[0].Log != "" || list[0].LogSize == 0 {
		t.Fatalf("list %+v", list)
	}
	if full, err := s.Get(ctx, job.ID); err != nil || full.Log == "" {
		t.Fatalf("get %v %v", full.Log, err)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrJobNotFound) {
		t.Fatal(err)
	}
}

func TestServiceFailureRecordedRedacted(t *testing.T) {
	ctx := context.Background()
	fb := &FakeBuilder{Err: errors.New("denied: password=pw-12345-secret rejected")}
	s := newService(fb)
	req := Request{Spec: goodSpec(), Target: Target{Registry: "ghcr-main", Repository: "team/shop", Tag: "1"}}
	p, _ := s.Plan(ctx, req)
	req.ApprovalDigest = p.Digest
	job, err := s.Start(ctx, "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != JobFailed || job.ImageDigest != "" || strings.Contains(job.Error, "pw-12345-secret") {
		t.Fatalf("job %+v", job)
	}
	stored, _ := s.Get(ctx, job.ID)
	if stored.Status != JobFailed {
		t.Fatal("failure not stored")
	}
}

func TestServiceRegistryResolution(t *testing.T) {
	ctx := context.Background()
	s := newService(&FakeBuilder{})
	if _, err := s.Plan(ctx, Request{Spec: goodSpec(), Target: Target{Registry: "missing", Repository: "a", Tag: "1"}}); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown registry: %v", err)
	}
	if _, err := s.Plan(ctx, Request{Spec: goodSpec(), Target: Target{Registry: "managed", Repository: "a", Tag: "1"}}); !errors.Is(err, registry.ErrInvalidConfig) {
		t.Fatalf("managed registry must not be a target yet: %v", err)
	}
	p, err := s.Plan(ctx, Request{Spec: goodSpec(), Target: Target{Registry: "hub", Repository: "me/web", Tag: "2"}})
	if err != nil || p.Reference != "docker.io/me/web:2" {
		t.Fatalf("%v %+v", err, p.Reference)
	}
	if _, err := s.Plan(ctx, Request{Spec: WebServerSpec{}, Target: Target{Registry: "hub", Repository: "a", Tag: "1"}}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("invalid spec: %v", err)
	}
}
