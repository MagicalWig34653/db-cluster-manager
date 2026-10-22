// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Catalog is schema metadata: captured from the source with a backup, or
// read from a live target before a selective restore. It never contains
// password hashes; accounts carry only a digest of their authentication.
type Catalog struct {
	Schemas  []Schema  `json:"schemas"`
	Accounts []Account `json:"accounts,omitempty"`
}

// Schema is one database.
type Schema struct {
	Name     string   `json:"name"`
	Tables   []Table  `json:"tables"`
	Views    []string `json:"views,omitempty"`
	Routines []string `json:"routines,omitempty"`
}

// Table is one base table.
type Table struct {
	Name        string       `json:"name"`
	Rows        int64        `json:"rows"` // estimate
	ForeignKeys []ForeignKey `json:"foreign_keys,omitempty"`
	// UpdateTime is the last write the server reports (may be absent).
	UpdateTime *time.Time `json:"update_time,omitempty"`
	// Checksum is a CHECKSUM TABLE baseline if one was captured.
	Checksum *uint64 `json:"checksum,omitempty"`
}

// ForeignKey references another table.
type ForeignKey struct {
	Name      string `json:"name"`
	RefSchema string `json:"ref_schema"`
	RefTable  string `json:"ref_table"`
}

// Account is a database account with its schema-level grants.
type Account struct {
	User       string `json:"user"`
	Host       string `json:"host"`
	AuthDigest string `json:"auth_digest,omitempty"` // sha256 of plugin+auth string, never the hash itself
	// Schemas lists the databases the account has grants on.
	Schemas []string `json:"schemas,omitempty"`
}

func (c *Catalog) schema(name string) *Schema {
	if c == nil {
		return nil
	}
	for i := range c.Schemas {
		if c.Schemas[i].Name == name {
			return &c.Schemas[i]
		}
	}
	return nil
}

func (s *Schema) table(name string) *Table {
	for i := range s.Tables {
		if s.Tables[i].Name == name {
			return &s.Tables[i]
		}
	}
	return nil
}

// Severity of a selective restore conflict.
type Severity string

