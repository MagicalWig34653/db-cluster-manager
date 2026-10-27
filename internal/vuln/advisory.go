// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Severity is the normalised severity of an advisory.
type Severity string

const (
	SeverityUnknown  Severity = "unknown"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Rank orders severities; unknown ranks lowest.
func (s Severity) Rank() int {
	switch s {
	case SeverityLow:
		return 1
	case SeverityMedium:
		return 2
	case SeverityHigh:
		return 3
	case SeverityCritical:
		return 4
	}
	return 0
}

// Valid reports whether s is one of the defined severities.
func (s Severity) Valid() bool { return s == SeverityUnknown || s.Rank() > 0 }

// ParseSeverity maps feed spellings (CRITICAL, Moderate, ...) to a Severity.
func ParseSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return SeverityCritical
	case "high", "important":
		return SeverityHigh
	case "medium", "moderate":
		return SeverityMedium
	case "low", "minor", "negligible":
		return SeverityLow
	}
	return SeverityUnknown
}

// SeverityFromCVSS maps a CVSS v3 base score to a severity band.
func SeverityFromCVSS(score float64) Severity {
	switch {
	case score >= 9.0:
		return SeverityCritical
	case score >= 7.0:
		return SeverityHigh
	case score >= 4.0:
		return SeverityMedium
	case score > 0:
		return SeverityLow
	}
	return SeverityUnknown
}

// Range is one affected version range of a package. Introduced empty means
// "from the first version"; Fixed is the first unaffected version;
// LastAffected is the last affected version when no fix exists. Exactly one
// of Fixed and LastAffected may be set; neither means "all versions from
// Introduced on".
type Range struct {
	Introduced   string `json:"introduced,omitempty"`
	Fixed        string `json:"fixed,omitempty"`
	LastAffected string `json:"lastAffected,omitempty"`
}

// Affected names one package of an ecosystem and the affected versions.
type Affected struct {
	// Ecosystem is a normalised ecosystem name (see NormalizeEcosystem).
	Ecosystem string   `json:"ecosystem"`
	Package   string   `json:"package"`
	Ranges    []Range  `json:"ranges,omitempty"`
	Versions  []string `json:"versions,omitempty"`
}

// Advisory is one vulnerability record as fetched from a feed. Summary,
// Details and References are untrusted text: they are displayed and passed
// to an AI provider only as delimited data, never interpreted.
type Advisory struct {
	// ID is the canonical identifier: the CVE ID when one is known,
	// otherwise the source's ID (GHSA-..., OSV ID).
	ID      string     `json:"id"`
	Aliases []string   `json:"aliases,omitempty"`
	Sources []string   `json:"sources"`
	Summary string     `json:"summary,omitempty"`
	Details string     `json:"details,omitempty"`
	Affect  []Affected `json:"affected"`
	// Severity, CVSS, KEV and EPSS are the prioritisation inputs. KEV
	// reports a listing in a known-exploited catalog (CISA KEV); EPSS is the
	// exploit prediction probability 0..1 where a feed provides it.
	Severity   Severity  `json:"severity"`
	CVSS       float64   `json:"cvss,omitempty"`
	KEV        bool      `json:"kev,omitempty"`
	EPSS       float64   `json:"epss,omitempty"`
	References []string  `json:"references,omitempty"`
	Published  time.Time `json:"published,omitempty"`
	Modified   time.Time `json:"modified,omitempty"`
	FetchedAt  time.Time `json:"fetchedAt"`
	// Withdrawn marks a retracted advisory; it no longer creates findings.
	Withdrawn bool `json:"withdrawn,omitempty"`
}

// Size caps for untrusted advisory text.
const (
	MaxSummaryBytes   = 1 << 10
	MaxDetailsBytes   = 16 << 10
	MaxReferences     = 32
	MaxReferenceBytes = 512
	MaxAffected       = 256
)

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func isCVE(id string) bool { return strings.HasPrefix(strings.ToUpper(id), "CVE-") }

