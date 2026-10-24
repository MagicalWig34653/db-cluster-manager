// SPDX-License-Identifier: AGPL-3.0-only

package ai

import (
	"context"
	"sync"
	"time"
)

// Budget enforces per-provider daily request and token limits. At the
// limit the caller skips the analysis and the finding stays deterministic
// (ADR 0015 section 6). The counters are in memory per manager replica, so
// a leader change resets them (a limitation of this groundwork).
type Budget struct {
	mu  sync.Mutex
	Now func() time.Time
	day string
	use map[string]*Usage
}

// Usage is one provider's usage of the current UTC day.
type Usage struct {
	Day      string `json:"day"`
	Requests int    `json:"requests"`
	Tokens   int    `json:"tokens"`
}

func (b *Budget) today() string {
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	d := now().UTC().Format("2006-01-02")
	if d != b.day {
		b.day, b.use = d, map[string]*Usage{}
	}
	if b.use == nil {
		b.use = map[string]*Usage{}
	}
	return d
}

func limitsOf(l Limits) (int, int) {
	r, t := l.RequestsPerDay, l.TokensPerDay
	if r <= 0 {
		r = 50
	}
	if t <= 0 {
		t = 200000
	}
	return r, t
}

// Reserve counts one request if the limits allow it.
func (b *Budget) Reserve(id string, l Limits) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.today()
	u := b.use[id]
	if u == nil {
		u = &Usage{Day: d}
		b.use[id] = u
	}
	maxReq, maxTok := limitsOf(l)
	if u.Requests >= maxReq || u.Tokens >= maxTok {
		return ErrBudgetExceeded
	}
	u.Requests++
	return nil
}

// Record adds the tokens of a completed request.
func (b *Budget) Record(id string, tokens int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.today()
	if u := b.use[id]; u != nil {
		u.Tokens += tokens
	}
}

// Usage returns the current day's usage of a provider.
func (b *Budget) Usage(id string) Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.today()
	if u := b.use[id]; u != nil {
		return *u
	}
	return Usage{Day: d}
}

// FakeProvider answers with a scripted text and records requests (a fake).
type FakeProvider struct {
	mu       sync.Mutex
	Requests []Request
	Text     string
	Err      error
	ModelIDs []string
}

// Complete implements Provider.
func (f *FakeProvider) Complete(_ context.Context, r Request) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, r)
	if f.Err != nil {
		return Response{}, f.Err
	}
	return Response{Text: f.Text, Model: r.Model, InputTokens: len(r.User) / 4, OutputTokens: len(f.Text) / 4}, nil
}

// Models implements Provider.
func (f *FakeProvider) Models(context.Context) ([]string, error) { return f.ModelIDs, nil }
