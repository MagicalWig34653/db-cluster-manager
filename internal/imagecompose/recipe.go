// SPDX-License-Identifier: AGPL-3.0-only

package imagecompose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Base selects the base image. Digest pins it (sha256:<64 hex>); the recipe
// is never built from a floating tag.
type Base struct {
	Distro  string `json:"distro"`
	Release string `json:"release"`
	Digest  string `json:"digest"`
}

// Selection is one chosen component with its version, selected features
// (for example PHP extensions) and settings. Settings only accepts keys of
// the catalog schema.
type Selection struct {
	ID       string         `json:"id"`
	Version  string         `json:"version"`
	Features []string       `json:"features,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

// Recipe is the typed, reviewable description of an image. It contains no
// shell fragments and no secrets by construction.
type Recipe struct {
	Name       string      `json:"name"`
	Base       Base        `json:"base"`
	Platforms  []string    `json:"platforms,omitempty"`
	Components []Selection `json:"components"`
}

// Issue is one validation finding addressed by a path such as
// "components[php].settings.pm".
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ErrInvalidRecipe wraps every recipe validation failure.
var ErrInvalidRecipe = errors.New("imagecompose: invalid recipe")

// ValidationError carries all findings at once so a form can show them.
type ValidationError struct{ Issues []Issue }

// Error implements error.
func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Issues))
	for _, i := range e.Issues {
		parts = append(parts, i.Path+": "+i.Message)
	}
	return ErrInvalidRecipe.Error() + ": " + strings.Join(parts, "; ")
}

// Is makes errors.Is(err, ErrInvalidRecipe) work.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalidRecipe }

var (
	digestRe   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}$`)
	platformRe = regexp.MustCompile(`^linux/(amd64|arm64)$`)
)

