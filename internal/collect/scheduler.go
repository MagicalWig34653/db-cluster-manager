// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
)

// Sink receives samples; *metrics.Store implements it.
type Sink interface {
	Append(entries []metrics.Entry) (int, error)
}

// Job is a collector with its schedule.
type Job struct {
	Collector Collector
	Interval  time.Duration
	// Timeout bounds one run (default: Interval, at most 30s).
	Timeout time.Duration
}

// SchedulerConfig configures a Scheduler.
type SchedulerConfig struct {
	Sink Sink
	Jobs []Job
	// Now is the injected clock (default time.Now).
	Now func() time.Time
	// Sleep waits d or until ctx ends (default: real timer). Tests inject it.
	Sleep func(ctx context.Context, d time.Duration)
	// JitterFraction spreads the first run of each job deterministically
	// over [0, Interval*JitterFraction) by hashing the job name, so that
	// jobs do not all hit the nodes at the same instant (default 0.1).
	JitterFraction float64
	// BackoffBase/BackoffMax apply when the sink pushes back (disk budget,
	// full head, busy). Defaults 5s and 5m. During backoff runs are skipped
	// and counted rather than queued.
	BackoffBase, BackoffMax time.Duration
	// SelfMetrics enables dcm_collector_* samples (default via NewScheduler: on).
	DisableSelfMetrics bool
}

// Scheduler runs collectors at their interval. It is safe for concurrent use.
type Scheduler struct {
	cfg  SchedulerConfig
	mu   sync.Mutex
	jobs []*jobState
}

type jobState struct {
	Job
	next     time.Time
	running  atomic.Bool
	backoff  time.Duration
	holdTill time.Time

	duration    time.Duration
	samples     int
	errors      int64
	skipped     int64
	rejected    int64
	lastSuccess time.Time
	lastErr     string
}

// JobStats is a snapshot of a job's counters.
type JobStats struct {
	Name                      string
	LastDuration              time.Duration
	LastSamples               int
	Errors, Skipped, Rejected int64
	LastSuccess               time.Time
	LastError                 string
	Backoff                   time.Duration
}

// NewScheduler validates cfg.
func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Sink == nil {
		return nil, errors.New("collect: scheduler needs a Sink")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	if cfg.JitterFraction <= 0 || cfg.JitterFraction > 1 {
		cfg.JitterFraction = 0.1
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 5 * time.Second
	}
	if cfg.BackoffMax < cfg.BackoffBase {
		cfg.BackoffMax = 5 * time.Minute
	}
	s := &Scheduler{cfg: cfg}
	seen := map[string]bool{}
	start := cfg.Now()
	for _, j := range cfg.Jobs {
		if j.Collector == nil || j.Interval <= 0 {
			return nil, errors.New("collect: job needs a collector and a positive interval")
		}
		n := j.Collector.Name()
		if seen[n] {
			return nil, fmt.Errorf("collect: duplicate job %q", n)
		}
		seen[n] = true
		if j.Timeout <= 0 || j.Timeout > 30*time.Second {
			j.Timeout = min(j.Interval, 30*time.Second)
		}
		h := fnv.New32a()
		h.Write([]byte(n))
		off := time.Duration(float64(j.Interval) * cfg.JitterFraction * float64(h.Sum32()%1000) / 1000)
		s.jobs = append(s.jobs, &jobState{Job: j, next: start.Add(off)})
	}
	return s, nil
}

// Run loops until ctx ends: it runs due jobs, then sleeps until the next one.
func (s *Scheduler) Run(ctx context.Context) {
	for ctx.Err() == nil {
		s.RunDue(ctx)
		s.cfg.Sleep(ctx, s.untilNext())
	}
}

func (s *Scheduler) untilNext() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	d := time.Second
	for _, j := range s.jobs {
		if w := j.next.Sub(now); w < d {
			d = w
		}
	}
	return max(d, 10*time.Millisecond)
}

type runResult struct {
	entries []metrics.Entry
	err     error
	took    time.Duration
}

