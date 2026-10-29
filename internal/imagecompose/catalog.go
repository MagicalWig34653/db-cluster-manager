// SPDX-License-Identifier: AGPL-3.0-only

// Package imagecompose is the image composer (X17, ADR 0026). It holds a
// versioned component catalog as embedded data, a typed recipe, a validator
// with allow-listed settings and a deterministic generator that turns a
// recipe into a Dockerfile and build context for the image build service
// (internal/imagebuild).
//
// Status: proposal implemented against fakes only. The catalog is data
// written from knowledge, not verified against live distribution
// repositories; every entry carries Evidence "unverified". No image has been
// built from a generated recipe, and no network access happens here.
package imagecompose

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// SchemaVersion is the catalog schema this code understands.
const SchemaVersion = 1

// GeneratorVersion is part of the recipe digest: a change of the generator
// changes the digest because the generated output changes with it.
const GeneratorVersion = "2"

// EvidenceUnverified marks catalog data that no upstream document or real
// build has confirmed.
const EvidenceUnverified = "unverified"

// I18n is a short text in both UI languages.
type I18n struct {
	En string `json:"en"`
	De string `json:"de"`
}

// Distro is a base distribution; Family selects the package manager rules
// (alpine: apk, debian: apt, rhel: dnf).
type Distro struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Family   string    `json:"family"`
	Releases []Release `json:"releases"`
}

// Release is one base image: image repository and tag are the human-readable
// source of the digest the recipe must pin.
type Release struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Image         string   `json:"image"`
	Tag           string   `json:"tag"`
	Architectures []string `json:"architectures"`
}

// PackageSpec names one distribution package. Constraint is a major.minor
// prefix applied as apk "~=", apt "=X.*"; dnf pins through Modules.
type PackageSpec struct {
	Name       string `json:"name"`
	Constraint string `json:"constraint,omitempty"`
}

// Support says that a component version is available on a distribution
// release for the listed architectures, and how to install it there.
type Support struct {
	Distro        string        `json:"distro"`
	Release       string        `json:"release"`
	Architectures []string      `json:"architectures"`
	Packages      []PackageSpec `json:"packages,omitempty"`
	// Modules are dnf module streams enabled before installing.
	Modules []string `json:"modules,omitempty"`
	// Prefix replaces {php} in feature package templates.
	Prefix string `json:"prefix,omitempty"`
	// Binary is the long-running binary of a runtime (php-fpm).
	Binary string `json:"binary,omitempty"`
	// CLI is a versioned CLI binary that needs a "php" symlink (Alpine).
	CLI string `json:"cli,omitempty"`
	// Layout holds distribution-specific file locations a renderer needs
	// (for example the Apache module directory). Keys are allow-listed.
	Layout map[string]string `json:"layout,omitempty"`
}

var layoutKeys = map[string]bool{"moduleDir": true, "mimeTypes": true}

var layoutValueRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,100}$`)

// ComponentVersion is one selectable version with its availability matrix.
type ComponentVersion struct {
	Version string    `json:"version"`
	Support []Support `json:"support"`
}

// Feature is a selectable add-on of a component (a PHP extension, a package).
// Packages maps a distribution family to package templates; a missing family
// means the feature is not available there, an empty list means it is built
// into the base package.
type Feature struct {
	ID       string              `json:"id"`
	Packages map[string][]string `json:"packages"`
}

// EnumValue is one allowed value of an enum field.
type EnumValue struct {
	Value string `json:"value"`
}

// Field describes one allow-listed setting. Only fields in the catalog can be
// set; values are typed and range-checked and never reach a shell.
type Field struct {
	Key       string      `json:"key"`
	Type      string      `json:"type"` // bool, int, enum, string
	Group     string      `json:"group,omitempty"`
	Label     I18n        `json:"label"`
	Help      *I18n       `json:"help,omitempty"`
	Default   any         `json:"default"`
	Min       *int64      `json:"min,omitempty"`
	Max       *int64      `json:"max,omitempty"`
	Enum      []EnumValue `json:"enum,omitempty"`
	Pattern   string      `json:"pattern,omitempty"`
	MaxLength int         `json:"maxLength,omitempty"`
	// Target is where the generator writes the value ("ini", "pool"); nginx
	// settings have none.
	Target string `json:"target,omitempty"`
	// Needs names a component that must be selected for the setting to apply.
	Needs string `json:"needs,omitempty"`
	// NeedsFeature names a feature of the same component that must be selected.
	NeedsFeature string `json:"needsFeature,omitempty"`
	// Multiline asks the UI for a multi-line input (PEM text); the value is
	// still validated by the mandatory pattern.
	Multiline bool `json:"multiline,omitempty"`
}

// Component is one building block of an image.
type Component struct {
	ID          string `json:"id"`
	Name        I18n   `json:"name"`
	Description I18n   `json:"description"`
	// Kind is server, runtime, tool or utility.
	Kind string `json:"kind"`
	// Process is main (a long-running main process), companion (runs next
	// to a main process, or becomes the main process when alone) or none.
	Process      string             `json:"process"`
	Requires     []string           `json:"requires,omitempty"`
	Conflicts    []string           `json:"conflicts,omitempty"`
	Versions     []ComponentVersion `json:"versions"`
	FeatureLabel *I18n              `json:"featureLabel,omitempty"`
	Features     []Feature          `json:"features,omitempty"`
	Settings     []Field            `json:"settings,omitempty"`
}

// Catalog is the component catalog. It is plain data: the embedded JSON is
// the source, validated at load time.
type Catalog struct {
	SchemaVersion int         `json:"schemaVersion"`
	Version       string      `json:"version"`
	Evidence      string      `json:"evidence"`
	Distros       []Distro    `json:"distros"`
	Components    []Component `json:"components"`
}

//go:embed catalog.json
var embedded []byte

var (
	defaultOnce sync.Once
	defaultCat  *Catalog
	defaultErr  error
)

// Default returns the built-in catalog. It panics if the embedded data is
// invalid; a test guards that, so a release never ships a broken catalog.
func Default() *Catalog {
	defaultOnce.Do(func() { defaultCat, defaultErr = Parse(embedded) })
	if defaultErr != nil {
		panic(defaultErr)
	}
	return defaultCat
}

// Parse decodes and validates catalog JSON. Unknown fields are rejected so a
// typo in the data cannot silently disable a rule.
func Parse(data []byte) (*Catalog, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Catalog
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("imagecompose: catalog: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ErrCatalog wraps every catalog self-check failure.
var ErrCatalog = errors.New("imagecompose: invalid catalog")

func badCatalog(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrCatalog, fmt.Sprintf(format, a...))
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,40}$`)

// Validate checks the catalog's internal consistency: unique ids, matrix
// entries that refer to existing distributions and architectures, settings
// whose defaults satisfy their own schema, and honest evidence markers.
func (c *Catalog) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return badCatalog("schemaVersion %d is not supported (want %d)", c.SchemaVersion, SchemaVersion)
	}
	if c.Version == "" {
		return badCatalog("version is empty")
	}
	if c.Evidence != EvidenceUnverified {
		return badCatalog("evidence must be %q until upstream documents or real builds verify the data", EvidenceUnverified)
	}
	families := map[string]bool{"alpine": true, "debian": true, "rhel": true}
	rel := map[string]Release{}
	fam := map[string]string{}
	seen := map[string]bool{}
	for _, d := range c.Distros {
		if !idRe.MatchString(d.ID) || seen[d.ID] {
			return badCatalog("distro id %q is malformed or duplicated", d.ID)
		}
		seen[d.ID] = true
		if !families[d.Family] {
			return badCatalog("distro %s: unknown family %q", d.ID, d.Family)
		}
		for _, r := range d.Releases {
			k := d.ID + "/" + r.ID
			if r.ID == "" || r.Image == "" || r.Tag == "" || len(r.Architectures) == 0 {
				return badCatalog("release %s is incomplete", k)
			}
			if _, dup := rel[k]; dup {
				return badCatalog("release %s is duplicated", k)
			}
			for _, a := range r.Architectures {
				if !archRe.MatchString(a) {
					return badCatalog("release %s: unknown architecture %q", k, a)
				}
			}
			rel[k], fam[k] = r, d.Family
		}
	}
	ids := map[string]bool{}
	for _, comp := range c.Components {
		if !idRe.MatchString(comp.ID) || ids[comp.ID] {
			return badCatalog("component id %q is malformed or duplicated", comp.ID)
		}
		ids[comp.ID] = true
	}
	for _, comp := range c.Components {
		if err := comp.validate(c, ids, rel, fam); err != nil {
			return err
		}
	}
	return nil
}

var archRe = regexp.MustCompile(`^(amd64|arm64)$`)