// Normalize returns a copy with a canonical ID, sorted and de-duplicated
// lists, normalised ecosystems and capped untrusted text.
func (a Advisory) Normalize() Advisory {
	out := a
	ids := append([]string{a.ID}, a.Aliases...)
	canon := ""
	var cves []string
	for _, id := range ids {
		if isCVE(id) {
			cves = append(cves, strings.ToUpper(id))
		}
	}
	sort.Strings(cves)
	if len(cves) > 0 {
		canon = cves[0]
	} else {
		canon = strings.TrimSpace(a.ID)
	}
	out.ID = canon
	var aliases []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if isCVE(id) {
			id = strings.ToUpper(id)
		}
		if id != "" && id != canon {
			aliases = append(aliases, id)
		}
	}
	out.Aliases = sortedUnique(aliases)
	out.Sources = sortedUnique(a.Sources)
	out.Summary = truncate(strings.TrimSpace(a.Summary), MaxSummaryBytes)
	out.Details = truncate(strings.TrimSpace(a.Details), MaxDetailsBytes)
	var refs []string
	for _, r := range a.References {
		if r = strings.TrimSpace(r); r != "" && len(r) <= MaxReferenceBytes {
			refs = append(refs, r)
		}
	}
	refs = sortedUnique(refs)
	if len(refs) > MaxReferences {
		refs = refs[:MaxReferences]
	}
	out.References = refs
	if !out.Severity.Valid() || out.Severity == "" {
		out.Severity = SeverityUnknown
	}
	if out.Severity == SeverityUnknown && out.CVSS > 0 {
		out.Severity = SeverityFromCVSS(out.CVSS)
	}
	if out.EPSS < 0 || out.EPSS > 1 {
		out.EPSS = 0
	}
	aff := make([]Affected, 0, len(a.Affect))
	pos := map[[2]string]int{}
	for _, af := range a.Affect {
		af.Ecosystem = NormalizeEcosystem(af.Ecosystem)
		af.Package = NormalizePackage(af.Package)
		if af.Package == "" {
			continue
		}
		k := [2]string{af.Ecosystem, af.Package}
		if i, ok := pos[k]; ok {
			aff[i].Ranges = uniqueRanges(append(aff[i].Ranges, af.Ranges...))
			aff[i].Versions = sortedUnique(append(aff[i].Versions, af.Versions...))
			continue
		}
		pos[k] = len(aff)
		af.Versions = sortedUnique(af.Versions)
		af.Ranges = uniqueRanges(af.Ranges)
		aff = append(aff, af)
	}
	sort.SliceStable(aff, func(i, j int) bool {
		if aff[i].Ecosystem != aff[j].Ecosystem {
			return aff[i].Ecosystem < aff[j].Ecosystem
		}
		return aff[i].Package < aff[j].Package
	})
	if len(aff) > MaxAffected {
		aff = aff[:MaxAffected]
	}
	out.Affect = aff
	return out
}

// Merge combines two records of the same vulnerability from different
// feeds (dedup). Prioritisation takes the stronger signal of either.
func Merge(a, b Advisory) Advisory {
	out := a
	out.Aliases = append(slices.Clone(a.Aliases), b.Aliases...)
	out.Aliases = append(out.Aliases, b.ID)
	out.Sources = append(slices.Clone(a.Sources), b.Sources...)
	if out.Summary == "" {
		out.Summary = b.Summary
	}
	if out.Details == "" {
		out.Details = b.Details
	}
	out.References = append(slices.Clone(a.References), b.References...)
	if b.Severity.Rank() > a.Severity.Rank() {
		out.Severity = b.Severity
	}
	out.CVSS = max(a.CVSS, b.CVSS)
	out.KEV = a.KEV || b.KEV
	out.EPSS = max(a.EPSS, b.EPSS)
	if out.Published.IsZero() || (!b.Published.IsZero() && b.Published.Before(out.Published)) {
		out.Published = b.Published
	}
	if b.Modified.After(out.Modified) {
		out.Modified = b.Modified
	}
	if b.FetchedAt.After(out.FetchedAt) {
		out.FetchedAt = b.FetchedAt
	}
	out.Withdrawn = a.Withdrawn && b.Withdrawn
	// Normalize combines the affected entries of the same package.
	out.Affect = append(slices.Clone(a.Affect), b.Affect...)
	return out.Normalize()
}

func uniqueRanges(rs []Range) []Range {
	var out []Range
	for _, r := range rs {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// FixedVersions returns the fixed versions the advisory states for a
// package, sorted ascending.
func (a Advisory) FixedVersions(ecosystem, pkg string) []string {
	var out []string
	for _, af := range a.Affect {
		if af.Ecosystem != ecosystem || af.Package != pkg {
			continue
		}
		for _, r := range af.Ranges {
			if r.Fixed != "" {
				out = append(out, r.Fixed)
			}
		}
	}
	out = sortedUnique(out)
	sort.SliceStable(out, func(i, j int) bool { return CompareIn(ecosystem, out[i], out[j]) < 0 })
	return out
}

// NormalizeEcosystem maps feed spellings to one ecosystem name: "alpine"
// (also "Alpine:v3.20"), "debian", "npm", "pypi", "go", "maven",
// "packagist" (PHP composer), "docker" (image names) and "cpe" (NVD
// product matches). Unknown names are lower-cased.
func NormalizeEcosystem(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	if i := strings.IndexByte(e, ':'); i >= 0 && e != "cpe" {
		e = e[:i]
	}
	switch e {
	case "composer", "packagist", "php":
		return "packagist"
	case "golang", "go":
		return "go"
	case "pip", "pypi":
		return "pypi"
	case "oci", "container", "docker":
		return "docker"
	}
	return e
}

// NormalizePackage trims and lower-cases a package name.
func NormalizePackage(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

func sortedUnique(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}
