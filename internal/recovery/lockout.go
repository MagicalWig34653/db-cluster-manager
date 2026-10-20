// SPDX-License-Identifier: AGPL-3.0-only

package recovery

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrLocked is returned while a key is locked out.
var ErrLocked = errors.New("recovery: too many failed attempts, locked")

// LockedError carries the time until which a key is locked.
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string {
	return fmt.Sprintf("recovery: too many failed attempts, locked until %s", e.Until.UTC().Format(time.RFC3339))
}

// Unwrap lets errors.Is(err, ErrLocked) succeed.
func (e *LockedError) Unwrap() error { return ErrLocked }

// LockoutPolicy configures attempt limiting.
type LockoutPolicy struct {
	// MaxFailures within Window trigger a lockout.
	MaxFailures int
	Window      time.Duration
	// BaseLockout is the first lockout duration; each further lockout before
	// a success or reset doubles it, up to MaxLockout.
	BaseLockout time.Duration
	MaxLockout  time.Duration
}

// DefaultLockoutPolicy is a proposed default: 5 failures in 15 minutes lock
// for 15 minutes, doubling up to 24 hours.
var DefaultLockoutPolicy = LockoutPolicy{
	MaxFailures: 5,
	Window:      15 * time.Minute,
	BaseLockout: 15 * time.Minute,
	MaxLockout:  24 * time.Hour,
}

// LockState is the state of one key.
type LockState string

const (
	StateOpen   LockState = "open"
	StateLocked LockState = "locked"
)

// LockStatus describes one key.
type LockStatus struct {
	State       LockState
	Failures    int
	Lockouts    int
	LockedUntil time.Time
}

type lockEntry struct {
	failures     int
	windowStart  time.Time
	lockouts     int
	lockedUntil  time.Time
	lastActivity time.Time
}

// Limiter is a per-key attempt limiter with lockout. Attempts are counted
// pessimistically: Begin counts the attempt as a failure before the secret is
// verified, and Succeed clears the count. Concurrent guesses therefore cannot
// exceed MaxFailures per window.
type Limiter struct {
	mu      sync.Mutex
	policy  LockoutPolicy
	now     Clock
	entries map[string]*lockEntry
}

// NewLimiter returns a Limiter. now may be nil for time.Now.
func NewLimiter(p LockoutPolicy, now Clock) *Limiter {
	if p.MaxFailures < 1 {
		p.MaxFailures = 1
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{policy: p, now: now, entries: make(map[string]*lockEntry)}
}

func (l *Limiter) lockDuration(lockouts int) time.Duration {
	d := l.policy.BaseLockout
	for i := 1; i < lockouts; i++ {
		d *= 2
		if l.policy.MaxLockout > 0 && d >= l.policy.MaxLockout {
			return l.policy.MaxLockout
		}
	}
	if l.policy.MaxLockout > 0 && d > l.policy.MaxLockout {
		return l.policy.MaxLockout
	}
	return d
}

// Begin registers an attempt for key. It returns a *LockedError while the key
// is locked. Otherwise the attempt is counted as a failure until Succeed is
// called; reaching MaxFailures starts a lockout after this attempt.
func (l *Limiter) Begin(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil {
		e = &lockEntry{}
		l.entries[key] = e
	}
	e.lastActivity = now
	if now.Before(e.lockedUntil) {
		return &LockedError{Until: e.lockedUntil}
	}
	if e.failures == 0 || now.Sub(e.windowStart) >= l.policy.Window {
		e.failures = 0
		e.windowStart = now
	}
	e.failures++
	if e.failures >= l.policy.MaxFailures {
		e.lockouts++
		e.lockedUntil = now.Add(l.lockDuration(e.lockouts))
		e.failures = 0
	}
	return nil
}

// Succeed records that the attempt begun for key succeeded; it clears
// failures, lockouts and any lockout started by that attempt.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// Reset clears the state of key, for example by an administrator.
func (l *Limiter) Reset(key string) { l.Succeed(key) }

// Status reports the state of key.
func (l *Limiter) Status(key string) LockStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return LockStatus{State: StateOpen}
	}
	st := LockStatus{State: StateOpen, Failures: e.failures, Lockouts: e.lockouts}
	if l.now().Before(e.lockedUntil) {
		st.State = StateLocked
		st.LockedUntil = e.lockedUntil
	}
	return st
}

// Prune removes idle entries that are not locked and whose last activity is
// older than both the window and the maximum lockout, bounding memory.
func (l *Limiter) Prune() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	keep := max(l.policy.Window, l.policy.MaxLockout, l.policy.BaseLockout)
	n := 0
	for k, e := range l.entries {
		if !now.Before(e.lockedUntil) && now.Sub(e.lastActivity) > keep {
			delete(l.entries, k)
			n++
		}
	}
	return n
}