func (comp Component) validate(c *Catalog, ids map[string]bool, rel map[string]Release, fam map[string]string) error {
	id := comp.ID
	if !slices.Contains([]string{"server", "runtime", "tool", "utility"}, comp.Kind) {
		return badCatalog("component %s: unknown kind %q", id, comp.Kind)
	}
	if !slices.Contains([]string{"main", "companion", "none"}, comp.Process) {
		return badCatalog("component %s: unknown process %q", id, comp.Process)
	}
	for _, r := range slices.Concat(comp.Requires, comp.Conflicts) {
		if !ids[r] || r == id {
			return badCatalog("component %s refers to unknown component %q", id, r)
		}
	}
	if len(comp.Versions) == 0 {
		return badCatalog("component %s has no versions", id)
	}
	features := map[string]bool{}
	for _, f := range comp.Features {
		if !idRe.MatchString(f.ID) || features[f.ID] {
			return badCatalog("component %s: feature id %q is malformed or duplicated", id, f.ID)
		}
		features[f.ID] = true
		for family, pkgs := range f.Packages {
			if family != "alpine" && family != "debian" && family != "rhel" {
				return badCatalog("component %s feature %s: unknown family %q", id, f.ID, family)
			}
			for _, p := range pkgs {
				if !pkgRe.MatchString(strings.ReplaceAll(p, "{php}", "php")) {
					return badCatalog("component %s feature %s: package %q is malformed", id, f.ID, p)
				}
			}
		}
	}
	vs := map[string]bool{}
	for _, v := range comp.Versions {
		if !versionRe.MatchString(v.Version) || vs[v.Version] {
			return badCatalog("component %s: version %q is malformed or duplicated", id, v.Version)
		}
		vs[v.Version] = true
		if len(v.Support) == 0 {
			return badCatalog("component %s %s: no support entries", id, v.Version)
		}
		sup := map[string]bool{}
		for _, s := range v.Support {
			k := s.Distro + "/" + s.Release
			r, ok := rel[k]
			if !ok || sup[k] {
				return badCatalog("component %s %s: support for %q refers to an unknown or duplicated release", id, v.Version, k)
			}
			sup[k] = true
			if len(s.Architectures) == 0 {
				return badCatalog("component %s %s on %s: no architectures", id, v.Version, k)
			}
			for _, a := range s.Architectures {
				if !slices.Contains(r.Architectures, a) {
					return badCatalog("component %s %s on %s: architecture %s is not offered by the base image", id, v.Version, k, a)
				}
			}
			for _, p := range s.Packages {
				if !pkgRe.MatchString(p.Name) || (p.Constraint != "" && !versionRe.MatchString(p.Constraint)) {
					return badCatalog("component %s %s on %s: package %q is malformed", id, v.Version, k, p.Name)
				}
			}
			for lk, lv := range s.Layout {
				if !layoutKeys[lk] || !layoutValueRe.MatchString(lv) {
					return badCatalog("component %s %s on %s: layout %q is not allowed", id, v.Version, k, lk)
				}
			}
			if s.Binary != "" && !binaryRe.MatchString(s.Binary) {
				return badCatalog("component %s %s on %s: binary %q is malformed", id, v.Version, k, s.Binary)
			}
			for _, m := range s.Modules {
				if !moduleRe.MatchString(m) {
					return badCatalog("component %s %s on %s: module %q is malformed", id, v.Version, k, m)
				}
			}
			if fam[k] != "rhel" && len(s.Modules) > 0 {
				return badCatalog("component %s %s on %s: dnf modules on a non-dnf family", id, v.Version, k)
			}
			if comp.ID == "php" && (s.Prefix == "" || s.Binary == "") {
				return badCatalog("component %s %s on %s: prefix and binary are required", id, v.Version, k)
			}
		}
	}
	keys := map[string]bool{}
	for _, f := range comp.Settings {
		if keys[f.Key] {
			return badCatalog("component %s: setting %q is duplicated", id, f.Key)
		}
		keys[f.Key] = true
		if err := f.validateSchema(); err != nil {
			return badCatalog("component %s setting %s: %v", id, f.Key, err)
		}
		if f.Needs != "" && !ids[f.Needs] {
			return badCatalog("component %s setting %s: needs unknown component %q", id, f.Key, f.Needs)
		}
		if f.NeedsFeature != "" && !features[f.NeedsFeature] {
			return badCatalog("component %s setting %s: needs unknown feature %q", id, f.Key, f.NeedsFeature)
		}
		if f.Target != "" && f.Target != "ini" && f.Target != "pool" {
			return badCatalog("component %s setting %s: unknown target %q", id, f.Key, f.Target)
		}
		if _, err := f.coerce(f.Default); err != nil {
			return badCatalog("component %s setting %s: default is invalid: %v", id, f.Key, err)
		}
	}
	return nil
}