// Severities. Blockers prevent planning; warnings need the approval that
// covers the preview digest; info is shown only.
const (
	SeverityBlocker Severity = "blocker"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// ConflictKind classifies a conflict.
type ConflictKind string

// Conflict kinds.
const (
	ConflictSchemaExists      ConflictKind = "schema-exists"
	ConflictTableReplaced     ConflictKind = "table-replaced"
	ConflictTableRetained     ConflictKind = "table-retained"
	ConflictObjectReplaced    ConflictKind = "object-replaced" // view or routine
	ConflictMissingDependency ConflictKind = "missing-dependency"
	ConflictTargetActive      ConflictKind = "target-active"
	ConflictAccountMissing    ConflictKind = "account-missing"
	ConflictAccountDiffers    ConflictKind = "account-differs"
	ConflictNoExport          ConflictKind = "no-export"
)

// Conflict is one finding of the preview.
type Conflict struct {
	Kind     ConflictKind `json:"kind"`
	Severity Severity     `json:"severity"`
	Schema   string       `json:"schema"`
	Object   string       `json:"object,omitempty"`
	Detail   string       `json:"detail"`
}

// Mapping restores the export of Source into Target.
type Mapping struct {
	Source string `json:"source"`
	Target string `json:"target"`
	// Replace allows writing into an existing target schema; the export's
	// tables are dropped and recreated there.
	Replace bool `json:"replace,omitempty"`
	// AcceptActive acknowledges recent writes to the target schema.
	AcceptActive bool `json:"accept_active,omitempty"`
}

// SelectiveRequest asks for a preview.
type SelectiveRequest struct {
	BackupID        string
	SourceClusterID string
	TargetClusterID string
	// Source is the catalog captured with the backup; Exports maps a source
	// database to its logical export object.
	Source   Catalog
	Exports  map[string]string
	Target   Catalog
	Mappings []Mapping
	// ActiveWindow: target tables written within it count as active.
	// Default 15 minutes.
	ActiveWindow time.Duration
	Now          time.Time
}

// Action is one effect the restore will have.
type Action struct {
	Kind   string   `json:"kind"` // "create-schema", "load-into-new", "replace-tables"
	Schema string   `json:"schema"`
	Source string   `json:"source"`
	Object string   `json:"object"`
	Tables []string `json:"tables,omitempty"`
}

// Preview is the diff a user approves. Digest covers everything else.
type Preview struct {
	BackupID        string     `json:"backup_id"`
	SourceClusterID string     `json:"source_cluster_id"`
	TargetClusterID string     `json:"target_cluster_id"`
	Mappings        []Mapping  `json:"mappings"`
	Actions         []Action   `json:"actions"`
	Conflicts       []Conflict `json:"conflicts"`
	Digest          string     `json:"digest"`
}

// Blocked reports whether the preview has blockers.
func (p Preview) Blocked() bool {
	return slices.ContainsFunc(p.Conflicts, func(c Conflict) bool { return c.Severity == SeverityBlocker })
}

var schemaNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

var systemSchemas = map[string]bool{"mysql": true, "sys": true, "information_schema": true, "performance_schema": true}

// PreviewSelective computes what restoring the mapped databases would do to
// the target and which conflicts need a decision. It is pure.
func PreviewSelective(req SelectiveRequest) (Preview, error) {
	if req.BackupID == "" || req.SourceClusterID == "" || req.TargetClusterID == "" || len(req.Mappings) == 0 {
		return Preview{}, fmt.Errorf("%w: backup, clusters and mappings required", ErrInvalidRequest)
	}
	window := req.ActiveWindow
	if window <= 0 {
		window = 15 * time.Minute
	}
	maps := slices.Clone(req.Mappings)
	sort.Slice(maps, func(i, j int) bool { return maps[i].Target < maps[j].Target })
	targets := map[string]bool{}
	selected := map[string]bool{} // target schema names after restore
	for _, m := range maps {
		if !schemaNameRe.MatchString(m.Source) || !schemaNameRe.MatchString(m.Target) || systemSchemas[m.Target] {
			return Preview{}, fmt.Errorf("%w: mapping %s -> %s", ErrInvalidRequest, m.Source, m.Target)
		}
		if targets[m.Target] {
			return Preview{}, fmt.Errorf("%w: target %s mapped twice", ErrInvalidRequest, m.Target)
		}
		targets[m.Target] = true
		selected[m.Source] = true
	}
	p := Preview{BackupID: req.BackupID, SourceClusterID: req.SourceClusterID, TargetClusterID: req.TargetClusterID, Mappings: maps}
	add := func(k ConflictKind, sev Severity, schema, obj, f string, a ...any) {
		p.Conflicts = append(p.Conflicts, Conflict{Kind: k, Severity: sev, Schema: schema, Object: obj, Detail: fmt.Sprintf(f, a...)})
	}
	renamed := map[string]string{}
	for _, m := range maps {
		renamed[m.Source] = m.Target
	}
	for _, m := range maps {
		src := req.Source.schema(m.Source)
		obj := req.Exports[m.Source]
		if src == nil || obj == "" {
			add(ConflictNoExport, SeverityBlocker, m.Target, "", "the backup has no logical export of %s", m.Source)
			continue
		}
		tables := make([]string, 0, len(src.Tables))
		for _, t := range src.Tables {
			tables = append(tables, t.Name)
		}
		slices.Sort(tables)
		dst := req.Target.schema(m.Target)
		if dst == nil {
			p.Actions = append(p.Actions, Action{Kind: "create-schema", Schema: m.Target, Source: m.Source, Object: obj})
			p.Actions = append(p.Actions, Action{Kind: "load-into-new", Schema: m.Target, Source: m.Source, Object: obj, Tables: tables})
		} else {
			if !m.Replace {
				add(ConflictSchemaExists, SeverityBlocker, m.Target, "", "schema %s exists in the target; choose another target name or allow replacing", m.Target)
			} else {
				add(ConflictSchemaExists, SeverityWarning, m.Target, "", "schema %s exists and will be written", m.Target)
			}
			var replaced []string
			for _, t := range tables {
				if tt := dst.table(t); tt != nil {
					replaced = append(replaced, t)
					add(ConflictTableReplaced, SeverityWarning, m.Target, t, "table %s.%s (%d rows) is dropped and recreated from the backup", m.Target, t, tt.Rows)
				}
			}
			for _, t := range dst.Tables {
				if !slices.Contains(tables, t.Name) {
					add(ConflictTableRetained, SeverityInfo, m.Target, t.Name, "table %s.%s is not in the backup and stays unchanged", m.Target, t.Name)
				}
				if t.UpdateTime != nil && req.Now.Sub(*t.UpdateTime) < window {
					sev := SeverityBlocker
					if m.AcceptActive {
						sev = SeverityWarning
					}
					add(ConflictTargetActive, sev, m.Target, t.Name, "table %s.%s was written at %s; stop writers or acknowledge", m.Target, t.Name, t.UpdateTime.UTC().Format(time.RFC3339))
				}
			}
			for _, v := range slices.Concat(src.Views, src.Routines) {
				if slices.Contains(dst.Views, v) || slices.Contains(dst.Routines, v) {
					add(ConflictObjectReplaced, SeverityWarning, m.Target, v, "view or routine %s.%s is replaced", m.Target, v)
				}
			}
			p.Actions = append(p.Actions, Action{Kind: "replace-tables", Schema: m.Target, Source: m.Source, Object: obj, Tables: replaced})
		}
		// Dependencies: a foreign key must resolve after the restore, either
		// inside the restored set or in the target as it stays.
		for _, t := range src.Tables {
			for _, fk := range t.ForeignKeys {
				refSchema := fk.RefSchema
				if refSchema == "" || refSchema == m.Source {
					refSchema = m.Source
				}
				if selected[refSchema] {
					rs := req.Source.schema(refSchema)
					if rs != nil && rs.table(fk.RefTable) != nil {
						if refSchema != m.Source && renamed[refSchema] != refSchema {
							add(ConflictMissingDependency, SeverityBlocker, m.Target, t.Name,
								"foreign key %s references %s.%s, which is restored under another name (%s); the dump keeps the original name", fk.Name, refSchema, fk.RefTable, renamed[refSchema])
						}
						continue
					}
				}
				if ts := req.Target.schema(refSchema); ts != nil && ts.table(fk.RefTable) != nil {
					continue
				}
				add(ConflictMissingDependency, SeverityBlocker, m.Target, t.Name,
					"foreign key %s references %s.%s, which is neither restored nor present in the target", fk.Name, refSchema, fk.RefTable)
			}
		}
		// Accounts are never created or changed by a selective restore.
		for _, a := range req.Source.Accounts {
			if !slices.Contains(a.Schemas, m.Source) {
				continue
			}
			var found *Account
			for i := range req.Target.Accounts {
				t := &req.Target.Accounts[i]
				if t.User == a.User && t.Host == a.Host {
					found = t
				}
			}
			acct := a.User + "@" + a.Host
			switch {
			case found == nil:
				add(ConflictAccountMissing, SeverityInfo, m.Target, acct, "account %s had grants on %s in the source and does not exist in the target; it is not created", acct, m.Source)
			case found.AuthDigest != a.AuthDigest:
				add(ConflictAccountDiffers, SeverityInfo, m.Target, acct, "account %s exists in the target with other credentials; it is not changed", acct)
			}
		}
	}
	p.Digest = previewDigest(p)
	return p, nil
}

func previewDigest(p Preview) string {
	p.Digest = ""
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Approval is a user's explicit decision on a preview.
type Approval struct {
	ID       string    `json:"id"`
	Digest   string    `json:"digest"`
	By       string    `json:"by"`
	Approved time.Time `json:"approved"`
}

// ErrApprovalMismatch: the approval was given for another preview.
var ErrApprovalMismatch = errors.New("dbbackup: approval does not match the preview")

// ErrBlocked: the preview has blockers.
var ErrBlocked = errors.New("dbbackup: preview has unresolved blockers")

// SelectiveBindings supply runtime facts for the selective restore plan.
type SelectiveBindings struct {
	OpID               string
	Writer             NodeState // the target cluster's current writer
	ClusterName        string    // target cluster name (for the writer hostname)
	JobImage           string
	Network            string
	DBCredentialSecret string
	BackendSecret      string
	KeySecret          string
	Job                agent.JobResources
	StepTimeout        time.Duration
}

// SelectivePlan plans the approved selective restore: per mapping one
// logical restore job against the target's writer. It refuses previews with
// blockers and approvals for another digest (the preview must be recomputed
// and approved again if anything changed).
func SelectivePlan(p Preview, a Approval, b SelectiveBindings) (runner.Operation, error) {
	if a.ID == "" || a.By == "" {
		return runner.Operation{}, ErrApprovalRequired
	}
	if p.Digest == "" || previewDigest(p) != p.Digest || a.Digest != p.Digest {
		return runner.Operation{}, ErrApprovalMismatch
	}
	if p.Blocked() {
		return runner.Operation{}, ErrBlocked
	}
	if b.OpID == "" || b.Writer.HostID == "" || b.ClusterName == "" || b.JobImage == "" || b.Network == "" ||
		b.DBCredentialSecret == "" || b.BackendSecret == "" || b.KeySecret == "" || b.Job.NanoCPUs <= 0 || b.Job.MemoryBytes <= 0 {
		return runner.Operation{}, fmt.Errorf("%w: selective bindings incomplete", ErrInvalidRequest)
	}
	s := &steps{b: BackupRequest{StepTimeout: b.StepTimeout}}
	s.add(gate("approved", runner.Condition{Kind: CondApproved, Target: a.ID}))
	host := galera.ServiceName(b.ClusterName, b.Writer.Slot)
	for _, act := range p.Actions {
		if act.Kind == "create-schema" {
			continue // the job creates a missing schema before loading
		}
		job := JobName(p.BackupID, "sr-"+act.Schema)
		s.add(command("restore-"+act.Schema, b.Writer.HostID, agent.KindRunRestore, agent.RunRestoreArgs{
			JobName: job, Image: b.JobImage, Mode: agent.RestoreLogical, BackupID: p.BackupID, Object: act.Object,
			ApprovalID: a.ID, SourceClusterID: p.SourceClusterID, Database: act.Source, TargetDatabase: act.Schema,
			DBHost: host, Network: b.Network, DBCredentialSecret: b.DBCredentialSecret, BackendSecret: b.BackendSecret,
			KeySecret: b.KeySecret, Resources: b.Job,
		}), runner.Condition{Kind: runner.CondNodeSynced, Target: b.Writer.Slot})
		s.add(gate("wait-restore-"+act.Schema, runner.Condition{Kind: CondRestoreComplete, Target: job}))
		s.add(command("remove-job-"+act.Schema, b.Writer.HostID, agent.KindRemoveJob, agent.RemoveJobArgs{JobName: job}))
	}
	op := runner.Operation{ID: b.OpID, Kind: OpSelective, Locks: []string{"cluster/" + p.TargetClusterID}, Steps: s.list}
	requireApproval(&op, a.ID)
	if err := op.Validate(); err != nil {
		return runner.Operation{}, err
	}
	return op, nil
}
