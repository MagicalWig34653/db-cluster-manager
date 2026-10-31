// SPDX-License-Identifier: AGPL-3.0-only

package imagecompose

import (
	"strings"
	"testing"
)

// Catalog corrections found by the package audit of lab E17 (every package
// spec the catalog can emit was resolved in the real base images).

func TestBrotliIsRefusedOnUbuntu2204(t *testing.T) {
	c := Default()
	r := Recipe{Name: "web", Base: Base{"ubuntu", "22.04", testDigest},
		Components: []Selection{{ID: "nginx", Version: "1.18", Features: []string{"brotli"}}}}
	_, err := c.Generate(r)
	wantIssue(t, err, "components[nginx].features")
	r.Components[0].Features = nil
	if _, err := c.Generate(r); err != nil {
		t.Fatalf("nginx 1.18 without brotli must stay possible: %v", err)
	}
	// The same module exists on Debian 12 and stays selectable there.
	d := Recipe{Name: "web", Base: Base{"debian", "12", testDigest},
		Components: []Selection{{ID: "nginx", Version: "1.22", Features: []string{"brotli"}}}}
	if _, err := c.Generate(d); err != nil {
		t.Fatalf("brotli on debian 12: %v", err)
	}
}

func TestRhelBasesDoNotOfferEpelOnlyUtilities(t *testing.T) {
	c := Default()
	for _, feat := range []string{"imagemagick", "msmtp"} {
		r := Recipe{Name: "web", Base: Base{"almalinux", "9", testDigest},
			Components: []Selection{{ID: "nginx", Version: "1.26"}, {ID: "utilities", Version: "distro", Features: []string{feat}}}}
		_, err := c.Generate(r)
		wantIssue(t, err, "components[utilities].features")
	}
}

// curl-minimal of the AlmaLinux and Rocky base images already provides curl;
// `dnf install curl` fails there with a conflict.
func TestCurlUtilityAddsNoPackageOnRhel(t *testing.T) {
	c := Default()
	r := Recipe{Name: "web", Base: Base{"almalinux", "9", testDigest},
		Components: []Selection{{ID: "nginx", Version: "1.26"}, {ID: "utilities", Version: "distro", Features: []string{"curl"}}}}
	res, err := c.Generate(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Packages {
		if p.Name == "curl" {
			t.Fatalf("curl must not be installed on a dnf base: %+v", res.Packages)
		}
	}
	if strings.Contains(res.Rendered.Dockerfile, "\n      curl") {
		t.Fatal("curl in the dnf install line")
	}
}

func TestSupportWithoutMustNameAFeature(t *testing.T) {
	bad := strings.Replace(string(embedded), `"without": ["brotli"]`, `"without": ["nope"]`, 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("unknown feature in without accepted")
	}
}
