// SPDX-License-Identifier: AGPL-3.0-only

package ai

import (
	"regexp"
	"slices"
	"strings"
)

// Redactor removes secret-looking content and environment identifiers
// from text before it leaves the manager. It runs on every string of the
// prompt, including untrusted advisory text (which could carry planted
// "secrets" meant to look like ours) and configuration values.
type Redactor struct {
	// Names are environment identifiers to remove (hostnames, registry
	// hosts, Swarm and cluster names). Matching is case-insensitive.
	Names []string
}

// RedactedMarker replaces every redacted span.
const RedactedMarker = "[REDACTED]"

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`\bsk-(proj-)?[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)\b(mysql|mariadb|postgres|postgresql|mongodb(\+srv)?|redis|amqp|jdbc:[a-z]+)://[^\s"'<>]+`),
	regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@"'<>]+:[^\s/@"'<>]+@`),
	regexp.MustCompile(`(?i)[a-z0-9_.-]*(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?)[a-z0-9_.-]*\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`),
	regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
	regexp.MustCompile(`\b(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){3}(/\d{1,2})?\b`),
	regexp.MustCompile(`(?i)\b([0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\b`),
}

// Redact returns s with secrets and environment names replaced and the
// number of replacements.
func (r Redactor) Redact(s string) (string, int) {
	n := 0
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return RedactedMarker })
	}
	names := slices.Clone(r.Names)
	// Longer names first so a host is not partly replaced by its domain.
	slices.SortFunc(names, func(a, b string) int { return len(b) - len(a) })
	for _, name := range names {
		if len(name) < 3 {
			continue
		}
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(name))
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return RedactedMarker })
	}
	return s, n
}

// Clean removes control characters except newline and tab, so untrusted
// text cannot hide content from the admin's preview.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		case r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0xfeff:
			return -1 // zero-width and bidi control characters
		}
		return r
	}, s)
}
