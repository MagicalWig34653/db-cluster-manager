// SPDX-License-Identifier: AGPL-3.0-only

// Package authz evaluates resource/action permissions (U02) with deny-by-default
// semantics. Roles are named sets of permissions (resource type + action,
// optionally a single object ID). A role takes effect only through a binding
// that attaches it to a subject at a scope: global, a Swarm, a cluster in a
// Swarm, or a single database in a cluster. A binding at a scope covers that
// scope and everything below it, never siblings.
//
// Resources owned by the manager itself (Resource.Protected) can never receive
// destructive actions through ordinary roles, including wildcard roles such as
// the Administrator preset. Only system roles, which can only be bound to the
// manager's own system subject, pass that rule.
//
// The package is a pure in-memory policy model. Persistence, replication and
// the HTTP layer are not part of it.
package authz

import (
	"errors"
	"fmt"
)

// Action is an operation on a resource.
type Action string

const (
	ActionRead              Action = "read"
	ActionCreate            Action = "create"
	ActionUpdate            Action = "update"
	ActionDelete            Action = "delete"
	ActionOperate           Action = "operate" // restart, failover, maintenance
	ActionBackup            Action = "backup"
	ActionRestore           Action = "restore"
	ActionManagePermissions Action = "manage_permissions"
	// ActionAny is a wildcard usable in permissions only, never in a check.
	ActionAny Action = "*"
)

var knownActions = map[Action]bool{
	ActionRead: true, ActionCreate: true, ActionUpdate: true, ActionDelete: true,
	ActionOperate: true, ActionBackup: true, ActionRestore: true,
	ActionManagePermissions: true,
}

// Valid reports whether a is a concrete, known action.
func (a Action) Valid() bool { return knownActions[a] }

// ChangesState reports whether the action can change state. Every decision
// for such an action produces an audit record.
func (a Action) ChangesState() bool { return a.Valid() && a != ActionRead }

// Destructive reports whether the action can destroy or overwrite a resource.
// Destructive actions on protected resources require a system role.
func (a Action) Destructive() bool {
	switch a {
	case ActionDelete, ActionUpdate, ActionRestore, ActionManagePermissions:
		return true
	}
	return false
}

// ResourceType classifies resources.
type ResourceType string

const (
	ResourceSwarm        ResourceType = "swarm"
	ResourceHost         ResourceType = "host"
	ResourceCluster      ResourceType = "cluster"
	ResourceDatabase     ResourceType = "database"
	ResourceDatabaseUser ResourceType = "database_user"
	ResourceBackup       ResourceType = "backup"
	ResourceSettings     ResourceType = "settings"
	ResourceDashboard    ResourceType = "dashboard"
	ResourceUserAccount  ResourceType = "user_account"
	ResourceRole         ResourceType = "role"
	ResourceAuditLog     ResourceType = "audit_log"
	// ResourceAny is a wildcard usable in permissions only, never in a check.
	ResourceAny ResourceType = "*"
)

var knownResourceTypes = map[ResourceType]bool{
	ResourceSwarm: true, ResourceHost: true, ResourceCluster: true,
	ResourceDatabase: true, ResourceDatabaseUser: true, ResourceBackup: true,
	ResourceSettings: true, ResourceDashboard: true, ResourceUserAccount: true,
	ResourceRole: true, ResourceAuditLog: true,
}

// Valid reports whether t is a concrete, known resource type.
func (t ResourceType) Valid() bool { return knownResourceTypes[t] }

// Level is the depth of a scope.
type Level int

const (
	LevelGlobal Level = iota
	LevelSwarm
	LevelCluster
	LevelDatabase
)

// Scope locates a resource in the Swarm/cluster/database hierarchy. Empty
// trailing fields mean "the whole level above"; the zero value is global.
// Cluster IDs are qualified by their Swarm, so equal cluster IDs in two
// Swarms are different scopes.
type Scope struct {
	Swarm    string `json:"swarm,omitempty"`
	Cluster  string `json:"cluster,omitempty"`
	Database string `json:"database,omitempty"`
}

// Global is the scope that covers everything.
var Global = Scope{}

// SwarmScope returns the scope of one Swarm.
func SwarmScope(swarm string) Scope { return Scope{Swarm: swarm} }

// ClusterScope returns the scope of one cluster.
func ClusterScope(swarm, cluster string) Scope { return Scope{Swarm: swarm, Cluster: cluster} }

// DatabaseScope returns the scope of one database.
func DatabaseScope(swarm, cluster, database string) Scope {
	return Scope{Swarm: swarm, Cluster: cluster, Database: database}
}

// ErrInvalidScope is returned for a scope with a gap, such as a cluster
// without a Swarm.
var ErrInvalidScope = errors.New("authz: invalid scope")

