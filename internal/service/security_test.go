// SPDX-License-Identifier: AGPL-3.0-only

package service

import "testing"

func TestRoutePathTraversalCorpus(t *testing.T) {
	for _, s := range []string{"/admin", "/.well-known/acme-challenge", "/v1/items", "/api/"} {
		if !safeRoutePath(s) {
			t.Errorf("rejected safe path %q", s)
		}
	}
	for _, s := range []string{"/../admin", "/a/../admin", "/a//b", "/a/./b", "/%2e%2e/admin", "/%2fetc", "/a\\b", "//etc", "/a?b"} {
		if safeRoutePath(s) {
			t.Errorf("accepted ambiguous path %q", s)
		}
	}
}

func FuzzRoutePath(f *testing.F) {
	for _, s := range []string{"/ok", "/a/../b", "/%2e%2e/etc", "/a//b", "/a\\b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if safeRoutePath(s) && (s == "" || s[0] != '/' || s == "//") {
			t.Fatalf("accepted noncanonical path %q", s)
		}
	})
}

func TestSFTPHomeTraversalCorpus(t *testing.T) {
	for _, home := range []string{"/../etc", "/alice/../../etc", "/alice/./x", "/alice\\..\\etc", "/alice//x", "/alice/%2e%2e/etc", "/alice\nother"} {
		var p problems
		validateUsers(&p, []StorageUser{{Name: "alice", UID: 1000, Home: home, PasswordSecret: "alice-password"}}, true)
		if p.err() == nil {
			t.Errorf("accepted home %q", home)
		}
	}
}
