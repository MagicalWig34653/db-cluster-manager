// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

const collDenyState = "deny-state"
const denyStateKey = "revision"

type denyState struct {
	Revision int64 `json:"revision"`
}

// Deny-list distribution to the agents.
//
// The replicated CollRevocations holds every revoked serial: superseded
// and revoked agent certificates (the manager's own client rejects those)
// and revoked manager certificates (RevokeReplicaCertificates), which only
// the agents can reject. While a replica leads, DenyListDistributor sends
// the full list with agent command set-deny-list to every enrolled host
// whose acknowledged list digest differs, and records the digest on the
// host after the agent confirmed it. The revision is the newest revocation
// time, so an agent never replaces a newer list with an older one.

// DenyList is the distributable deny list.
type DenyList struct {
	Revision int64
	Serials  []string
	Digest   string
}

// CurrentDenyList builds the list from the replicated revocations.
func CurrentDenyList(s *Store) (DenyList, error) {
	revs, err := List[certs.Revocation](s, CollRevocations)
	if err != nil {
		return DenyList{}, err
	}
	var dl DenyList
	for _, r := range revs {
		serial := strings.ToLower(r.Key)
		dl.Serials = append(dl.Serials, serial)
		if t := r.Value.RevokedAt.UnixNano(); t > dl.Revision {
			dl.Revision = t
		}
	}
	state, ok, err := Get[denyState](s, collDenyState, denyStateKey)
	if err != nil {
		return DenyList{}, err
	}
	if ok && state.Value.Revision > dl.Revision {
		dl.Revision = state.Value.Revision
	}
	sort.Strings(dl.Serials)
	h := sha256.Sum256([]byte(strings.Join(dl.Serials, "\n")))
	dl.Digest = hex.EncodeToString(h[:])
	return dl, nil
}

// DenyListCompactor deletes only revocations whose actual certificate expiry
// is known and past. A durable revision fence lets agents accept the shorter
// list even when the newest revoked entry was among those removed.
type DenyListCompactor struct {
	Interval time.Duration
	Now      func() time.Time
}

func (DenyListCompactor) Name() string { return "deny-list-compaction" }

func (d DenyListCompactor) Run(ctx context.Context, l *Leadership) error {
	iv := d.Interval
	if iv <= 0 {
		iv = time.Hour
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if _, err := d.CompactOnce(l.svc.store); err != nil && ctx.Err() == nil {
			l.svc.log.Warn("manager: deny list compaction failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// CompactOnce removes up to 64 expired entries in one conditional transaction.
// Unknown expiry is retained, including records written by older versions.
func (d DenyListCompactor) CompactOnce(s *Store) (int, error) {
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	revs, err := List[certs.Revocation](s, CollRevocations, Consistent())
	if err != nil {
		return 0, err
	}
	dl, err := CurrentDenyList(s)
	if err != nil {
		return 0, err
	}
	state, ok, err := Get[denyState](s, collDenyState, denyStateKey, Consistent())
	if err != nil {
		return 0, err
	}
	var ops []metastore.DocOp
	for _, r := range revs {
		if r.Value.NotAfter.IsZero() || now().Before(r.Value.NotAfter) {
			continue
		}
		ops = append(ops, metastore.DocOp{Collection: CollRevocations, Key: r.Key, Delete: true, IfVersion: IfVersion(r.Version)})
		if len(ops) == 64 {
			break
		}
	}
	if len(ops) == 0 {
		return 0, nil
	}
	revision := now().UnixNano()
	if revision <= dl.Revision {
		revision = dl.Revision + 1
	}
	stateOp, err := PutOp(collDenyState, denyStateKey, denyState{Revision: revision}, IfVersion(state.Version))
	if err != nil {
		return 0, err
	}
	if !ok {
		stateOp.IfVersion = IfVersion(0)
	}
	ops = append(ops, stateOp)
	if _, err := s.Txn(ops...); err != nil {
		return 0, err
	}
	return len(ops) - 1, nil
}

// DenyListDistributor pushes the deny list to the agents while this
// replica leads.
type DenyListDistributor struct {
	Interval time.Duration // default 30s
	Timeout  time.Duration // per host, default 10s
}

// Name implements Reconciler.
func (DenyListDistributor) Name() string { return "deny-list" }

// Run implements Reconciler.
func (d DenyListDistributor) Run(ctx context.Context, l *Leadership) error {
	iv := d.Interval
	if iv <= 0 {
		iv = 30 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := d.DistributeOnce(ctx, l); err != nil && ctx.Err() == nil {
			l.svc.log.Info("manager: deny list not delivered everywhere", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// DistributeOnce sends the current list to every enrolled host that has
// not confirmed it.
func (d DenyListDistributor) DistributeOnce(ctx context.Context, l *Leadership) error {
	store := l.svc.store
	dl, err := CurrentDenyList(store)
	if err != nil {
		return err
	}
	hosts, err := List[HostRecord](store, CollHosts)
	if err != nil {
		return err
	}
	args, err := json.Marshal(agent.SetDenyListArgs{Revision: dl.Revision, Serials: append([]string{}, dl.Serials...)})
	if err != nil {
		return err
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	var errs []error
	for _, h := range hosts {
		hv := h.Value
		if hv.Enrollment != EnrollEnrolled || hv.AgentSerial == "" || hv.DenyListDigest == dl.Digest {
			continue
		}
		cmd := agent.Command{Term: l.Term, OpID: "deny-list-" + dl.Digest[:16], StepID: "host-" + hv.NodeID,
			Kind: agent.KindSetDenyList, Args: args}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		res, err := l.Agents.Send(cctx, hv.NodeID, cmd)
		cancel()
		if err == nil && res.Status != agent.StatusSucceeded {
			err = fmt.Errorf("%s: %s", res.Code, res.Message)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("host %s: %w", hv.NodeID, err))
			continue
		}
		if _, _, err := Update(store, CollHosts, h.Key, func(cur HostRecord, exists bool) (HostRecord, error) {
			if !exists {
				return cur, ErrNotFound
			}
			cur.DenyListDigest = dl.Digest
			return cur, nil
		}); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
