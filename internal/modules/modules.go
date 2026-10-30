// SPDX-License-Identifier: AGPL-3.0-only

// Package modules is the registry of optional features. Each module can be
// enabled and disabled at runtime by an administrator; the state lives in the
// replicated metastore and the configuration file only supplies the default
// for a module that was never switched. Disabling stops new work (routes
// answer module_disabled, schedulers skip) and never deletes data.
package modules

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

// Text is a UI string in both supported languages.
type Text struct {
	DE string `json:"de"`
	EN string `json:"en"`
}

// Module describes one optional feature.
type Module struct {
	ID          string   `json:"id"`
	Title       Text     `json:"title"`
	Description Text     `json:"description"`
	Requires    []string `json:"requires"`
	// routes are API route templates (relative to /api/v1) the module owns;
	// a trailing "/..." matches the path and everything below it.
	routes []string
}

// Catalog lists the modules; a module follows the modules it requires.
func Catalog() []Module { return slices.Clone(catalog) }

var catalog = []Module{
	{ID: "patch", Title: Text{"OS-Patchverwaltung", "OS patch management"},
		Description: Text{"Berichte, Pläne und gesteuerte Updates der Host-Pakete über den nativen Agenten.", "Reports, plans and guarded updates of host packages through the native agent."},
		routes:      []string{"/patch/..."}},
	{ID: "host-backups", Title: Text{"Host-Backups (restic)", "Host backups (restic)"},
		Description: Text{"Dateisystem-Quellen der Hosts per restic sichern.", "Back up host filesystem sources with restic."},
		routes:      []string{"/host-sources/..."}},
	{ID: "backup-copies", Title: Text{"Backup-Kopien", "Backup copies"},
		Description: Text{"Backups auf weitere Ziele kopieren und von dort wiederherstellen.", "Copy backups to further targets and restore from them."},
		routes:      []string{"/backup-copies/...", "/clusters/{id}/backup-copies"}},
	{ID: "image-build", Title: Text{"Image-Build", "Image build"},
		Description: Text{"Container-Images über Docker Buildx bauen.", "Build container images with Docker buildx."},
		routes:      []string{"/image-builds/..."}},
	{ID: "image-composer", Title: Text{"Image-Composer", "Image composer"}, Requires: []string{"image-build"},
		Description: Text{"Images aus einem geprüften Katalog zusammenstellen und bauen.", "Compose images from a reviewed catalog and build them."},
		routes:      []string{"/image-composer/..."}},
	{ID: "image-update", Title: Text{"Image-Updates", "Image updates"},
		Description: Text{"Neue Image-Versionen erkennen und Dienste kontrolliert aktualisieren.", "Detect new image versions and update services under control."},
		routes:      []string{"/image-updates/..."}},
	{ID: "vuln", Title: Text{"Schwachstellen und KI-Antwort", "Vulnerabilities and AI response"},
		Description: Text{"Advisories auswerten, Funde bewerten und Maßnahmen vorschlagen.", "Evaluate advisories, assess findings and propose actions."},
		routes:      []string{"/vuln/..."}},
	{ID: "bootstrap", Title: Text{"Server-Bootstrap", "Server bootstrap"},
		Description: Text{"Frische Server per SSH einrichten und in den Cluster aufnehmen.", "Set up fresh servers over SSH and join them to the cluster."},
		routes:      []string{"/bootstrap/..."}},
	{ID: "provisioning", Title: Text{"SSH-Provisionierung", "SSH provisioning"},
		Description: Text{"Swarm-Hosts per genehmigtem Plan über SSH bereitstellen.", "Provision Swarm hosts over SSH with an approved plan."},
		routes:      []string{"/provision/..."}},
	{ID: "kubernetes", Title: Text{"Kubernetes", "Kubernetes"},
		Description: Text{"Kubernetes-Cluster registrieren und per SSH bereitstellen (experimentell).", "Register and provision Kubernetes clusters (experimental)."},
		routes:      []string{"/kubernetes-clusters/..."}},
}

// ForRoute returns the module owning an API route template, or "".
func ForRoute(route string) string {
	for _, m := range catalog {
		for _, r := range m.routes {
			if p, ok := strings.CutSuffix(r, "/..."); ok {
				if route == p || strings.HasPrefix(route, p+"/") {
					return m.ID
				}
			} else if route == r {
				return m.ID
			}
		}
	}
	return ""
}

