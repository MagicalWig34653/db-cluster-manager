// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"time"
)

// PatchScheduler runs the OS patch service's periodic work (deny list
// delivery, due report polls) while this replica leads. Poll is called with
// the current leadership's service by the caller; a follower's Poll is a
// no-op.
type PatchScheduler struct {
	Poll func(ctx context.Context) error
	// Interval defaults to five minutes. Hosts are only polled when their
	// policy's report interval has elapsed (patch.Service.PollDue).
	Interval time.Duration
	// InitialDelay is waited before the first run (default 20 s).
	InitialDelay time.Duration
	Logf         func(msg string, args ...any)
}

// Name implements Reconciler.
func (PatchScheduler) Name() string { return "patch-scheduler" }

// Run implements Reconciler.
func (s PatchScheduler) Run(ctx context.Context, _ *Leadership) error {
	iv := s.Interval
	if iv <= 0 {
		iv = 5 * time.Minute
	}
	delay := s.InitialDelay
	if delay <= 0 {
		delay = 20 * time.Second
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
				s.Logf("manager: patch scheduler run failed", "err", err)
			}
		}
		timer.Reset(iv)
	}
}
