// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

// serveOnce accepts connections and answers each with payload as packet
// seq 0 (nil: accept and say nothing).
func serveOnce(t *testing.T, payload []byte) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if payload != nil {
				_ = writePacket(c, 0, payload)
			}
			time.Sleep(50 * time.Millisecond)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = l.Close(); <-done })
	return l.Addr().String()
}

func closedAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}

func probe(p Prober, addr string) ProbeResult {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	return p.Probe(ctx, addr)
}

func TestTCPProber(t *testing.T) {
	r := probe(TCPProber{}, serveOnce(t, nil))
	if !r.Healthy || r.Latency <= 0 || r.Sync != routing.SyncUnknown {
		t.Fatalf("live: %+v", r)
	}
	if r := probe(TCPProber{}, closedAddr(t)); r.Healthy || r.Err == nil {
		t.Fatalf("closed: %+v", r)
	}
}

func TestGreetingProber(t *testing.T) {
	r := probe(GreetingProber{}, serveOnce(t, fakeGreeting("10.11.8-MariaDB", capClientProtocol41)))
	if !r.Healthy || r.Latency <= 0 || r.Detail != "server 10.11.8-MariaDB" || r.Sync != routing.SyncUnknown {
		t.Fatalf("greeting: %+v", r)
	}
	if r := probe(GreetingProber{}, serveOnce(t, errPayload(1040, "", "Too many connections", false))); r.Healthy {
		t.Fatalf("ERR greeting healthy: %+v", r)
	}
	if r := probe(GreetingProber{}, serveOnce(t, []byte("garbage"))); r.Healthy {
		t.Fatalf("garbage healthy: %+v", r)
	}
	start := time.Now()
	if r := probe(GreetingProber{}, serveOnce(t, nil)); r.Healthy || time.Since(start) > time.Second {
		t.Fatalf("silent server: %+v after %s", r, time.Since(start))
	}
}

func TestSyncFromGalera(t *testing.T) {
	tests := []struct {
		in   GaleraStatus
		want routing.SyncState
	}{
		{GaleraStatus{LocalState: 4, ClusterStatus: "Primary", Ready: true}, routing.SyncSynced},
		{GaleraStatus{LocalState: 4, ClusterStatus: "non-Primary", Ready: true}, routing.SyncDesynced},
		{GaleraStatus{LocalState: 4, ClusterStatus: "Primary", Ready: false}, routing.SyncDesynced},
		{GaleraStatus{LocalState: 2, LocalStateComment: "Donor/Desynced", ClusterStatus: "Primary", Ready: true}, routing.SyncDonor},
		{GaleraStatus{LocalState: 2, LocalStateComment: "Desynced", ClusterStatus: "Primary", Ready: true}, routing.SyncDesynced},
		{GaleraStatus{LocalState: 1, ClusterStatus: "Primary", Ready: true}, routing.SyncJoining},
		{GaleraStatus{LocalState: 3, ClusterStatus: "Primary", Ready: true}, routing.SyncJoining},
		{GaleraStatus{LocalState: 0, ClusterStatus: "Primary", Ready: true}, routing.SyncUnknown},
	}
	for _, tt := range tests {
		if got, _ := SyncFromGalera(tt.in); got != tt.want {
			t.Errorf("%+v: %s, want %s", tt.in, got, tt.want)
		}
	}
}

type fixedStatus struct {
	st  GaleraStatus
	err error
}

func (f fixedStatus) GaleraStatus(context.Context, string) (GaleraStatus, error) { return f.st, f.err }

func TestNodeProber(t *testing.T) {
	live := ProberFunc(func(context.Context, string) ProbeResult {
		return ProbeResult{Healthy: true, Latency: time.Millisecond}
	})
	dead := ProberFunc(func(context.Context, string) ProbeResult { return ProbeResult{Err: errors.New("down")} })

	if r := (NodeProber{Liveness: live, AssumeSynced: true}).Probe(context.Background(), "x"); r.Sync != routing.SyncSynced {
		t.Fatalf("assume: %+v", r)
	}
	if r := (NodeProber{Liveness: dead, AssumeSynced: true}).Probe(context.Background(), "x"); r.Healthy || r.Sync != routing.SyncUnknown {
		t.Fatalf("dead node assumed synced: %+v", r)
	}
	if r := (NodeProber{Liveness: live}).Probe(context.Background(), "x"); r.Sync != routing.SyncUnknown {
		t.Fatalf("no source: %+v", r)
	}
	ok := fixedStatus{st: GaleraStatus{LocalState: 4, ClusterStatus: "Primary", Ready: true}}
	if r := (NodeProber{Liveness: live, Status: ok}).Probe(context.Background(), "x"); r.Sync != routing.SyncSynced {
		t.Fatalf("galera synced: %+v", r)
	}
	boom := errors.New("access denied")
	r := (NodeProber{Liveness: live, Status: failingStatus{err: boom}}).Probe(context.Background(), "x")
	if r.Sync != routing.SyncUnknown || !errors.Is(r.Err, boom) {
		t.Fatalf("failing status source: %+v", r)
	}
}

type failingStatus struct{ err error }

func (f failingStatus) GaleraStatus(context.Context, string) (GaleraStatus, error) {
	return GaleraStatus{}, f.err
}
