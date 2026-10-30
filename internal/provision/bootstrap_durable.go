// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"context"
	"errors"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// ErrNotLeader is returned when a run is requested on a replica that does not
// hold the manager leadership. Only the leader reaches a host.
var ErrNotLeader = errors.New("provision: bootstrap runs only on the manager leader")

// interruptedText explains a run whose end was never recorded.
const interruptedText = "run interrupted by a manager restart or leadership change; the outcome of the step that was running is unknown. " +
	"Run again to resume: every step is checked on the host before it is repeated"

// DraftRecord is the durable, credential-free part of a draft. A DraftStore
// seals it at rest. It holds the pinned host key (public) and the reviewed
// plan; credentials live in SecretsRecord.
type DraftRecord struct {
	Request          BootstrapRequest `json:"request"`
	Status           Status           `json:"status"`
	HostKeyType      string           `json:"hostKeyType,omitempty"`
	HostFingerprint  string           `json:"hostFingerprint,omitempty"`
	HostKnownHosts   string           `json:"hostKnownHosts,omitempty"`
	Plan             *BootstrapPlan   `json:"plan,omitempty"`
	MainApproved     bool             `json:"mainApproved,omitempty"`
	LockdownApproved bool             `json:"lockdownApproved,omitempty"`
	Error            string           `json:"error,omitempty"`
	// Done lists the steps that succeeded. The journal is in memory, so this
	// is how a new leader knows the main phase finished (the lockdown phase
	// needs it). Steps are still checked on the host before any is repeated.
	Done      []string  `json:"done,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SecretsRecord holds the write-only credentials of one draft. It is stored
// apart from the draft so that wiping is the deletion of one document, and it
// carries its own expiry, which the service enforces on every read, sweep and
// restore.
type SecretsRecord struct {
	Root      Credential `json:"root"`
	Dedicated Credential `json:"dedicated"`
	JoinToken string     `json:"joinToken,omitempty"`
	Expires   time.Time  `json:"expires"`
}

// DraftStore persists bootstrap drafts so that they survive a restart or a
// failover of the manager. The manager implements it on the replicated
// metastore with the data keyring (sealed documents); tests use a fake.
type DraftStore interface {
	SaveDraft(DraftRecord) error
	LoadDrafts() ([]DraftRecord, error)
	SaveSecrets(id string, s SecretsRecord) error
	LoadSecrets(id string) (SecretsRecord, bool, error)
	DeleteSecrets(id string) error
}

func (s *BootstrapService) record(d *bootDraft) DraftRecord {
	r := DraftRecord{Request: d.req, Status: d.status, Plan: d.plan, MainApproved: d.mainApproved,
		LockdownApproved: d.lockdownApproved, Error: d.errText, UpdatedAt: s.Now()}
	if d.plan != nil {
		for _, st := range progressOf(*d.plan, s.deps.Journal).Steps {
			if st.State == string(opjournal.Succeeded) {
				r.Done = append(r.Done, st.ID)
			}
		}
	}
	r.HostKeyType, r.HostFingerprint, r.HostKnownHosts = d.hostKey.Type, d.hostKey.Fingerprint, d.hostKey.KnownHosts
	return r
}

// save persists the draft document. Without a store it is a no-op.
func (s *BootstrapService) save(d *bootDraft) error {
	if s.Store == nil {
		return nil
	}
	return s.Store.SaveDraft(s.record(d))
}

// saveSecrets persists the draft's current credentials.
func (s *BootstrapService) saveSecrets(d *bootDraft) error {
	if s.Store == nil {
		return nil
	}
	if d.secrets == (BootstrapSecrets{}) {
		return s.Store.DeleteSecrets(d.req.ID)
	}
	return s.Store.SaveSecrets(d.req.ID, SecretsRecord{Root: d.secrets.Root, Dedicated: d.secrets.Dedicated,
		JoinToken: d.secrets.JoinToken, Expires: d.expires})
}

// Restore loads the drafts of the store. Credentials past their expiry are
// deleted instead of loaded. A run whose end was not recorded is marked
// unknown: the previous process may have been in the middle of a step, and
// nothing in the store proves how that step ended. Run resumes it; the
// executor checks each step on the host before repeating it.
func (s *BootstrapService) Restore(ctx context.Context) error {
	if s.Store == nil {
		return nil
	}
	recs, err := s.Store.LoadDrafts()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range recs {
		if _, ok := s.drafts[r.Request.ID]; ok {
			continue
		}
		d := &bootDraft{req: r.Request, status: r.Status, plan: r.Plan, mainApproved: r.MainApproved,
			lockdownApproved: r.LockdownApproved, errText: r.Error,
			hostKey: HostKey{Type: r.HostKeyType, Fingerprint: r.HostFingerprint, KnownHosts: r.HostKnownHosts}}
		sec, ok, err := s.Store.LoadSecrets(r.Request.ID)
		switch {
		case err != nil:
			return err
		case ok && s.Now().After(sec.Expires):
			d.wipePending = true
		case ok:
			d.secrets = BootstrapSecrets{Root: sec.Root, Dedicated: sec.Dedicated, JoinToken: sec.JoinToken}
			d.expires = sec.Expires
		}
		if d.wipePending {
			s.finishWipe(d)
		}
		if err := s.seedJournal(d, r.Done); err != nil {
			return err
		}
		if d.status == StatusRunning {
			d.status, d.errText = StatusUnknown, interruptedText
			if err := s.save(d); err != nil {
				return err
			}
		}
		s.drafts[r.Request.ID] = d
	}
	return nil
}

// seedJournal records the persisted successful steps in this process's
// journal under the current leader term.
func (s *BootstrapService) seedJournal(d *bootDraft, done []string) error {
	term := s.Term()
	if d.plan == nil || len(done) == 0 || term == 0 {
		return nil
	}
	ids := make([]string, len(d.plan.Steps))
	for i, st := range d.plan.Steps {
		ids[i] = st.ID
	}
	op := OpID(d.plan.ID)
	if err := s.deps.Journal.Begin(op, "bootstrap", ids, term); err != nil {
		if errors.Is(err, opjournal.ErrExists) {
			return nil
		}
		return err
	}
	for _, id := range done {
		if err := s.deps.Journal.StartStep(op, id, term); err != nil {
			return err
		}
		if err := s.deps.Journal.FinishStep(op, id, opjournal.Succeeded, term); err != nil {
			return err
		}
	}
	return nil
}

// Sweep expires credentials past their lifetime and retries wipes that
// failed. The manager calls it periodically on the leader.
func (s *BootstrapService) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.drafts {
		s.expire(d)
		if d.wipePending {
			s.finishWipe(d)
		}
	}
}

// finishWipe deletes the stored credentials; on failure the wipe stays
// pending and is retried by the next expire or sweep.
func (s *BootstrapService) finishWipe(d *bootDraft) {
	if s.Store == nil {
		d.wipePending = false
		return
	}
	d.wipePending = s.Store.DeleteSecrets(d.req.ID) != nil
}
