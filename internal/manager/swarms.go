// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

// Swarm connection, inventory and agent enrollment in the replicated
// store. Several independent Swarms can be connected (P06); each has its
// own record and its own host records. Writes happen on the leader only.

// Swarms manages connected Swarms.
type Swarms struct {
	Svc        *Service
	Authorizer *authz.Authorizer
	Matrix     swarm.Matrix
	// Dial opens a Connector for a Swarm record's Docker host.
	Dial func(dockerHost string) (swarm.Connector, error)
}

var errNoDial = errors.New("manager: no swarm connector configured")

func (s *Swarms) authorize(actor authz.Subject, act authz.Action, r authz.Resource) error {
	if d := s.Authorizer.Authorize(actor, act, r); !d.Allowed {
		return &authz.DeniedError{Decision: d}
	}
	return nil
}

// Connect records a new Swarm connection. Nothing is contacted yet; Sync
// observes it.
func (s *Swarms) Connect(actor authz.Subject, name, dockerHost string) (SwarmRecord, error) {
	if name == "" || dockerHost == "" {
		return SwarmRecord{}, errors.New("manager: swarm name and docker host required")
	}
	if err := s.authorize(actor, authz.ActionCreate, authz.Resource{Type: authz.ResourceSwarm, Scope: authz.Global}); err != nil {
		return SwarmRecord{}, err
	}
	rec := SwarmRecord{ID: newID("swarm"), Name: name, DockerHost: dockerHost, CreatedAt: s.Svc.cfg.Now()}
	if _, err := Put(s.Svc.store, CollSwarms, rec.ID, rec, IfVersion(0)); err != nil {
		return SwarmRecord{}, err
	}
	return rec, nil
}

// Sync observes a Swarm and updates its record and host records. Enrollment
// state and agent endpoints of known hosts are kept; new nodes are added as
// discovered. A failed observation marks the Swarm unavailable.
func (s *Swarms) Sync(ctx context.Context, swarmID string) error {
	if s.Dial == nil {
		return errNoDial
	}
	cur, ok, err := Get[SwarmRecord](s.Svc.store, CollSwarms, swarmID, Consistent())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: swarm %q", api.ErrNotFound, swarmID)
	}
	now := s.Svc.cfg.Now()
	conn, err := s.Dial(cur.Value.DockerHost)
	var inv swarm.Inventory
	if err == nil {
		inv, err = swarm.Observe(ctx, conn, s.Matrix)
	}
	if err != nil {
		_, _, uerr := Update(s.Svc.store, CollSwarms, swarmID, func(r SwarmRecord, exists bool) (SwarmRecord, error) {
			if !exists {
				return r, ErrNotFound
			}
			r.Observed = &Observation{State: api.StateUnavailable, At: now, Detail: err.Error()}
			return r, nil
		})
		return errors.Join(err, uerr)
	}
	var ops []metastore.DocOp
	known, err := List[HostRecord](s.Svc.store, CollHosts)
	if err != nil {
		return err
	}
	byKey := map[string]Versioned[HostRecord]{}
	for _, h := range known {
		byKey[h.Key] = h
	}
	state := api.StateHealthy
	for _, n := range inv.Nodes {
		key := HostKey(swarmID, n.ID)
		prev, exists := byKey[key]
		h := prev.Value
		if !exists {
			h = HostRecord{Swarm: swarmID, NodeID: n.ID, Enrollment: EnrollDiscovered}
		}
		h.Hostname, h.Role, h.Availability, h.State = n.Hostname, n.Role, n.Availability, n.State
		h.EngineVersion, h.Arch, h.Labels = n.EngineVersion, n.Arch, maps.Clone(n.Labels)
		h.NanoCPUs, h.MemoryBytes, h.ObservedAt = n.NanoCPUs, n.MemoryBytes, now
		if h.Enrollment == EnrollEnrolled && n.State != "ready" {
			state = api.StateDegraded
		}
		op, err := PutOp(CollHosts, key, h, IfVersion(prev.Version))
		if err != nil {
			return err
		}
		ops = append(ops, op)
	}
	notes := make([]CompatibilityNote, 0, len(inv.Findings))
	for _, f := range inv.Findings {
		notes = append(notes, CompatibilityNote{NodeID: f.NodeID, Version: f.Version, Level: string(f.Level), Message: f.Message})
	}
	rec := cur.Value
	rec.Observed = &Observation{State: state, At: now}
	rec.Compatibility = notes
	op, err := PutOp(CollSwarms, swarmID, rec, IfVersion(cur.Version))
	if err != nil {
		return err
	}
	_, err = s.Svc.store.Txn(append(ops, op)...)
	return err
}

