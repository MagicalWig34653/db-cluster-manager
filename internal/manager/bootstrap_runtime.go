// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// Collections of the fresh-server bootstrap (ADR 0028). All documents are
// sealed with the data keyring.
const (
	CollBootstrapDrafts   = "bootstrap-drafts"    // key: bootstrap ID; plan, pinned host key, state (no credential)
	CollBootstrapSecrets  = "bootstrap-secrets"   // key: bootstrap ID; root/dedicated login, join token, expiry
	CollBootstrapHostKeys = "bootstrap-host-keys" // key: host ID; dedicated private key of a locked-down host
)

// BootstrapDraftStore keeps bootstrap drafts in the replicated metastore. The
// draft document holds no credential; the credentials are a second sealed
// document whose deletion is the credential wipe.
type BootstrapDraftStore struct {
	Store *Store
	Keys  KeyProvider
}

var _ provision.DraftStore = BootstrapDraftStore{}

// SaveDraft implements provision.DraftStore.
func (s BootstrapDraftStore) SaveDraft(r provision.DraftRecord) error {
	return PutSealed(s.Store, s.Keys, CollBootstrapDrafts, r.Request.ID, r)
}

// LoadDrafts implements provision.DraftStore.
func (s BootstrapDraftStore) LoadDrafts() ([]provision.DraftRecord, error) {
	docs, err := ListSealed[provision.DraftRecord](s.Store, s.Keys, CollBootstrapDrafts, Consistent())
	if err != nil {
		return nil, err
	}
	out := make([]provision.DraftRecord, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Value)
	}
	return out, nil
}

// SaveSecrets implements provision.DraftStore.
func (s BootstrapDraftStore) SaveSecrets(id string, r provision.SecretsRecord) error {
	return PutSealed(s.Store, s.Keys, CollBootstrapSecrets, id, r)
}

// LoadSecrets implements provision.DraftStore.
func (s BootstrapDraftStore) LoadSecrets(id string) (provision.SecretsRecord, bool, error) {
	return GetSealed[provision.SecretsRecord](s.Store, s.Keys, CollBootstrapSecrets, id, Consistent())
}

// DeleteSecrets implements provision.DraftStore. A missing document is not
// an error: wiping twice is fine.
func (s BootstrapDraftStore) DeleteSecrets(id string) error {
	if _, ok, err := s.Store.Raw(CollBootstrapSecrets, id); err != nil || !ok {
		return err
	}
	return Delete(s.Store, CollBootstrapSecrets, id, nil)
}

// PatchInstallSealer is the sealed document store of the SSH installer of
// OS patch management (patch.Sealer): install plans and their SSH credential
// are encrypted with the data keyring and survive a restart or failover.
type PatchInstallSealer struct {
	Store *Store
	Keys  KeyProvider
}

var (
	_ patch.Sealer    = PatchInstallSealer{}
	_ patch.KeyLister = PatchInstallSealer{}
)

// Put implements patch.Sealer.
func (s PatchInstallSealer) Put(coll, key string, v any) error {
	return PutSealed(s.Store, s.Keys, coll, key, v)
}

