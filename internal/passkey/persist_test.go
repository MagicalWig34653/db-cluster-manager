// SPDX-License-Identifier: AGPL-3.0-only

package passkey

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

type credStore struct {
	fail  error
	creds map[string][]byte // JSON, as a durable store would hold it
}

func (s *credStore) PutCredential(c StoredCredential) error {
	if s.fail != nil {
		return s.fail
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	s.creds[c.Info.ID] = b
	return nil
}

func (s *credStore) DeleteCredential(id string) error {
	delete(s.creds, id)
	return nil
}

func (s *credStore) decoded(t *testing.T) []StoredCredential {
	t.Helper()
	var out []StoredCredential
	for _, b := range s.creds {
		var c StoredCredential
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func TestCredentialsPersistAndReloadForLogin(t *testing.T) {
	f := newFixture(t)
	store := &credStore{creds: map[string][]byte{}}
	f.m.SetPersister(store, nil)
	a := newSoftAuthenticator(t)
	info := f.mustRegisterNew(a, "alice")
	if len(store.creds) != 1 {
		t.Fatalf("persisted %d credentials", len(store.creds))
	}
	if _, err := f.login(a, "alice", tweak{}); err != nil {
		t.Fatal(err)
	}
	var stored StoredCredential
	_ = json.Unmarshal(store.creds[info.ID], &stored)
	if stored.Info.LastUsedAt.IsZero() {
		t.Fatal("login state not persisted")
	}

	// A new Manager loaded from the store (and accounts reloaded) logs in.
	g := newFixture(t)
	if err := g.accounts.Load(f.accounts.List()); err != nil {
		t.Fatal(err)
	}
	if err := g.m.Load(store.decoded(t)); err != nil {
		t.Fatal(err)
	}
	g.clock.Advance(1)
	res, err := g.login(a, "alice", tweak{})
	if err != nil {
		t.Fatalf("login after reload: %v", err)
	}
	if res.AccountID != "alice" {
		t.Fatalf("result %+v", res)
	}
	if got := g.m.Export(); len(got) != 1 || got[0].Info.ID != info.ID {
		t.Fatalf("export %+v", got)
	}
}

func TestRegistrationFailsClosedWhenPersistenceFails(t *testing.T) {
	f := newFixture(t)
	store := &credStore{creds: map[string][]byte{}, fail: errors.New("no quorum")}
	f.m.SetPersister(store, nil)
	_, err := f.register(newSoftAuthenticator(t), RegistrationRequest{AccountID: "bob", NewAccount: true}, tweak{})
	if err == nil {
		t.Fatal("registration succeeded without persistence")
	}
	if _, err := f.accounts.Get("bob"); !errors.Is(err, recovery.ErrUnknownAccount) {
		t.Fatal("account created although its passkey was not persisted")
	}
}

func TestLoginFailsClosedWhenCounterCannotBePersisted(t *testing.T) {
	f := newFixture(t)
	store := &credStore{creds: map[string][]byte{}}
	f.m.SetPersister(store, nil)
	a := newSoftAuthenticator(t)
	f.mustRegisterNew(a, "alice")
	store.fail = errors.New("no quorum")
	if _, err := f.login(a, "alice", tweak{}); err == nil {
		t.Fatal("login succeeded without persisting the counter")
	}
}

func TestLoadRejectsInconsistentRecords(t *testing.T) {
	f := newFixture(t)
	h := make([]byte, 32)
	if err := f.m.Load([]StoredCredential{{Info: CredentialInfo{ID: "x", AccountID: "a"}, UserHandle: h}}); err == nil {
		t.Fatal("record ID mismatch accepted")
	}
	if err := f.m.Load([]StoredCredential{{Info: CredentialInfo{ID: "", AccountID: "a"}, UserHandle: h}}); err == nil {
		t.Fatal("empty ID accepted")
	}
}
