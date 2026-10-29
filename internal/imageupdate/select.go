// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

import (
	"regexp"
	"strconv"
	"strings"
)

// Track says which newer tags of the same repository are candidates.
type Track string

// Tracks. A track never lowers a version and never leaves the variant
// suffix (for example "-alpine") of the current tag.
const (
	// TrackDigest follows the current tag only: the same tag with a new
	// digest (a rebuild of the base image or of a patched release).
	TrackDigest Track = "digest"
	// TrackPatch also accepts higher patch versions of the same major.minor.
	TrackPatch Track = "patch"
	// TrackMinor also accepts higher minor versions of the same major.
	TrackMinor Track = "minor"
	// TrackMajor also accepts higher major versions. Never for databases
	// and never with automatic mode.
	TrackMajor Track = "major"
)

// Valid reports whether t is a known track.
func (t Track) Valid() bool {
	switch t {
	case TrackDigest, TrackPatch, TrackMinor, TrackMajor:
		return true
	}
	return false
}

// Kind classifies a candidate.
type Kind string

// Candidate kinds.
const (
	KindDigest Kind = "digest"
	KindPatch  Kind = "patch"
	KindMinor  Kind = "minor"
	KindMajor  Kind = "major"
	// KindBuilt is an image the manager built itself (vulnerability
	// response, X13); it is never applied without a human approval.
	KindBuilt Kind = "built"
	// KindRollback reverses an earlier update.
	KindRollback Kind = "rollback"
)

type tagVersion struct {
	prefix       string
	major, minor int
	patch        int
	precision    int // number of numeric components, 1..3
	suffix       string
}

var tagVersionRe = regexp.MustCompile(`^(v?)([0-9]{1,6})(?:\.([0-9]{1,6}))?(?:\.([0-9]{1,6}))?(?:([-_][A-Za-z0-9._-]+))?$`)

// prereleaseRe marks suffixes that are not stable variants.
var prereleaseRe = regexp.MustCompile(`(?i)(^|[-_.])(rc|alpha|beta|dev|pre|snapshot|nightly|edge|test|canary|preview|m)[0-9]*([-_.]|$)`)

func parseTagVersion(tag string) (tagVersion, bool) {
	m := tagVersionRe.FindStringSubmatch(tag)
	if m == nil {
		return tagVersion{}, false
	}
	v := tagVersion{prefix: m[1], suffix: m[5], precision: 1}
	v.major, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		v.minor, _ = strconv.Atoi(m[3])
		v.precision = 2
	}
	if m[4] != "" {
		v.patch, _ = strconv.Atoi(m[4])
		v.precision = 3
	}
	return v, true
}

func (a tagVersion) less(b tagVersion) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	return a.patch < b.patch
}

// PickTag returns the best newer tag for the current reference under the
// track: the highest version that stays inside the track's limits, has the
// same variant suffix, the same "v" prefix and the same precision (11.4 only
// moves to 11.5, never to 11.4.2) and is not a pre-release. Databases never
// receive a major jump regardless of the track. The tag "latest" and other
// non-version tags are never candidates. It returns false when no newer
// version tag exists (a digest-only follow-up of the current tag is the
// caller's job).
func PickTag(cur Ref, tags []string, track Track, database bool) (tag string, kind Kind, ok bool) {
	if cur.Tag == "" || track == TrackDigest || !track.Valid() {
		return "", "", false
	}
	cv, vok := parseTagVersion(cur.Tag)
	if !vok || prereleaseRe.MatchString(cv.suffix) {
		return "", "", false
	}
	var best tagVersion
	for _, t := range tags {
		v, ok := parseTagVersion(t)
		if !ok || v.prefix != cv.prefix || v.suffix != cv.suffix || v.precision != cv.precision || !cv.less(v) {
			continue
		}
		if prereleaseRe.MatchString(v.suffix) {
			continue
		}
		k := classify(cv, v)
		if !allowed(track, k, database, cv.precision) {
			continue
		}
		if tag == "" || best.less(v) {
			tag, best, kind = t, v, k
		}
	}
	return tag, kind, tag != ""
}

func classify(cur, v tagVersion) Kind {
	switch {
	case v.major != cur.major:
		return KindMajor
	case v.minor != cur.minor:
		return KindMinor
	}
	return KindPatch
}

func allowed(track Track, k Kind, database bool, precision int) bool {
	switch k {
	case KindMajor:
		return track == TrackMajor && !database
	case KindMinor:
		return track == TrackMinor || track == TrackMajor
	case KindPatch:
		// A "major.minor" tag has no patch component to move.
		return precision >= 3 && track != TrackDigest
	}
	return false
}

// databaseNames are repository name parts that mark a database image.
var databaseNames = []string{"mariadb", "mysql", "percona", "postgres", "mongo", "redis", "valkey", "cassandra", "cockroach", "clickhouse", "galera", "couchdb", "influxdb", "timescale", "memcached", "etcd", "elasticsearch", "opensearch"}

// IsDatabase reports whether a service of this type and image repository
// counts as a database: the service type "galera", or a repository name
// containing a well-known database name. The rule is deliberately broad; a
// false positive only restricts the track.
func IsDatabase(serviceType, repo string) bool {
	if serviceType == "galera" {
		return true
	}
	r := strings.ToLower(repo)
	for _, n := range databaseNames {
		if strings.Contains(r, n) {
			return true
		}
	}
	return false
}
