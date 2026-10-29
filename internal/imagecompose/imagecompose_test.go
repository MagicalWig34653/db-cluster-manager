// SPDX-License-Identifier: AGPL-3.0-only

package imagecompose

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func webRecipe(distro, release, nginx, php string) Recipe {
	r := Recipe{
		Name: "shop", Base: Base{Distro: distro, Release: release, Digest: testDigest},
		Components: []Selection{{ID: "nginx", Version: nginx}},
	}
	if php != "" {
		r.Components = append(r.Components, Selection{ID: "php", Version: php,
			Features: []string{"mysqli", "mbstring", "opcache", "zip"},
			Settings: map[string]any{"memory_limit": "256M", "upload_max_filesize": "64M", "post_max_size": "64M", "date.timezone": "Europe/Berlin"}})
	}
	return r
}

func wantIssue(t *testing.T, err error, path string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || !errors.Is(err, ErrInvalidRecipe) {
		t.Fatalf("want validation error at %q, got %v", path, err)
	}
	for _, i := range ve.Issues {
		if strings.HasPrefix(i.Path, path) {
			return
		}
	}
	t.Fatalf("no issue at %q: %v", path, err)
}

func TestEmbeddedCatalogIsValidAndHonest(t *testing.T) {
	c := Default()
	if c.Evidence != EvidenceUnverified || c.Version == "" {
		t.Fatalf("%+v", c)
	}
	for _, id := range []string{"nginx", "php", "composer", "utilities"} {
		if _, ok := c.Component(id); !ok {
			t.Errorf("component %s missing", id)
		}
	}
	// The compatibility matrix is not a full cross product: PHP 8.3 does not
	// exist on Debian 12 and nginx 1.22 only there.
	php, _ := c.Component("php")
	v83, _ := php.Version("8.3")
	if _, ok := v83.SupportFor("debian", "12"); ok {
		t.Error("php 8.3 must not be offered on Debian 12")
	}
}

