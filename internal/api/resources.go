// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// Resource services. The API only defines the interfaces; real stores
// (Swarm inventory, operation journal, event log) are not wired yet.
// Implementations report what they observed. The API never fills in a
// state: a missing or unrecognized state is returned as "unknown".

var (
	// ErrNotFound is returned by services for a missing object.
	ErrNotFound = errors.New("api: not found")
	// ErrUnavailable is returned by services that cannot answer.
	ErrUnavailable = errors.New("api: service unavailable")
)

// State is the observed health of a Swarm, cluster or node.
type State string

const (
	StateHealthy     State = "healthy"
	StateDegraded    State = "degraded"
	StateUnavailable State = "unavailable"
	StateUnknown     State = "unknown"
)

func normalizeState(s State) State {
	switch s {
	case StateHealthy, StateDegraded, StateUnavailable:
		return s
	}
	return StateUnknown
}

// OperationState is the lifecycle state of an operation.
type OperationState string

const (
	OperationPending   OperationState = "pending"
	OperationRunning   OperationState = "running"
	OperationSucceeded OperationState = "succeeded"
	OperationFailed    OperationState = "failed"
	OperationUnknown   OperationState = "unknown"
)

func normalizeOperationState(s OperationState) OperationState {
	switch s {
	case OperationPending, OperationRunning, OperationSucceeded, OperationFailed:
		return s
	}
	return OperationUnknown
}

// Severity classifies an event.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
	SeverityUnknown  Severity = "unknown"
)

func normalizeSeverity(s Severity) Severity {
	switch s {
	case SeverityInfo, SeverityWarning, SeverityCritical:
		return s
	}
	return SeverityUnknown
}

// Swarm is one managed Docker Swarm.
type Swarm struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	State      State     `json:"state"`
	ObservedAt time.Time `json:"observedAt,omitzero"`
}

// ClusterNode is one database node of a cluster. Role is the node's
// Galera role from its last fresh observation (NodeRole*); it is
// NodeRoleUnknown when the observation is missing or stale. ObservedAt is
// the time of the last observation, fresh or not.
type ClusterNode struct {
	ID         string    `json:"id"`
	Host       string    `json:"host,omitempty"`
	State      State     `json:"state"`
	Role       string    `json:"role"`
	ObservedAt time.Time `json:"observedAt,omitzero"`
}

// Galera roles of a cluster node.
const (
	NodeRoleSynced      = "synced"
	NodeRoleDonor       = "donor"
	NodeRoleJoining     = "joining"
	NodeRoleJoined      = "joined"
	NodeRoleInitialized = "initialized"
	NodeRoleUnknown     = "unknown"
)

// Cluster is one database cluster. ID is unique within the manager; Swarm
// qualifies it for authorization.
type Cluster struct {
	ID         string        `json:"id"`
	Swarm      string        `json:"swarm"`
	Name       string        `json:"name"`
	Engine     string        `json:"engine,omitempty"`
	State      State         `json:"state"`
	Nodes      []ClusterNode `json:"nodes"`
	ObservedAt time.Time     `json:"observedAt,omitzero"`
	// Endpoints are the client addresses of the cluster as configured (not
	// a reachability claim). Absent means not reported.
	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// LastBackupAt is the end of the newest successful backup operation,
	// LastVerifiedRestoreAt the newest passed restore verification. Absent
	// means unknown, never "none".
	LastBackupAt          time.Time `json:"lastBackupAt,omitzero"`
	LastVerifiedRestoreAt time.Time `json:"lastVerifiedRestoreAt,omitzero"`
	// AutomationPolicy is "auto-replace" or "manual" (automatic node
	// replacement; the quorum rules apply either way).
	AutomationPolicy string `json:"automationPolicy,omitempty"`
}

// Endpoint is one client address of a cluster.
type Endpoint struct {
	Name string `json:"name,omitempty"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Operation is one journaled management operation. Swarm and Cluster locate
// it; both empty means a manager-level operation.
type Operation struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Swarm     string         `json:"swarm,omitempty"`
	Cluster   string         `json:"cluster,omitempty"`
	State     OperationState `json:"state"`
	StartedAt time.Time      `json:"startedAt,omitzero"`
	EndedAt   time.Time      `json:"endedAt,omitzero"`
	// Approval is set for operations whose destructive steps wait for an
	// explicit approval (POST /operations/{id}/approve).
	Approval *OperationApproval `json:"approval,omitempty"`
	// Reason is the runner's short status reason (for example the unmet
	// precondition of a paused operation).
	Reason string `json:"reason,omitempty"`
	// Progress is the step progress as last stored by the leader (absent
	// before the operation started).
	Progress *OperationProgress `json:"progress,omitempty"`
}

// OperationProgress shows where an operation is, also while a step polls
// its preconditions (lab E4: the reason stayed stale during long gates).
type OperationProgress struct {
	StepsDone  int `json:"stepsDone"`
	StepsTotal int `json:"stepsTotal"`
	// Step is the first step that has not succeeded, with its command and
	// target host (empty for a gate) and its attempts.
	Step     string `json:"step,omitempty"`
	Command  string `json:"command,omitempty"`
	Target   string `json:"target,omitempty"`
	State    string `json:"state,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	// Waiting is the latest unmet precondition or retry reason of Step;
	// Polls counts its precondition polls and retries; Since is the time of
	// its latest timeline entry.
	Waiting string    `json:"waiting,omitempty"`
	Polls   int       `json:"polls,omitempty"`
	Since   time.Time `json:"since,omitzero"`
	// UpdatedAt is when the leader stored this view.
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// Event is one observed event, located like an Operation.
type Event struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Swarm    string    `json:"swarm,omitempty"`
	Cluster  string    `json:"cluster,omitempty"`
	Severity Severity  `json:"severity"`
	Message  string    `json:"message"`
}

