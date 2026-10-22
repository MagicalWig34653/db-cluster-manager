// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"context"
	"errors"
	"testing"
)

func goodDash() Dashboard {
	return Dashboard{ID: "d1", Title: "DB overview", Owner: "alice", Panels: []Panel{
		{ID: "p1", Title: "CPU", Kind: "line", Selector: `cpu{host="a"}`, Func: "avg", WindowSeconds: 300, StepSeconds: 60, X: 0, Y: 0, W: 12, H: 6},
		{ID: "p2", Title: "Latency p95", Kind: "stat", Selector: `lat`, Func: "quantile", Quantile: 0.95, StepSeconds: 60, X: 12, Y: 0, W: 12, H: 6},
	}}
}

func TestDashboardValidate(t *testing.T) {
	d := goodDash()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	mut := map[string]func(*Dashboard){
		"no title":     func(d *Dashboard) { d.Title = "" },
		"no owner":     func(d *Dashboard) { d.Owner = "" },
		"dup panel id": func(d *Dashboard) { d.Panels[1].ID = "p1" },
		"bad kind":     func(d *Dashboard) { d.Panels[0].Kind = "pie3d" },
		"bad selector": func(d *Dashboard) { d.Panels[0].Selector = `cpu{` },
		"bad func":     func(d *Dashboard) { d.Panels[0].Func = "median" },
		"bad quantile": func(d *Dashboard) { d.Panels[1].Quantile = 1.5 },
		"zero step":    func(d *Dashboard) { d.Panels[0].StepSeconds = 0 },
		"bad layout":   func(d *Dashboard) { d.Panels[0].W = 99 },
		"too many": func(d *Dashboard) {
			for i := 0; i < 101; i++ {
				p := d.Panels[0]
				p.ID = string(rune('a'+i%26)) + string(rune('a'+i/26))
				d.Panels = append(d.Panels, p)
			}
		},
	}
	for name, m := range mut {
		d := goodDash()
		m(&d)
		if err := d.Validate(); !errors.Is(err, ErrInvalidDashboard) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestDashboardVisibility(t *testing.T) {
	d := goodDash()
	if !d.CanView("alice") || d.CanView("bob") {
		t.Fatal("personal dashboard visible to others")
	}
	d.Shared = true
	if !d.CanView("bob") {
		t.Fatal("shared dashboard not visible")
	}
}

// Sharing a dashboard shares the definition only: each viewer's own Authorizer
// still filters every series.
func TestSharedDashboardNeverBypassesAuthorizer(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	appendOK(t, s,
		e(MustLabels("cpu", "host", "a"), base, 10),
		e(MustLabels("cpu", "host", "b"), base, 20))
	d := goodDash()
	d.Shared = true
	d.Panels[0].Selector = "cpu"
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	run := func(auth Authorizer) *Result {
		q, err := d.PanelQuery(0, base, base, auth)
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	owner := run(AllowAll)
	viewer := run(func(l Labels) bool { return l.Get("host") == "b" })
	nobody := run(func(Labels) bool { return false })
	if len(owner.Series) != 2 || len(viewer.Series) != 1 || viewer.Series[0].Labels.Get("host") != "b" || len(nobody.Series) != 0 {
		t.Fatalf("owner=%d viewer=%d nobody=%d", len(owner.Series), len(viewer.Series), len(nobody.Series))
	}
	if _, err := d.PanelQuery(0, base, base, nil); !errors.Is(err, ErrNoAuthorizer) {
		t.Fatalf("PanelQuery without viewer authorizer: %v", err)
	}
	if _, err := d.PanelQuery(7, base, base, AllowAll); !errors.Is(err, ErrInvalidDashboard) {
		t.Fatalf("got %v", err)
	}
}
