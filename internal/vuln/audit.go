// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
)

// AuditRecord is one step of the vulnerability response. Together the
// records let an admin reconstruct each decision (R10 exit gate): the
// finding, provider, model, prompt and response hashes, validator result,
// plan digest, who approved or declined, and what was executed. Prompts and
// secrets are never part of a record.
type AuditRecord struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
	// Seq orders records written within the same clock reading.
	Seq         uint64 `json:"seq"`
	Actor       string `json:"actor"`
	Event       string `json:"event"`
	FindingID   string `json:"findingId,omitempty"`
	ProposalID  string `json:"proposalId,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	PromptSHA   string `json:"promptSha256,omitempty"`
	ResponseSHA string `json:"responseSha256,omitempty"`
	PlanDigest  string `json:"planDigest,omitempty"`
	Result      string `json:"result"`
	Detail      string `json:"detail,omitempty"`
}

// Audit events.
const (
	EventPoll               = "poll"
	EventFindingCreated     = "finding_created"
	EventFindingResolved    = "finding_resolved"
	EventFindingStatus      = "finding_status"
	EventAnalysis           = "ai_analysis"
	EventProposalCreated    = "proposal_created"
	EventProposalInvalid    = "proposal_invalid"
	EventApproved           = "proposal_approved"
	EventAutoApproved       = "proposal_auto_approved"
	EventDeclined           = "proposal_declined"
	EventExecution          = "execution"
	EventPolicyChanged      = "policy_changed"
	EventProviderAdded      = "provider_added"
	EventProviderConsent    = "provider_consent"
	EventMitigationExpiring = "mitigation_expiring"
)

// Key orders records by time and sequence; the random suffix keeps keys
// unique across replicas.
func (r AuditRecord) Key() string {
	return fmt.Sprintf("%s-%012d-%s", r.At.UTC().Format("20060102T150405.000000000Z"), r.Seq, r.ID)
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// Notification is a message to the admin (new finding, proposal waiting,
// execution result, mitigation expiring).
type Notification struct {
	Kind       string   `json:"kind"`
	FindingID  string   `json:"findingId,omitempty"`
	ProposalID string   `json:"proposalId,omitempty"`
	Severity   Severity `json:"severity,omitempty"`
	Priority   Priority `json:"priority,omitempty"`
	Title      string   `json:"title"`
}

// Notifier delivers notifications. The adapter to internal/notify is not
// wired yet; MemoryNotifier is the fake.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// MemoryNotifier records notifications (a fake).
type MemoryNotifier struct {
	mu   sync.Mutex
	Sent []Notification
}

// Notify implements Notifier.
func (m *MemoryNotifier) Notify(_ context.Context, n Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = append(m.Sent, n)
	return nil
}

// All returns a copy of the sent notifications.
func (m *MemoryNotifier) All() []Notification {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Notification(nil), m.Sent...)
}

// ProviderView is the non-secret view of a provider configuration.
type ProviderView struct {
	ai.ProviderConfig
	// CredentialSet reports that an API key is stored; the value is never
	// returned.
	CredentialSet bool     `json:"credentialSet"`
	Local         bool     `json:"local"`
	Egress        string   `json:"egressStatement"`
	Usage         ai.Usage `json:"usage"`
}

// ProviderConfigs returns the stored provider configurations.
func (s *Store) ProviderConfigs() ([]ai.ProviderConfig, error) {
	out, _, err := list[ai.ProviderConfig](s.KV, CollProviders)
	return out, err
}

// ProviderConfig returns one provider configuration.
func (s *Store) ProviderConfig(id string) (ai.ProviderConfig, uint64, bool, error) {
	return get[ai.ProviderConfig](s.KV, CollProviders, id)
}

// PutProviderConfig writes a provider configuration conditionally.
func (s *Store) PutProviderConfig(c ai.ProviderConfig, ifVersion uint64) error {
	p, err := put(CollProviders, c.ID, c, &ifVersion)
	if err != nil {
		return err
	}
	return s.KV.Txn(p)
}