// SetEnrollment enrolls (agentURL may be empty until the agent endpoint is
// known) or revokes a discovered host.
func (s *Swarms) SetEnrollment(actor authz.Subject, swarmID, nodeID string, enroll bool, agentURL string) (HostRecord, error) {
	if err := s.authorize(actor, authz.ActionUpdate, authz.Resource{Type: authz.ResourceHost, Scope: authz.SwarmScope(swarmID), ID: nodeID}); err != nil {
		return HostRecord{}, err
	}
	h, _, err := Update(s.Svc.store, CollHosts, HostKey(swarmID, nodeID), func(h HostRecord, exists bool) (HostRecord, error) {
		if !exists {
			return h, fmt.Errorf("%w: host %s/%s (run Sync first)", api.ErrNotFound, swarmID, nodeID)
		}
		if enroll {
			h.Enrollment, h.EnrolledAt, h.AgentURL = EnrollEnrolled, s.Svc.cfg.Now(), agentURL
		} else {
			h.Enrollment, h.AgentURL = EnrollRevoked, ""
		}
		return h, nil
	})
	return h, err
}

// DeployAgents creates the agent global service of the installation in the
// Swarm (if missing) and sets or removes the enrollment labels according to
// the host records. The service ID is recorded on every enrolled host.
func (s *Swarms) DeployAgents(ctx context.Context, actor authz.Subject, swarmID string) (swarm.DeployResult, error) {
	if s.Dial == nil {
		return swarm.DeployResult{}, errNoDial
	}
	if err := s.authorize(actor, authz.ActionOperate, authz.Resource{Type: authz.ResourceSwarm, Scope: authz.SwarmScope(swarmID)}); err != nil {
		return swarm.DeployResult{}, err
	}
	sw, ok, err := Get[SwarmRecord](s.Svc.store, CollSwarms, swarmID, Consistent())
	if err != nil {
		return swarm.DeployResult{}, err
	}
	if !ok {
		return swarm.DeployResult{}, fmt.Errorf("%w: swarm %q", api.ErrNotFound, swarmID)
	}
	st, ok, err := Get[Settings](s.Svc.store, CollSettings, SettingsKey, Consistent())
	if err != nil {
		return swarm.DeployResult{}, err
	}
	if !ok || st.Value.InstallationID == "" || st.Value.VolumeBase == "" || st.Value.AgentImage == "" {
		return swarm.DeployResult{}, fmt.Errorf("%w; agentImage required too", ErrSettingsIncomplete)
	}
	hosts, err := List[HostRecord](s.Svc.store, CollHosts)
	if err != nil {
		return swarm.DeployResult{}, err
	}
	d := swarm.AgentDeployment{InstallationID: st.Value.InstallationID, Image: st.Value.AgentImage, VolumeBase: st.Value.VolumeBase}
	for _, h := range hosts {
		if h.Value.Swarm != swarmID {
			continue
		}
		switch h.Value.Enrollment {
		case EnrollEnrolled:
			d.Enrolled = append(d.Enrolled, h.Value.NodeID)
		case EnrollRevoked:
			d.Revoked = append(d.Revoked, h.Value.NodeID)
		}
	}
	conn, err := s.Dial(sw.Value.DockerHost)
	if err != nil {
		return swarm.DeployResult{}, err
	}
	res, err := swarm.DeployAgents(ctx, conn, d)
	if err != nil {
		return res, err
	}
	for _, id := range res.Labeled {
		_, _, uerr := Update(s.Svc.store, CollHosts, HostKey(swarmID, id), func(h HostRecord, exists bool) (HostRecord, error) {
			if !exists {
				return h, ErrNotFound
			}
			h.AgentServiceID = res.ServiceID
			return h, nil
		})
		err = errors.Join(err, uerr)
	}
	s.Svc.recordEvent(swarmID, "", api.SeverityInfo, fmt.Sprintf("%s deployed agents: service %s, %d enrolled, %d revoked, %d failed",
		actor.ID, res.ServiceID, len(res.Labeled), len(res.Unlabeled), len(res.Failed)))
	return res, err
}

// SyncReconciler observes every connected Swarm periodically while this
// replica leads.
type SyncReconciler struct {
	Swarms   *Swarms
	Interval time.Duration // default 30s
}

// Name implements Reconciler.
func (r SyncReconciler) Name() string { return "swarm-sync" }

// Run implements Reconciler.
func (r SyncReconciler) Run(ctx context.Context, _ *Leadership) error {
	iv := r.Interval
	if iv <= 0 {
		iv = 30 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		recs, err := List[SwarmRecord](r.Swarms.Svc.store, CollSwarms)
		if err == nil {
			for _, sw := range recs {
				if ctx.Err() != nil {
					return nil
				}
				if err := r.Swarms.Sync(ctx, sw.Key); err != nil {
					r.Swarms.Svc.log.Info("manager: swarm sync failed", "swarm", sw.Key, "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