// Resolve validates the recipe against the catalog and returns the
// normalized form: defaults applied for every applicable setting, lists
// sorted and de-duplicated, values in canonical types. Equal intent gives an
// equal normalized recipe. On failure the returned error is a
// *ValidationError listing every finding; the normalized recipe is then
// best-effort and must not be built.
func (c *Catalog) Resolve(r Recipe) (Recipe, error) {
	var is []Issue
	add := func(path, format string, a ...any) { is = append(is, Issue{path, fmt.Sprintf(format, a...)}) }

	out := Recipe{Name: strings.TrimSpace(r.Name), Base: r.Base}
	out.Base.Digest = strings.TrimSpace(r.Base.Digest)
	if !nameRe.MatchString(out.Name) {
		add("name", "must be 1-63 characters of letters, digits, space, '.', '_' or '-'")
	}
	rel, family, ok := c.Release(r.Base.Distro, r.Base.Release)
	if !ok {
		add("base", "distribution %q release %q is not in the catalog", r.Base.Distro, r.Base.Release)
	}
	if !digestRe.MatchString(out.Base.Digest) {
		add("base.digest", "the base image must be pinned by digest (sha256:<64 hex>); floating tags are not built")
	}

	seen := map[string]bool{}
	var mains, selected []string
	for _, sel := range r.Components {
		path := "components[" + sel.ID + "]"
		comp, found := c.Component(sel.ID)
		if !found {
			add(path, "unknown component")
			continue
		}
		if seen[sel.ID] {
			add(path, "selected more than once")
			continue
		}
		seen[sel.ID] = true
		selected = append(selected, sel.ID)
		ns := Selection{ID: sel.ID, Version: strings.TrimSpace(sel.Version)}
		cv, vok := comp.Version(ns.Version)
		if !vok {
			add(path+".version", "version %q is not in the catalog (available: %s)", ns.Version, strings.Join(comp.sortedVersions(), ", "))
		} else if ok {
			if _, sok := cv.SupportFor(r.Base.Distro, r.Base.Release); !sok {
				add(path+".version", "%s %s is not available on %s; versions available there: %s", comp.ID, ns.Version, rel.Name, strings.Join(c.versionsOn(comp, r.Base.Distro, r.Base.Release), ", "))
			}
		}
		if comp.Process == "main" {
			mains = append(mains, sel.ID)
		}

		// Features.
		for _, f := range sel.Features {
			f = strings.TrimSpace(f)
			feat, fok := comp.Feature(f)
			switch {
			case !fok:
				add(path+".features", "%q is not an allowed option of %s", f, comp.ID)
			case ok && !feat.FeatureAvailable(family):
				add(path+".features", "%q is not available on %s", f, rel.Name)
			default:
				ns.Features = append(ns.Features, f)
			}
		}
		sort.Strings(ns.Features)
		ns.Features = slices.Compact(ns.Features)

		// Settings: unknown keys are rejected, defaults fill the rest.
		known := map[string]Field{}
		for _, f := range comp.Settings {
			known[f.Key] = f
		}
		keys := make([]string, 0, len(sel.Settings))
		for k := range sel.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, kok := known[k]; !kok {
				add(path+".settings."+k, "setting is not in the allow-list of %s", comp.ID)
			}
		}
		if len(comp.Settings) > 0 {
			ns.Settings = map[string]any{}
		}
		for _, f := range comp.Settings {
			applicable := (f.Needs == "" || hasID(r.Components, f.Needs)) && (f.NeedsFeature == "" || slices.Contains(ns.Features, f.NeedsFeature))
			v, given := sel.Settings[f.Key]
			if !applicable {
				if given {
					add(path+".settings."+f.Key, "only applies when %s is selected", firstNonEmpty(f.Needs, f.NeedsFeature))
				}
				continue
			}
			if !given {
				v = f.Default
			}
			cv, err := f.coerce(v)
			if err != nil {
				add(path+".settings."+f.Key, "%v", err)
				continue
			}
			ns.Settings[f.Key] = cv
		}
		out.Components = append(out.Components, ns)
	}
	if len(r.Components) == 0 {
		add("components", "select at least one component")
	}
	sort.Slice(out.Components, func(i, j int) bool { return out.Components[i].ID < out.Components[j].ID })

	// Dependencies and the single-process rule.
	for _, sel := range selected {
		comp, _ := c.Component(sel)
		for _, req := range comp.Requires {
			if !seen[req] {
				add("components["+sel+"]", "%s requires %s", sel, req)
			}
		}
		for _, con := range comp.Conflicts {
			if seen[con] {
				add("components["+sel+"]", "%s cannot be combined with %s", sel, con)
			}
		}
	}
	if len(mains) > 1 {
		add("components", "single-process rule: only one main process per image (%s selected)", strings.Join(mains, ", "))
	}
	if len(selected) > 0 && len(mains) == 0 && !seen["php"] {
		add("components", "select a main process: a web server, an application runtime, SFTP, the job runner or PHP")
	}
	if seen["php"] && len(mains) > 0 && !slices.ContainsFunc(webServerIDs, func(id string) bool { return seen[id] }) {
		add("components[php]", "PHP can only accompany a web server (nginx, apache or caddy), not %s", strings.Join(mains, ", "))
	}

	// Cross-field rules on selected settings.
	if p := settingsOf(out, "php"); p != nil {
		is = append(is, phpCross(p)...)
	}
	is = append(is, crossRules(out)...)
	is = append(is, c.artifactIssues(out)...)

	// Platforms.
	for _, pl := range r.Platforms {
		pl = strings.TrimSpace(pl)
		if !platformRe.MatchString(pl) {
			add("platforms", "%q is not supported (linux/amd64 or linux/arm64)", pl)
			continue
		}
		out.Platforms = append(out.Platforms, pl)
	}
	sort.Strings(out.Platforms)
	out.Platforms = slices.Compact(out.Platforms)
	if len(out.Platforms) == 0 {
		out.Platforms = []string{"linux/amd64"}
	}
	if ok {
		for _, pl := range out.Platforms {
			arch := strings.TrimPrefix(pl, "linux/")
			if !slices.Contains(rel.Architectures, arch) {
				add("platforms", "%s is not offered by the %s base image", pl, rel.Name)
				continue
			}
			for _, sel := range out.Components {
				comp, _ := c.Component(sel.ID)
				if cv, vok := comp.Version(sel.Version); vok {
					if s, sok := cv.SupportFor(r.Base.Distro, r.Base.Release); sok && !slices.Contains(s.Architectures, arch) {
						add("platforms", "%s %s is not available for %s on %s", comp.ID, sel.Version, pl, rel.Name)
					}
				}
			}
		}
	}
	if len(is) > 0 {
		return out, &ValidationError{Issues: is}
	}
	return out, nil
}

