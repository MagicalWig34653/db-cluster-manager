// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// executedBy lists, per step that an agent answered, the runtime that
// executed it and the host whose agent identity answered (ADR 0024).
func executedBy(v runner.View) []api.OperationExecution {
	var out []api.OperationExecution
	for _, s := range v.Steps {
		if s.Runtime == "" || s.Target == "" {
			continue
		}
		out = append(out, api.OperationExecution{Step: s.ID, Command: string(s.Command), Host: s.Target,
			Runtime: string(s.Runtime), Agent: "agent/" + s.Target})
	}
	return out
}

// auditExecutions writes one audit record per executed step naming the
// runtime and the agent identity. Best effort: the operation already ended.
func (s *Service) auditExecutions(rec OperationRecord, v runner.View) {
	for _, e := range executedBy(v) {
		r := authz.AuditRecord{
			Time:     s.cfg.Now(),
			Actor:    authz.Subject{ID: "manager", Kind: authz.SubjectSystem},
			Action:   authz.ActionOperate,
			Resource: authz.Resource{Type: authz.ResourceHost, Scope: authz.Global, ID: e.Host},
			Outcome:  authz.OutcomeSucceeded,
			Reason:   authz.ReasonGranted,
			Scope:    authz.Global,
			Change:   fmt.Sprintf("operation %s step %s (%s) executed by runtime=%s agent=%s", rec.ID, e.Step, e.Command, e.Runtime, e.Agent),
		}
		_, _ = Put(s.store, CollAudit, timeKey(time.Now()), r, IfVersion(0))
	}
}