var (
	pkgRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]{0,80}$`)
	versionRe = regexp.MustCompile(`^[0-9a-z][0-9a-z.]{0,15}$`)
	moduleRe  = regexp.MustCompile(`^[a-z0-9]+:[0-9][0-9.]{0,10}$`)
	binaryRe  = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)*/?[A-Za-z0-9][A-Za-z0-9._-]*$`)
	keyRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]{0,63}$`)
)

func (f Field) validateSchema() error {
	if !keyRe.MatchString(f.Key) {
		return errors.New("key is malformed")
	}
	switch f.Type {
	case "bool":
	case "int":
		if f.Min == nil || f.Max == nil || *f.Min > *f.Max {
			return errors.New("int needs min <= max")
		}
	case "enum":
		if len(f.Enum) == 0 {
			return errors.New("enum needs values")
		}
	case "string":
		if f.Pattern == "" || f.MaxLength <= 0 {
			return errors.New("string needs a pattern and maxLength (free text is not allowed)")
		}
		if _, err := regexp.Compile(f.Pattern); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown type %q", f.Type)
	}
	if f.Label.En == "" || f.Label.De == "" {
		return errors.New("label needs both languages")
	}
	return nil
}

// coerce converts a JSON-decoded value to the field's canonical Go type
// (bool, int64 or string) and checks it against the schema.
func (f Field) coerce(v any) (any, error) {
	switch f.Type {
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("must be true or false")
		}
		return b, nil
	case "int":
		var n int64
		switch x := v.(type) {
		case int:
			n = int64(x)
		case int64:
			n = x
		case float64:
			if x != float64(int64(x)) {
				return nil, errors.New("must be a whole number")
			}
			n = int64(x)
		case json.Number:
			i, err := x.Int64()
			if err != nil {
				return nil, errors.New("must be a whole number")
			}
			n = i
		default:
			return nil, errors.New("must be a whole number")
		}
		if n < *f.Min || n > *f.Max {
			return nil, fmt.Errorf("must be between %d and %d", *f.Min, *f.Max)
		}
		return n, nil
	case "enum":
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be text")
		}
		for _, e := range f.Enum {
			if e.Value == s {
				return s, nil
			}
		}
		return nil, errors.New("is not one of the allowed values")
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be text")
		}
		s = strings.TrimSpace(s)
		if len(s) > f.MaxLength {
			return nil, fmt.Errorf("must be at most %d characters", f.MaxLength)
		}
		if !regexp.MustCompile(f.Pattern).MatchString(s) {
			return nil, errors.New("has an unsupported format")
		}
		return s, nil
	}
	return nil, errors.New("unknown field type")
}

// Component returns the component with the id.
func (c *Catalog) Component(id string) (Component, bool) {
	for _, comp := range c.Components {
		if comp.ID == id {
			return comp, true
		}
	}
	return Component{}, false
}

// Release returns the release and its family.
func (c *Catalog) Release(distro, release string) (Release, string, bool) {
	for _, d := range c.Distros {
		if d.ID != distro {
			continue
		}
		for _, r := range d.Releases {
			if r.ID == release {
				return r, d.Family, true
			}
		}
	}
	return Release{}, "", false
}

// Version returns a component version.
func (comp Component) Version(v string) (ComponentVersion, bool) {
	for _, cv := range comp.Versions {
		if cv.Version == v {
			return cv, true
		}
	}
	return ComponentVersion{}, false
}

// Support returns the support entry of a component version on a release.
func (cv ComponentVersion) SupportFor(distro, release string) (Support, bool) {
	for _, s := range cv.Support {
		if s.Distro == distro && s.Release == release {
			return s, true
		}
	}
	return Support{}, false
}

// Feature returns a feature by id.
func (comp Component) Feature(id string) (Feature, bool) {
	for _, f := range comp.Features {
		if f.ID == id {
			return f, true
		}
	}
	return Feature{}, false
}

// FeatureAvailable reports whether a feature exists for the distribution family.
func (f Feature) FeatureAvailable(family string) bool {
	_, ok := f.Packages[family]
	return ok
}

// sortedVersions lists versions newest first by simple dotted-number order.
func (comp Component) sortedVersions() []string {
	out := make([]string, 0, len(comp.Versions))
	for _, v := range comp.Versions {
		out = append(out, v.Version)
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[j], out[i]) })
	return out
}

func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			if len(as[i]) != len(bs[i]) {
				return len(as[i]) < len(bs[i])
			}
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}
