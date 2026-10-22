// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
)

// ContainerStats is one container's resource snapshot (Docker stats, already
// converted to ratios and absolute counters). Swarm and Cluster come from the
// manager's ownership labels, not from the container's free-form labels.
type ContainerStats struct {
	Swarm, Cluster, Node, Host, Container string
	CPURatio                              float64 // fraction of one core
	MemUsed, MemLimit                     float64 // MemLimit 0 = unlimited
	NetRx, NetTx                          float64
	BlockRead, BlockWrite                 float64
}

// HostStats is one host's snapshot. DiskTotal 0 means unknown.
type HostStats struct {
	Swarm, Host         string
	CPURatio, MemRatio  float64
	DiskUsed, DiskTotal float64
}

// Snapshot is what a StatsSource returns.
type Snapshot struct {
	ObservedAt time.Time
	Containers []ContainerStats
	Hosts      []HostStats
}

// StatsSource returns Docker stats of the managed containers and hosts.
type StatsSource interface {
	Stats(ctx context.Context) (Snapshot, error)
}

// HostConfig configures a HostCollector.
type HostConfig struct {
	Source        StatsSource
	StaleAfter    time.Duration // default 1 minute
	MaxContainers int           // default 512; extra entries are dropped
	MaxHosts      int           // default 128
	MaxSamples    int           // default 20000
}

// HostCollector maps Docker stats to the catalog.
type HostCollector struct{ cfg HostConfig }

// NewHostCollector validates cfg.
func NewHostCollector(cfg HostConfig) (*HostCollector, error) {
	if cfg.Source == nil {
		return nil, errors.New("collect: host collector needs a Source")
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = time.Minute
	}
	if cfg.MaxContainers <= 0 {
		cfg.MaxContainers = 512
	}
	if cfg.MaxHosts <= 0 {
		cfg.MaxHosts = 128
	}
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = 20000
	}
	return &HostCollector{cfg: cfg}, nil
}

// Name implements Collector.
func (h *HostCollector) Name() string { return "host" }

// Collect implements Collector. A failed or stale snapshot yields no value
// samples and a single dcm_target_up=0 for the collector.
func (h *HostCollector) Collect(ctx context.Context, now time.Time) ([]metrics.Entry, error) {
	b := newBuilder(now, h.cfg.MaxSamples)
	snap, err := h.cfg.Source.Stats(ctx)
	if err != nil {
		b.add(MTargetUp, 0, LabelCollector, "host")
		return b.entries, fmt.Errorf("docker stats: %w", err)
	}
	if snap.ObservedAt.IsZero() || now.Sub(snap.ObservedAt) > h.cfg.StaleAfter {
		b.add(MTargetUp, 0, LabelCollector, "host")
		return b.entries, nil
	}
	b.add(MTargetUp, 1, LabelCollector, "host")
	var errs []error
	cs := snap.Containers
	if len(cs) > h.cfg.MaxContainers {
		errs = append(errs, fmt.Errorf("collect: %d containers exceed the limit of %d", len(cs), h.cfg.MaxContainers))
		cs = cs[:h.cfg.MaxContainers]
	}
	for _, c := range cs {
		l := []string{LabelSwarm, c.Swarm, LabelCluster, c.Cluster, LabelNode, c.Node, LabelHost, c.Host, LabelContainer, c.Container}
		b.add(MContainerCPURatio, c.CPURatio, l...)
		b.add(MContainerMemUsed, c.MemUsed, l...)
		if c.MemLimit > 0 {
			b.add(MContainerMemLimit, c.MemLimit, l...)
			b.add(MContainerMemRatio, clamp01(c.MemUsed/c.MemLimit), l...)
		}
		b.add(MContainerNetRx, c.NetRx, l...)
		b.add(MContainerNetTx, c.NetTx, l...)
		b.add(MContainerBlockRead, c.BlockRead, l...)
		b.add(MContainerBlockWrite, c.BlockWrite, l...)
	}
	hs := snap.Hosts
	if len(hs) > h.cfg.MaxHosts {
		errs = append(errs, fmt.Errorf("collect: %d hosts exceed the limit of %d", len(hs), h.cfg.MaxHosts))
		hs = hs[:h.cfg.MaxHosts]
	}
	for _, x := range hs {
		l := []string{LabelSwarm, x.Swarm, LabelHost, x.Host}
		b.add(MHostCPURatio, clamp01(x.CPURatio), l...)
		b.add(MHostMemRatio, clamp01(x.MemRatio), l...)
		if x.DiskTotal > 0 {
			b.add(MHostDiskUsed, x.DiskUsed, l...)
			b.add(MHostDiskTotal, x.DiskTotal, l...)
			b.add(MHostDiskRatio, clamp01(x.DiskUsed/x.DiskTotal), l...)
		}
	}
	if b.dropped > 0 {
		errs = append(errs, fmt.Errorf("collect: %d samples dropped", b.dropped))
	}
	return b.entries, errors.Join(errs...)
}
