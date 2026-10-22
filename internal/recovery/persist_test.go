// SPDX-License-Identifier: AGPL-3.0-only

package recovery

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type memPersister struct {
	mu       sync.Mutex
	fail     error
	accounts map[string]Account
	sessions map[string]SessionRecord
	codes    map[string]CodeSet
	puts     int
}

func newMemPersister() *memPersister {
	return &memPersister{accounts: map[string]Account{}, sessions: map[string]SessionRecord{}, codes: map[string]CodeSet{}}
}

func (m *memPersister) PutAccount(a Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.accounts[a.ID] = a
	return nil
}

func (m *memPersister) PutSession(r SessionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.puts++
	m.sessions[r.Digest] = r
	return nil
}

func (m *memPersister) DeleteSession(d string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	delete(m.sessions, d)
	return nil
}

func (m *memPersister) PutCodes(id string, s CodeSet) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.codes[id] = s
	return nil
}

func (m *memPersister) DeleteCodes(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.codes, id)
	return nil
}

func persistAccount(id string) Account {
	return Account{ID: id, Credentials: []Credential{{ID: "pk-" + id, Kind: CredentialPasskey}}}
}

func TestAccountsPersistAndLoad(t *testing.T) {
	p := newMemPersister()
	a := NewAccounts()
	a.SetPersister(p)
	if err := a.Create(persistAccount("alice")); err != nil {
		t.Fatal(err)
	}
	if err := a.AddCredential("alice", Credential{ID: "pw", Kind: CredentialPassword}); err != nil {
		t.Fatal(err)
	}
	if got := p.accounts["alice"]; got.CredentialEpoch != 1 || len(got.Credentials) != 2 {
		t.Fatalf("persisted %+v", got)
	}
	p.fail = errors.New("disk full")
	if err := a.Create(persistAccount("bob")); err == nil {
		t.Fatal("create succeeded although persistence failed")
	}
	if _, err := a.Get("bob"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatal("rejected account is visible")
	}
	if err := a.RecordCredentialChange("alice"); err == nil {
		t.Fatal("mutation succeeded although persistence failed")
	}
	if e, _ := a.CredentialEpoch("alice"); e != 1 {
		t.Fatalf("epoch advanced to %d", e)
	}
	b := NewAccounts()
	if err := b.Load([]Account{p.accounts["alice"]}); err != nil {
		t.Fatal(err)
	}
	if got := b.List(); len(got) != 1 || got[0].CredentialEpoch != 1 {
		t.Fatalf("loaded %+v", got)
	}
	if err := b.Load([]Account{{ID: "x"}}); err == nil {
		t.Fatal("invalid account loaded")
	}
}

func TestSessionsPersistAndLoad(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	accounts := NewAccounts()
	_ = accounts.Create(persistAccount("alice"))
	p := newMemPersister()
	s := NewSessions(DefaultSessionPolicy, accounts, clock)
	var bgErrs []error
	s.SetPersister(p, func(err error) { bgErrs = append(bgErrs, err) })
	tok, _, err := s.Issue("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.sessions) != 1 {
		t.Fatalf("sessions persisted: %d", len(p.sessions))
	}
	// Activity within a quarter of the idle timeout is not written.
	now = now.Add(time.Minute)
	if _, err := s.Validate(tok); err != nil {
		t.Fatal(err)
	}
	if p.puts != 1 {
		t.Fatalf("puts %d", p.puts)
	}
	now = now.Add(DefaultSessionPolicy.IdleTimeout / 4)
	if _, err := s.Validate(tok); err != nil {
		t.Fatal(err)
	}
	if p.puts != 2 {
		t.Fatalf("activity not persisted: puts %d", p.puts)
	}
	// A restarted store accepts the same token.
	s2 := NewSessions(DefaultSessionPolicy, accounts, clock)
	var recs []SessionRecord
	for _, r := range p.sessions {
		recs = append(recs, r)
	}
	if err := s2.Load(recs); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Validate(tok); err != nil {
		t.Fatalf("loaded session: %v", err)
	}
	// Rotation writes the new session and deletes the old.
	tok2, _, err := s.Rotate(tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.sessions) != 1 {
		t.Fatalf("after rotate %d sessions", len(p.sessions))
	}
	s.Revoke(tok2)
	if len(p.sessions) != 0 {
		t.Fatal("revoke not persisted")
	}
	// Issue fails closed when the write fails.
	p.fail = errors.New("no quorum")
	if _, _, err := s.Issue("alice"); err == nil {
		t.Fatal("issue succeeded without persistence")
	}
	if len(s.List("alice")) != 0 {
		t.Fatal("unpersisted session stored")
	}
	if err := s2.Load([]SessionRecord{{Digest: "zz"}}); err == nil {
		t.Fatal("bad digest loaded")
	}
	_ = bgErrs
}

func TestCodesPersistAndLoad(t *testing.T) {
	codes, set, err := GenerateCodes(2, 1000, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	p := newMemPersister()
	v := NewCodeVerifier(NewLimiter(DefaultLockoutPolicy, nil), nil)
	v.SetPersister(p, nil)
	v.Enroll("alice", set)
	if p.codes["alice"].Remaining() != 2 {
		t.Fatal("enrolment not persisted")
	}
	if err := v.Verify("alice", codes[0]); err != nil {
		t.Fatal(err)
	}
	if p.codes["alice"].Remaining() != 1 {
		t.Fatal("consumption not persisted")
	}
	v2 := NewCodeVerifier(NewLimiter(DefaultLockoutPolicy, nil), nil)
	v2.Load(map[string]CodeSet{"alice": p.codes["alice"]})
	if err := v2.Verify("alice", codes[0]); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("consumed code accepted after reload: %v", err)
	}
	if got := v2.Export(); got["alice"].Remaining() != 1 {
		t.Fatalf("export %+v", got)
	}
	p.fail = errors.New("no quorum")
	if err := v.Verify("alice", codes[1]); err == nil {
		t.Fatal("verify succeeded without persisting the consumption")
	}
	v.Revoke("alice")
	if _, ok := p.codes["alice"]; ok {
		t.Fatal("revoke not persisted")
	}
}
