// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imageupdate"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// ImageUpdateAdapter connects the image update service (X16, ADR 0025) to
// the managed services: it lists the manager's own services (every one has
// the installation's ownership labels, the agent enforces them) and updates
// them through the approval-gated service update operation, so the existing
// authorization, agent update-service (Swarm UpdateConfig/RollbackConfig),
// await-service and health gate apply unchanged. Nothing here talks to
// Docker directly.
type ImageUpdateAdapter struct {
	Svc *Services
}

var (
	_ imageupdate.Source = (*ImageUpdateAdapter)(nil)
	_ imageupdate.Driver = (*ImageUpdateAdapter)(nil)
)

func (a *ImageUpdateAdapter) installation() (string, error) {
	st, err := a.Svc.Ops.settings()
	if err != nil {
		return "", err
	}
	return st.InstallationID, nil
}

// InstallationID returns the installation whose ownership label a service
// carries (used to configure the service's scope).
func (a *ImageUpdateAdapter) InstallationID() (string, error) { return a.installation() }

func specImage(spec json.RawMessage) string {
	var v struct {
		Image string `json:"image"`
	}
	if json.Unmarshal(spec, &v) != nil {
		return ""
	}
	return v.Image
}

// Services implements imageupdate.Source.
func (a *ImageUpdateAdapter) Services(ctx context.Context) ([]imageupdate.ManagedService, error) {
	inst, err := a.installation()
	if errors.Is(err, ErrSettingsIncomplete) {
		return nil, nil // nothing is in scope before the installation exists
	}
	if err != nil {
		return nil, err
	}
	infos, err := a.Svc.Services(ctx)
	if err != nil {
		return nil, err
	}
	var out []imageupdate.ManagedService
	for _, in := range infos {
		img := specImage(in.Spec)
		if img == "" || in.DeleteOperation != "" {
			continue
		}
		out = append(out, imageupdate.ManagedService{ID: in.ID, Name: in.Name, Swarm: in.Swarm, Type: in.Type, Image: img,
			Labels: map[string]string{imageupdate.LabelInstallation: inst}})
	}
	return out, nil
}

func imageUpdateSubject(actor string) authz.Subject {
	if actor == imageupdate.AutoActor {
		return collect.SystemSubject
	}
	return authz.Subject{ID: actor, Kind: authz.SubjectUser}
}

func mapImageUpdateError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, api.ErrBusy), errors.Is(err, runner.ErrBusy), errors.Is(err, runner.ErrActive):
		return fmt.Errorf("%w: %v", imageupdate.ErrBusy, err)
	}
	return err
}

func updateStrategy(s imageupdate.Strategy) api.ServiceUpdateStrategy {
	return api.ServiceUpdateStrategy{Order: s.Order, Parallelism: s.Parallelism, DelaySeconds: s.DelaySeconds,
		MonitorSeconds: s.MonitorSeconds, MaxFailureRatio: s.MaxFailureRatio, OnFailure: "rollback"}
}

// withImage returns spec with only the image replaced.
func withImage(spec json.RawMessage, image string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(spec, &m); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(image)
	m["image"] = b
	return json.Marshal(m)
}

// StartUpdate implements imageupdate.Driver. The operation waits for an
// approval bound to its digest (Approve).
func (a *ImageUpdateAdapter) StartUpdate(ctx context.Context, actor, serviceID, image string, s imageupdate.Strategy) (imageupdate.Started, error) {
	info, err := a.Svc.Service(ctx, serviceID)
	if err != nil {
		return imageupdate.Started{}, err
	}
	spec, err := withImage(info.Spec, image)
	if err != nil {
		return imageupdate.Started{}, err
	}
	out, err := a.Svc.UpdateService(ctx, imageUpdateSubject(actor), serviceID, api.ServiceUpdateInput{Spec: spec, Strategy: updateStrategy(s)})
	if err != nil {
		return imageupdate.Started{}, mapImageUpdateError(err)
	}
	return imageupdate.Started{OperationID: out.Operation.ID, Digest: out.Approval.Digest}, nil
}

