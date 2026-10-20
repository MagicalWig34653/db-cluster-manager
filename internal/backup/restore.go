// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Section names used in summaries and plans.
const (
	SectionUsers       = "users"
	SectionPermissions = "permissions"
	SectionClusters    = "clusters"
	SectionSecrets     = "secrets"
	SectionSettings    = "settings"
)

// Mode is the operating state of a manager after a restore.
type Mode string

const (
	// ModeInspection means the manager has loaded restored metadata but must
	// not manage anything: it only reconciles with live state (B09).
	ModeInspection Mode = "inspection"
	// ModeResumed is only reachable through ConfirmResume.
	ModeResumed Mode = "resumed"
)

// Item identifies one object by ID and a content digest. Digests let a plan
// detect modified objects without exposing contents.
type Item struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

// Summary is the redacted result of inspecting a backup. It contains only
// IDs, digests and counts; no secret value and no record content.
type Summary struct {
	Header      Header
	Users       []Item
	Permissions []Item
	Clusters    []Item
	Secrets     []Item
	Settings    []Item
}

// Counts returns the number of items per section.
func (s Summary) Counts() map[string]int {
	return map[string]int{
		SectionUsers: len(s.Users), SectionPermissions: len(s.Permissions),
		SectionClusters: len(s.Clusters), SectionSecrets: len(s.Secrets),
		SectionSettings: len(s.Settings),
	}
}

// Digest returns the content digest used in Items.
func Digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func recordItems(rs []Record) []Item {
	out := make([]Item, 0, len(rs))
	for _, r := range rs {
		out = append(out, Item{ID: r.ID, Digest: Digest(r.Data)})
	}
	sortItems(out)
	return out
}

func sortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
}

// Inspect decrypts a backup and returns a redacted summary without applying
// anything.
func Inspect(data []byte, key RecoveryKey) (Summary, error) {
	s, h, err := decodeSnapshot(data, key)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{
		Header:      h,
		Users:       recordItems(s.Users),
		Permissions: recordItems(s.Permissions),
		Clusters:    recordItems(s.Clusters),
		Secrets:     make([]Item, 0, len(s.Secrets)),
		Settings:    make([]Item, 0, len(s.Settings)),
	}
	for _, sec := range s.Secrets {
		sum.Secrets = append(sum.Secrets, Item{ID: sec.ID, Digest: Digest(sec.Value)})
	}
	for k, v := range s.Settings {
		sum.Settings = append(sum.Settings, Item{ID: k, Digest: Digest([]byte(v))})
	}
	sortItems(sum.Secrets)
	sortItems(sum.Settings)
	return sum, nil
}

// LiveState is the currently observed state the backup is compared with.
type LiveState struct {
	Users       []Item
	Permissions []Item
	Clusters    []Item
	Secrets     []Item
	Settings    []Item
	// MaxTerm is the highest fencing term observed in live state.
	MaxTerm uint64
}

// ChangeKind describes how applying the backup would change live state.
type ChangeKind string

const (
	// ChangeAdd: present in the backup, missing live.
	ChangeAdd ChangeKind = "add"
	// ChangeRemove: present live, missing in the backup (would be dropped).
	ChangeRemove ChangeKind = "remove"
	// ChangeModify: present in both with different content.
	ChangeModify ChangeKind = "modify"
)

// Change is one difference between backup and live state.
type Change struct {
	Section string     `json:"section"`
	ID      string     `json:"id"`
	Kind    ChangeKind `json:"kind"`
}

// Plan is the diff that must be explicitly approved before management
// resumes. It never applies anything itself.
type Plan struct {
	Changes []Change
	// BackupTerm is the metastore term recorded in the backup.
	BackupTerm uint64
	// LiveTerm is the highest term observed live.
	LiveTerm uint64
}

// Digest identifies the plan content for approval binding.
func (p Plan) Digest() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d\n", p.BackupTerm, p.LiveTerm)
	for _, c := range p.Changes {
		fmt.Fprintf(&b, "%s/%s/%s\n", c.Section, c.Kind, c.ID)
	}
	return Digest([]byte(b.String()))
}

// PlanRestore diffs the backup summary against live state.
func PlanRestore(sum Summary, live LiveState) Plan {
	p := Plan{BackupTerm: sum.Header.Meta.MetaTerm, LiveTerm: live.MaxTerm}
	for _, sec := range []struct {
		name         string
		backup, live []Item
	}{
		{SectionUsers, sum.Users, live.Users},
		{SectionPermissions, sum.Permissions, live.Permissions},
		{SectionClusters, sum.Clusters, live.Clusters},
		{SectionSecrets, sum.Secrets, live.Secrets},
		{SectionSettings, sum.Settings, live.Settings},
	} {
		p.Changes = append(p.Changes, diff(sec.name, sec.backup, sec.live)...)
	}
	return p
}

func diff(section string, backup, live []Item) []Change {
	lm := make(map[string]string, len(live))
	for _, i := range live {
		lm[i.ID] = i.Digest
	}
	bm := make(map[string]string, len(backup))
	var out []Change
	for _, i := range backup {
		bm[i.ID] = i.Digest
		d, ok := lm[i.ID]
		switch {
		case !ok:
			out = append(out, Change{section, i.ID, ChangeAdd})
		case d != i.Digest:
			out = append(out, Change{section, i.ID, ChangeModify})
		}
	}
	for _, i := range live {
		if _, ok := bm[i.ID]; !ok {
			out = append(out, Change{section, i.ID, ChangeRemove})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].ID != out[b].ID {
			return out[a].ID < out[b].ID
		}
		return out[a].Kind < out[b].Kind
	})
	return out
}

// Result is the outcome of a restore load. The manager is in inspection mode
// and must stay there until ConfirmResume succeeds.
type Result struct {
	Mode Mode
	Plan Plan
	// RequiredTerm is the minimum new fencing term; it is strictly greater
	// than every term in the backup and in live state.
	RequiredTerm uint64
	// ApprovalRequired is always true: there is no automatic resumption.
	ApprovalRequired bool
}

// BeginRestore turns a plan into a restore result in inspection mode.
func BeginRestore(p Plan) Result {
	t := max(p.BackupTerm, p.LiveTerm) + 1
	return Result{Mode: ModeInspection, Plan: p, RequiredTerm: t, ApprovalRequired: true}
}

// Approval is the explicit operator confirmation to resume management.
type Approval struct {
	ApprovedBy string
	// PlanDigest must equal Plan.Digest() of the reviewed plan.
	PlanDigest string
	// NewTerm is the fencing term the manager will assume; it must be
	// at least Result.RequiredTerm.
	NewTerm uint64
}

// ErrApproval reports an invalid or insufficient approval.
var ErrApproval = errors.New("backup: invalid restore approval")

// ConfirmResume validates an approval. On success it returns the resumed mode
// and the term the caller must durably commit before issuing any command.
// This package performs no state change.
func ConfirmResume(r Result, a Approval) (Mode, uint64, error) {
	switch {
	case r.Mode != ModeInspection:
		return r.Mode, 0, fmt.Errorf("%w: not in inspection mode", ErrApproval)
	case a.ApprovedBy == "":
		return r.Mode, 0, fmt.Errorf("%w: missing approver", ErrApproval)
	case a.PlanDigest != r.Plan.Digest():
		return r.Mode, 0, fmt.Errorf("%w: plan digest mismatch", ErrApproval)
	case a.NewTerm < r.RequiredTerm:
		return r.Mode, 0, fmt.Errorf("%w: term %d below required %d", ErrApproval, a.NewTerm, r.RequiredTerm)
	}
	return ModeResumed, a.NewTerm, nil
}
