// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
)

// ErrConflict is returned by services when an object already exists.
var ErrConflict = errors.New("api: conflict")

// Notification is one UI inbox entry. Swarm and Cluster locate it for
// permission filtering; both empty means manager-level.
type Notification struct {
	ID       string            `json:"id"`
	Time     time.Time         `json:"time"`
	Severity string            `json:"severity"` // info, warning, error, critical, unknown
	Kind     string            `json:"kind"`
	Resource string            `json:"resource,omitempty"`
	Message  string            `json:"message"`
	Swarm    string            `json:"swarm,omitempty"`
	Cluster  string            `json:"cluster,omitempty"`
	Details  map[string]string `json:"details,omitempty"`
	Read     bool              `json:"read"`
	Acked    bool              `json:"acked"`
	AckedAt  time.Time         `json:"ackedAt,omitzero"`
	// Acknowledged repeats Acked under the name the UI uses; the API sets
	// it from Acked.
	Acknowledged bool `json:"acknowledged"`
}

// Monitoring provides dashboards, metric queries and the notification inbox.
// The API performs all authorization; implementations store and execute.
type Monitoring interface {
	// Dashboards returns every stored dashboard (the API filters).
	Dashboards(ctx context.Context) ([]metrics.Dashboard, error)
	// Dashboard returns ErrNotFound for an unknown ID.
	Dashboard(ctx context.Context, id string) (metrics.Dashboard, error)
	// CreateDashboard returns ErrConflict if the ID exists.
	CreateDashboard(ctx context.Context, d metrics.Dashboard) error
	// UpdateDashboard replaces name, sharing and panels of an existing
	// dashboard; ErrNotFound if it does not exist.
	UpdateDashboard(ctx context.Context, d metrics.Dashboard) error
	DeleteDashboard(ctx context.Context, id string) error
	// Query runs a range query. q.Authorizer is set by the API.
	Query(ctx context.Context, q metrics.Query) (*metrics.Result, error)
	Notifications(ctx context.Context) ([]Notification, error)
	// Notification returns ErrNotFound for an unknown ID.
	Notification(ctx context.Context, id string) (Notification, error)
	AckNotification(ctx context.Context, id, by string) error
	// MarkNotificationRead marks one notification read.
	MarkNotificationRead(ctx context.Context, id string) error
}

// Monitoring methods of Unavailable: every call fails with ErrUnavailable.
func (Unavailable) Dashboards(context.Context) ([]metrics.Dashboard, error) {
	return nil, ErrUnavailable
}
func (Unavailable) Dashboard(context.Context, string) (metrics.Dashboard, error) {
	return metrics.Dashboard{}, ErrUnavailable
}
func (Unavailable) CreateDashboard(context.Context, metrics.Dashboard) error { return ErrUnavailable }
func (Unavailable) UpdateDashboard(context.Context, metrics.Dashboard) error { return ErrUnavailable }
func (Unavailable) DeleteDashboard(context.Context, string) error            { return ErrUnavailable }
func (Unavailable) Query(context.Context, metrics.Query) (*metrics.Result, error) {
	return nil, ErrUnavailable
}
func (Unavailable) Notifications(context.Context) ([]Notification, error) {
	return nil, ErrUnavailable
}
func (Unavailable) Notification(context.Context, string) (Notification, error) {
	return Notification{}, ErrUnavailable
}
func (Unavailable) AckNotification(context.Context, string, string) error { return ErrUnavailable }
func (Unavailable) MarkNotificationRead(context.Context, string) error    { return ErrUnavailable }

var dashboardIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func dashboardResource(id string) authz.Resource {
	return authz.Resource{Type: authz.ResourceDashboard, Scope: authz.Global, ID: id}
}

// DashboardInput is the writable part of a dashboard. ID comes from the path
// and Owner from the session.
type DashboardInput struct {
	Title  string          `json:"title"`
	Shared bool            `json:"shared"`
	Panels []metrics.Panel `json:"panels"`
}

