// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

func TestSnippetInjectionCorpus(t *testing.T) {
	for _, snippet := range []string{
		"location /x { root /etc; }", "alias /run/secrets/;",
		"location /x { try_files /../../etc/passwd =404; }",
		"location /x { proxy_pass http://169.254.169.254; }",
		"location /x { fastcgi_param SCRIPT_FILENAME /run/secrets/key; }",
		"location /x { auth_basic_user_file /etc/shadow; }",
		"location /x { mirror /internal; }",
		"location /x { dav_methods PUT; }",
		"location /x { autoindex on; }",
		"location /x { ssi on; }",
		"add_header X-Test 'line\nInjected: yes';",
		"add_header X-Test 'line\\\nInjected: yes';",
		"location /x { ROOT /etc; }",
		"location /x { alias \"/var/www/../etc\"; }",
		"location /x { include\t/etc/passwd; }",
		"location /x { return 200; }\nload_module /tmp/x.so;",
	} {
		if err := ValidateSnippet(snippet); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("accepted unsafe snippet %q: %v", snippet, err)
		}
	}
}

func TestBuilderRunnerErrorCannotExposeCredential(t *testing.T) {
	secret := "synthetic-builder-password"
	r, err := Render(goodSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	in := BuildInput{Rendered: r, Platforms: []string{"linux/amd64"}, Reference: "ghcr.io/team/test:1", Push: true,
		Auth: &RegistryAuth{Server: "ghcr.io", Username: "bot", Password: registry.NewSecret(secret)}}
	_, err = (&DockerBuilder{Runner: &recordRunner{err: errors.New("request failed: " + secret)}, WorkDir: t.TempDir()}).Build(context.Background(), in)
	if !errors.Is(err, ErrBuild) || strings.Contains(err.Error(), secret) {
		t.Fatalf("runner error leaked credential: %v", err)
	}
}

func TestBuildContextPathTraversalCorpus(t *testing.T) {
	for _, name := range []string{"../escape", "/tmp/escape", "nginx/../../escape", "nginx\\..\\escape", "nginx//config", "nginx/./config", "nginx/x\nRUN bad"} {
		root := t.TempDir()
		err := WriteContext(root, Rendered{Dockerfile: "FROM scratch\n", Files: []File{{Path: name, Mode: 0o644, Content: "x"}}})
		if err == nil {
			t.Errorf("accepted context path %q", name)
		}
		if _, statErr := os.Stat(filepath.Join(root, "escape")); statErr == nil {
			t.Fatalf("wrote outside intended path for %q", name)
		}
	}
}

func FuzzValidateSnippet(f *testing.F) {
	for _, seed := range []string{"location /health { return 204; }", "include /etc/passwd;", "root /etc;", "location /x { alias /../etc; }", "# comment\nreturn 200;", "return 200 '\\';"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		err := ValidateSnippet(s)
		if err != nil && !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("unexpected error type: %v", err)
		}
		if err == nil && strings.Contains(strings.ToLower(s), "include /etc/passwd;") {
			t.Fatal("accepted include")
		}
	})
}

func TestCatalogEvidenceIsExplicit(t *testing.T) {
	c := DefaultCatalog()
	check := func(kind, version, name string) {
		t.Helper()
		if evidence, ok := c.Evidence(kind, version, name); !ok || evidence != Unverified {
			t.Fatalf("%s %s %s: evidence=%q, known=%t", kind, version, name, evidence, ok)
		}
	}
	for _, v := range c.NginxVersions() {
		check("nginx", v, "")
	}
	for _, v := range c.PHPVersions() {
		check("php", v, "")
		for _, ext := range c.PHPExtensions(v) {
			check("extension", v, ext)
		}
	}
	for _, p := range c.Packages() {
		check("package", "", p)
	}
	if _, ok := c.Evidence("extension", "8.4", "redis"); ok {
		t.Fatal("unavailable extension marked selectable")
	}
}
