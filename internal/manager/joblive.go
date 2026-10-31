// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// DefaultJobIdleGrace is how long a job service may have no running task
// (image pull, scheduling) before a gate that still waits for its result
// treats it as dead.
const DefaultJobIdleGrace = 10 * time.Minute

// jobWatch remembers since when a job service had no running task.
type jobWatch struct {
	mu   sync.Mutex
	idle map[string]time.Time
}

func (w *jobWatch) idleSince(key string, running bool, now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if running {
		delete(w.idle, key)
		return 0
	}
	if w.idle == nil {
		w.idle = map[string]time.Time{}
	}
	t, ok := w.idle[key]
	if !ok {
		w.idle[key] = now
		return 0
	}
	return now.Sub(t)
}

// jobNameOf resolves the job a result gate waits for. A backup gate names
// the object; its job is the run-backup step of the operation with that
// object. A restore gate names the job.
func (c *RoutingChecker) jobNameOf(rec OperationRecord, cond runner.Condition) string {
	if cond.Kind == dbbackup.CondRestoreComplete {
		return cond.Target
	}
	for _, st := range rec.Definition.Steps {
		if st.Command != agent.KindRunBackup {
			continue
		}
		var a agent.RunBackupArgs
		if json.Unmarshal(st.Args, &a) == nil && a.Object == cond.Target {
			return a.JobName
		}
	}
	return ""
}

// jobDead reports why the job a result gate waits for can no longer
// deliver: its Swarm service is gone, or it has had no running task for
// longer than the grace without reporting a result (crash, image that
// cannot be pulled, node lost). Anything not observable counts as alive.
func (c *RoutingChecker) jobDead(ctx context.Context, opID string, cond runner.Condition) (string, bool) {
	if c.Ops == nil || c.Ops.Swarms == nil || c.Ops.Swarms.Dial == nil {
		return "", false
	}
	rec, ok, err := Get[OperationRecord](c.Store, CollOperations, opID)
	if err != nil || !ok {
		return "", false
	}
	job := c.jobNameOf(rec.Value, cond)
	if job == "" {
		return "", false
	}
	sw, ok, err := Get[SwarmRecord](c.Store, CollSwarms, rec.Value.Swarm)
	if err != nil || !ok {
		return "", false
	}
	conn, err := c.Ops.Swarms.Dial(sw.Value.DockerHost)
	if err != nil {
		return "", false
	}
	var found *agent.Object
	for _, kind := range []string{agent.JobBackup, agent.JobRestore} {
		objs, err := conn.ListByLabel(ctx, agent.KindService, agent.LabelJob, kind)
		if err != nil {
			return "", false
		}
		for i := range objs {
			if objs[i].Name == job {
				found = &objs[i]
			}
		}
	}
	if found == nil {
		return fmt.Sprintf("job service %s no longer exists and reported no result", job), true
	}
	if found.RunningTasks == nil {
		return "", false
	}
	grace := c.JobIdleGrace
	if grace <= 0 {
		grace = DefaultJobIdleGrace
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	idle := c.jobs.idleSince(opID+"/"+job, *found.RunningTasks > 0, now)
	if idle > grace {
		return fmt.Sprintf("job service %s has had no running task for %s and reported no result (it crashed, its image cannot be pulled or its node is gone); inspect with docker service ps %s", job, idle.Round(time.Second), job), true
	}
	return "", false
}