func (s *server) handleDashboards(w http.ResponseWriter, r *http.Request, c *caller) {
	all, err := s.monitoring().Dashboards(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	visible := make([]metrics.Dashboard, 0, len(all))
	for _, d := range all {
		if d.CanView(c.subject.ID) {
			visible = append(visible, d)
		}
	}
	visible = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, visible, func(d metrics.Dashboard) authz.Resource {
		return dashboardResource(d.ID)
	})
	for i := range visible {
		if visible[i].Panels == nil {
			visible[i].Panels = []metrics.Panel{}
		}
	}
	writeJSON(w, http.StatusOK, List[metrics.Dashboard]{Items: visible})
}

func (s *server) resolveNewDashboard(r *http.Request) (authz.Resource, any, error) {
	return dashboardResource(r.PathValue("id")), nil, nil
}

func (s *server) resolveDashboard(r *http.Request) (authz.Resource, any, error) {
	d, err := s.monitoring().Dashboard(r.Context(), r.PathValue("id"))
	if err != nil {
		return authz.Resource{}, nil, err
	}
	return dashboardResource(d.ID), d, nil
}

func (s *server) handleDashboardCreate(w http.ResponseWriter, r *http.Request, c *caller) {
	id := r.PathValue("id")
	if !dashboardIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	var in DashboardInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	d := metrics.Dashboard{ID: id, Title: in.Title, Owner: c.subject.ID, Shared: in.Shared, Panels: in.Panels}
	if err := d.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if err := s.monitoring().CreateDashboard(r.Context(), d); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// ownDashboard returns the resolved dashboard if the caller owns it. Other
// users' dashboards, shared or not, are reported as missing: sharing grants
// viewing only.
func ownDashboard(w http.ResponseWriter, c *caller) (metrics.Dashboard, bool) {
	d := c.target.(metrics.Dashboard)
	if d.Owner != c.subject.ID {
		writeError(w, http.StatusNotFound, errNotFound)
		return d, false
	}
	return d, true
}

func (s *server) handleDashboardUpdate(w http.ResponseWriter, r *http.Request, c *caller) {
	cur, ok := ownDashboard(w, c)
	if !ok {
		return
	}
	var in DashboardInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	d := metrics.Dashboard{ID: cur.ID, Title: in.Title, Owner: cur.Owner, Shared: in.Shared, Template: cur.Template, Panels: in.Panels}
	if cur.Template || d.Validate() != nil {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if err := s.monitoring().UpdateDashboard(r.Context(), d); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) handleDashboardDelete(w http.ResponseWriter, r *http.Request, c *caller) {
	cur, ok := ownDashboard(w, c)
	if !ok {
		return
	}
	if cur.Template {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if err := s.monitoring().DeleteDashboard(r.Context(), cur.ID); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// QueryRequest is the body of POST /metrics/query. Times are milliseconds
// since the Unix epoch.
type QueryRequest struct {
	Selector      string  `json:"selector"`
	Start         int64   `json:"start"`
	End           int64   `json:"end"`
	StepSeconds   int     `json:"stepSeconds"`
	WindowSeconds int     `json:"windowSeconds,omitempty"`
	Func          string  `json:"func,omitempty"`
	Quantile      float64 `json:"quantile,omitempty"`
}

// QueryPoint is one step. V is null unless State is "ok".
type QueryPoint struct {
	T     int64    `json:"t"`
	V     *float64 `json:"v"`
	State string   `json:"state"`
}

// QuerySeries is one series of a result.
type QuerySeries struct {
	Labels map[string]string `json:"labels"`
	Points []QueryPoint      `json:"points"`
}

// QueryGap is an interval that could not be read.
type QueryGap struct {
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	Reason string `json:"reason"`
}

// QueryResponse is the result of POST /metrics/query.
type QueryResponse struct {
	Start         int64         `json:"start"`
	End           int64         `json:"end"`
	StepSeconds   int64         `json:"stepSeconds"`
	WindowSeconds int64         `json:"windowSeconds"`
	Series        []QuerySeries `json:"series"`
	Gaps          []QueryGap    `json:"gaps"`
}

func (s *server) handleMetricsQuery(w http.ResponseWriter, r *http.Request, c *caller) {
	var in QueryRequest
	if !decodeJSON(w, r, &in, false) {
		return
	}
	sel, err := metrics.ParseSelector(in.Selector)
	if err != nil || in.StepSeconds <= 0 || in.WindowSeconds < 0 || in.End < in.Start || len(in.Selector) > 1024 ||
		math.IsNaN(in.Quantile) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	fn := metrics.FuncLast
	if in.Func != "" {
		if fn, err = metrics.ParseFunc(in.Func); err != nil {
			writeError(w, http.StatusBadRequest, errBadRequest)
			return
		}
	}
	q := metrics.Query{
		Selector: sel, Start: in.Start, End: in.End, Step: int64(in.StepSeconds) * 1000,
		Window: int64(in.WindowSeconds) * 1000, Func: fn, Quantile: in.Quantile,
		// The viewer's own permissions decide which series are readable;
		// nothing else (dashboards, sharing) widens this.
		Authorizer: collect.SeriesAuthorizer(s.cfg.Authorizer, c.subject),
	}
	res, err := s.monitoring().Query(r.Context(), q)
	switch {
	case err == nil:
	case errors.Is(err, metrics.ErrInvalidQuery), errors.Is(err, metrics.ErrTooManySamples), errors.Is(err, metrics.ErrTooManySteps):
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	case errors.Is(err, metrics.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, errRateLimited)
		return
	default:
		s.writeServiceError(w, r, err)
		return
	}
	out := QueryResponse{
		Start: res.Start, End: res.End, StepSeconds: res.Step / 1000, WindowSeconds: res.Window / 1000,
		Series: make([]QuerySeries, 0, len(res.Series)), Gaps: make([]QueryGap, 0, len(res.Gaps)),
	}
	for _, sr := range res.Series {
		qs := QuerySeries{Labels: map[string]string{}, Points: make([]QueryPoint, 0, len(sr.Points))}
		for _, l := range sr.Labels {
			if l.Name == metrics.NameLabel {
				qs.Labels["name"] = l.Value
			} else {
				qs.Labels[l.Name] = l.Value
			}
		}
		for _, p := range sr.Points {
			pt := QueryPoint{T: p.T, State: p.State.String()}
			if p.State == metrics.StateOK && !math.IsNaN(p.V) && !math.IsInf(p.V, 0) {
				v := p.V
				pt.V = &v
			} else if p.State == metrics.StateOK {
				pt.State = metrics.StateNoData.String()
			}
			qs.Points = append(qs.Points, pt)
		}
		out.Series = append(out.Series, qs)
	}
	for _, g := range res.Gaps {
		out.Gaps = append(out.Gaps, QueryGap{From: g.From, To: g.To, Reason: g.Reason})
	}
	writeJSON(w, http.StatusOK, out)
}

func notificationResource(n Notification) authz.Resource {
	return locatedResource(n.Swarm, n.Cluster)
}

func (s *server) handleNotifications(w http.ResponseWriter, r *http.Request, c *caller) {
	items, err := s.monitoring().Notifications(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, notificationResource)
	if r.URL.Query().Get("unread") == "true" {
		kept := items[:0]
		for _, n := range items {
			if !n.Read {
				kept = append(kept, n)
			}
		}
		items = kept
	}
	for i := range items {
		items[i].Acknowledged = items[i].Acked
	}
	writeJSON(w, http.StatusOK, List[Notification]{Items: items})
}

func (s *server) resolveNotification(r *http.Request) (authz.Resource, any, error) {
	n, err := s.monitoring().Notification(r.Context(), r.PathValue("id"))
	if err != nil {
		return authz.Resource{}, nil, err
	}
	return notificationResource(n), n, nil
}

func (s *server) handleNotificationAck(w http.ResponseWriter, r *http.Request, c *caller) {
	n := c.target.(Notification)
	if err := s.monitoring().AckNotification(r.Context(), n.ID, c.subject.ID); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleNotificationRead marks a notification read for the inbox. It needs
// only read permission: the read flag is presentation state, not an
// acknowledgement.
func (s *server) handleNotificationRead(w http.ResponseWriter, r *http.Request, c *caller) {
	n := c.target.(Notification)
	if err := s.monitoring().MarkNotificationRead(r.Context(), n.ID); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) monitoring() Monitoring {
	if s.cfg.Monitoring != nil {
		return s.cfg.Monitoring
	}
	return Unavailable{}
}
