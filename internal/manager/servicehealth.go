// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// Service health (W07; fakes only).
//
// The health of a managed service comes from the Swarm task state of its
// owned service: the Swarm connector lists the services carrying the
// service's ownership label with their running/desired task counts and
// update state (the same read the drift check uses), and the service type
// judges that observation (service.Type.Health). An unknown observation
// (Swarm unreachable, service not found) is "unknown", never healthy.
// ServiceHealthCollector stores the verdicts in the service records and
// hands transitions between healthy and not healthy to Notify (the
// monitoring dispatcher: UI inbox and configured channels); the API shows
// the verdict on GET /services. The update plans' health gate
// (runner.CondServiceHealthy) evaluates the same verdict live, not cached.

// ServiceObservation is the stored health of a service.
type ServiceObservation struct {
	State       service.HealthState `json:"state"`
	Reasons     []string            `json:"reasons,omitempty"`
	Running     int                 `json:"running"`
	Desired     int                 `json:"desired"`
	UpdateState string              `json:"updateState,omitempty"`
	ObservedAt  time.Time           `json:"observedAt"`
}

// ServiceHealthMaxAge is how long an observation counts; older ones are
// reported as unknown.
const ServiceHealthMaxAge = 2 * time.Minute

// Notification kinds of service health transitions.
const (
	KindServiceUnhealthy = "health.service_unhealthy"
	KindServiceRecovered = "health.service_recovered"
)

func apiServiceHealth(o *ServiceObservation, now time.Time) *api.ServiceHealth {
	if o == nil {
		return &api.ServiceHealth{State: string(service.HealthUnknown), Reasons: []string{"not observed yet"}}
	}
	h := &api.ServiceHealth{State: string(o.State), Reasons: o.Reasons, Running: o.Running, Desired: o.Desired,
		UpdateState: o.UpdateState, ObservedAt: o.ObservedAt}
	if now.Sub(o.ObservedAt) > ServiceHealthMaxAge {
		h.State = string(service.HealthUnknown)
		h.Reasons = append([]string{"observation is stale"}, h.Reasons...)
	}
	return h
}

// ObservationFromSwarm builds the type's input from the owned Swarm
// services listed for a service ID. Only services whose ownership label is
// the service ID count.
func ObservationFromSwarm(serviceID string, objs []agent.Object) (service.Observation, string) {
	var o service.Observation
	var update string
	for _, x := range objs {
		if x.Labels[agent.LabelCluster] != serviceID {
			continue
		}
		if x.RunningTasks == nil {
			return service.Observation{Reason: "the Swarm reported no task status"}, x.UpdateState
		}
		o.Known = true
		o.Desired += int(x.Replicas)
		o.Running += int(*x.RunningTasks)
		update = x.UpdateState
	}
	if !o.Known {
		o.Reason = "no owned Swarm service found"
	}
	switch update {
	case "paused", "rollback_started", "rollback_paused", "rollback_completed":
		o.Reason = "Swarm update state " + update
	}
	return o, update
}

// judgeService observes a service now and returns the type's verdict.
func (o *Operations) judgeService(ctx context.Context, r ServiceRecord, reg *service.Registry) (ServiceObservation, error) {
	now := o.Svc.cfg.Now()
	if o.Swarms == nil || o.Swarms.Dial == nil {
		return ServiceObservation{}, errNoDial
	}
	sw, ok, err := Get[SwarmRecord](o.Svc.store, CollSwarms, r.Swarm)
	if err != nil {
		return ServiceObservation{}, err
	}
	if !ok {
		return ServiceObservation{}, fmt.Errorf("%w: swarm %q", api.ErrNotFound, r.Swarm)
	}
	conn, err := o.Swarms.Dial(sw.Value.DockerHost)
	if err != nil {
		return ServiceObservation{}, err
	}
	objs, err := conn.ListByLabel(ctx, agent.KindService, agent.LabelCluster, r.ID)
	if err != nil {
		return ServiceObservation{}, err
	}
	obs, update := ObservationFromSwarm(r.ID, objs)
	ty, err := reg.Get(r.Type)
	if err != nil {
		return ServiceObservation{}, err
	}
	h, err := ty.Health(service.HealthInput{Context: service.Context{ServiceID: r.ID, Name: r.Name}, Spec: r.Spec, Observed: obs})
	if err != nil {
		return ServiceObservation{}, err
	}
	if h.State == service.HealthHealthy && obs.Reason != "" {
		// Every task runs, but Swarm reports a paused or rolled-back update.
		h = service.Health{State: service.HealthDegraded, Reasons: []string{obs.Reason}}
	}
	return ServiceObservation{State: h.State, Reasons: h.Reasons, Running: obs.Running, Desired: obs.Desired,
		UpdateState: update, ObservedAt: now}, nil
}

