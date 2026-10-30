// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"time"
)

// DraftSweeper runs Poll periodically while this replica leads. The manager
// uses it to delete expired sealed credentials of bootstrap and install
// drafts and to retry failed deletions.
type DraftSweeper struct {
	Poll     func(ctx context.Context) error
	Interval time.Duration // default one minute
	Logf     func(msg string, args ...any)
}

// Name implements Reconciler.
func (DraftSweeper) Name() string { return "draft-sweeper" }

// Run implements Reconciler.
func (s DraftSweeper) Run(ctx context.Context, _ *Leadership) error {
	iv := s.Interval
	if iv <= 0 {
		iv = time.Minute
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if s.Poll != nil {
			if err := s.Poll(ctx); err != nil && ctx.Err() == nil && s.Logf != nil {
				s.Logf("manager: draft sweep failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