// Inventory provides Swarms and clusters.
type Inventory interface {
	Swarms(ctx context.Context) ([]Swarm, error)
	Clusters(ctx context.Context) ([]Cluster, error)
	// Cluster returns ErrNotFound for an unknown ID.
	Cluster(ctx context.Context, id string) (Cluster, error)
}

// Activity provides operations and events.
type Activity interface {
	Operations(ctx context.Context) ([]Operation, error)
	Events(ctx context.Context) ([]Event, error)
}

// Unavailable implements Inventory and Activity for a manager without a
// backing store: every call fails with ErrUnavailable, so clients see 503
// instead of an empty list that would read as "nothing exists".
type Unavailable struct{}

func (Unavailable) Swarms(context.Context) ([]Swarm, error)     { return nil, ErrUnavailable }
func (Unavailable) Clusters(context.Context) ([]Cluster, error) { return nil, ErrUnavailable }
func (Unavailable) Cluster(context.Context, string) (Cluster, error) {
	return Cluster{}, ErrUnavailable
}
func (Unavailable) Operations(context.Context) ([]Operation, error) { return nil, ErrUnavailable }
func (Unavailable) Events(context.Context) ([]Event, error)         { return nil, ErrUnavailable }

func swarmResource(sw Swarm) authz.Resource {
	return authz.Resource{Type: authz.ResourceSwarm, Scope: authz.SwarmScope(sw.ID)}
}

func clusterResource(c Cluster) authz.Resource {
	return authz.Resource{Type: authz.ResourceCluster, Scope: authz.ClusterScope(c.Swarm, c.ID)}
}

// locatedResource maps an operation or event to the resource whose read
// permission it needs: the cluster, else the Swarm, else the manager's
// settings for manager-level entries.
func locatedResource(swarm, cluster string) authz.Resource {
	switch {
	case swarm != "" && cluster != "":
		return authz.Resource{Type: authz.ResourceCluster, Scope: authz.ClusterScope(swarm, cluster)}
	case swarm != "":
		return authz.Resource{Type: authz.ResourceSwarm, Scope: authz.SwarmScope(swarm)}
	}
	return authz.Resource{Type: authz.ResourceSettings, Scope: authz.Global}
}

func normalizeCluster(c Cluster) Cluster {
	c.State = normalizeState(c.State)
	nodes := make([]ClusterNode, len(c.Nodes))
	for i, n := range c.Nodes {
		n.State = normalizeState(n.State)
		nodes[i] = n
	}
	c.Nodes = nodes
	return c
}

// List is the envelope of every listing.
type List[T any] struct {
	Items []T `json:"items"`
}

func (s *server) handleSwarms(w http.ResponseWriter, r *http.Request, c *caller) {
	items, err := s.cfg.Inventory.Swarms(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, swarmResource)
	for i := range items {
		items[i].State = normalizeState(items[i].State)
	}
	writeJSON(w, http.StatusOK, List[Swarm]{Items: items})
}

func (s *server) handleClusters(w http.ResponseWriter, r *http.Request, c *caller) {
	items, err := s.cfg.Inventory.Clusters(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, clusterResource)
	for i := range items {
		items[i] = normalizeCluster(items[i])
	}
	writeJSON(w, http.StatusOK, List[Cluster]{Items: items})
}

func (s *server) resolveCluster(r *http.Request) (authz.Resource, any, error) {
	cl, err := s.cfg.Inventory.Cluster(r.Context(), r.PathValue("id"))
	if err != nil {
		return authz.Resource{}, nil, err
	}
	return clusterResource(cl), cl, nil
}

func (s *server) handleCluster(w http.ResponseWriter, _ *http.Request, c *caller) {
	writeJSON(w, http.StatusOK, normalizeCluster(c.target.(Cluster)))
}

func (s *server) handleOperations(w http.ResponseWriter, r *http.Request, c *caller) {
	items, err := s.cfg.Activity.Operations(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, func(o Operation) authz.Resource {
		return locatedResource(o.Swarm, o.Cluster)
	})
	for i := range items {
		items[i].State = normalizeOperationState(items[i].State)
	}
	writeJSON(w, http.StatusOK, List[Operation]{Items: items})
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request, c *caller) {
	items, err := s.cfg.Activity.Events(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, func(e Event) authz.Resource {
		return locatedResource(e.Swarm, e.Cluster)
	})
	for i := range items {
		items[i].Severity = normalizeSeverity(items[i].Severity)
	}
	writeJSON(w, http.StatusOK, List[Event]{Items: items})
}
