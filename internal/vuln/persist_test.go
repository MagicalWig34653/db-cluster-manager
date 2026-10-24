// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
)

func TestBudgetAndAuditSeqSurviveRestart(t *testing.T) {
	for name, kv := range kvBackends(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			l := ai.Limits{RequestsPerDay: 2, TokensPerDay: 1000}
			b1 := &ai.Budget{Now: clock, Backing: BudgetStore{KV: kv}}
			if err := b1.Reserve("p", l); err != nil {
				t.Fatal(err)
			}
			b1.Record("p", 40)
			// A new process (or a new leader) sees the saved counters.
			b2 := &ai.Budget{Now: clock, Backing: BudgetStore{KV: kv}}
			if u := b2.Usage("p"); u.Requests != 1 || u.Tokens != 40 {
				t.Fatalf("usage after restart: %+v", u)
			}
			if err := b2.Reserve("p", l); err != nil {
				t.Fatal(err)
			}
			if err := b2.Reserve("p", l); err == nil {
				t.Fatal("limit must hold across restarts")
			}
			// A stale cache is replaced by Reload.
			b1.Reload()
			if u := b1.Usage("p"); u.Requests != 2 {
				t.Fatalf("reload: %+v", u)
			}
			// The next day starts empty.
			now = now.Add(24 * time.Hour)
			b3 := &ai.Budget{Now: clock, Backing: BudgetStore{KV: kv}}
			if u := b3.Usage("p"); u.Requests != 0 {
				t.Fatalf("new day: %+v", u)
			}

			st := &Store{KV: kv}
			s1 := &Service{Store: st, Now: clock}
			a := s1.nextSeq()
			if err := s1.audit(AuditRecord{Actor: "t", Event: EventPoll}); err != nil {
				t.Fatal(err)
			}
			s2 := &Service{Store: st, Now: clock}
			if got := s2.nextSeq(); got <= a+1 {
				t.Fatalf("sequence did not continue after restart: first %d, next %d", a, got)
			}
		})
	}
}
