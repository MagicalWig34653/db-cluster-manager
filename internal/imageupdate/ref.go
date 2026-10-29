// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidRef: not a usable image reference.
var ErrInvalidRef = errors.New("imageupdate: invalid image reference")

var (
	repoPartRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagRe      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	hostRe     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
)

// DockerHub is the registry host of references without a host part.
const DockerHub = "docker.io"

// Ref is a parsed image reference. Registry and Repo are normalised for
// registry API calls (Docker Hub: docker.io, "library/" for official
// images); String renders the reference the way it was written, so a spec
// that said "nginx:1.25" still says "nginx:1.26@sha256:..." afterwards.
type Ref struct {
	Registry string
	Repo     string
	Tag      string
	Digest   string

	// written is the registry and repository as the spec spelled them.
	written string
}

// ParseRef parses [host[:port]/]repo[:tag][@sha256:digest].
func ParseRef(s string) (Ref, error) {
	if s == "" || len(s) > 255 || strings.ContainsAny(s, " \t\r\n\x00") || strings.Contains(s, "://") {
		return Ref{}, ErrInvalidRef
	}
	var r Ref
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		r.Digest = rest[i+1:]
		rest = rest[:i]
		if !digestRe.MatchString(r.Digest) {
			return Ref{}, fmt.Errorf("%w: digest", ErrInvalidRef)
		}
	}
	last := rest[strings.LastIndex(rest, "/")+1:]
	if i := strings.LastIndex(last, ":"); i >= 0 {
		r.Tag = last[i+1:]
		rest = rest[:len(rest)-len(last)+i]
		if !tagRe.MatchString(r.Tag) {
			return Ref{}, fmt.Errorf("%w: tag", ErrInvalidRef)
		}
	}
	r.written = rest
	r.Registry = DockerHub
	path := rest
	if i := strings.Index(rest, "/"); i > 0 {
		first := rest[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			if !hostRe.MatchString(first) {
				return Ref{}, fmt.Errorf("%w: registry host", ErrInvalidRef)
			}
			r.Registry, path = first, rest[i+1:]
		}
	}
	if path == "" || len(path) > 200 {
		return Ref{}, fmt.Errorf("%w: repository", ErrInvalidRef)
	}
	for _, part := range strings.Split(path, "/") {
		if !repoPartRe.MatchString(part) {
			return Ref{}, fmt.Errorf("%w: repository", ErrInvalidRef)
		}
	}
	if r.Registry == DockerHub && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	r.Repo = path
	if r.Tag == "" && r.Digest == "" {
		return Ref{}, fmt.Errorf("%w: neither tag nor digest", ErrInvalidRef)
	}
	return r, nil
}

// String renders the reference as it was written (with the new tag and
// digest of Pinned).
func (r Ref) String() string {
	out := r.written
	if out == "" {
		out = r.Repo
		if r.Registry != "" && r.Registry != DockerHub {
			out = r.Registry + "/" + r.Repo
		}
	}
	if r.Tag != "" {
		out += ":" + r.Tag
	}
	if r.Digest != "" {
		out += "@" + r.Digest
	}
	return out
}

// Pinned returns the reference with another tag and a digest. The receiver
// is unchanged.
func (r Ref) Pinned(tag, digest string) Ref {
	r.Tag, r.Digest = tag, digest
	return r
}

// SameRepo reports whether both references name the same repository.
func (r Ref) SameRepo(o Ref) bool { return r.Registry == o.Registry && r.Repo == o.Repo }

// ValidDigest reports whether d is a sha256 digest.
func ValidDigest(d string) bool { return digestRe.MatchString(d) }
