// SPDX-License-Identifier: AGPL-3.0-only

// Package api implements the versioned HTTP API of the manager.
//
// The contract is described in api/openapi.yaml. Only endpoints that report
// real process state exist; no cluster operations are implemented yet.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

// Health is the liveness response. It reports only that the process serves requests.
type Health struct {
	Status string `json:"status"`
}

// NewHandler returns the API handler, rooted at /api/v1.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Health{Status: "ok"})
	})
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, version.Get("manager"))
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