func hasID(sels []Selection, id string) bool {
	return slices.ContainsFunc(sels, func(s Selection) bool { return s.ID == id })
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func settingsOf(r Recipe, id string) map[string]any {
	for _, s := range r.Components {
		if s.ID == id {
			return s.Settings
		}
	}
	return nil
}

// sizeBytes parses 128M, 2K, 1G or a bare number of bytes; -1 is unlimited.
func sizeBytes(s string) int64 {
	if s == "-1" {
		return -1
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(strings.ToUpper(s), "K"):
		mult = 1 << 10
	case strings.HasSuffix(strings.ToUpper(s), "M"):
		mult = 1 << 20
	case strings.HasSuffix(strings.ToUpper(s), "G"):
		mult = 1 << 30
	}
	n, _ := strconv.ParseInt(strings.TrimRight(s, "kKmMgG"), 10, 64)
	return n * mult
}

func phpCross(s map[string]any) []Issue {
	var is []Issue
	str := func(k string) string { v, _ := s[k].(string); return v }
	num := func(k string) int64 { v, _ := s[k].(int64); return v }
	base := "components[php].settings."
	up, post, mem := sizeBytes(str("upload_max_filesize")), sizeBytes(str("post_max_size")), sizeBytes(str("memory_limit"))
	if up > 0 && post > 0 && up > post {
		is = append(is, Issue{base + "post_max_size", "must not be smaller than upload_max_filesize"})
	}
	if mem > 0 && post > 0 && post > mem {
		is = append(is, Issue{base + "memory_limit", "must not be smaller than post_max_size"})
	}
	if str("pm") == "dynamic" {
		mn, st, mx, ch := num("pm.min_spare_servers"), num("pm.start_servers"), num("pm.max_spare_servers"), num("pm.max_children")
		switch {
		case mn > mx:
			is = append(is, Issue{base + "pm.min_spare_servers", "must not exceed pm.max_spare_servers"})
		case st < mn || st > mx:
			is = append(is, Issue{base + "pm.start_servers", "must lie between pm.min_spare_servers and pm.max_spare_servers"})
		case mx > ch:
			is = append(is, Issue{base + "pm.max_spare_servers", "must not exceed pm.max_children"})
		}
	}
	return is
}

// artifactIssues checks that every selected downloaded component has a
// pinned artifact for the chosen variant and version.
func (c *Catalog) artifactIssues(n Recipe) []Issue {
	var is []Issue
	for _, sel := range n.Components {
		if sel.ID != "minecraft" {
			continue
		}
		comp, _ := c.Component(sel.ID)
		cv, ok := comp.Version(sel.Version)
		if !ok {
			continue
		}
		if variant := gs(sel.Settings, "variant"); variant != "" {
			if _, has := cv.Artifacts[variant]; !has {
				is = append(is, Issue{"components[minecraft].settings.variant", fmt.Sprintf("%s has no pinned download for Minecraft %s in the catalog", variant, sel.Version)})
			}
		}
		if gb(sel.Settings, "crossplay") {
			if gs(sel.Settings, "variant") != "paper" {
				is = append(is, Issue{"components[minecraft].settings.crossplay", "crossplay (Geyser + Floodgate) needs the paper variant; it is not available for vanilla"})
			} else if len(cv.Crossplay) == 0 {
				is = append(is, Issue{"components[minecraft].settings.crossplay", fmt.Sprintf("no verified Geyser/Floodgate build for Minecraft %s in the catalog", sel.Version)})
			}
		}
	}
	return is
}

func (c *Catalog) versionsOn(comp Component, distro, release string) []string {
	var out []string
	for _, cv := range comp.Versions {
		if _, ok := cv.SupportFor(distro, release); ok {
			out = append(out, cv.Version)
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[j], out[i]) })
	return out
}

// Digest is the recipe digest: a hash over the normalized recipe, the
// catalog version and the generator version, so identical intent yields an
// identical digest and a changed catalog or generator is visible. The recipe
// must have been normalized by Resolve.
func (c *Catalog) Digest(norm Recipe) string {
	b, err := json.Marshal(struct {
		Kind      string `json:"kind"`
		Generator string `json:"generator"`
		Catalog   string `json:"catalog"`
		Recipe    Recipe `json:"recipe"`
	}{"dbcm-imagecompose-recipe", GeneratorVersion, c.Version, norm})
	if err != nil {
		panic(err) // plain data
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Variant says whether the recipe's component choice can be built on a
// release, and with which versions.
type Variant struct {
	Distro  string `json:"distro"`
	Release string `json:"release"`
	Name    string `json:"name"`
	// Compatible is true when every selected component version, feature and
	// platform is available on the release unchanged.
	Compatible bool `json:"compatible"`
	// Problems lists what differs (empty when Compatible).
	Problems []string `json:"problems,omitempty"`
	// Versions lists, per selected component, the newest version available
	// on the release; switching the base to it is a one-click variant.
	Versions map[string]string `json:"versions,omitempty"`
}

// Variants checks the recipe's selection against every release, in catalog
// order. It uses only component ids, versions, features and platforms, so it
// also works for a recipe that is not valid yet.
func (c *Catalog) Variants(r Recipe) []Variant {
	var out []Variant
	for _, d := range c.Distros {
		for _, rel := range d.Releases {
			v := Variant{Distro: d.ID, Release: rel.ID, Name: rel.Name, Versions: map[string]string{}}
			for _, sel := range r.Components {
				comp, ok := c.Component(sel.ID)
				if !ok {
					continue
				}
				avail := c.versionsOn(comp, d.ID, rel.ID)
				if avail[0] == "none" {
					v.Problems = append(v.Problems, comp.ID+" is not available")
					continue
				}
				v.Versions[comp.ID] = avail[0]
				cv, vok := comp.Version(sel.Version)
				if !vok {
					continue
				}
				s, sok := cv.SupportFor(d.ID, rel.ID)
				if !sok {
					v.Problems = append(v.Problems, fmt.Sprintf("%s %s is not available (newest here: %s)", comp.ID, sel.Version, avail[0]))
					continue
				}
				for _, f := range sel.Features {
					if feat, fok := comp.Feature(f); fok && !feat.FeatureAvailable(d.Family) {
						v.Problems = append(v.Problems, fmt.Sprintf("%s option %s is not available", comp.ID, f))
					}
				}
				for _, pl := range r.Platforms {
					if !slices.Contains(s.Architectures, strings.TrimPrefix(pl, "linux/")) {
						v.Problems = append(v.Problems, fmt.Sprintf("%s is not available for %s", comp.ID, pl))
					}
				}
			}
			v.Compatible = len(v.Problems) == 0
			out = append(out, v)
		}
	}
	return out
}
