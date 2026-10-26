// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"slices"
	"sort"
	"strings"
)

// Catalog is the allow-list of selectable versions, extensions and packages.
// The default catalog is a proposal. Every entry has a provenance status;
// no bundled upstream package index or extension documentation currently
// verifies these names. A real build is still required before release.
type Catalog struct {
	nginx    []string
	php      map[string]map[string]bool // version -> extension -> available
	known    map[string]bool
	packages []string
}

// CatalogEvidence distinguishes an upstream-document-checked name from a
// proposal. It does not claim that a package is available in a given base
// image or Alpine repository snapshot.
type CatalogEvidence string

const (
	VerifiedByDoc CatalogEvidence = "verified-by-doc"
	Unverified    CatalogEvidence = "unverified"
)

// Evidence returns the status for a selectable catalog entry. kind is
// "nginx", "php", "extension" or "package". The current bundled catalog
// has no upstream documentation snapshot, so every selectable entry is
// explicitly unverified; unknown entries return false.
func (c *Catalog) Evidence(kind, version, name string) (CatalogEvidence, bool) {
	switch kind {
	case "nginx":
		return Unverified, c.HasNginx(version)
	case "php":
		return Unverified, c.HasPHP(version)
	case "extension":
		return Unverified, c.PHPExtensionAvailable(version, name)
	case "package":
		return Unverified, c.HasPackage(name)
	}
	return "", false
}

// DefaultCatalog returns the built-in catalog.
func DefaultCatalog() *Catalog {
	all := []string{"bcmath", "curl", "exif", "gd", "gmp", "intl", "mbstring", "mysqli",
		"opcache", "pdo_mysql", "pdo_pgsql", "pgsql", "redis", "sodium", "xml", "zip"}
	c := &Catalog{
		nginx:    []string{"1.26", "1.27"},
		php:      map[string]map[string]bool{},
		known:    map[string]bool{},
		packages: []string{"ca-certificates", "curl", "ghostscript", "imagemagick", "msmtp", "tzdata"},
	}
	for _, e := range all {
		c.known[e] = true
	}
	for _, v := range []string{"8.2", "8.3", "8.4"} {
		m := map[string]bool{}
		for _, e := range all {
			m[e] = true
		}
		c.php[v] = m
	}
	// Per-version availability differences (proposal): the redis extension
	// package is assumed absent for the newest PHP until verified, and gmp
	// is assumed absent for 8.2.
	c.php["8.4"]["redis"] = false
	c.php["8.2"]["gmp"] = false
	return c
}

// NginxVersions lists selectable nginx versions.
func (c *Catalog) NginxVersions() []string { return slices.Clone(c.nginx) }

// HasNginx reports whether the nginx version is selectable.
func (c *Catalog) HasNginx(v string) bool { return slices.Contains(c.nginx, v) }

// PHPVersions lists selectable PHP versions, sorted.
func (c *Catalog) PHPVersions() []string {
	out := make([]string, 0, len(c.php))
	for v := range c.php {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// HasPHP reports whether the PHP version is selectable.
func (c *Catalog) HasPHP(v string) bool { _, ok := c.php[v]; return ok }

// KnownExtension reports whether the extension is in the allow-list for any
// version.
func (c *Catalog) KnownExtension(e string) bool { return c.known[e] }

// PHPExtensionAvailable reports whether the extension may be selected for
// the PHP version.
func (c *Catalog) PHPExtensionAvailable(version, ext string) bool { return c.php[version][ext] }

// PHPExtensions lists the extensions available for a PHP version, sorted.
func (c *Catalog) PHPExtensions(version string) []string {
	var out []string
	for e, ok := range c.php[version] {
		if ok {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

// Packages lists allow-listed extra packages.
func (c *Catalog) Packages() []string { return slices.Clone(c.packages) }

// HasPackage reports whether the package is allow-listed.
func (c *Catalog) HasPackage(p string) bool { return slices.Contains(c.packages, p) }

// phpPkg is the Alpine package prefix of a PHP version ("8.3" -> "php83").
func phpPkg(version string) string { return "php" + strings.ReplaceAll(version, ".", "") }
