// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"strconv"
	"strings"
)

// RPM version ordering (rpmvercmp). Versions look like
// "[epoch:]version[-release]", for example "1:3.0.7-27.el9_2". The epoch is
// numeric (missing = 0); version and release are compared with rpmvercmp:
// separators other than letters, digits, '~' and '^' are skipped, digit and
// letter runs alternate, a digit run is newer than a letter run, '~' sorts
// before everything including the end, '^' sorts after the end but before
// any other character. The release is only compared when both sides have
// one.

// rpmEcosystems are the normalised ecosystem names (see NormalizeEcosystem)
// whose package versions follow RPM rules.
var rpmEcosystems = map[string]bool{
	"almalinux": true, "rocky linux": true, "red hat": true, "redhat": true, "rhel": true,
	"centos": true, "fedora": true, "oracle linux": true, "suse": true, "opensuse": true,
	"amazon linux": true, "rocky": true, "mageia": true,
}

// CompareRPM returns -1, 0 or 1 following rpm's version comparison.
func CompareRPM(a, b string) int {
	ea, va, ra := splitRPM(normVersion(a))
	eb, vb, rb := splitRPM(normVersion(b))
	if ea != eb {
		if ea < eb {
			return -1
		}
		return 1
	}
	if c := rpmvercmp(va, vb); c != 0 {
		return c
	}
	if ra != "" && rb != "" {
		return rpmvercmp(ra, rb)
	}
	return 0
}

func splitRPM(v string) (epoch uint64, version, release string) {
	if i := strings.IndexByte(v, ':'); i > 0 {
		if n, err := strconv.ParseUint(v[:i], 10, 64); err == nil {
			epoch, v = n, v[i+1:]
		}
	}
	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		return epoch, v[:i], v[i+1:]
	}
	return epoch, v, ""
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func rpmvercmp(a, b string) int {
	if a == b {
		return 0
	}
	skip := func(s string) string {
		for s != "" && !isAlpha(s[0]) && !isDigit(s[0]) && s[0] != '~' && s[0] != '^' {
			s = s[1:]
		}
		return s
	}
	for a != "" || b != "" {
		a, b = skip(a), skip(b)
		if (a != "" && a[0] == '~') || (b != "" && b[0] == '~') {
			if a == "" || a[0] != '~' {
				return 1
			}
			if b == "" || b[0] != '~' {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if (a != "" && a[0] == '^') || (b != "" && b[0] == '^') {
			if a == "" {
				return -1
			}
			if b == "" {
				return 1
			}
			if a[0] != '^' {
				return 1
			}
			if b[0] != '^' {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if a == "" || b == "" {
			break
		}
		num := isDigit(a[0])
		pred := isAlpha
		if num {
			pred = isDigit
		}
		i, j := 0, 0
		for i < len(a) && pred(a[i]) {
			i++
		}
		for j < len(b) && pred(b[j]) {
			j++
		}
		sa, sb := a[:i], b[:j]
		a, b = a[i:], b[j:]
		if sa == "" { // cannot happen: a[0] is a letter or digit
			return -1
		}
		if sb == "" { // the other side is of the other kind
			if num {
				return 1
			}
			return -1
		}
		if num {
			sa, sb = strings.TrimLeft(sa, "0"), strings.TrimLeft(sb, "0")
			if len(sa) != len(sb) {
				if len(sa) < len(sb) {
					return -1
				}
				return 1
			}
		}
		if sa != sb {
			if sa < sb {
				return -1
			}
			return 1
		}
	}
	switch {
	case a == "" && b == "":
		return 0
	case a != "":
		return 1
	}
	return -1
}
