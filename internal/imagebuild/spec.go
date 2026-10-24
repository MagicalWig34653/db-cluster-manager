// SPDX-License-Identifier: AGPL-3.0-only

// Package imagebuild models a web server composition (X04), renders it
// deterministically to a Dockerfile and build context, explains the result
// as a plan before anything is built (X03) and records build jobs (X08).
//
// Status: proposal implemented against fakes only. Nothing here has built a
// real image. The command-line construction of the docker/BuildKit builder is
// unit-tested but never executed, and the catalog of versions, PHP extensions
// and Alpine package names has not been verified against the real
// repositories.
package imagebuild

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

// ErrInvalidSpec wraps every validation failure of a composition.
var ErrInvalidSpec = errors.New("imagebuild: invalid spec")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSpec, fmt.Sprintf(format, a...))
}

// WebServerSpec is the typed composition of one web server image.
type WebServerSpec struct {
	// Name labels the composition; it becomes the image title.
	Name  string    `json:"name"`
	Nginx NginxSpec `json:"nginx"`
	// PHP is optional; an empty Version builds a static nginx image.
	PHP           PHPSpec  `json:"php"`
	ExtraPackages []string `json:"extraPackages,omitempty"`
	// Platforms are OCI platforms such as linux/amd64. Empty means
	// linux/amd64 only.
	Platforms []string `json:"platforms,omitempty"`
}

// NginxSpec selects the nginx version and its configuration.
type NginxSpec struct {
	// Version must be in the catalog (for example "1.27").
	Version string `json:"version"`
	// BaseDigest pins the base image (sha256:<64 hex>). The base image
	// policy is "digest required": a floating tag is never built. Resolve the
	// digest through the registry layer and review it in the plan.
	BaseDigest string       `json:"baseDigest"`
	Options    NginxOptions `json:"options"`
	// CustomSnippet is validated, size-limited server-level configuration
	// included after the structured options.
	CustomSnippet string `json:"customSnippet,omitempty"`
}

// NginxOptions are the structured nginx settings. Zero values select the
// defaults in DefaultNginxOptions.
type NginxOptions struct {
	WorkerProcesses   string `json:"workerProcesses,omitempty"`   // "auto" or 1..64
	WorkerConnections int    `json:"workerConnections,omitempty"` // 64..65535
	ServerName        string `json:"serverName,omitempty"`        // host name or "_"
	DocumentRoot      string `json:"documentRoot,omitempty"`      // below /var/www
	ClientMaxBodySize string `json:"clientMaxBodySize,omitempty"` // 1m, 64M, ...
	KeepaliveTimeout  int    `json:"keepaliveTimeout,omitempty"`  // seconds 0..600
	Gzip              *bool  `json:"gzip,omitempty"`
	ServerTokens      *bool  `json:"serverTokens,omitempty"` // default off
	SecurityHeaders   *bool  `json:"securityHeaders,omitempty"`
	// FastCGIReadTimeout applies when PHP is present, seconds 1..3600.
	FastCGIReadTimeout int `json:"fastcgiReadTimeout,omitempty"`
}

// PHPSpec selects PHP-FPM, extensions and php.ini settings.
type PHPSpec struct {
	Version    string            `json:"version,omitempty"`
	Extensions []string          `json:"extensions,omitempty"`
	INI        map[string]string `json:"ini,omitempty"`
}

// Defaults for NginxOptions.
func DefaultNginxOptions() NginxOptions {
	t, f := true, false
	return NginxOptions{
		WorkerProcesses: "auto", WorkerConnections: 1024, ServerName: "_",
		DocumentRoot: "/var/www/html", ClientMaxBodySize: "1m", KeepaliveTimeout: 65,
		Gzip: &t, ServerTokens: &f, SecurityHeaders: &t, FastCGIReadTimeout: 60,
	}
}