// Get implements patch.Sealer.
func (s PatchInstallSealer) Get(coll, key string, v any) (bool, error) {
	raw, ok, err := GetSealed[json.RawMessage](s.Store, s.Keys, coll, key, Consistent())
	if err != nil || !ok {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// Delete implements patch.Sealer. A missing document is not an error.
func (s PatchInstallSealer) Delete(coll, key string) error {
	if _, ok, err := s.Store.Raw(coll, key); err != nil || !ok {
		return err
	}
	return Delete(s.Store, coll, key, nil)
}

// List implements patch.KeyLister.
func (s PatchInstallSealer) List(coll string) ([]string, error) {
	docs, err := List[sealed](s.Store, coll, Consistent())
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(docs))
	for _, d := range docs {
		keys = append(keys, d.Key)
	}
	return keys, nil
}

// HostTokenTTL is the lifetime of an enrollment token issued for one install
// or bootstrap. The agent enrolls within minutes; the token is not stored in
// plain form and is limited to the one host.
const HostTokenTTL = 2 * time.Hour

// HostEnrollmentTokens issues one-time enrollment tokens for a single host.
// It implements provision.TokenSource for the bootstrap and, through
// InstallToken, the token function of the patch SSH installer.
//
// The host must have an enrolled host record (a Swarm node the administrator
// enrolled) whose node ID equals the host ID: the agent presents that ID at
// enrollment. A server that is not yet a Swarm node has no such record and
// gets no token (see ADR 0028, limits).
type HostEnrollmentTokens struct {
	PKI *AgentPKI
	TTL time.Duration
}

var _ provision.TokenSource = HostEnrollmentTokens{}

// EnrollToken implements provision.TokenSource.
func (h HostEnrollmentTokens) EnrollToken(ctx context.Context, host provision.Host, runtime string) (string, error) {
	if runtime != "native" && runtime != "container" {
		return "", fmt.Errorf("manager: unknown agent runtime %q", runtime)
	}
	return h.InstallToken(ctx, host.ID)
}

// InstallToken returns a token for the host with this ID (the signature of
// patch.Installer.Token).
func (h HostEnrollmentTokens) InstallToken(_ context.Context, hostID string) (string, error) {
	if h.PKI == nil {
		return "", errors.New("manager: agent transport not configured")
	}
	inst, err := h.PKI.installation()
	if err != nil {
		return "", err
	}
	hosts, err := List[HostRecord](h.PKI.Store, CollHosts, Consistent())
	if err != nil {
		return "", err
	}
	var rec *Versioned[HostRecord]
	for i := range hosts {
		if hosts[i].Value.NodeID == hostID && hosts[i].Value.Enrollment == EnrollEnrolled {
			rec = &hosts[i]
			break
		}
	}
	if rec == nil {
		return "", fmt.Errorf("manager: host %q has no enrolled host record", hostID)
	}
	ttl := h.TTL
	if ttl <= 0 {
		ttl = HostTokenTTL
	}
	tok, plain, err := enroll.NewToken(inst, []string{hostID}, h.PKI.now(), ttl)
	if err != nil {
		return "", err
	}
	if _, err := Put(h.PKI.Store, CollEnrollTokens, tok.ID, EnrollTokenRecord{Swarm: rec.Value.Swarm, Token: tok}, IfVersion(0)); err != nil {
		return "", err
	}
	return plain, nil
}

// BootstrapManagerActions are the bootstrap steps that run inside the
// manager: deploying the container agent through the existing Swarm
// deployment code and sealing the dedicated key of a locked-down host.
type BootstrapManagerActions struct {
	Swarms *Swarms
	Store  *Store
	Keys   KeyProvider
	// Actor is the subject the Swarm operations run as: the manager's own
	// system subject. The administrator approved the plan that names this
	// step.
	Actor authz.Subject
}

var _ provision.ManagerActions = BootstrapManagerActions{}

// DeployContainerAgent enrolls the host's Swarm node (a revoked host is never
// re-enrolled here) and runs the existing agent deployment for its Swarm,
// which labels the node and creates the agent service when it is missing. The
// host ID must be the Swarm node ID of a synced node.
func (a BootstrapManagerActions) DeployContainerAgent(ctx context.Context, h provision.Host) error {
	hosts, err := List[HostRecord](a.Store, CollHosts, Consistent())
	if err != nil {
		return err
	}
	var rec *Versioned[HostRecord]
	for i := range hosts {
		if hosts[i].Value.NodeID == h.ID {
			rec = &hosts[i]
			break
		}
	}
	if rec == nil {
		return fmt.Errorf("manager: no Swarm node %q is known (the Swarm must be connected and synced)", h.ID)
	}
	switch rec.Value.Enrollment {
	case EnrollRevoked:
		return fmt.Errorf("manager: host %q was revoked; enroll it explicitly first", h.ID)
	case EnrollEnrolled:
	default:
		if _, err := a.Swarms.SetEnrollment(a.Actor, rec.Value.Swarm, h.ID, true, ""); err != nil {
			return err
		}
	}
	res, err := a.Swarms.DeployAgents(ctx, a.Actor, rec.Value.Swarm)
	if err != nil {
		return err
	}
	if msg, bad := res.Failed[h.ID]; bad {
		return fmt.Errorf("manager: labeling node %q failed: %s", h.ID, msg)
	}
	// Labeled lists nodes whose label changed; an already labeled node is
	// not listed and is fine.
	if !slices.Contains(res.Labeled, h.ID) {
		fresh, _, _ := Get[HostRecord](a.Store, CollHosts, HostKey(rec.Value.Swarm, h.ID), Consistent())
		if fresh.Value.AgentServiceID == "" {
			return fmt.Errorf("manager: node %q is not served by the agent service", h.ID)
		}
	}
	return nil
}

// StoreDedicatedKey seals the dedicated private key of the host.
func (a BootstrapManagerActions) StoreDedicatedKey(_ context.Context, h provision.Host, c provision.Credential) error {
	if c.PrivateKey == "" {
		return errors.New("manager: no dedicated key to store")
	}
	return PutSealed(a.Store, a.Keys, CollBootstrapHostKeys, h.ID, c)
}