func TestCatalogValidationRejectsBrokenData(t *testing.T) {
	good := string(embedded)
	for name, mutate := range map[string]func(string) string{
		"unknown field": func(s string) string {
			return strings.Replace(s, `"kind": "server"`, `"kind": "server", "shell": "x"`, 1)
		},
		"verified claim": func(s string) string {
			return strings.Replace(s, `"evidence": "unverified"`, `"evidence": "verified"`, 1)
		},
		"bad schema":        func(s string) string { return strings.Replace(s, `"schemaVersion": 1`, `"schemaVersion": 9`, 1) },
		"unknown release":   func(s string) string { return strings.Replace(s, `"release": "3.20"`, `"release": "9.9"`, 1) },
		"default off range": func(s string) string { return strings.Replace(s, `"default": 1024`, `"default": 99999999`, 1) },
		"free text setting": func(s string) string { return strings.Replace(s, `"maxLength": 253,`, ``, 1) },
		"unknown arch": func(s string) string {
			return strings.Replace(s, `"architectures": ["amd64", "arm64"]}`, `"architectures": ["riscv"]}`, 1)
		},
		"bad package name": func(s string) string {
			return strings.Replace(s, `{"name": "nginx", "constraint": "1.26"}`, `{"name": "nginx; rm -rf /"}`, 1)
		},
		"bad component link": func(s string) string { return strings.Replace(s, `"requires": ["php"]`, `"requires": ["nope"]`, 1) },
	} {
		if _, err := Parse([]byte(mutate(good))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
}

func TestEveryFeatureMapsToFamilyPackages(t *testing.T) {
	c := Default()
	for _, comp := range c.Components {
		for _, f := range comp.Features {
			for fam, pkgs := range f.Packages {
				for _, p := range pkgs {
					if strings.ContainsAny(p, " ;&|$`") {
						t.Errorf("%s/%s/%s: %q", comp.ID, f.ID, fam, p)
					}
				}
			}
		}
	}
}

func TestResolveAppliesDefaultsAndIsCanonical(t *testing.T) {
	c := Default()
	a, err := c.Resolve(webRecipe("alpine", "3.20", "1.26", "8.3"))
	if err != nil {
		t.Fatal(err)
	}
	b := webRecipe("alpine", "3.20", "1.26", "8.3")
	// Same intent in a different spelling: order, duplicates, float numbers,
	// explicit defaults.
	b.Components[0], b.Components[1] = b.Components[1], b.Components[0]
	b.Components[0].Features = []string{"zip", "opcache", "mbstring", "mysqli", "mysqli"}
	b.Components[1].Settings = map[string]any{"workerConnections": float64(1024), "gzip": true}
	b.Platforms = []string{"linux/amd64", "linux/amd64"}
	bn, err := c.Resolve(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest(a) != c.Digest(bn) {
		t.Fatalf("equal intent, different digest:\n%+v\n%+v", a, bn)
	}
	php := settingsOf(a, "php")
	if php["memory_limit"] != "256M" || php["pm"] != "dynamic" || php["expose_php"] != false || php["opcache.enable"] != true {
		t.Fatalf("defaults not applied: %v", php)
	}
	d2 := webRecipe("alpine", "3.20", "1.26", "8.3")
	d2.Components[1].Settings["memory_limit"] = "512M"
	n2, _ := c.Resolve(d2)
	if c.Digest(n2) == c.Digest(a) {
		t.Fatal("changed setting kept the digest")
	}
}

func TestDigestIncludesCatalogAndGeneratorVersion(t *testing.T) {
	c := Default()
	n, _ := c.Resolve(webRecipe("alpine", "3.20", "1.26", ""))
	d := c.Digest(n)
	c2 := *c
	c2.Version = "other"
	if c2.Digest(n) == d {
		t.Fatal("catalog version not part of the digest")
	}
}

func TestValidationRejects(t *testing.T) {
	c := Default()
	mod := func(f func(*Recipe)) Recipe { r := webRecipe("alpine", "3.20", "1.26", "8.3"); f(&r); return r }
	cases := map[string]struct {
		r    Recipe
		path string
	}{
		"floating base":      {mod(func(r *Recipe) { r.Base.Digest = "" }), "base.digest"},
		"short digest":       {mod(func(r *Recipe) { r.Base.Digest = "sha256:abc" }), "base.digest"},
		"unknown distro":     {mod(func(r *Recipe) { r.Base.Distro = "gentoo" }), "base"},
		"bad name":           {mod(func(r *Recipe) { r.Name = "x;rm" }), "name"},
		"unknown component":  {mod(func(r *Recipe) { r.Components = append(r.Components, Selection{ID: "shell", Version: "1"}) }), "components[shell]"},
		"duplicate":          {mod(func(r *Recipe) { r.Components = append(r.Components, r.Components[0]) }), "components[nginx]"},
		"version off matrix": {mod(func(r *Recipe) { r.Components[0].Version = "1.22" }), "components[nginx].version"},
		"unknown version":    {mod(func(r *Recipe) { r.Components[1].Version = "5.6" }), "components[php].version"},
		"unknown extension":  {mod(func(r *Recipe) { r.Components[1].Features = []string{"phar-evil"} }), "components[php].features"},
		"unknown setting":    {mod(func(r *Recipe) { r.Components[1].Settings["auto_prepend_file"] = "/tmp/x" }), "components[php].settings.auto_prepend_file"},
		"shell in setting":   {mod(func(r *Recipe) { r.Components[0].Settings = map[string]any{"serverName": "a; rm -rf /"} }), "components[nginx].settings.serverName"},
		"newline injection": {mod(func(r *Recipe) {
			r.Components[0].Settings = map[string]any{"documentRoot": "/var/www/a\nlocation /x { }"}
		}), "components[nginx].settings.documentRoot"},
		"traversal root":    {mod(func(r *Recipe) { r.Components[0].Settings = map[string]any{"documentRoot": "/etc"} }), "components[nginx].settings.documentRoot"},
		"int out of range":  {mod(func(r *Recipe) { r.Components[0].Settings = map[string]any{"workerConnections": 5} }), "components[nginx].settings.workerConnections"},
		"int as text":       {mod(func(r *Recipe) { r.Components[0].Settings = map[string]any{"workerConnections": "10; id"} }), "components[nginx].settings.workerConnections"},
		"enum":              {mod(func(r *Recipe) { r.Components[1].Settings["pm"] = "static; id" }), "components[php].settings.pm"},
		"bool as text":      {mod(func(r *Recipe) { r.Components[1].Settings["expose_php"] = "yes" }), "components[php].settings.expose_php"},
		"post below upload": {mod(func(r *Recipe) { r.Components[1].Settings["post_max_size"] = "8M" }), "components[php].settings.post_max_size"},
		"memory below post": {mod(func(r *Recipe) { r.Components[1].Settings["memory_limit"] = "32M" }), "components[php].settings.memory_limit"},
		"opcache w/o ext": {mod(func(r *Recipe) {
			r.Components[1].Features = []string{"mbstring"}
			r.Components[1].Settings["opcache.enable"] = true
		}), "components[php].settings.opcache.enable"},
		"pool spare order": {mod(func(r *Recipe) {
			r.Components[1].Settings["pm.min_spare_servers"] = 5
			r.Components[1].Settings["pm.max_spare_servers"] = 3
		}), "components[php].settings.pm.min_spare_servers"},
		"no components": {mod(func(r *Recipe) { r.Components = nil }), "components"},
		"composer w/o php": {mod(func(r *Recipe) {
			r.Components = []Selection{r.Components[0], {ID: "composer", Version: "2"}}
		}), "components[composer]"},
		"composer on alma": {Recipe{Name: "x", Base: Base{"almalinux", "9", testDigest}, Components: []Selection{
			{ID: "php", Version: "8.3"}, {ID: "composer", Version: "2"}}}, "components[composer].version"},
		"redis on alma": {Recipe{Name: "x", Base: Base{"almalinux", "9", testDigest}, Components: []Selection{{ID: "php", Version: "8.3", Features: []string{"redis"}}}}, "components[php].features"},
		"nginx setting w/o php": {Recipe{Name: "x", Base: Base{"alpine", "3.20", testDigest}, Components: []Selection{
			{ID: "nginx", Version: "1.26", Settings: map[string]any{"fastcgiReadTimeout": 30}}}}, "components[nginx].settings.fastcgiReadTimeout"},
		"platform":       {mod(func(r *Recipe) { r.Platforms = []string{"linux/s390x"} }), "platforms"},
		"utilities only": {Recipe{Name: "x", Base: Base{"alpine", "3.20", testDigest}, Components: []Selection{{ID: "utilities", Version: "distro"}}}, "components"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Generate(tc.r)
			wantIssue(t, err, tc.path)
		})
	}
}

func TestSingleProcessRule(t *testing.T) {
	// A second main process is rejected by the generic rule: simulate a
	// catalog where php claims to be a main process too.
	c := *Default()
	comps := append([]Component(nil), c.Components...)
	for i := range comps {
		if comps[i].ID == "php" {
			comps[i].Process = "main"
		}
	}
	c.Components = comps
	_, err := c.Resolve(webRecipe("alpine", "3.20", "1.26", "8.3"))
	wantIssue(t, err, "components")
	if !strings.Contains(err.Error(), "single-process rule") {
		t.Fatal(err)
	}
}

func TestGeneratedOutputIsHardenedAndDeterministic(t *testing.T) {
	c := Default()
	for _, tc := range []struct{ d, r, ng, php string }{
		{"alpine", "3.20", "1.26", "8.3"}, {"debian", "12", "1.22", "8.2"},
		{"ubuntu", "24.04", "1.24", "8.3"}, {"almalinux", "9", "1.26", "8.3"},
	} {
		r := webRecipe(tc.d, tc.r, tc.ng, tc.php)
		a, err := c.Generate(r)
		if err != nil {
			t.Fatalf("%s: %v", tc.d, err)
		}
		b, _ := c.Generate(r)
		if a.Rendered.Dockerfile != b.Rendered.Dockerfile || a.Rendered.ContextHash != b.Rendered.ContextHash || a.Digest != b.Digest {
			t.Fatalf("%s: not deterministic", tc.d)
		}
		df := a.Rendered.Dockerfile
		for _, want := range []string{"@" + testDigest, "USER 10001:10001", "nginx -t -c", "dbcm.recipe-digest=\"" + a.Digest + "\""} {
			if !strings.Contains(df, want) {
				t.Errorf("%s: Dockerfile lacks %q", tc.d, want)
			}
		}
		// Hardening invariants: no root runtime, no remote scripts, no
		// secrets or build args, no ADD, no privileged port.
		for _, bad := range []string{"curl -", "curl http", "wget", "| sh", "| bash", "ARG ", "ADD ", "EXPOSE 80\n", "USER root", "--privileged", "latest"} {
			if strings.Contains(df, bad) {
				t.Errorf("%s: Dockerfile contains %q", tc.d, bad)
			}
		}
		if strings.LastIndex(df, "USER 10001") > strings.Index(df, "ENTRYPOINT") {
			t.Errorf("%s: USER must precede ENTRYPOINT", tc.d)
		}
		for _, f := range a.Rendered.Files {
			if strings.Contains(f.Content, "listen 80;") {
				t.Errorf("%s: %s binds a privileged port", tc.d, f.Path)
			}
		}
		if len(a.Packages) == 0 || a.Role != RoleWeb {
			t.Errorf("%s: %+v", tc.d, a.Packages)
		}
	}
}

func TestVariantsFollowTheMatrix(t *testing.T) {
	c := Default()
	n, _ := c.Resolve(webRecipe("alpine", "3.20", "1.26", "8.3"))
	byRel := map[string]Variant{}
	for _, v := range c.Variants(n) {
		byRel[v.Distro+"/"+v.Release] = v
	}
	if !byRel["alpine/3.20"].Compatible || !byRel["almalinux/9"].Compatible {
		t.Fatalf("%+v", byRel)
	}
	deb := byRel["debian/12"]
	if deb.Compatible || deb.Versions["php"] != "8.2" || deb.Versions["nginx"] != "1.22" {
		t.Fatalf("debian variant: %+v", deb)
	}
}

func TestGoldenFiles(t *testing.T) {
	c := Default()
	pool := webRecipe("alpine", "3.20", "1.26", "8.3")
	pool.Components[1].Settings["pm"] = "ondemand"
	static := Recipe{Name: "static site", Base: Base{"debian", "12", testDigest}, Components: []Selection{
		{ID: "nginx", Version: "1.22", Settings: map[string]any{"serverName": "example.org", "gzip": false}},
		{ID: "utilities", Version: "distro", Features: []string{"tzdata", "ca-certificates"}}}}
	fpm := Recipe{Name: "fpm only", Base: Base{"ubuntu", "24.04", testDigest}, Platforms: []string{"linux/amd64", "linux/arm64"},
		Components: []Selection{{ID: "php", Version: "8.3", Features: []string{"pdo_mysql", "gd"}}, {ID: "composer", Version: "2"}}}
	for name, r := range map[string]Recipe{
		"alpine-nginx-php": webRecipe("alpine", "3.20", "1.26", "8.3"),
		"debian-nginx-php": webRecipe("debian", "12", "1.22", "8.2"),
		"ubuntu-nginx-php": webRecipe("ubuntu", "24.04", "1.24", "8.3"),
		"alma-nginx-php":   webRecipe("almalinux", "9", "1.26", "8.3"),
		"alpine-ondemand":  pool,
		"debian-static":    static,
		"ubuntu-fpm-only":  fpm,
	} {
		t.Run(name, func(t *testing.T) {
			res, err := c.Generate(r)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join("testdata", "golden", name)
			got := map[string]string{"Dockerfile": res.Rendered.Dockerfile}
			for _, f := range res.Rendered.Files {
				got[f.Path] = f.Content
			}
			if *update {
				_ = os.RemoveAll(dir)
				for p, content := range got {
					full := filepath.Join(dir, p)
					if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			for p, content := range got {
				want, err := os.ReadFile(filepath.Join(dir, p))
				if err != nil {
					t.Fatalf("missing golden %s (run with -update): %v", p, err)
				}
				if string(want) != content {
					t.Errorf("%s differs from golden:\n%s", p, content)
				}
			}
			// No stale golden files.
			_ = filepath.Walk(dir, func(path string, info os.FileInfo, _ error) error {
				if info != nil && !info.IsDir() {
					rel, _ := filepath.Rel(dir, path)
					if _, ok := got[filepath.ToSlash(rel)]; !ok {
						t.Errorf("stale golden file %s", rel)
					}
				}
				return nil
			})
		})
	}
}

// The web mock server carries a copy of the catalog; it must not drift.
func TestWebMockCatalogEqualsEmbedded(t *testing.T) {
	mock, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "mock-composer-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mock) != string(embedded) {
		t.Fatal("web/src/lib/mock-composer-catalog.json differs from internal/imagecompose/catalog.json; copy the file")
	}
}