// Validate checks that the scope has no gaps.
func (s Scope) Validate() error {
	if s.Cluster != "" && s.Swarm == "" {
		return fmt.Errorf("%w: cluster %q without swarm", ErrInvalidScope, s.Cluster)
	}
	if s.Database != "" && s.Cluster == "" {
		return fmt.Errorf("%w: database %q without cluster", ErrInvalidScope, s.Database)
	}
	return nil
}

// Level returns the depth of the scope.
func (s Scope) Level() Level {
	switch {
	case s.Database != "":
		return LevelDatabase
	case s.Cluster != "":
		return LevelCluster
	case s.Swarm != "":
		return LevelSwarm
	}
	return LevelGlobal
}

// Covers reports whether s equals o or is an ancestor of o. Siblings are never
// covered. Both scopes must be valid.
func (s Scope) Covers(o Scope) bool {
	if s.Swarm == "" {
		return true
	}
	if s.Swarm != o.Swarm {
		return false
	}
	if s.Cluster == "" {
		return true
	}
	if s.Cluster != o.Cluster {
		return false
	}
	if s.Database == "" {
		return true
	}
	return s.Database == o.Database
}

func (s Scope) String() string {
	switch s.Level() {
	case LevelDatabase:
		return "swarm/" + s.Swarm + "/cluster/" + s.Cluster + "/database/" + s.Database
	case LevelCluster:
		return "swarm/" + s.Swarm + "/cluster/" + s.Cluster
	case LevelSwarm:
		return "swarm/" + s.Swarm
	}
	return "global"
}

// Resource is the target of a check. Scope is where the resource lives (a
// database's own scope is its database scope). ID identifies an object within
// the scope where needed, for example a backup or dashboard ID. Protected is
// set by the resource catalog for resources owned by the manager itself, such
// as replication and monitoring accounts or agent services.
type Resource struct {
	Type      ResourceType `json:"type"`
	Scope     Scope        `json:"scope"`
	ID        string       `json:"id,omitempty"`
	Protected bool         `json:"protected,omitempty"`
}

func (r Resource) String() string {
	s := string(r.Type) + "@" + r.Scope.String()
	if r.ID != "" {
		s += "#" + r.ID
	}
	return s
}

// Permission allows one action on one resource type, optionally restricted to
// a single object ID. Type and Action accept the wildcard "*". Where the
// permission applies is decided by the binding scope, not the permission.
type Permission struct {
	Type   ResourceType `json:"type"`
	Action Action       `json:"action"`
	ID     string       `json:"id,omitempty"`
}

// Validate checks the permission's fields.
func (p Permission) Validate() error {
	if p.Type != ResourceAny && !p.Type.Valid() {
		return fmt.Errorf("authz: unknown resource type %q", p.Type)
	}
	if p.Action != ActionAny && !p.Action.Valid() {
		return fmt.Errorf("authz: unknown action %q", p.Action)
	}
	return nil
}

func (p Permission) matches(t ResourceType, a Action, id string) bool {
	if p.Type != ResourceAny && p.Type != t {
		return false
	}
	if p.Action != ActionAny && p.Action != a {
		return false
	}
	return p.ID == "" || p.ID == id
}

// covers reports whether p allows at least everything q allows.
func (p Permission) covers(q Permission) bool {
	if p.Type != ResourceAny && p.Type != q.Type {
		return false
	}
	if p.Action != ActionAny && p.Action != q.Action {
		return false
	}
	return p.ID == "" || p.ID == q.ID
}

// SubjectKind distinguishes human users, automation rules and the manager.
type SubjectKind string

const (
	SubjectUser       SubjectKind = "user"
	SubjectAutomation SubjectKind = "automation"
	// SubjectSystem is the manager acting on its own behalf. Only it may hold
	// system roles.
	SubjectSystem SubjectKind = "system"
)

// Subject is an actor whose requests are checked.
type Subject struct {
	ID   string      `json:"id"`
	Kind SubjectKind `json:"kind"`
}

// Validate checks the subject's fields.
func (s Subject) Validate() error {
	if s.ID == "" {
		return errors.New("authz: empty subject ID")
	}
	switch s.Kind {
	case SubjectUser, SubjectAutomation, SubjectSystem:
		return nil
	}
	return fmt.Errorf("authz: unknown subject kind %q", s.Kind)
}

// Role is a named set of permissions. Preset roles are shipped with the
// manager and immutable; custom roles are user-defined. System roles may only
// be bound to SubjectSystem and are the only roles that may perform
// destructive actions on protected resources.
type Role struct {
	Name        string       `json:"name"`
	Permissions []Permission `json:"permissions"`
	Preset      bool         `json:"preset,omitempty"`
	System      bool         `json:"system,omitempty"`
}

// Binding attaches a role to a subject at a scope.
type Binding struct {
	Subject Subject `json:"subject"`
	Role    string  `json:"role"`
	Scope   Scope   `json:"scope"`
}

// Grant is one effective permission of a subject.
type Grant struct {
	Permission Permission `json:"permission"`
	Scope      Scope      `json:"scope"`
	Role       string     `json:"role"`
}