// serviceHealthy evaluates runner.CondServiceHealthy live.
func (c *RoutingChecker) serviceHealthy(ctx context.Context, serviceID string) (bool, string, error) {
	if c.Ops == nil {
		return false, "no operations for service checks", nil
	}
	r, ok, err := Get[ServiceRecord](c.Store, CollServices, serviceID)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "service " + serviceID + " not found", nil
	}
	reg := c.Registry
	if reg == nil {
		reg = service.DefaultRegistry()
	}
	obs, err := c.Ops.judgeService(ctx, r.Value, reg)
	if err != nil {
		return false, "service health not observable: " + err.Error(), nil
	}
	if obs.State != service.HealthHealthy {
		return false, fmt.Sprintf("service is %s (%d of %d tasks run) %v", obs.State, obs.Running, obs.Desired, obs.Reasons), nil
	}
	return true, "", nil
}

// ServiceHealthCollector observes every managed service while this
// replica leads.
type ServiceHealthCollector struct {
	Swarms   *Swarms
	Registry *service.Registry
	Interval time.Duration // default 30s
	// Notify receives transitions (typically Monitoring.Notify).
	Notify func(ctx context.Context, e notify.Event) bool
}

// Name implements Reconciler.
func (ServiceHealthCollector) Name() string { return "service-health-collector" }

// Run implements Reconciler.
func (h ServiceHealthCollector) Run(ctx context.Context, l *Leadership) error {
	iv := h.Interval
	if iv <= 0 {
		iv = 30 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := h.CollectOnce(ctx, l); err != nil && ctx.Err() == nil {
			l.svc.log.Info("manager: service health collection incomplete", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// CollectOnce observes every service once and stores the verdicts. A
// service that cannot be observed is stored as unknown with the reason.
func (h ServiceHealthCollector) CollectOnce(ctx context.Context, l *Leadership) error {
	reg := h.Registry
	if reg == nil {
		reg = service.DefaultRegistry()
	}
	ops := &Operations{Svc: l.svc}
	if h.Swarms != nil {
		sw := *h.Swarms
		sw.Svc = l.svc
		ops.Swarms = &sw
	}
	recs, err := List[ServiceRecord](l.svc.store, CollServices)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range recs {
		obs, err := ops.judgeService(ctx, r.Value, reg)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			obs = ServiceObservation{State: service.HealthUnknown, Reasons: []string{"not observable: " + err.Error()}, ObservedAt: l.svc.cfg.Now()}
		}
		var prev *ServiceObservation
		var rec ServiceRecord
		_, _, err = Update(l.svc.store, CollServices, r.Key, func(cur ServiceRecord, exists bool) (ServiceRecord, error) {
			if !exists {
				return cur, ErrNotFound
			}
			prev = cur.Observed
			o := obs
			cur.Observed = &o
			rec = cur
			return cur, nil
		})
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("service %s: %w", r.Key, err))
			continue
		}
		h.report(ctx, l, rec, prev, obs)
	}
	return errors.Join(errs...)
}

// report records and notifies a change between healthy and not healthy.
// The first observation is no transition; nothing is reported while the
// service is being created, updated or deleted (expected task churn).
func (h ServiceHealthCollector) report(ctx context.Context, l *Leadership, r ServiceRecord, prev *ServiceObservation, next ServiceObservation) {
	if prev == nil {
		return
	}
	was, is := prev.State == service.HealthHealthy, next.State == service.HealthHealthy
	if was == is || r.DeleteOp != "" || r.UpdateOp != "" {
		return
	}
	if op, ok, err := Get[OperationRecord](l.svc.store, CollOperations, r.CreateOp); err == nil && ok && op.Value.EndedAt.IsZero() {
		return
	}
	sev, nsev, kind := api.SeverityWarning, notify.SeverityWarning, KindServiceUnhealthy
	msg := fmt.Sprintf("service %s became %s (%d of %d tasks run)", r.Name, next.State, next.Running, next.Desired)
	if len(next.Reasons) > 0 {
		msg += ": " + next.Reasons[0]
	}
	if is {
		sev, nsev, kind = api.SeverityInfo, notify.SeverityInfo, KindServiceRecovered
		msg = fmt.Sprintf("service %s is healthy again", r.Name)
	}
	l.svc.recordEvent(r.Swarm, "", sev, msg)
	if h.Notify == nil {
		return
	}
	h.Notify(ctx, notify.Event{Time: l.svc.cfg.Now(), Severity: nsev, Kind: kind,
		Resource: "swarm/" + r.Swarm + "/service/" + r.ID, Message: msg,
		Details: map[string]string{"swarm": r.Swarm, "service": r.ID, "type": r.Type, "state": string(next.State)}})
}