// State is the stored switch of one module.
type State struct {
	Enabled bool      `json:"enabled"`
	By      string    `json:"by"`
	At      time.Time `json:"at"`
}

// Store persists module states.
type Store interface {
	Load() (map[string]State, error)
	Save(id string, s State) error
}

// ErrUnknown is returned for an id that is not in the catalog.
var ErrUnknown = errors.New("modules: unknown module")

// BlockedError says why a switch is refused.
type BlockedError struct {
	Config     []string // missing configuration keys (enable)
	Modules    []string // required modules that are off (enable)
	Dependents []string // switched-on modules that need this one (disable)
}

func (e *BlockedError) Error() string {
	return "modules: switch blocked (config: " + strings.Join(e.Config, ", ") + "; modules: " + strings.Join(e.Modules, ", ") + "; dependents: " + strings.Join(e.Dependents, ", ") + ")"
}

// Info is one module with its current state for the API.
type Info struct {
	Module
	Enabled bool `json:"enabled"`
	// MissingConfig and MissingModules explain why it cannot be enabled;
	// Dependents why it cannot be disabled.
	MissingConfig  []string   `json:"missingConfig"`
	MissingModules []string   `json:"missingModules"`
	Dependents     []string   `json:"dependents"`
	By             string     `json:"by,omitempty"`
	At             *time.Time `json:"at,omitempty"`
}

// Registry answers Enabled for routes and schedulers and applies switches.
type Registry struct {
	store    Store
	defaults map[string]bool
	missing  func(id string) []string
	now      func() time.Time

	mu     sync.RWMutex
	states map[string]State
}

// New loads the stored states. defaults is the configuration file's initial
// choice; missing reports the configuration a module cannot run without
// (nil: none).
func New(store Store, defaults map[string]bool, missing func(id string) []string) (*Registry, error) {
	states, err := store.Load()
	if err != nil {
		return nil, err
	}
	if missing == nil {
		missing = func(string) []string { return nil }
	}
	return &Registry{store: store, defaults: defaults, missing: missing, now: time.Now, states: states}, nil
}

func find(id string) (Module, bool) {
	i := slices.IndexFunc(catalog, func(m Module) bool { return m.ID == id })
	if i < 0 {
		return Module{}, false
	}
	return catalog[i], true
}

// switched is the module's own switch, ignoring prerequisites.
func (r *Registry) switched(id string) bool {
	if s, ok := r.states[id]; ok {
		return s.Enabled
	}
	return r.defaults[id]
}

func (r *Registry) enabled(id string) bool {
	m, ok := find(id)
	return ok && r.switched(id) && len(r.missing(id)) == 0 && len(r.offModules(m)) == 0
}

// Enabled reports whether the module runs: switched on, configuration
// present and every required module running. It fails closed.
func (r *Registry) Enabled(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled(id)
}

func (r *Registry) offModules(m Module) []string {
	var off []string
	for _, d := range m.Requires {
		if !r.enabled(d) {
			off = append(off, d)
		}
	}
	return off
}

func (r *Registry) dependents(id string) []string {
	var out []string
	for _, m := range catalog {
		if slices.Contains(m.Requires, id) && r.switched(m.ID) {
			out = append(out, m.ID)
		}
	}
	return out
}

// List returns every module in catalog order.
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(catalog))
	for _, m := range catalog {
		in := Info{Module: m, Enabled: r.enabled(m.ID), MissingConfig: nonNil(r.missing(m.ID)), MissingModules: nonNil(r.offModules(m)), Dependents: nonNil(r.dependents(m.ID))}
		in.Requires = nonNil(m.Requires)
		if s, ok := r.states[m.ID]; ok {
			in.By, in.At = s.By, &s.At
		}
		out = append(out, in)
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Set switches a module. Enabling needs its configuration and required
// modules; disabling needs every dependent module off. The state is stored
// before it takes effect and no data is removed.
func (r *Registry) Set(id string, on bool, by string) error {
	m, ok := find(id)
	if !ok {
		return ErrUnknown
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		if cfg, mods := r.missing(id), r.offModules(m); len(cfg)+len(mods) > 0 {
			return &BlockedError{Config: cfg, Modules: mods}
		}
	} else if dep := r.dependents(id); len(dep) > 0 {
		return &BlockedError{Dependents: dep}
	}
	s := State{Enabled: on, By: by, At: r.now().UTC()}
	if err := r.store.Save(id, s); err != nil {
		return err
	}
	r.states[id] = s
	return nil
}
