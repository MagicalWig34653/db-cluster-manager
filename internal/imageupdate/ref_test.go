// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

import "testing"

func TestParseRef(t *testing.T) {
	dg := "sha256:" + hex64('a')
	cases := []struct {
		in, reg, repo, tag, digest, out string
	}{
		{"nginx:1.25.3", "docker.io", "library/nginx", "1.25.3", "", "nginx:1.25.3"},
		{"bitnami/redis:7.2", "docker.io", "bitnami/redis", "7.2", "", "bitnami/redis:7.2"},
		{"ghcr.io/acme/web:2.0.1", "ghcr.io", "acme/web", "2.0.1", "", "ghcr.io/acme/web:2.0.1"},
		{"localhost:5000/x/y:3", "localhost:5000", "x/y", "3", "", "localhost:5000/x/y:3"},
		{"ghcr.io/acme/web:2.0.1@" + dg, "ghcr.io", "acme/web", "2.0.1", dg, "ghcr.io/acme/web:2.0.1@" + dg},
		{"ghcr.io/acme/web@" + dg, "ghcr.io", "acme/web", "", dg, "ghcr.io/acme/web@" + dg},
	}
	for _, c := range cases {
		r, err := ParseRef(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if r.Registry != c.reg || r.Repo != c.repo || r.Tag != c.tag || r.Digest != c.digest || r.String() != c.out {
			t.Fatalf("%s: %+v -> %q", c.in, r, r.String())
		}
	}
	for _, bad := range []string{"", "a b", "ghcr.io/", "x:", "nginx@sha256:short", "UPPER/x:1", "http://x/y:1"} {
		if _, err := ParseRef(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestRefPinned(t *testing.T) {
	r, _ := ParseRef("nginx:1.25")
	p := r.Pinned("1.26", "sha256:"+hex64('b'))
	if p.String() != "nginx:1.26@sha256:"+hex64('b') {
		t.Fatal(p.String())
	}
	if r.String() != "nginx:1.25" {
		t.Fatal("Pinned changed the receiver")
	}
}

func hex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func mustRef(t *testing.T, s string) Ref {
	t.Helper()
	r, err := ParseRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
