// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"errors"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/modules"
)

// moduleEndpoints lists the optional features (any session, the UI needs it
// for navigation) and lets an administrator switch one. The switch is
// audited like every state change.
func (s *server) moduleEndpoints() []endpoint {
	return []endpoint{
		{method: "GET", path: "/modules", access: accessSession, handle: s.handleModules},
		{
			method: "PUT", path: "/modules/{id}", access: accessPermission, adminOnly: true,
			action: authz.ActionUpdate, resolve: s.resolveSettings, handle: s.handleModulePut,
		},
	}
}

func (s *server) handleModules(w http.ResponseWriter, _ *http.Request, _ *caller) {
	if s.cfg.Modules == nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, List[modules.Info]{Items: s.cfg.Modules.List()})
}

func (s *server) handleModulePut(w http.ResponseWriter, r *http.Request, c *caller) {
	if s.cfg.Modules == nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &in, false) {
		return
	}
	id := r.PathValue("id")
	switch err := s.cfg.Modules.Set(id, in.Enabled, c.subject.ID); {
	case errors.Is(err, modules.ErrUnknown):
		writeError(w, http.StatusNotFound, errNotFound)
	case errors.As(err, new(*modules.BlockedError)):
		writeError(w, http.StatusConflict, errModuleBlocked)
	case err != nil:
		s.writeServiceError(w, r, err)
	default:
		for _, m := range s.cfg.Modules.List() {
			if m.ID == id {
				writeJSON(w, http.StatusOK, m)
			}
		}
	}
}
