// SPDX-License-Identifier: AGPL-3.0-only

package fencing

import (
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakePersister records Save calls and can be told to fail.
type fakePersister struct {
	mu      sync.Mutex
	stored  uint64
	saves   []uint64
	failErr error
}

func (f *fakePersister) Load() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stored, nil
}

func (f *fakePersister) Save(term uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	f.stored = term
	f.saves = append(f.saves, term)
	return nil
}

func newGuard(t *testing.T, p Persister) *Guard {
	t.Helper()
	g, err := NewGuard(p)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return g
}

func TestAdmitIncreasingTerms(t *testing.T) {
	g := newGuard(t, &fakePersister{})
	for _, term := range []uint64{1, 2, 5, 100} {
		if err := g.Admit(term); err != nil {
			t.Fatalf("Admit(%d): %v", term, err)
		}
		if got := g.Highest(); got != term {
			t.Fatalf("Highest = %d, want %d", got, term)
		}
	}
}

func TestAdmitEqualTerm(t *testing.T) {
	g := newGuard(t, &fakePersister{})
	if err := g.Admit(7); err != nil {
		t.Fatal(err)
	}
	if err := g.Admit(7); err != nil {
		t.Fatalf("equal term rejected: %v", err)
	}
}

func TestRejectLowerTerm(t *testing.T) {
	g := newGuard(t, &fakePersister{})
	if err := g.Admit(10); err != nil {
		t.Fatal(err)
	}
	err := g.Admit(9)
	if !errors.Is(err, ErrStaleTerm) {
		t.Fatalf("err = %v, want ErrStaleTerm", err)
	}
	if g.Highest() != 10 {
		t.Fatalf("Highest = %d, want 10", g.Highest())
	}
}

func TestRejectTermZero(t *testing.T) {
	g := newGuard(t, &fakePersister{})
	if err := g.Admit(0); !errors.Is(err, ErrStaleTerm) {
		t.Fatalf("err = %v, want ErrStaleTerm", err)
	}
	if err := g.Admit(3); err != nil {
		t.Fatal(err)
	}
	if err := g.Admit(0); !errors.Is(err, ErrStaleTerm) {
		t.Fatalf("err = %v, want ErrStaleTerm", err)
	}
}

func TestPersistsBeforeReturning(t *testing.T) {
	p := &fakePersister{}
	g := newGuard(t, p)
	if err := g.Admit(4); err != nil {
		t.Fatal(err)
	}
	// By the time Admit returns nil the term is already stored.
	if stored, _ := p.Load(); stored != 4 {
		t.Fatalf("stored = %d, want 4", stored)
	}
	if len(p.saves) != 1 || p.saves[0] != 4 {
		t.Fatalf("saves = %v, want [4]", p.saves)
	}
}

func TestSaveFailureKeepsHighest(t *testing.T) {
	p := &fakePersister{}
	g := newGuard(t, p)
	if err := g.Admit(2); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("disk full")
	p.failErr = boom
	err := g.Admit(5)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if g.Highest() != 2 {
		t.Fatalf("Highest = %d, want 2", g.Highest())
	}
	// Equal term needs no save and is still admitted.
	if err := g.Admit(2); err != nil {
		t.Fatalf("Admit(2): %v", err)
	}
}

func TestEqualTermDoesNotSave(t *testing.T) {
	p := &fakePersister{}
	g := newGuard(t, p)
	_ = g.Admit(3)
	_ = g.Admit(3)
	if len(p.saves) != 1 {
		t.Fatalf("saves = %v, want one", p.saves)
	}
}

func TestFilePersisterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "term")
	g1 := newGuard(t, NewFilePersister(path))
	if err := g1.Admit(42); err != nil {
		t.Fatal(err)
	}
	g2 := newGuard(t, NewFilePersister(path))
	if g2.Highest() != 42 {
		t.Fatalf("Highest after restart = %d, want 42", g2.Highest())
	}
	if err := g2.Admit(41); !errors.Is(err, ErrStaleTerm) {
		t.Fatalf("err = %v, want ErrStaleTerm", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "42\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
}

func TestFilePersisterMissingFile(t *testing.T) {
	g := newGuard(t, NewFilePersister(filepath.Join(t.TempDir(), "none")))
	if g.Highest() != 0 {
		t.Fatalf("Highest = %d, want 0", g.Highest())
	}
}

func TestCorruptFile(t *testing.T) {
	for _, content := range []string{"abc\n", "", "-1\n", "1 2\n"} {
		path := filepath.Join(t.TempDir(), "term")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewGuard(NewFilePersister(path)); err == nil {
			t.Fatalf("content %q: expected error", content)
		}
	}
}

func TestConcurrentAdmit(t *testing.T) {
	g := newGuard(t, NewFilePersister(filepath.Join(t.TempDir(), "term")))
	var max uint64
	terms := make([]uint64, 50)
	for i := range terms {
		terms[i] = uint64(rand.Intn(1000)) + 1
		if terms[i] > max {
			max = terms[i]
		}
	}
	var wg sync.WaitGroup
	for _, term := range terms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Admit(term)
		}()
	}
	wg.Wait()
	if g.Highest() != max {
		t.Fatalf("Highest = %d, want %d", g.Highest(), max)
	}
}
