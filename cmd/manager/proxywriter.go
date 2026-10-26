// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
)

var writerIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type proxyWriterMember struct {
	ID        string `json:"id"`
	TokenFile string `json:"tokenFile"`
}

type proxyWriterConfig struct {
	ClusterID      string              `json:"clusterId"`
	InitialWriter  string              `json:"initialWriter"`
	Candidates     []string            `json:"candidates"`
	AutoFailover   bool                `json:"autoFailover,omitempty"`
	Members        []proxyWriterMember `json:"members"`
	AdminTokenFile string              `json:"adminTokenFile"`
}

func validateProxyWriters(configs []proxyWriterConfig) error {
	clusters := map[string]bool{}
	for _, c := range configs {
		if !writerIDPattern.MatchString(c.ClusterID) || !writerIDPattern.MatchString(c.InitialWriter) || clusters[c.ClusterID] || len(c.Members) == 0 || len(c.Candidates) == 0 || !slices.Contains(c.Candidates, c.InitialWriter) || c.AdminTokenFile == "" {
			return errors.New("manager.proxyWriters needs unique clusterId, initialWriter, candidates, members and adminTokenFile")
		}
		clusters[c.ClusterID] = true
		if !filepath.IsAbs(c.AdminTokenFile) {
			return errors.New("manager.proxyWriters token paths must be absolute")
		}
		seen := map[string]bool{}
		paths := map[string]bool{c.AdminTokenFile: true}
		for _, writer := range c.Candidates {
			if !writerIDPattern.MatchString(writer) || seen[writer] {
				return errors.New("manager.proxyWriters candidates need unique IDs")
			}
			seen[writer] = true
		}
		seen = map[string]bool{}
		for _, m := range c.Members {
			if !writerIDPattern.MatchString(m.ID) || seen[m.ID] || m.TokenFile == "" || !filepath.IsAbs(m.TokenFile) || paths[m.TokenFile] {
				return errors.New("manager.proxyWriters members need unique IDs and absolute tokenFile paths")
			}
			seen[m.ID] = true
			paths[m.TokenFile] = true
		}
	}
	return nil
}

func (c proxyWriterConfig) coordinator(st *manager.Store) manager.ProxyWriterStore {
	out := manager.ProxyWriterStore{Store: st, Cluster: c.ClusterID, InitialWriter: c.InitialWriter, Candidates: slices.Clone(c.Candidates)}
	for _, m := range c.Members {
		out.Members = append(out.Members, m.ID)
	}
	slices.Sort(out.Members)
	return out
}

func bearerMatches(r *http.Request, file string) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	secret, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	want := strings.TrimSpace(string(secret))
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return want != "" && len(want) == len(got) && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func (rt *managedRuntime) serveProxyWriter(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if rt.svc == nil || !rt.svc.IsLeader() {
		http.Error(w, "not leader", http.StatusServiceUnavailable)
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/proxy-writers/")
	parts := strings.Split(tail, "/")
	if len(parts) < 1 || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	cfg, ok := rt.proxyWriters[parts[0]]
	if !ok {
		http.NotFound(w, r)
		return
	}
	coord := cfg.coordinator(rt.svc.Store())
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if action == "switch" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !bearerMatches(r, cfg.AdminTokenFile) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Writer string `json:"writer"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || !slices.Contains(cfg.Candidates, body.Writer) {
			http.Error(w, "invalid writer", http.StatusBadRequest)
			return
		}
		if err := (manager.AuditStore{Store: rt.svc.Store()}).Record(authz.AuditRecord{
			Actor:  authz.Subject{ID: "proxy-writer-admin", Kind: authz.SubjectAutomation},
			Action: authz.ActionUpdate, Resource: authz.Resource{Type: authz.ResourceCluster, ID: cfg.ClusterID},
			Outcome: authz.OutcomeAllowed, Change: "request writer switch to " + body.Writer,
		}); err != nil {
			http.Error(w, "audit unavailable", http.StatusServiceUnavailable)
			return
		}
		choice, err := coord.Switch(body.Writer)
		if errors.Is(err, manager.ErrWriterSwitchPending) {
			http.Error(w, "switch pending", http.StatusConflict)
			return
		}
		respondWriterChoice(w, choice, err)
		return
	}
	id := r.Header.Get("X-Proxy-ID")
	member := false
	for _, m := range cfg.Members {
		if m.ID == id && bearerMatches(r, m.TokenFile) {
			member = true
			break
		}
	}
	if !member {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch action {
	case "":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		choice, err := coord.Current()
		respondWriterChoice(w, choice, err)
	case "ack":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ProxyID string `json:"proxy_id"`
			Epoch   uint64 `json:"epoch"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.ProxyID != id || body.Epoch == 0 {
			http.Error(w, "invalid acknowledgement", http.StatusBadRequest)
			return
		}
		choice, err := coord.Ack(id, body.Epoch)
		if errors.Is(err, manager.ErrConflict) {
			http.Error(w, "stale acknowledgement", http.StatusConflict)
			return
		}
		respondWriterChoice(w, choice, err)
	default:
		http.NotFound(w, r)
	}
}

func respondWriterChoice(w http.ResponseWriter, c manager.WriterChoice, err error) {
	if err != nil {
		http.Error(w, "writer choice unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(c)
}