// RunDue runs every job whose time has come and waits for the runs (bounded
// by each job's timeout). A job whose previous run is still in flight (a
// collector that ignored its timeout) is skipped, never stacked.
func (s *Scheduler) RunDue(ctx context.Context) {
	now := s.cfg.Now()
	var due []*jobState
	s.mu.Lock()
	for _, j := range s.jobs {
		if j.next.After(now) {
			continue
		}
		j.next = j.next.Add(j.Interval)
		if j.next.Before(now) { // fell behind: do not replay missed runs
			j.next = now.Add(j.Interval)
		}
		switch {
		case j.running.Load():
			j.skipped++
		case now.Before(j.holdTill):
			j.skipped++
		default:
			due = append(due, j)
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, j := range due {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runOne(ctx, j, now)
		}()
	}
	wg.Wait()
}

func (s *Scheduler) runOne(ctx context.Context, j *jobState, now time.Time) {
	j.running.Store(true)
	cctx, cancel := context.WithTimeout(ctx, j.Timeout)
	defer cancel()
	ch := make(chan runResult, 1)
	go func() {
		defer j.running.Store(false)
		var r runResult
		t0 := time.Now()
		func() {
			defer func() {
				if p := recover(); p != nil {
					r.err = fmt.Errorf("collector panicked: %v", p)
				}
			}()
			r.entries, r.err = j.Collector.Collect(cctx, now)
		}()
		r.took = time.Since(t0)
		ch <- r
	}()
	var r runResult
	select {
	case r = <-ch:
	case <-cctx.Done():
		// The collector did not honour its timeout; abandon the run. It stays
		// marked as running, so the next due run is skipped, not stacked.
		r = runResult{err: fmt.Errorf("collector %s: %w", j.Collector.Name(), cctx.Err()), took: j.Timeout}
	}
	s.finish(j, now, r)
}

func (s *Scheduler) finish(j *jobState, now time.Time, r runResult) {
	var rejected int
	var pushback bool
	if len(r.entries) > 0 {
		_, err := s.cfg.Sink.Append(r.entries)
		if err != nil {
			rejected = len(r.entries)
			pushback = errors.Is(err, metrics.ErrDiskBudget) || errors.Is(err, metrics.ErrHeadFull) || errors.Is(err, metrics.ErrBusy)
			if r.err == nil {
				r.err = fmt.Errorf("store: %w", err)
			}
		}
	}
	s.mu.Lock()
	j.duration, j.samples = r.took, len(r.entries)
	j.rejected += int64(rejected)
	if r.err != nil {
		j.errors++
		j.lastErr = r.err.Error()
	} else {
		j.lastSuccess, j.lastErr = now, ""
	}
	if pushback {
		j.backoff = min(max(2*j.backoff, s.cfg.BackoffBase), s.cfg.BackoffMax)
		j.holdTill = now.Add(j.backoff)
	} else if rejected == 0 {
		j.backoff, j.holdTill = 0, time.Time{}
	}
	self := s.selfEntries(j, now)
	s.mu.Unlock()
	if len(self) > 0 {
		_, _ = s.cfg.Sink.Append(self) // best effort: self-metrics must not mask the real error
	}
}

func (s *Scheduler) selfEntries(j *jobState, now time.Time) []metrics.Entry {
	if s.cfg.DisableSelfMetrics {
		return nil
	}
	b := newBuilder(now, 0)
	l := []string{LabelCollector, j.Collector.Name()}
	b.add(MCollectDuration, j.duration.Seconds(), l...)
	b.add(MCollectSamples, float64(j.samples), l...)
	b.add(MCollectErrors, float64(j.errors), l...)
	b.add(MCollectSkipped, float64(j.skipped), l...)
	b.add(MCollectRejected, float64(j.rejected), l...)
	b.add(MCollectBackoff, j.backoff.Seconds(), l...)
	if !j.lastSuccess.IsZero() {
		b.add(MCollectLastSuccess, float64(j.lastSuccess.Unix()), l...)
	}
	return b.entries
}

// Stats returns a snapshot per job, sorted by name.
func (s *Scheduler) Stats() []JobStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]JobStats, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, JobStats{
			Name: j.Collector.Name(), LastDuration: j.duration, LastSamples: j.samples,
			Errors: j.errors, Skipped: j.skipped, Rejected: j.rejected,
			LastSuccess: j.lastSuccess, LastError: j.lastErr, Backoff: j.backoff,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}
