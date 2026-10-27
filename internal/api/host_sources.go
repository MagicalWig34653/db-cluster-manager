// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

// HostSource is a host filesystem source, independent of a database cluster.
// Credential values are write-only and never appear in this view.
type HostSource struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Kind             string           `json:"kind"`      // managed-host or external-host
	Transport        string           `json:"transport"` // agent or ssh-plan
	Swarm            string           `json:"swarm,omitempty"`
	HostID           string           `json:"hostId"`
	SSHAddress       string           `json:"sshAddress,omitempty"`
	SSHUser          string           `json:"sshUser,omitempty"`
	Paths            []string         `json:"paths"`
	Excludes         []string         `json:"excludes,omitempty"`
	Backend          restic.Backend   `json:"backend"`
	Repository       string           `json:"repository"`
	PasswordSecret   string           `json:"passwordSecret"`
	CredentialSecret string           `json:"credentialSecret,omitempty"`
	Schedule         string           `json:"schedule,omitempty"`
	Retention        restic.Retention `json:"retention"`
	CheckSubset      string           `json:"checkSubset,omitempty"`
	LastRunAt        string           `json:"lastRunAt,omitempty"`
	LastRunStatus    string           `json:"lastRunStatus,omitempty"`
}

type HostSourceInput struct {
	HostSource
	// Password and Credentials are sealed in the manager's replicated store;
	// agent execution still requires corresponding mounted secret names.
	Password      string            `json:"password,omitempty"`
	Credentials   map[string]string `json:"credentials,omitempty"`
	SSHPrivateKey string            `json:"sshPrivateKey,omitempty"`
	SSHKnownHosts string            `json:"sshKnownHosts,omitempty"`
}

type HostRestoreInput struct {
	Snapshot string `json:"snapshot"`
	Target   string `json:"target"`
}

type HostSources interface {
	List(context.Context) ([]HostSource, error)
	Get(context.Context, string) (HostSource, error)
	Put(context.Context, authz.Subject, HostSourceInput) (HostSource, error)
	Delete(context.Context, authz.Subject, string) error
	Run(context.Context, authz.Subject, string) (OperationRef, error)
	Restore(context.Context, authz.Subject, string, HostRestoreInput) (OperationRef, error)
}

func hostSourceResource(h HostSource) authz.Resource {
	scope := authz.Global
	if h.Swarm != "" {
		scope = authz.SwarmScope(h.Swarm)
	}
	return authz.Resource{Type: authz.ResourceBackup, Scope: scope, ID: h.ID}
}

func (s *server) hostSourceEndpoints() []endpoint {
	return []endpoint{
		{method: "GET", path: "/host-sources", access: accessFiltered, handle: s.handleHostSources},
		{method: "POST", path: "/host-sources", access: accessPermission, action: authz.ActionCreate, resolve: s.resolveNewHostSource, handle: s.handleHostSourcePut},
		{method: "GET", path: "/host-sources/{id}", access: accessPermission, action: authz.ActionRead, resolve: s.resolveHostSource, hideDenied: true, handle: s.handleHostSourceGet},
		{method: "PUT", path: "/host-sources/{id}", access: accessPermission, action: authz.ActionUpdate, resolve: s.resolveHostSource, hideDenied: true, handle: s.handleHostSourcePut},
		{method: "DELETE", path: "/host-sources/{id}", access: accessPermission, action: authz.ActionDelete, resolve: s.resolveHostSource, hideDenied: true, handle: s.handleHostSourceDelete},
		{method: "POST", path: "/host-sources/{id}/backups", access: accessPermission, action: authz.ActionBackup, resolve: s.resolveHostSource, hideUnreadable: true, handle: s.handleHostSourceRun},
		{method: "POST", path: "/host-sources/{id}/restore", access: accessPermission, action: authz.ActionRestore, resolve: s.resolveHostSource, hideUnreadable: true, handle: s.handleHostSourceRestore},
	}
}

func (s *server) resolveNewHostSource(r *http.Request) (authz.Resource, any, error) {
	var in HostSourceInput
	if err := readJSON(r, &in, false); err != nil {
		return authz.Resource{}, nil, err
	}
	return hostSourceResource(in.HostSource), in, nil
}

func (s *server) resolveHostSource(r *http.Request) (authz.Resource, any, error) {
	if s.cfg.HostSources == nil {
		return authz.Resource{}, nil, ErrUnavailable
	}
	h, err := s.cfg.HostSources.Get(r.Context(), r.PathValue("id"))
	return hostSourceResource(h), h, err
}

func (s *server) handleHostSources(w http.ResponseWriter, r *http.Request, c *caller) {
	if s.cfg.HostSources == nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	items, err := s.cfg.HostSources.List(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, List[HostSource]{Items: authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, hostSourceResource)})
}

func (s *server) handleHostSourceGet(w http.ResponseWriter, _ *http.Request, c *caller) {
	writeJSON(w, http.StatusOK, c.target.(HostSource))
}

func (s *server) handleHostSourcePut(w http.ResponseWriter, r *http.Request, c *caller) {
	if s.cfg.HostSources == nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	var in HostSourceInput
	status := http.StatusCreated
	if r.Method == "POST" {
		in = c.target.(HostSourceInput)
	} else {
		status = http.StatusOK
		if !decodeJSON(w, r, &in, false) {
			return
		}
		in.ID = r.PathValue("id")
		// Scope cannot change through an update authorized on the old scope.
		old := c.target.(HostSource)
		if in.Swarm != old.Swarm || in.Kind != old.Kind || in.HostID != old.HostID {
			writeError(w, http.StatusBadRequest, errBadRequest)
			return
		}
	}
	out, err := s.cfg.HostSources.Put(r.Context(), c.subject, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, status, out)
}

func (s *server) handleHostSourceDelete(w http.ResponseWriter, r *http.Request, c *caller) {
	if err := s.cfg.HostSources.Delete(r.Context(), c.subject, c.target.(HostSource).ID); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleHostSourceRun(w http.ResponseWriter, r *http.Request, c *caller) {
	var empty struct{}
	if !decodeJSON(w, r, &empty, true) {
		return
	}
	ref, err := s.cfg.HostSources.Run(r.Context(), c.subject, c.target.(HostSource).ID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ref)
}

func (s *server) handleHostSourceRestore(w http.ResponseWriter, r *http.Request, c *caller) {
	var in HostRestoreInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	ref, err := s.cfg.HostSources.Restore(r.Context(), c.subject, c.target.(HostSource).ID, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ref)
}