// StartRollback implements imageupdate.Driver: the update back to the spec
// before the last successful update.
func (a *ImageUpdateAdapter) StartRollback(ctx context.Context, actor, serviceID string, s imageupdate.Strategy) (imageupdate.Started, error) {
	out, err := a.Svc.UpdateService(ctx, imageUpdateSubject(actor), serviceID, api.ServiceUpdateInput{Rollback: true, Strategy: updateStrategy(s)})
	if err != nil {
		return imageupdate.Started{}, mapImageUpdateError(err)
	}
	return imageupdate.Started{OperationID: out.Operation.ID, Digest: out.Approval.Digest}, nil
}

// Approve implements imageupdate.Driver with the actor's own permission
// (update on the service resource; the system subject for automatic plans).
func (a *ImageUpdateAdapter) Approve(_ context.Context, actor, operationID, digest string) error {
	_, err := a.Svc.Ops.Approve(imageUpdateSubject(actor), operationID, digest)
	return mapImageUpdateError(MapCommandError(err))
}

// Status implements imageupdate.Driver.
func (a *ImageUpdateAdapter) Status(_ context.Context, operationID string) (imageupdate.OpStatus, error) {
	rec, ok, err := Get[OperationRecord](a.Svc.Ops.Svc.store, CollOperations, operationID, Consistent())
	if err != nil {
		return imageupdate.OpStatus{}, err
	}
	if !ok {
		return imageupdate.OpStatus{}, fmt.Errorf("%w: operation %q", imageupdate.ErrNotFound, operationID)
	}
	return opStatusOf(rec.Value), nil
}

// opStatusOf derives the update state from an operation record. A failed
// await-service with Swarm's "rollback_completed" means the health or
// monitor check failed and Swarm restored the previous spec (the
// RollbackConfig the plan set); any other failed end is a failure. A paused
// operation (health gate, retries) is still running for this purpose; the
// service's wait limit ends it.
func opStatusOf(rec OperationRecord) imageupdate.OpStatus {
	v := rec.View
	if v == nil {
		return imageupdate.OpStatus{State: imageupdate.OpRunning}
	}
	st := imageupdate.OpStatus{State: imageupdate.OpRunning, Detail: v.Reason}
	for _, s := range v.Steps {
		if s.Command == agent.KindUpdateService && s.Runtime != "" {
			st.Runtime = s.Runtime
		}
	}
	if st.Runtime == "" {
		for _, s := range v.Steps {
			if s.Runtime != "" {
				st.Runtime = agentrt.Runtime(s.Runtime)
				break
			}
		}
	}
	switch v.Status {
	case runner.StatusSucceeded:
		st.State = imageupdate.OpSucceeded
	case runner.StatusFailed, runner.StatusAborted, runner.StatusStopped:
		st.State = imageupdate.OpFailed
		for _, s := range v.Steps {
			if s.Command != agent.KindAwaitService || s.LastResult == nil {
				continue
			}
			if strings.Contains(s.LastResult.Message, "rollback_completed") {
				st.State, st.Detail = imageupdate.OpRolledBack, s.LastResult.Message
			} else if s.LastResult.Message != "" && st.Detail == "" {
				st.Detail = s.LastResult.Message
			}
		}
	}
	return st
}

// ImageUpdateScheduler runs the image update checks while this replica
// leads. Poll is the current leadership's service run; it returns nil when
// the leadership has none.
type ImageUpdateScheduler struct {
	Poll func(ctx context.Context) error
	// Interval defaults to five minutes. A service is only checked when its
	// own policy interval elapsed.
	Interval time.Duration
	// InitialDelay is waited before the first run (default 45 s).
	InitialDelay time.Duration
	Logf         func(msg string, args ...any)
}

// Name implements Reconciler.
func (ImageUpdateScheduler) Name() string { return "image-update-scheduler" }

// Run implements Reconciler.
func (s ImageUpdateScheduler) Run(ctx context.Context, _ *Leadership) error {
	iv := s.Interval
	if iv <= 0 {
		iv = 5 * time.Minute
	}
	delay := s.InitialDelay
	if delay <= 0 {
		delay = 45 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if s.Poll != nil {
			if err := s.Poll(ctx); err != nil && ctx.Err() == nil && s.Logf != nil {
				s.Logf("manager: image update run failed", "err", err)
			}
		}
		timer.Reset(iv)
	}
}
