// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"slices"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// Handlers of the command endpoints (hosts, Swarms, clusters, operations,
// backups, settings). Every handler runs after wrap has checked the origin,
// the session and the declared permission (audited for state changes).

func hostResource(h Host) authz.Resource {
	return authz.Resource{Type: authz.ResourceHost, Scope: authz.SwarmScope(h.Swarm), ID: h.NodeID}
}

func backupResource(swarm, cluster string) authz.Resource {
	return authz.Resource{Type: authz.ResourceBackup, Scope: authz.ClusterScope(swarm, cluster)}
}

var settingsResource = authz.Resource{Type: authz.ResourceSettings, Scope: authz.Global}

func normalizeHost(h Host) Host {
	switch h.Enrollment {
	case EnrollmentDiscovered, EnrollmentEnrolled, EnrollmentRevoked:
	default:
		// Not managed unless the store says so.
		h.Enrollment = EnrollmentDiscovered
	}
	return h
}

func (s *server) handleHosts(w http.ResponseWriter, r *http.Request, c *caller) {
	items, err := s.control().Hosts(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, hostResource)
	for i := range items {
		items[i] = normalizeHost(items[i])
	}
	writeJSON(w, http.StatusOK, List[Host]{Items: items})
}

// resolveHost finds a host by its Swarm node ID. Node IDs are random and
// unique in practice; a duplicate across Swarms is refused as a conflict
// rather than guessed.
func (s *server) resolveHost(r *http.Request) (authz.Resource, any, error) {
	hosts, err := s.control().Hosts(r.Context())
	if err != nil {
		return authz.Resource{}, nil, err
	}
	id := r.PathValue("id")
	var found []Host
	for _, h := range hosts {
		if h.NodeID == id {
			found = append(found, h)
		}
	}
	switch len(found) {
	case 0:
		return authz.Resource{}, nil, ErrNotFound
	case 1:
		return hostResource(found[0]), found[0], nil
	}
	return authz.Resource{}, nil, ErrConflict
}

func (s *server) handleHostEnrollment(enroll bool) func(http.ResponseWriter, *http.Request, *caller) {
	return func(w http.ResponseWriter, r *http.Request, c *caller) {
		var empty struct{}
		if !decodeJSON(w, r, &empty, true) {
			return
		}
		h := c.target.(Host)
		out, err := s.control().SetEnrollment(r.Context(), c.subject, h.Swarm, h.NodeID, enroll)
		if err != nil {
			s.writeServiceError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, normalizeHost(out))
	}
}

func (s *server) resolveNewSwarm(*http.Request) (authz.Resource, any, error) {
	return authz.Resource{Type: authz.ResourceSwarm, Scope: authz.Global}, nil, nil
}

func (s *server) resolveSwarm(r *http.Request) (authz.Resource, any, error) {
	items, err := s.cfg.Inventory.Swarms(r.Context())
	if err != nil {
		return authz.Resource{}, nil, err
	}
	id := r.PathValue("id")
	for _, sw := range items {
		if sw.ID == id {
			return swarmResource(sw), sw, nil
		}
	}
	return authz.Resource{}, nil, ErrNotFound
}

