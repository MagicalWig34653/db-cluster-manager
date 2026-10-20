// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// maxBody bounds a POST /apply request body.
const maxBody = 1 << 20

// barrierTimeout bounds GET /state?barrier=1.
const barrierTimeout = 5 * time.Second

type stateBody struct {
	ID            string                `json:"id"`
	Leader        bool                  `json:"leader"`
	LeaderID      string                `json:"leader_id"`
	LeaderAddress string                `json:"leader_address"`
	Term          uint64                `json:"term"`
	Barrier       bool                  `json:"barrier"`
	Unfinished    []opjournal.Operation `json:"unfinished"`
}

type errorBody struct {
	Error         string `json:"error"`
	Code          string `json:"code"`
	LeaderID      string `json:"leader_id,omitempty"`
	LeaderAddress string `json:"leader_address,omitempty"`
}

func newHandler(n *metastore.Node) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "use GET", Code: "method"})
			return
		}
		barrier := r.URL.Query().Get("barrier") == "1"
		if barrier {
			if err := n.Barrier(barrierTimeout); err != nil {
				writeApplyError(w, n, err)
				return
			}
		}
		lid, laddr := n.Leader()
		unfinished := n.Journal().Unfinished()
		if unfinished == nil {
			unfinished = []opjournal.Operation{}
		}
		writeJSON(w, http.StatusOK, stateBody{
			ID: n.ID(), Leader: n.IsLeader(), LeaderID: lid, LeaderAddress: laddr,
			Term: n.Term(), Barrier: barrier, Unfinished: unfinished,
		})
	})
	mux.HandleFunc("/apply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "use POST", Code: "method"})
			return
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		dec.DisallowUnknownFields()
		var cmd metastore.Command
		if err := dec.Decode(&cmd); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
			return
		}
		if err := n.Apply(cmd); err != nil {
			writeApplyError(w, n, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "term": n.Term()})
	})
	return mux
}

// writeApplyError maps metastore and journal errors to HTTP responses:
// 503 not_leader, 504 outcome_unknown (the command may still commit),
// 409 for journal rejections and 500 otherwise.
func writeApplyError(w http.ResponseWriter, n *metastore.Node, err error) {
	body := errorBody{Error: err.Error()}
	status := http.StatusConflict
	switch {
	case errors.Is(err, metastore.ErrNotLeader):
		status, body.Code = http.StatusServiceUnavailable, "not_leader"
		body.LeaderID, body.LeaderAddress = n.Leader()
	case errors.Is(err, metastore.ErrOutcomeUnknown):
		status, body.Code = http.StatusGatewayTimeout, "outcome_unknown"
	case errors.Is(err, opjournal.ErrNotOwner):
		body.Code = "not_owner"
	case errors.Is(err, opjournal.ErrExists):
		body.Code = "exists"
	case errors.Is(err, opjournal.ErrNotFound):
		body.Code = "not_found"
	default:
		// Other journal validation errors (unknown kind, invalid step
		// transition) are deterministic rejections. Barrier failures that
		// are not leadership errors land here too.
		body.Code = "rejected"
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
