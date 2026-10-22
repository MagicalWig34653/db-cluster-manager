// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"fmt"
	"time"
)

// Sample is one data point: timestamp in milliseconds since the Unix epoch.
type Sample struct {
	T int64
	V float64
}

// Entry is one sample for one series in an ingestion batch.
type Entry struct {
	Labels Labels
	Sample Sample
}

// Authorizer decides whether the caller may see a series. It is applied to the
// label set before any sample of the series is read.
type Authorizer func(series Labels) bool

// AllowAll is an Authorizer for trusted internal callers.
func AllowAll(Labels) bool { return true }

// Options configures a Store. Start from DefaultOptions; zero values are
// invalid for the numeric limits so that no limit is silently disabled.
type Options struct {
	Dir string

	// BlockDuration is the width of one sealed block; it must be a multiple of
	// every rollup resolution.
	BlockDuration time.Duration
	// OutOfOrderWindow: samples older than (newest sample - window) are
	// rejected with ErrTooOld; within the window they are inserted in order.
	OutOfOrderWindow time.Duration
	// MaxFutureSkew rejects samples further ahead of the clock.
	MaxFutureSkew time.Duration
	Resolutions   []time.Duration

	RawRetention time.Duration // detailed values, default 7 days
	AggRetention time.Duration // aggregated values, default 90 days
	MaxBytes     int64         // disk budget for blocks plus WAL

	MaxSeries          int // active (head) series
	MaxValuesPerLabel  int // distinct values per label name among active series
	MaxLabelsPerSeries int
	MaxLabelNameLen    int
	MaxLabelValueLen   int
	MaxBatch           int
	MaxHeadSamples     int

	QueryTimeout         time.Duration
	MaxQuerySamples      int
	MaxQuerySteps        int
	MaxConcurrentQueries int

	SyncWAL bool

	Archive              Archive
	ArchiveCacheBytes    int64
	MaxArchiveBlockBytes int64

	Now func() time.Time
}

// DefaultOptions returns conservative defaults (U05: 7 days detailed, 90 days
// aggregated).
func DefaultOptions(dir ...string) Options {
	o := Options{
		BlockDuration:        2 * time.Hour,
		OutOfOrderWindow:     5 * time.Minute,
		MaxFutureSkew:        5 * time.Minute,
		Resolutions:          []time.Duration{time.Minute, 15 * time.Minute, time.Hour},
		RawRetention:         7 * 24 * time.Hour,
		AggRetention:         90 * 24 * time.Hour,
		MaxBytes:             2 << 30,
		MaxSeries:            100000,
		MaxValuesPerLabel:    10000,
		MaxLabelsPerSeries:   32,
		MaxLabelNameLen:      64,
		MaxLabelValueLen:     256,
		MaxBatch:             10000,
		MaxHeadSamples:       5000000,
		QueryTimeout:         30 * time.Second,
		MaxQuerySamples:      5000000,
		MaxQuerySteps:        11000,
		MaxConcurrentQueries: 4,
		SyncWAL:              true,
		ArchiveCacheBytes:    64 << 20,
		MaxArchiveBlockBytes: 256 << 20,
		Now:                  time.Now,
	}
	if len(dir) > 0 {
		o.Dir = dir[0]
	}
	return o
}

func (o *Options) validate() error {
	bad := func(f string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidOptions, fmt.Sprintf(f, a...))
	}
	if o.Dir == "" {
		return bad("Dir is empty")
	}
	if o.BlockDuration < time.Minute || o.BlockDuration%time.Millisecond != 0 {
		return bad("BlockDuration must be at least 1m")
	}
	if len(o.Resolutions) == 0 {
		return bad("at least one rollup resolution is required")
	}
	for i, r := range o.Resolutions {
		if r < time.Second || o.BlockDuration%r != 0 {
			return bad("BlockDuration must be a multiple of resolution %v", r)
		}
		if i > 0 && r <= o.Resolutions[i-1] {
			return bad("resolutions must be strictly ascending")
		}
	}
	if o.OutOfOrderWindow < 0 || o.MaxFutureSkew < 0 {
		return bad("negative window")
	}
	if o.RawRetention <= 0 || o.AggRetention < o.RawRetention {
		return bad("need 0 < RawRetention <= AggRetention")
	}
	ints := map[string]int64{
		"MaxBytes": o.MaxBytes, "MaxSeries": int64(o.MaxSeries), "MaxValuesPerLabel": int64(o.MaxValuesPerLabel),
		"MaxLabelsPerSeries": int64(o.MaxLabelsPerSeries), "MaxLabelNameLen": int64(o.MaxLabelNameLen),
		"MaxLabelValueLen": int64(o.MaxLabelValueLen), "MaxBatch": int64(o.MaxBatch),
		"MaxHeadSamples": int64(o.MaxHeadSamples), "MaxQuerySamples": int64(o.MaxQuerySamples),
		"MaxQuerySteps": int64(o.MaxQuerySteps), "MaxConcurrentQueries": int64(o.MaxConcurrentQueries),
		"QueryTimeout": int64(o.QueryTimeout),
	}
	for k, v := range ints {
		if v <= 0 {
			return bad("%s must be positive", k)
		}
	}
	if o.Archive != nil && (o.ArchiveCacheBytes <= 0 || o.MaxArchiveBlockBytes <= 0) {
		return bad("archive cache and block limits must be positive")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return nil
}
