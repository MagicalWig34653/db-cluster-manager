// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestPatchSchedulerRunsPeriodicallyAndStops(t *testing.T) {
	var n atomic.Int32
	var logged atomic.Int32
	s := PatchScheduler{Interval: 10 * time.Millisecond, InitialDelay: time.Millisecond,
		Poll: func(context.Context) error {
			if n.Add(1) == 1 {
				return errors.New("first run fails")
			}
			return nil
		},
		Logf: func(string, ...any) { logged.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n.Load() < 3 || logged.Load() != 1 {
		t.Fatalf("runs %d, logged %d", n.Load(), logged.Load())
	}
	if s.Name() != "patch-scheduler" {
		t.Fatal(s.Name())
	}
}
