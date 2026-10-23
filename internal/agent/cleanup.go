// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// Cleanup commands: cluster deletion and inspection teardown remove the
// objects an installation created. Every removal passes the ownership guard
// (allow-list and installation label); objects the agent does not own are
// refused, never removed.

// Cleanup command kinds.
const (
	// KindRemoveClusterService removes the database service of a cluster by
	// name (args: RemoveClusterServiceArgs). The service must be owned and
	// carry the cluster's dbcm.cluster label; jobs are refused (they are
	// removed by remove-job). A missing service counts as removed, so a
	// deletion after a failed create needs no recorded service ID.
	KindRemoveClusterService Kind = "remove-cluster-service"
	// KindRemoveSecret removes an owned Swarm secret by name or ID (args:
	// RemoveSecretArgs). A missing secret counts as removed.
	KindRemoveSecret Kind = "remove-secret"
	// KindCheckRestoreTarget checks a physical restore target on this host
	// without changing it (args: CheckRestoreTargetArgs): the volume is
	// owned and bound to its root, carries the expected identity and its
	// data directory is empty. It precedes a delegated run-restore, whose
	// agent (on a Swarm manager node) cannot see this host's files.
	KindCheckRestoreTarget Kind = "check-restore-target"
)

// RemoveClusterServiceArgs are the arguments of KindRemoveClusterService.
type RemoveClusterServiceArgs struct {
	ServiceName string `json:"service_name"`
	ClusterID   string `json:"cluster_id"`
}

// RemoveSecretArgs are the arguments of KindRemoveSecret.
type RemoveSecretArgs struct {
	Secret string `json:"secret"`
}

// CheckRestoreTargetArgs are the arguments of KindCheckRestoreTarget.
type CheckRestoreTargetArgs struct {
	TargetVolume     string   `json:"target_volume"`
	TargetVolumeRoot string   `json:"target_volume_root"`
	Expected         Identity `json:"expected"`
}

// CodeSecretInUse: Docker refused to remove a secret a service still uses.
const CodeSecretInUse Code = "secret-in-use"

func (e *Executor) removeClusterService(ctx context.Context, a RemoveClusterServiceArgs) Result {
	objs, err := e.docker.ListByLabel(ctx, KindService, LabelInstallation, e.cfg.InstallationID)
	if err != nil {
		return outcome(err)
	}
	for _, o := range objs {
		if o.Name != a.ServiceName {
			continue
		}
		if err := e.owner.Check(o, KindService); err != nil {
			return outcome(err)
		}
		if o.Labels[LabelJob] != "" {
			return Result{Status: StatusRefused, Code: CodeNotOwned,
				Message: fmt.Sprintf("service %q is a job, not a database service", a.ServiceName)}
		}
		if got := o.Labels[LabelCluster]; got != a.ClusterID {
			return Result{Status: StatusRefused, Code: CodeNotOwned,
				Message: fmt.Sprintf("service %q has %s=%q, want %q", a.ServiceName, LabelCluster, got, a.ClusterID)}
		}
		res := e.removeService(ctx, o.ID)
		if res.Status == StatusSucceeded {
			res = succeeded(map[string]string{"removed": o.ID})
		}
		return res
	}
	return succeeded(map[string]string{"removed": "absent"})
}

func (e *Executor) removeSecret(ctx context.Context, a RemoveSecretArgs) Result {
	obj, err := e.docker.Inspect(ctx, KindSecret, a.Secret)
	if errors.Is(err, ErrObjectNotFound) {
		return succeeded(map[string]string{"removed": "absent"})
	}
	if err != nil {
		return outcome(err)
	}
	if err := e.owner.Check(obj, KindSecret); err != nil {
		return outcome(err)
	}
	if err := e.docker.RemoveSecret(ctx, obj.ID); err != nil {
		if errors.Is(err, ErrConflict) {
			return Result{Status: StatusFailed, Code: CodeSecretInUse, Message: err.Error()}
		}
		return outcome(err)
	}
	e.forget(obj.ID)
	return succeeded(map[string]string{"removed": obj.ID})
}

func (e *Executor) checkRestoreTarget(ctx context.Context, a CheckRestoreTargetArgs) Result {
	root, res, ok := e.ownedVolumeAt(ctx, a.TargetVolume, a.TargetVolumeRoot)
	if !ok {
		return res
	}
	if err := Verify(root, a.Expected); err != nil {
		return outcome(err)
	}
	if err := emptyOrMissing(filepath.Join(root, "data")); err != nil {
		return outcome(err)
	}
	return succeeded(nil)
}
