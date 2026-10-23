// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"io"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

// RegistryAuth is the credential a build uses to push. The password is a
// registry.Secret and is revealed only where the docker config is written.
type RegistryAuth struct {
	// Server is the docker config key, for example ghcr.io or
	// https://index.docker.io/v1/.
	Server   string
	Username string
	Password registry.Secret
}

// BuildInput is everything a Builder needs. Log receives build output; the
// caller redacts it.
type BuildInput struct {
	Rendered  Rendered
	Platforms []string
	// Reference is host/repository:tag.
	Reference string
	// Push uploads the result to the registry. Multi-platform builds
	// require it.
	Push bool
	Auth *RegistryAuth
	Log  io.Writer
}

// SBOMRef is a placeholder for the software bill of materials. Nothing
// generates one yet; Status stays "not_generated".
type SBOMRef struct {
	Status string `json:"status"`
	Ref    string `json:"ref,omitempty"`
}

// SBOMNotGenerated is the placeholder value.
var SBOMNotGenerated = SBOMRef{Status: "not_generated"}

// BuildOutput is the result of a successful build.
type BuildOutput struct {
	// ImageDigest is the digest of the pushed manifest (or index).
	ImageDigest string
	// BaseDigests are the base image digests the build used.
	BaseDigests []string
	SBOM        SBOMRef
}

// Builder turns a rendered context into an image. Implementations: the
// docker/BuildKit CLI builder (never executed in tests) and FakeBuilder.
type Builder interface {
	Build(ctx context.Context, in BuildInput) (BuildOutput, error)
}
