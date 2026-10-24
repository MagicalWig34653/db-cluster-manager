// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OSVFeed queries the OSV API (POST {BaseURL}/v1/query) once per distinct
// inventory component. OSV has no "changed since" listing in its query API,
// so every poll re-queries the inventory; MaxQueries bounds one poll.
type OSVFeed struct {
	// BaseURL is for example https://api.osv.dev; no default, so a
	// configuration error cannot reach the network by accident.
	BaseURL    string
	HTTP       HTTPDoer
	MaxQueries int
	MaxBytes   int64
	Now        func() time.Time
}

// Name implements Feed.
func (f *OSVFeed) Name() string { return "osv" }

// osvEcosystem maps a component ecosystem to OSV's spelling. A component
// ecosystem with a release suffix ("Alpine:v3.20", "Debian:12") is passed
// with OSV capitalisation.
func osvEcosystem(e string) string {
	base, rel, _ := strings.Cut(strings.TrimSpace(e), ":")
	m := map[string]string{
		"alpine": "Alpine", "debian": "Debian", "npm": "npm", "pypi": "PyPI", "pip": "PyPI", "go": "Go",
		"golang": "Go", "maven": "Maven", "packagist": "Packagist", "composer": "Packagist",
		"crates.io": "crates.io", "nuget": "NuGet", "rubygems": "RubyGems",
	}
	name, ok := m[strings.ToLower(base)]
	if !ok {
		return ""
	}
	if rel != "" {
		return name + ":" + rel
	}
	return name
}

type osvRecord struct {
	ID        string   `json:"id"`
	Summary   string   `json:"summary"`
	Details   string   `json:"details"`
	Aliases   []string `json:"aliases"`
	Modified  string   `json:"modified"`
	Published string   `json:"published"`
	Withdrawn string   `json:"withdrawn"`
	Affected  []struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		Ranges []struct {
			Type   string              `json:"type"`
			Events []map[string]string `json:"events"`
		} `json:"ranges"`
		Versions []string `json:"versions"`
	} `json:"affected"`
	Severity []struct {
		Type  string `json:"type"`
		Score string `json:"score"`
	} `json:"severity"`
	DatabaseSpecific struct {
		Severity string `json:"severity"`
	} `json:"database_specific"`
	References []struct {
		URL string `json:"url"`
	} `json:"references"`
}

func (r osvRecord) advisory(source string, fetched time.Time) Advisory {
	a := Advisory{
		ID: r.ID, Aliases: r.Aliases, Sources: []string{source}, Summary: r.Summary, Details: r.Details,
		Published: parseTime(r.Published), Modified: parseTime(r.Modified), FetchedAt: fetched,
		Withdrawn: r.Withdrawn != "", Severity: ParseSeverity(r.DatabaseSpecific.Severity),
	}
	for _, ref := range r.References {
		a.References = append(a.References, ref.URL)
	}
	for _, af := range r.Affected {
		out := Affected{Ecosystem: af.Package.Ecosystem, Package: af.Package.Name, Versions: af.Versions}
		for _, rg := range af.Ranges {
			if rg.Type == "GIT" {
				continue // commit ranges cannot be matched against versions
			}
			out.Ranges = append(out.Ranges, eventsToRanges(rg.Events)...)
		}
		a.Affect = append(a.Affect, out)
	}
	return a.Normalize()
}

// eventsToRanges converts OSV's ordered introduced/fixed/last_affected
// events into ranges.
func eventsToRanges(events []map[string]string) []Range {
	var out []Range
	var cur *Range
	for _, ev := range events {
		switch {
		case ev["introduced"] != "":
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &Range{Introduced: ev["introduced"]}
		case ev["fixed"] != "" && cur != nil:
			cur.Fixed = ev["fixed"]
			out = append(out, *cur)
			cur = nil
		case ev["last_affected"] != "" && cur != nil:
			cur.LastAffected = ev["last_affected"]
			out = append(out, *cur)
			cur = nil
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// Fetch implements Feed.
func (f *OSVFeed) Fetch(ctx context.Context, st FeedState, comps []Component) (FetchResult, error) {
	if f.BaseURL == "" {
		return FetchResult{}, fmt.Errorf("%w: OSV base URL not configured", ErrFeedFormat)
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	type q struct{ eco, name, ver string }
	seen := map[q]bool{}
	var queries []q
	for _, c := range comps {
		eco := osvEcosystem(c.Ecosystem)
		if eco == "" {
			continue
		}
		k := q{eco, c.Name, c.Version}
		if !seen[k] {
			seen[k] = true
			queries = append(queries, k)
		}
	}
	sort.Slice(queries, func(i, j int) bool {
		return queries[i].eco+queries[i].name+queries[i].ver < queries[j].eco+queries[j].name+queries[j].ver
	})
	limit := f.MaxQueries
	if limit <= 0 {
		limit = 500
	}
	if len(queries) > limit {
		queries = queries[:limit]
	}
	h := feedHTTP{Doer: f.HTTP, MaxBytes: f.MaxBytes}
	byID := map[string]Advisory{}
	for _, qq := range queries {
		body := map[string]any{"package": map[string]string{"name": qq.name, "ecosystem": qq.eco}, "version": qq.ver}
		var resp struct {
			Vulns []osvRecord `json:"vulns"`
		}
		if _, _, err := h.do(ctx, http.MethodPost, strings.TrimRight(f.BaseURL, "/")+"/v1/query", body, nil, nil, &resp); err != nil {
			return FetchResult{}, err
		}
		for _, r := range resp.Vulns {
			a := r.advisory(f.Name(), now().UTC())
			if prev, ok := byID[a.ID]; ok {
				a = Merge(prev, a)
			}
			byID[a.ID] = a
		}
	}
	out := FetchResult{State: st}
	for _, a := range byID {
		out.Advisories = append(out.Advisories, a)
	}
	sort.Slice(out.Advisories, func(i, j int) bool { return out.Advisories[i].ID < out.Advisories[j].ID })
	return out, nil
}

// OfflineFeed reads an admin-imported mirror file (air gap): a JSON array
// of OSV records. Load returns the file content.
type OfflineFeed struct {
	Label    string
	Load     func() ([]byte, error)
	MaxBytes int
	Now      func() time.Time
}

// Name implements Feed.
func (f *OfflineFeed) Name() string { return "offline:" + f.Label }

// Fetch implements Feed.
func (f *OfflineFeed) Fetch(_ context.Context, st FeedState, _ []Component) (FetchResult, error) {
	data, err := f.Load()
	if err != nil {
		return FetchResult{}, err
	}
	limit := f.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxFeedBytes
	}
	if len(data) > limit {
		return FetchResult{}, ErrFeedTooBig
	}
	var recs []osvRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return FetchResult{}, fmt.Errorf("%w: %v", ErrFeedFormat, err)
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	out := FetchResult{State: st}
	for _, r := range recs {
		out.Advisories = append(out.Advisories, r.advisory(f.Name(), now().UTC()))
	}
	return out, nil
}