var (
	digestRe   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}$`)
	sizeRe     = regexp.MustCompile(`^[1-9][0-9]{0,5}[kKmMgG]?$`)
	hostRe     = regexp.MustCompile(`^(_|[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?)$`)
	rootRe     = regexp.MustCompile(`^/var/www(/[A-Za-z0-9._-]+)*$`)
	platformRe = regexp.MustCompile(`^linux/(amd64|arm64)$`)
)

// Normalize returns a copy with defaults applied and lists sorted and
// de-duplicated, so equal intent yields an equal spec.
func (s WebServerSpec) Normalize() WebServerSpec {
	out := s
	out.Name = strings.TrimSpace(s.Name)
	out.Nginx.Options = mergeOptions(DefaultNginxOptions(), s.Nginx.Options)
	out.Nginx.CustomSnippet = strings.ReplaceAll(strings.TrimSpace(s.Nginx.CustomSnippet), "\r\n", "\n")
	out.PHP.Extensions = sortedUnique(s.PHP.Extensions)
	out.PHP.INI = nil
	if len(s.PHP.INI) > 0 {
		out.PHP.INI = make(map[string]string, len(s.PHP.INI))
		for k, v := range s.PHP.INI {
			out.PHP.INI[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	out.ExtraPackages = sortedUnique(s.ExtraPackages)
	out.Platforms = sortedUnique(s.Platforms)
	if len(out.Platforms) == 0 {
		out.Platforms = []string{"linux/amd64"}
	}
	return out
}

func mergeOptions(d, o NginxOptions) NginxOptions {
	if o.WorkerProcesses != "" {
		d.WorkerProcesses = o.WorkerProcesses
	}
	if o.WorkerConnections != 0 {
		d.WorkerConnections = o.WorkerConnections
	}
	if o.ServerName != "" {
		d.ServerName = o.ServerName
	}
	if o.DocumentRoot != "" {
		d.DocumentRoot = o.DocumentRoot
	}
	if o.ClientMaxBodySize != "" {
		d.ClientMaxBodySize = o.ClientMaxBodySize
	}
	if o.KeepaliveTimeout != 0 {
		d.KeepaliveTimeout = o.KeepaliveTimeout
	}
	if o.Gzip != nil {
		d.Gzip = o.Gzip
	}
	if o.ServerTokens != nil {
		d.ServerTokens = o.ServerTokens
	}
	if o.SecurityHeaders != nil {
		d.SecurityHeaders = o.SecurityHeaders
	}
	if o.FastCGIReadTimeout != 0 {
		d.FastCGIReadTimeout = o.FastCGIReadTimeout
	}
	return d
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// Hash is the content hash of the normalized spec: identical intent gives an
// identical hash. It is computed over canonical JSON (struct field order,
// sorted map keys).
func (s WebServerSpec) Hash() string {
	b, err := json.Marshal(s.Normalize())
	if err != nil {
		panic(err) // plain data; cannot fail
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Validate checks the spec against the catalog. It validates the normalized
// form, so defaults count.
func (s WebServerSpec) Validate(c *Catalog) error {
	if c == nil {
		c = DefaultCatalog()
	}
	n := s.Normalize()
	if !nameRe.MatchString(n.Name) {
		return invalid("name must be 1-63 characters of letters, digits, space, '.', '_' or '-'")
	}
	if !c.HasNginx(n.Nginx.Version) {
		return invalid("nginx version %q is not in the catalog (available: %s)", n.Nginx.Version, strings.Join(c.NginxVersions(), ", "))
	}
	if !digestRe.MatchString(n.Nginx.BaseDigest) {
		return invalid("base image must be pinned by digest (sha256:<64 hex>); floating tags are not built")
	}
	if err := validateOptions(n.Nginx.Options, n.PHP.Version != ""); err != nil {
		return err
	}
	if err := ValidateSnippet(n.Nginx.CustomSnippet); err != nil {
		return err
	}
	if err := validatePHP(n.PHP, c); err != nil {
		return err
	}
	for _, p := range n.ExtraPackages {
		if !c.HasPackage(p) {
			return invalid("package %q is not in the allow-list (allowed: %s)", p, strings.Join(c.Packages(), ", "))
		}
	}
	for _, p := range n.Platforms {
		if !platformRe.MatchString(p) {
			return invalid("platform %q is not supported (linux/amd64, linux/arm64)", p)
		}
	}
	return nil
}

func validateOptions(o NginxOptions, php bool) error {
	if o.WorkerProcesses != "auto" {
		if n, err := strconv.Atoi(o.WorkerProcesses); err != nil || n < 1 || n > 64 {
			return invalid("workerProcesses must be \"auto\" or 1-64")
		}
	}
	if o.WorkerConnections < 64 || o.WorkerConnections > 65535 {
		return invalid("workerConnections must be 64-65535")
	}
	if !hostRe.MatchString(o.ServerName) {
		return invalid("serverName %q is not a host name", o.ServerName)
	}
	if !rootRe.MatchString(o.DocumentRoot) || strings.Contains(o.DocumentRoot, "..") {
		return invalid("documentRoot must be /var/www or a subdirectory")
	}
	if !sizeRe.MatchString(o.ClientMaxBodySize) {
		return invalid("clientMaxBodySize %q is not a size such as 1m or 64M", o.ClientMaxBodySize)
	}
	if o.KeepaliveTimeout < 0 || o.KeepaliveTimeout > 600 {
		return invalid("keepaliveTimeout must be 0-600 seconds")
	}
	if o.FastCGIReadTimeout < 1 || o.FastCGIReadTimeout > 3600 {
		return invalid("fastcgiReadTimeout must be 1-3600 seconds")
	}
	_ = php
	return nil
}

func validatePHP(p PHPSpec, c *Catalog) error {
	if p.Version == "" {
		if len(p.Extensions) > 0 || len(p.INI) > 0 {
			return invalid("PHP extensions or ini settings were given without a PHP version")
		}
		return nil
	}
	if !c.HasPHP(p.Version) {
		return invalid("PHP version %q is not in the catalog (available: %s)", p.Version, strings.Join(c.PHPVersions(), ", "))
	}
	for _, e := range p.Extensions {
		if !c.PHPExtensionAvailable(p.Version, e) {
			if c.KnownExtension(e) {
				return invalid("PHP extension %q is not available for PHP %s", e, p.Version)
			}
			return invalid("PHP extension %q is not in the allow-list", e)
		}
	}
	keys := make([]string, 0, len(p.INI))
	for k := range p.INI {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rule, ok := iniRules[k]
		if !ok {
			return invalid("php.ini setting %q is not in the allow-list", k)
		}
		if err := rule(p.INI[k]); err != nil {
			return invalid("php.ini %s: %v", k, err)
		}
	}
	return nil
}

// iniRules is the php.ini allow-list with a validator per setting.
var iniRules = map[string]func(string) error{
	"memory_limit":               sizeOrUnlimited,
	"upload_max_filesize":        sizeValue,
	"post_max_size":              sizeValue,
	"max_execution_time":         intRange(0, 3600),
	"max_input_time":             intRange(-1, 3600),
	"max_input_vars":             intRange(100, 100000),
	"date.timezone":              timezoneValue,
	"display_errors":             oneOf("On", "Off"),
	"expose_php":                 oneOf("On", "Off"),
	"error_reporting":            oneOf("E_ALL", "E_ALL & ~E_DEPRECATED", "E_ALL & ~E_NOTICE & ~E_DEPRECATED"),
	"opcache.enable":             oneOf("0", "1"),
	"opcache.memory_consumption": intRange(8, 4096),
	"session.cookie_secure":      oneOf("0", "1"),
	"session.cookie_httponly":    oneOf("0", "1"),
	"realpath_cache_size":        sizeValue,
	// Attack-surface settings used by vulnerability mitigations (X13).
	"session.upload_progress.enabled": oneOf("0", "1"),
	"file_uploads":                    oneOf("On", "Off"),
	"allow_url_fopen":                 oneOf("On", "Off"),
	"allow_url_include":               oneOf("Off"),
}

var (
	iniSizeRe = regexp.MustCompile(`^[1-9][0-9]{0,6}[KMG]?$`)
	tzRe      = regexp.MustCompile(`^(UTC|[A-Z][A-Za-z]+/[A-Za-z_]+(/[A-Za-z_]+)?)$`)
)

func sizeValue(v string) error {
	if !iniSizeRe.MatchString(v) {
		return fmt.Errorf("%q is not a size such as 64M", v)
	}
	return nil
}

func sizeOrUnlimited(v string) error {
	if v == "-1" {
		return nil
	}
	return sizeValue(v)
}

func timezoneValue(v string) error {
	if !tzRe.MatchString(v) {
		return fmt.Errorf("%q is not UTC or an Area/Location name", v)
	}
	return nil
}

func intRange(lo, hi int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("%q must be an integer from %d to %d", v, lo, hi)
		}
		return nil
	}
}

func oneOf(allowed ...string) func(string) error {
	return func(v string) error {
		if !slices.Contains(allowed, v) {
			return fmt.Errorf("%q must be one of %s", v, strings.Join(allowed, ", "))
		}
		return nil
	}
}

// IniSettings lists the allow-listed php.ini keys, sorted.
func IniSettings() []string {
	out := make([]string, 0, len(iniRules))
	for k := range iniRules {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
