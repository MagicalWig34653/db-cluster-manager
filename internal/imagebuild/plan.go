// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrApprovalMismatch is returned when an approval digest does not match the
// plan recomputed from the request.
var ErrApprovalMismatch = errors.New("imagebuild: approval does not match the plan")

// Target says where the built image goes. Registry is the ID of a registry
// known to the manager; Repository and Tag name the image in it.
type Target struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

var (
	repoRe = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	tagRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	idRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// Validate checks the target's syntax (not that the registry exists).
func (t Target) Validate() error {
	switch {
	case !idRe.MatchString(t.Registry):
		return invalid("target registry ID is malformed")
	case len(t.Repository) > 255 || !repoRe.MatchString(t.Repository):
		return invalid("target repository must be lower-case path components")
	case !tagRe.MatchString(t.Tag):
		return invalid("target tag is malformed")
	}
	return nil
}

// Step is one step of the plan in execution order.
type Step struct {
	Order  int    `json:"order"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// Plan is the preview of a build: what will be generated, built and pushed.
// Nothing has happened when a Plan exists. Approval binds to Digest.
type Plan struct {
	SpecHash    string   `json:"specHash"`
	ContextHash string   `json:"contextHash"`
	Digest      string   `json:"digest"`
	BaseImage   string   `json:"baseImage"`
	Platforms   []string `json:"platforms"`
	Target      Target   `json:"target"`
	// Reference is the full image reference (host/repository:tag); the
	// build service fills it once the registry is resolved.
	Reference  string `json:"reference,omitempty"`
	Dockerfile string `json:"dockerfile"`
	Files      []File `json:"files"`
	Steps      []Step `json:"steps"`
	// Warnings are limits the user should know before approving.
	Warnings []string `json:"warnings"`
	// Explanation is a human-readable summary of the same facts.
	Explanation string `json:"explanation"`
}

// NewPlan renders the spec and describes the build. It is a pure function:
// no network, no registry lookup, no build.
func NewPlan(s WebServerSpec, t Target, c *Catalog) (Plan, error) {
	if err := t.Validate(); err != nil {
		return Plan{}, err
	}
	r, err := Render(s, c)
	if err != nil {
		return Plan{}, err
	}
	n := s.Normalize()
	p := Plan{
		SpecHash: r.SpecHash, ContextHash: r.ContextHash, BaseImage: r.BaseImage,
		Platforms: n.Platforms, Target: t, Dockerfile: r.Dockerfile, Files: r.Files,
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"dbcm-imagebuild-plan-v1", r.SpecHash, r.ContextHash, strings.Join(n.Platforms, ","),
		t.Registry, t.Repository, t.Tag,
	}, "\n")))
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])

	target := fmt.Sprintf("%s/%s:%s", t.Registry, t.Repository, t.Tag)
	p.Steps = []Step{
		{1, "Generate build context", fmt.Sprintf("%d files plus a Dockerfile (context hash %s).", len(r.Files), short(r.ContextHash))},
		{2, "Build", fmt.Sprintf("Build for %s from base %s using BuildKit; the base image is pinned by digest.", strings.Join(n.Platforms, ", "), r.BaseImage)},
		{3, "Verify configuration", "The Dockerfile runs `nginx -t`" + phpCheck(n) + " during the build; a configuration error fails the build before any push."},
		{4, "Push", "Push the image to " + target + " using the stored credential of that registry; the credential is never written to logs or the build context."},
		{5, "Record", "Record spec hash, base digest, resulting image digest and the redacted build log. An SBOM is not generated yet (placeholder only)."},
	}
	p.Warnings = []string{
		"Alpine package versions are not pinned: two builds on different days can contain different PHP or extension patch versions. Only the base image is pinned by digest.",
		"No vulnerability scan and no image signature are produced yet.",
	}
	if n.Nginx.CustomSnippet != "" {
		p.Warnings = append(p.Warnings, "The custom nginx snippet passed a deny-list check, not a full parser; `nginx -t` in the build is the final check.")
	}
	if len(n.Platforms) > 1 {
		p.Warnings = append(p.Warnings, "Multi-platform builds are pushed directly to the registry and need emulation or native builders; nothing was verified on this manager.")
	}
	p.Explanation = explain(n, r, t)
	return p, nil
}

// CheckApproval verifies an approval digest against a freshly computed plan.
func (p Plan) CheckApproval(digest string) error {
	if digest == "" || digest != p.Digest {
		return ErrApprovalMismatch
	}
	return nil
}

func phpCheck(n WebServerSpec) string {
	if n.PHP.Version != "" {
		return " and `php-fpm -t`"
	}
	return ""
}

func short(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func explain(n WebServerSpec, r Rendered, t Target) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Image %q: nginx %s", n.Name, n.Nginx.Version)
	if n.PHP.Version != "" {
		fmt.Fprintf(&b, " with PHP-FPM %s", n.PHP.Version)
		if len(n.PHP.Extensions) > 0 {
			fmt.Fprintf(&b, " and extensions %s", strings.Join(n.PHP.Extensions, ", "))
		} else {
			b.WriteString(" and no additional extensions")
		}
	} else {
		b.WriteString(" as a static web server (no PHP)")
	}
	b.WriteString(".\n")
	fmt.Fprintf(&b, "Base image: %s (pinned by digest).\n", r.BaseImage)
	if len(n.PHP.INI) > 0 {
		fmt.Fprintf(&b, "php.ini overrides: %d setting(s), written to php/dbcm.ini.\n", len(n.PHP.INI))
	}
	if len(n.ExtraPackages) > 0 {
		fmt.Fprintf(&b, "Extra packages: %s.\n", strings.Join(n.ExtraPackages, ", "))
	}
	if n.Nginx.CustomSnippet != "" {
		fmt.Fprintf(&b, "Custom nginx snippet: %d bytes, included at server level.\n", len(n.Nginx.CustomSnippet))
	}
	fmt.Fprintf(&b, "Platforms: %s. Target: %s/%s:%s.\n", strings.Join(n.Platforms, ", "), t.Registry, t.Repository, t.Tag)
	fmt.Fprintf(&b, "Spec hash %s, context hash %s: the same composition always produces the same Dockerfile and files.", short(r.SpecHash), short(r.ContextHash))
	return b.String()
}
