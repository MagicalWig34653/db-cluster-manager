// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
)

// Collector produces samples for one observation round. now is the sample
// timestamp. A collector returns the samples it could gather plus an error
// describing what failed; partial results are still stored.
type Collector interface {
	Name() string
	Collect(ctx context.Context, now time.Time) ([]metrics.Entry, error)
}

// Target identifies a database node.
type Target struct {
	Swarm, Cluster, Node string
	// ExpectedSize is the desired cluster size; 0 means unknown, in which
	// case no size deficit is emitted.
	ExpectedSize int
}

// ErrNoObservation is returned by a source that has no data for a target.
var ErrNoObservation = errors.New("collect: no observation")

// builder accumulates entries with a per-collector sample cap.
type builder struct {
	ms      int64
	entries []metrics.Entry
	max     int
	dropped int
}

func newBuilder(now time.Time, max int) *builder { return &builder{ms: now.UnixMilli(), max: max} }

func (b *builder) add(name string, v float64, kv ...string) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return // never store a value that is not a number
	}
	if b.max > 0 && len(b.entries) >= b.max {
		b.dropped++
		return
	}
	l, err := metrics.NewLabels(name, kv...)
	if err != nil {
		b.dropped++
		return
	}
	b.entries = append(b.entries, metrics.Entry{Labels: l, Sample: metrics.Sample{T: b.ms, V: v}})
}

func boolV(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// parseF parses a status value. ok is false for absent, empty, "NULL" or
// unparsable values: those produce no sample.
func parseF(vars map[string]string, k string) (float64, bool) {
	s, present := vars[k]
	if !present || s == "" || s == "NULL" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func onOff(vars map[string]string, k string) (float64, bool) {
	switch vars[k] {
	case "ON":
		return 1, true
	case "OFF":
		return 0, true
	}
	return 0, false
}
