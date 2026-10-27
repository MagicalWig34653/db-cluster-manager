// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"regexp"
	"strconv"
	"strings"
)

// Version comparison.
//
// CompareVersions implements the Debian algorithm (epoch, then alternating
// non-digit and digit runs; '~' sorts before everything including the end;
// letters sort before other characters). CompareIn adapts the spelling of
// pre-releases per ecosystem first: semver-style ecosystems treat
// "-<letters>" as a pre-release, Alpine treats "_alpha", "_beta", "_pre"
// and "_rc" as pre-releases. This covers the version strings in the test
// corpus; it is not a full implementation of every ecosystem's rules
// (PEP 440 normalisation and Maven qualifiers are not handled
// separately). RPM-based distributions use CompareRPM (version_rpm.go).

// CompareVersions returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	ea, ra := splitEpoch(normVersion(a))
	eb, rb := splitEpoch(normVersion(b))
	if ea != eb {
		if ea < eb {
			return -1
		}
		return 1
	}
	return compareDebian(ra, rb)
}

var (
	semverPre = regexp.MustCompile(`-([A-Za-z])`)
	alpinePre = regexp.MustCompile(`_(alpha|beta|pre|rc)`)
)

// CompareIn compares two versions with the pre-release rules of ecosystem.
func CompareIn(ecosystem, a, b string) int {
	if rpmEcosystems[ecosystem] {
		return CompareRPM(a, b)
	}
	switch ecosystem {
	case "npm", "go", "packagist", "pypi", "crates.io", "nuget", "rubygems", "docker", "cpe":
		a, b = semverPre.ReplaceAllString(a, "~$1"), semverPre.ReplaceAllString(b, "~$1")
	case "alpine":
		a, b = alpinePre.ReplaceAllString(a, "~$1"), alpinePre.ReplaceAllString(b, "~$1")
	}
	return CompareVersions(a, b)
}

// Major returns the leading numeric component (after an epoch and a "v"
// prefix), or -1 when there is none.
func Major(v string) int {
	_, r := splitEpoch(normVersion(v))
	end := 0
	for end < len(r) && r[end] >= '0' && r[end] <= '9' {
		end++
	}
	if end == 0 {
		return -1
	}
	n, err := strconv.Atoi(r[:end])
	if err != nil {
		return -1
	}
	return n
}

func normVersion(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 1 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
		v = v[1:]
	}
	return v
}

func splitEpoch(v string) (int, string) {
	if i := strings.IndexByte(v, ':'); i > 0 {
		if n, err := strconv.Atoi(v[:i]); err == nil {
			return n, v[i+1:]
		}
	}
	return 0, v
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func order(c byte) int {
	switch {
	case c == '~':
		return -1
	case isDigit(c):
		return 0
	case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		return int(c)
	}
	return int(c) + 256
}

func compareDebian(a, b string) int {
	for a != "" || b != "" {
		// Non-digit prefix.
		for (a != "" && !isDigit(a[0])) || (b != "" && !isDigit(b[0])) {
			var ca, cb int
			if a != "" && !isDigit(a[0]) {
				ca = order(a[0])
			}
			if b != "" && !isDigit(b[0]) {
				cb = order(b[0])
			}
			if ca != cb {
				if ca < cb {
					return -1
				}
				return 1
			}
			if a != "" && !isDigit(a[0]) {
				a = a[1:]
			}
			if b != "" && !isDigit(b[0]) {
				b = b[1:]
			}
		}
		// Digit run.
		i, j := 0, 0
		for i < len(a) && isDigit(a[i]) {
			i++
		}
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		na := strings.TrimLeft(a[:i], "0")
		nb := strings.TrimLeft(b[:j], "0")
		if len(na) != len(nb) {
			if len(na) < len(nb) {
				return -1
			}
			return 1
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
		a, b = a[i:], b[j:]
	}
	return 0
}

// Affects reports whether version v of a package in ecosystem lies in r.
func (r Range) Affects(ecosystem, v string) bool {
	if r.Introduced != "" && r.Introduced != "0" && CompareIn(ecosystem, v, r.Introduced) < 0 {
		return false
	}
	if r.Fixed != "" && CompareIn(ecosystem, v, r.Fixed) >= 0 {
		return false
	}
	if r.LastAffected != "" && CompareIn(ecosystem, v, r.LastAffected) > 0 {
		return false
	}
	return true
}

// Affects reports whether version v is affected by af.
func (af Affected) Affects(v string) bool {
	for _, x := range af.Versions {
		if CompareIn(af.Ecosystem, x, v) == 0 {
			return true
		}
	}
	for _, r := range af.Ranges {
		if r.Affects(af.Ecosystem, v) {
			return true
		}
	}
	return false
}

// ParseRangeExpr parses a GitHub-style range expression such as
// ">= 1.0.0, < 1.2.3", "<= 2.1" or "= 3.0.1" into ranges. ok is false for
// an expression it does not understand; the caller then records no range
// (the advisory creates no finding for that package rather than a guessed
// one).
func ParseRangeExpr(expr string) (r Range, exact string, ok bool) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return Range{}, "", false
	}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		var op, v string
		for _, o := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(part, o) {
				op, v = o, strings.TrimSpace(part[len(o):])
				break
			}
		}
		if op == "" || v == "" || strings.ContainsAny(v, " <>=") {
			return Range{}, "", false
		}
		switch op {
		case ">=":
			r.Introduced = v
		case "<":
			r.Fixed = v
		case "<=":
			r.LastAffected = v
		case "=":
			exact = v
		default: // ">" has no exact Range equivalent.
			return Range{}, "", false
		}
	}
	if exact != "" && (r != Range{}) {
		return Range{}, "", false
	}
	return r, exact, true
}
