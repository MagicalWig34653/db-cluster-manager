// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imageupdate"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// imageUpdateConfig is the "manager.imageUpdate" section (X16, ADR 0025).
// The image update service is off unless "enabled" is true. Enabled, it
// still does nothing for a service until an administrator gives that
// service a notify or auto policy; every service starts manual.
type imageUpdateConfig struct {
	Enabled bool `json:"enabled"`
	// PollIntervalSeconds is how often the leader looks for services whose
	// own check interval elapsed (default 300, minimum 60).
	PollIntervalSeconds int `json:"pollIntervalSeconds"`
}

const defaultImageUpdateInterval = 300

func (c *imageUpdateConfig) on() bool { return c != nil && c.Enabled }

func (c *imageUpdateConfig) interval() time.Duration {
	if c.PollIntervalSeconds > 0 {
		return time.Duration(c.PollIntervalSeconds) * time.Second
	}
	return defaultImageUpdateInterval * time.Second
}

func (c *imageUpdateConfig) validate(authEnabled bool) error {
	if !c.on() {
		return nil
	}
	if c.PollIntervalSeconds != 0 && c.PollIntervalSeconds < 60 {
		return errors.New("manager.imageUpdate.pollIntervalSeconds must be at least 60")
	}
	if !authEnabled {
		return errors.New("manager.imageUpdate needs auth.enabled")
	}
	return nil
}

// buildLookup resolves finished image build jobs for update offers.
type buildLookup struct{ Jobs imagebuild.JobStore }

// Built implements api.ImageUpdateBuilds.
func (b buildLookup) Built(ctx context.Context, jobID, findingID string) (imageupdate.Built, error) {
	j, err := b.Jobs.Get(ctx, jobID)
	if err != nil {
		if errors.Is(err, imagebuild.ErrJobNotFound) {
			return imageupdate.Built{}, api.ErrNotFound
		}
		return imageupdate.Built{}, err
	}
	return imageupdate.BuiltFromJob(j, findingID)
}

var _ api.ImageUpdateBuilds = buildLookup{}

// newImageUpdateService builds the service of one leadership on the
// replicated metastore. Registry credentials come from the sealed registry
// store; updates go through the managed service update operation.
func (rt *managedRuntime) newImageUpdateService(svcs *manager.Services, regs imageupdate.RegistryOpener) *imageupdate.Service {
	store := rt.svc.Store()
	ad := &manager.ImageUpdateAdapter{Svc: svcs}
	return &imageupdate.Service{
		KV: vuln.MetastoreKV{Node: store.Node()}, Journal: opjournal.New(), Source: ad, Driver: ad,
		Resolver: &imageupdate.RegistryResolver{Registries: regs},
		// The installation is read from the replicated settings on each use.
		InstallationFunc: ad.InstallationID,
		Term: func() (uint64, error) {
			l := rt.svc.Current()
			if l == nil || !l.Valid() {
				return 0, manager.ErrNotLeader
			}
			return l.Term, nil
		},
		Notify: func(ctx context.Context, e notify.Event) {
			if rt.monitoring != nil {
				rt.monitoring.Notify(ctx, e)
			}
		},
	}
}

// pollImageUpdate is the scheduler's run of the current leadership's service.
func (rt *managedRuntime) pollImageUpdate(ctx context.Context) error {
	svc := rt.imageUpdate.Load()
	if svc == nil || !rt.on("image-update") {
		return nil
	}
	_, err := svc.RunDue(ctx)
	return err
}