func (s *server) handleSwarmConnect(w http.ResponseWriter, r *http.Request, c *caller) {
	var in SwarmInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	in.Name, in.DockerHost = strings.TrimSpace(in.Name), strings.TrimSpace(in.DockerHost)
	if !validDisplayName(in.Name, 64) || !validDockerHost(in.DockerHost) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	sw, err := s.control().ConnectSwarm(r.Context(), c.subject, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	sw.State = normalizeState(sw.State)
	writeJSON(w, http.StatusCreated, sw)
}

func (s *server) handleSwarmSync(w http.ResponseWriter, r *http.Request, c *caller) {
	sw := c.target.(Swarm)
	if err := s.control().SyncSwarm(r.Context(), c.subject, sw.ID); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleAgentsDeploy(w http.ResponseWriter, r *http.Request, c *caller) {
	sw := c.target.(Swarm)
	res, err := s.control().DeployAgents(r.Context(), c.subject, sw.ID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	for _, p := range []*[]string{&res.Labeled, &res.Unlabeled, &res.Failed} {
		if *p == nil {
			*p = []string{}
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// resolveNewCluster reads the request body: the target Swarm decides the
// permission scope.
func (s *server) resolveNewCluster(r *http.Request) (authz.Resource, any, error) {
	var req CreateClusterRequest
	if err := readJSON(r, &req, false); err != nil {
		return authz.Resource{}, nil, err
	}
	if req.Swarm == "" {
		return authz.Resource{}, nil, ErrInvalidInput
	}
	return authz.Resource{Type: authz.ResourceCluster, Scope: authz.SwarmScope(req.Swarm)}, req, nil
}

func (s *server) handleClusterCreate(w http.ResponseWriter, r *http.Request, c *caller) {
	req := c.target.(CreateClusterRequest)
	if !validName(req.Name, 48) || (req.Size != 3 && req.Size != 5) || req.Image == "" || req.Network == "" || req.RootPasswordSecret == "" {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	ref, err := s.control().CreateCluster(r.Context(), c.subject, req)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ref)
}

func (s *server) handleNodeReplace(w http.ResponseWriter, r *http.Request, c *caller) {
	var empty struct{}
	if !decodeJSON(w, r, &empty, true) {
		return
	}
	cl := c.target.(Cluster)
	slot := r.PathValue("slot")
	if !slices.ContainsFunc(cl.Nodes, func(n ClusterNode) bool { return n.ID == slot }) {
		writeError(w, http.StatusNotFound, errNotFound)
		return
	}
	ref, err := s.control().ReplaceNode(r.Context(), c.subject, cl.ID, slot)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ref)
}

func (s *server) handleOutageRecovery(w http.ResponseWriter, r *http.Request, c *caller) {
	var in RecoveryInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	cl := c.target.(Cluster)
	if !slices.ContainsFunc(cl.Nodes, func(n ClusterNode) bool { return n.ID == in.Candidate }) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.control().OutageRecovery(r.Context(), c.subject, cl.ID, in.Candidate)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if out.Diagnosis.Positions == nil {
		out.Diagnosis.Positions = []galera.NodePosition{}
	}
	if out.Diagnosis.Reasons == nil {
		out.Diagnosis.Reasons = []string{}
	}
	out.Diagnosis.RequiresApproval = true
	writeJSON(w, http.StatusAccepted, out)
}

func (s *server) handleOutageDiagnosisRecover(w http.ResponseWriter, r *http.Request, c *caller) {
	var empty struct{}
	if !decodeJSON(w, r, &empty, true) {
		return
	}
	cl := c.target.(Cluster)
	d, err := s.control().OutageDiagnosisRecover(r.Context(), c.subject, cl.ID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeDiagnosis(w, d)
}

func writeDiagnosis(w http.ResponseWriter, d Diagnosis) {
	if d.Positions == nil {
		d.Positions = []galera.NodePosition{}
	}
	if d.Reasons == nil {
		d.Reasons = []string{}
	}
	// Advice only: a full-cluster recovery always needs explicit approval.
	d.RequiresApproval = true
	writeJSON(w, http.StatusOK, d)
}

func (s *server) handleOutageDiagnosis(w http.ResponseWriter, r *http.Request, c *caller) {
	cl := c.target.(Cluster)
	d, err := s.control().OutageDiagnosis(r.Context(), c.subject, cl.ID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if d.Positions == nil {
		d.Positions = []galera.NodePosition{}
	}
	if d.Reasons == nil {
		d.Reasons = []string{}
	}
	// Advice only: a full-cluster recovery always needs explicit approval.
	d.RequiresApproval = true
	writeJSON(w, http.StatusOK, d)
}

func (s *server) resolveOperation(r *http.Request) (authz.Resource, any, error) {
	op, err := s.operation(r)
	if err != nil {
		return authz.Resource{}, nil, err
	}
	return locatedResource(op.Swarm, op.Cluster), op, nil
}

// resolveOperationApproval resolves the permission of ApprovalPermission:
// restore on the backups of the operation's cluster (manager settings for
// unlocated operations), delete on the cluster for a cluster deletion
// (approvalAction).
func (s *server) resolveOperationApproval(r *http.Request) (authz.Resource, any, error) {
	op, err := s.operation(r)
	if err != nil {
		return authz.Resource{}, nil, err
	}
	_, res := ApprovalPermission(op.Kind, op.Swarm, op.Cluster)
	return res, op, nil
}

func approvalAction(target any) authz.Action {
	op, ok := target.(Operation)
	if !ok {
		return authz.ActionRestore
	}
	act, _ := ApprovalPermission(op.Kind, op.Swarm, op.Cluster)
	return act
}

func (s *server) handleClusterDelete(w http.ResponseWriter, r *http.Request, c *caller) {
	cl := c.target.(Cluster)
	d, err := s.control().DeleteCluster(r.Context(), c.subject, cl.ID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: cluster deletion requested", "cluster", cl.ID, "operation", d.ID, "by", c.subject.ID)
	writeJSON(w, http.StatusAccepted, d)
}

func (s *server) operation(r *http.Request) (Operation, error) {
	ops, err := s.cfg.Activity.Operations(r.Context())
	if err != nil {
		return Operation{}, err
	}
	id := r.PathValue("id")
	for _, op := range ops {
		if op.ID == id {
			return op, nil
		}
	}
	return Operation{}, ErrNotFound
}

func (s *server) handleOperationControl(resume bool) func(http.ResponseWriter, *http.Request, *caller) {
	return func(w http.ResponseWriter, r *http.Request, c *caller) {
		var empty struct{}
		if !decodeJSON(w, r, &empty, true) {
			return
		}
		op := c.target.(Operation)
		var err error
		if resume {
			err = s.control().ResumeOperation(r.Context(), c.subject, op.ID)
		} else {
			err = s.control().AbortOperation(r.Context(), c.subject, op.ID)
		}
		if err != nil {
			s.writeServiceError(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, OperationRef{ID: op.ID, Kind: op.Kind})
	}
}

func (s *server) handleOperationApprove(w http.ResponseWriter, r *http.Request, c *caller) {
	var in ApprovalInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if !validDigest(in.Digest) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	op := c.target.(Operation)
	if op.Approval == nil {
		// Nothing to approve: approvals are bound to operations that ask
		// for one.
		writeError(w, http.StatusConflict, errConflict)
		return
	}
	a, err := s.control().ApproveOperation(r.Context(), c.subject, op.ID, in.Digest)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: operation approved", "operation", op.ID, "by", c.subject.ID, "digest", a.Digest)
	writeJSON(w, http.StatusOK, a)
}

func validDigest(d string) bool {
	if len(d) != 64 {
		return false
	}
	for _, c := range d {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// --- backups ---

func (s *server) handleBackups(w http.ResponseWriter, r *http.Request, c *caller) {
	items, err := s.backups().Backups(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, func(b BackupSummary) authz.Resource {
		return backupResource(b.Swarm, b.Cluster)
	})
	writeJSON(w, http.StatusOK, List[BackupSummary]{Items: items})
}

func (s *server) resolveClusterBackups(r *http.Request) (authz.Resource, any, error) {
	cl, err := s.cfg.Inventory.Cluster(r.Context(), r.PathValue("id"))
	if err != nil {
		return authz.Resource{}, nil, err
	}
	return backupResource(cl.Swarm, cl.ID), cl, nil
}

func (s *server) handleBackupPolicy(w http.ResponseWriter, r *http.Request, c *caller) {
	var p BackupPolicy
	if !decodeJSON(w, r, &p, false) {
		return
	}
	if len(p.Schedule) > 128 || len(p.Timezone) > 64 || (p.Verify != nil && (len(p.Verify.Schedule) > 128 || len(p.Verify.Timezone) > 64 || p.Verify.MaxMinutes < 0 || p.Verify.MaxMinutes > 240)) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	cl := c.target.(Cluster)
	out, err := s.backups().SetBackupPolicy(r.Context(), c.subject, cl.ID, p)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleBackupStart(w http.ResponseWriter, r *http.Request, c *caller) {
	var in BackupStartInput
	if !decodeJSON(w, r, &in, true) {
		return
	}
	cl := c.target.(Cluster)
	var ref OperationRef
	var err error
	switch in.Method {
	case "", "physical":
		if len(in.Databases) > 0 {
			writeError(w, http.StatusBadRequest, errBadRequest)
			return
		}
		ref, err = s.backups().StartBackup(r.Context(), c.subject, cl.ID)
	case "logical":
		if len(in.Databases) == 0 || len(in.Databases) > 64 {
			writeError(w, http.StatusBadRequest, errBadRequest)
			return
		}
		lb, ok := s.backups().(LogicalBackups)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, errUnavailable)
			return
		}
		ref, err = lb.StartLogicalBackup(r.Context(), c.subject, cl.ID, in.Databases)
	default:
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ref)
}

// resolveInspection reads the body: the cluster decides the scope.
func (s *server) resolveInspection(r *http.Request) (authz.Resource, any, error) {
	var in InspectionInput
	if err := readJSON(r, &in, false); err != nil {
		return authz.Resource{}, nil, err
	}
	if in.Cluster == "" {
		return authz.Resource{}, nil, ErrInvalidInput
	}
	cl, err := s.cfg.Inventory.Cluster(r.Context(), in.Cluster)
	if err != nil {
		return authz.Resource{}, nil, err
	}
	return backupResource(cl.Swarm, cl.ID), in, nil
}

func (s *server) handleInspectionStart(w http.ResponseWriter, r *http.Request, c *caller) {
	in := c.target.(InspectionInput)
	if len(in.Backup) > 64 || len(in.Host) > 64 || len(in.KeySecret) > 64 || in.TTLMinutes < 0 || in.TTLMinutes > 24*60 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	res, err := s.backups().StartInspection(r.Context(), c.subject, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// --- settings ---

func (s *server) resolveSettings(*http.Request) (authz.Resource, any, error) {
	return settingsResource, nil, nil
}

func (s *server) handleNotificationSettings(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.settings().NotificationSettings(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeNotificationSettings(out))
}

func (s *server) handleNotificationSettingsUpdate(w http.ResponseWriter, r *http.Request, c *caller) {
	var in NotificationSettingsInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	switch in.MinSeverity {
	case "", "info", "warning", "error", "critical":
	default:
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if len(in.Webhook.URL) > 2048 || len(in.Email.Recipients) > 50 ||
		(in.Webhook.Secret != nil && len(*in.Webhook.Secret) > 1024) || (in.Email.Password != nil && len(*in.Email.Password) > 1024) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.settings().SetNotificationSettings(r.Context(), c.subject, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeNotificationSettings(out))
}

func normalizeNotificationSettings(n NotificationSettings) NotificationSettings {
	if n.Email.Recipients == nil {
		n.Email.Recipients = []string{}
	}
	return n
}

func (s *server) handleNotificationTest(w http.ResponseWriter, r *http.Request, c *caller) {
	var in struct {
		Channel string `json:"channel"`
	}
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.Channel != "webhook" && in.Channel != "email" {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	res, err := s.settings().TestNotification(r.Context(), c.subject, in.Channel)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// validName accepts a lowercase DNS label of at most max characters.
func validName(n string, max int) bool {
	if n == "" || len(n) > max || n[0] == '-' || n[len(n)-1] == '-' {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validDisplayName accepts printable text without control characters.
func validDisplayName(n string, max int) bool {
	if n == "" || len(n) > max {
		return false
	}
	for _, c := range n {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// validDockerHost accepts unix:// and tcp:// Docker endpoints.
func validDockerHost(h string) bool {
	if len(h) > 256 || strings.ContainsAny(h, " \t\r\n") {
		return false
	}
	return strings.HasPrefix(h, "unix:///") || (strings.HasPrefix(h, "tcp://") && len(h) > len("tcp://"))
}
