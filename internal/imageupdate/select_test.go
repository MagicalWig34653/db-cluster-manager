// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

import "testing"

func TestPickTag(t *testing.T) {
	tags := []string{"1.25.2", "1.25.3", "1.25.4", "1.26.0", "1.26.1", "2.0.0", "2.0.0-rc1", "1.25.4-alpine", "latest", "stable", "v1.27.0", "1.25.10"}
	cur := mustRef(t, "nginx:1.25.3")
	cases := []struct {
		track Track
		db    bool
		want  string
		kind  Kind
		ok    bool
	}{
		{TrackDigest, false, "", "", false},
		{TrackPatch, false, "1.25.10", KindPatch, true},
		{TrackMinor, false, "1.26.1", KindMinor, true},
		{TrackMajor, false, "2.0.0", KindMajor, true},
		{TrackMajor, true, "1.26.1", KindMinor, true}, // never major for databases
	}
	for _, c := range cases {
		got, kind, ok := PickTag(cur, tags, c.track, c.db)
		if got != c.want || kind != c.kind || ok != c.ok {
			t.Fatalf("%s db=%v: %q %q %v, want %q %q %v", c.track, c.db, got, kind, ok, c.want, c.kind, c.ok)
		}
	}
}

func TestPickTagVariantAndPrecision(t *testing.T) {
	tags := []string{"1.25.3-alpine", "1.25.5-alpine", "1.25.9", "1.26-alpine", "1.26.0-alpine"}
	got, _, ok := PickTag(mustRef(t, "nginx:1.25.3-alpine"), tags, TrackMinor, false)
	if !ok || got != "1.26.0-alpine" {
		t.Fatalf("%q %v", got, ok)
	}
	got, _, ok = PickTag(mustRef(t, "nginx:1.25.3-alpine"), tags, TrackPatch, false)
	if !ok || got != "1.25.5-alpine" {
		t.Fatalf("%q %v", got, ok)
	}
	// Major.minor tags move on the same precision only.
	got, _, ok = PickTag(mustRef(t, "mariadb:11.4"), []string{"11.4", "11.4.2", "11.5", "12.0"}, TrackMinor, true)
	if !ok || got != "11.5" {
		t.Fatalf("%q %v", got, ok)
	}
	// Non-numeric current tags have no version candidates.
	if _, _, ok := PickTag(mustRef(t, "nginx:stable"), []string{"stable", "1.2.3"}, TrackMajor, false); ok {
		t.Fatal("stable has a version candidate")
	}
	// Digest-pinned only: no tag, no version candidates.
	if _, _, ok := PickTag(mustRef(t, "nginx@sha256:"+hex64('a')), []string{"1.2.3"}, TrackMajor, false); ok {
		t.Fatal("tagless ref has a version candidate")
	}
}

func TestIsDatabase(t *testing.T) {
	for _, c := range []struct {
		typ, repo string
		want      bool
	}{
		{"galera", "x/y", true}, {"container", "library/mariadb", true}, {"container", "bitnami/postgresql", true},
		{"container", "percona/percona-server", true}, {"container", "library/nginx", false}, {"traefik", "library/traefik", false},
	} {
		if got := IsDatabase(c.typ, c.repo); got != c.want {
			t.Fatalf("%s %s: %v", c.typ, c.repo, got)
		}
	}
}
