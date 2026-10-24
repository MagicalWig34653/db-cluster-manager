// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

func TestVulnSchedulerPollsAndStops(t *testing.T) {
	var n atomic.Int32
	logged := make(chan struct{}, 4)
	s := VulnScheduler{
		Interval: 10 * time.Millisecond, InitialDelay: time.Millisecond,
		Poll: func(context.Context) error {
			if n.Add(1) == 2 {
				return errors.New("feed down")
			}
			return nil
		},
		Logf: func(string, ...any) { logged <- struct{}{} },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, nil) }()
	waitFor(t, "three polls", func() bool { return n.Load() >= 3 })
	select {
	case <-logged:
	default:
		t.Fatal("poll failure not logged")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop with its context")
	}
}

func TestVulnNotifierMapsToDispatcherEvents(t *testing.T) {
	var got []notify.Event
	n := VulnNotifier{Send: func(_ context.Context, e notify.Event) bool { got = append(got, e); return true }}
	_ = n.Notify(context.Background(), vuln.Notification{Kind: vuln.EventFindingCreated, FindingID: "f1", Priority: vuln.P1, Title: "CVE-2026-0001 affects x"})
	_ = n.Notify(context.Background(), vuln.Notification{Kind: vuln.EventMitigationExpiring, FindingID: "f2", ProposalID: "p2", Title: "expires"})
	if len(got) != 2 || got[0].Severity != notify.SeverityCritical || got[0].Kind != "vuln.finding_created" ||
		got[0].Resource != "vuln/finding/f1" || got[0].Details["priority"] != "P1" {
		t.Fatalf("event 0: %+v", got)
	}
	if got[1].Severity != notify.SeverityWarning || got[1].Details["proposal"] != "p2" {
		t.Fatalf("event 1: %+v", got[1])
	}
	// No dispatcher: nothing happens, no error.
	if err := (VulnNotifier{}).Notify(context.Background(), vuln.Notification{Title: "x"}); err != nil {
		t.Fatal(err)
	}
}
