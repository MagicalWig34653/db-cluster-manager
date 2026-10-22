// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"fmt"
	"strings"
)

// Panel is one widget of a dashboard. Dashboards are plain data: they hold
// queries, never results and never permissions.
type Panel struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Kind          string  `json:"kind"` // "line", "stat" or "table"
	Selector      string  `json:"selector"`
	Func          string  `json:"func"`
	WindowSeconds int     `json:"windowSeconds"`
	StepSeconds   int     `json:"stepSeconds"`
	Quantile      float64 `json:"quantile,omitempty"`
	X             int     `json:"x"`
	Y             int     `json:"y"`
	W             int     `json:"w"`
	H             int     `json:"h"`
}

// Dashboard is a personal or shared set of panels. Shared only controls who
// may see the definition; the data shown is always filtered by the viewer's
// own Authorizer.
type Dashboard struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Owner    string  `json:"owner"`
	Shared   bool    `json:"shared"`
	Template bool    `json:"template,omitempty"`
	Panels   []Panel `json:"panels"`
}

const maxPanels = 100

// Validate checks structure and parses every selector and function.
func (d *Dashboard) Validate() error {
	bad := func(f string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidDashboard, fmt.Sprintf(f, a...))
	}
	if strings.TrimSpace(d.ID) == "" || strings.TrimSpace(d.Title) == "" || len(d.Title) > 200 {
		return bad("id and title (max 200 bytes) are required")
	}
	if strings.TrimSpace(d.Owner) == "" {
		return bad("owner is required")
	}
	if len(d.Panels) > maxPanels {
		return bad("more than %d panels", maxPanels)
	}
	seen := map[string]bool{}
	for i, p := range d.Panels {
		if p.ID == "" || seen[p.ID] {
			return bad("panel %d: empty or duplicate id %q", i, p.ID)
		}
		seen[p.ID] = true
		if p.Title == "" || len(p.Title) > 200 {
			return bad("panel %q: title required (max 200 bytes)", p.ID)
		}
		switch p.Kind {
		case "line", "stat", "table":
		default:
			return bad("panel %q: unknown kind %q", p.ID, p.Kind)
		}
		if _, err := ParseSelector(p.Selector); err != nil {
			return bad("panel %q: %v", p.ID, err)
		}
		f, err := ParseFunc(p.Func)
		if err != nil {
			return bad("panel %q: %v", p.ID, err)
		}
		if f == FuncQuantile && (p.Quantile < 0 || p.Quantile > 1) {
			return bad("panel %q: quantile outside [0,1]", p.ID)
		}
		if p.StepSeconds <= 0 || p.WindowSeconds < 0 {
			return bad("panel %q: step must be positive and window not negative", p.ID)
		}
		if p.X < 0 || p.Y < 0 || p.W <= 0 || p.H <= 0 || p.W > 24 {
			return bad("panel %q: invalid layout", p.ID)
		}
	}
	return nil
}

// CanView reports whether user may see the dashboard definition. This is
// about the definition only, never about the data.
func (d *Dashboard) CanView(user string) bool { return d.Shared || d.Owner == user }

// PanelQuery builds the query of panel index i for the given time range. The
// viewer's Authorizer is mandatory, so a shared dashboard never widens access.
func (d *Dashboard) PanelQuery(i int, start, end int64, viewer Authorizer) (Query, error) {
	if viewer == nil {
		return Query{}, ErrNoAuthorizer
	}
	if i < 0 || i >= len(d.Panels) {
		return Query{}, fmt.Errorf("%w: panel index %d", ErrInvalidDashboard, i)
	}
	p := d.Panels[i]
	sel, err := ParseSelector(p.Selector)
	if err != nil {
		return Query{}, err
	}
	f, err := ParseFunc(p.Func)
	if err != nil {
		return Query{}, err
	}
	step := int64(p.StepSeconds) * 1000
	win := int64(p.WindowSeconds) * 1000
	if win == 0 {
		win = step
	}
	return Query{Selector: sel, Start: start, End: end, Step: step, Window: win, Func: f, Quantile: p.Quantile, Authorizer: viewer}, nil
}
