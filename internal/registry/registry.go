// SPDX-License-Identifier: AGPL-3.0-only

// Package registry abstracts container registries for the image build
// service (X08): an external OCI distribution client (GHCR, Docker Hub,
// generic), a descriptor for a manager-operated registry:2 service and the
// image distribution policy.
//
// Status: proposal implemented against fakes only. The external client was
// tested against an httptest fake registry, never against ghcr.io, Docker Hub
// or a real registry:2. The managed registry is a deploy specification only;
// deployment needs the future service framework.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
)

// Registry operates on one registry. Repositories are names such as
// "team/web" without a host. References are tags or digests.
type Registry interface {
	// Push uploads the image's blobs and manifest under repo:tag and
	// returns the manifest digest.
	Push(ctx context.Context, repo, tag string, img Image) (digest string, err error)
	// Resolve returns the manifest digest of a tag (or checks a digest).
	Resolve(ctx context.Context, repo, reference string) (digest string, err error)
	// Tag makes `to` point to the manifest `from` (a tag or digest).
	Tag(ctx context.Context, repo, from, to string) error
	// Delete removes the manifest with the given digest.
	Delete(ctx context.Context, repo, digest string) error
	// List returns all tags of the repository, sorted.
	List(ctx context.Context, repo string) ([]string, error)
}

// Image is a manifest with the blobs it references.
type Image struct {
	// MediaType of the manifest, for example
	// application/vnd.oci.image.manifest.v1+json.
	MediaType string
	Manifest  []byte
	Blobs     []Blob
}

// Blob is one content-addressed layer or config blob.
type Blob struct {
	Digest string // sha256:<hex>
	Data   []byte
}

// BlobOf builds a Blob and computes its digest.
func BlobOf(data []byte) Blob { return Blob{Digest: DigestOf(data), Data: data} }

// DigestOf returns the sha256 digest string of data.
func DigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Errors. Messages never contain credentials, tokens or response bodies.
var (
	ErrInvalidName    = errors.New("registry: invalid repository or reference")
	ErrNotFound       = errors.New("registry: not found")
	ErrUnauthorized   = errors.New("registry: authentication failed or not permitted")
	ErrDigestMismatch = errors.New("registry: digest mismatch")
	ErrInvalidConfig  = errors.New("registry: invalid configuration")
)

// Error is a registry failure reported by the server. Code is the OCI error
// code (for example MANIFEST_UNKNOWN) when the server sent one.
type Error struct {
	Op     string
	Status int
	Code   string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("registry: %s failed: HTTP %d (%s)", e.Op, e.Status, e.Code)
	}
	return fmt.Sprintf("registry: %s failed: HTTP %d", e.Op, e.Status)
}

// Is maps HTTP statuses onto the sentinel errors.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == 404
	case ErrUnauthorized:
		return e.Status == 401 || e.Status == 403
	}
	return false
}

var (
	repoNameRe   = regexp.MustCompile(`^[a-z0-9]+([._-]{1,2}[a-z0-9]+)*(/[a-z0-9]+([._-]{1,2}[a-z0-9]+)*)*$`)
	tagNameRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestNameRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidRepository reports whether name is a valid repository path.
func ValidRepository(name string) bool { return len(name) <= 255 && repoNameRe.MatchString(name) }

// ValidReference reports whether ref is a valid tag or sha256 digest.
func ValidReference(ref string) bool {
	return tagNameRe.MatchString(ref) || digestNameRe.MatchString(ref)
}

// ValidDigest reports whether d is a sha256 digest.
func ValidDigest(d string) bool { return digestNameRe.MatchString(d) }
