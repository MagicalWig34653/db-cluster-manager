// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
)

// GitHubFeed lists reviewed global security advisories (GET
// {BaseURL}/advisories) sorted by update time, newer than the cursor,
// following Link rel="next" up to MaxPages.
type GitHubFeed struct {
	// BaseURL is for example https://api.github.com; no default.
	BaseURL string
	HTTP    HTTPDoer
	// Token is optional; it raises the rate limit. It is sent only in the
	// Authorization header and never logged.
	Token    ai.Secret
	MaxPages int
	MaxBytes int64
	Now      func() time.Time
}

// Name implements Feed.
func (f *GitHubFeed) Name() string { return "github" }

type ghAdvisory struct {
	GHSA        string   `json:"ghsa_id"`
	CVE         string   `json:"cve_id"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Severity    string   `json:"severity"`
	Published   string   `json:"published_at"`
	Updated     string   `json:"updated_at"`
	Withdrawn   string   `json:"withdrawn_at"`
	References  []string `json:"references"`
	CVSS        struct {
		Score float64 `json:"score"`
	} `json:"cvss"`
	EPSS []struct {
		Percentage float64 `json:"percentage"`
	} `json:"epss"`
	Vulnerabilities []struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		Range string `json:"vulnerable_version_range"`
		// FirstPatched is a string in the global advisory API and an
		// object {identifier} in repository advisories.
		FirstPatched json.RawMessage `json:"first_patched_version"`
	} `json:"vulnerabilities"`
}

func firstPatched(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Identifier string `json:"identifier"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Identifier
	}
	return ""
}

func (g ghAdvisory) advisory(fetched time.Time) Advisory {
	a := Advisory{
		ID: g.GHSA, Sources: []string{"github"}, Summary: g.Summary, Details: g.Description,
		Severity: ParseSeverity(g.Severity), CVSS: g.CVSS.Score, References: g.References,
		Published: parseTime(g.Published), Modified: parseTime(g.Updated), FetchedAt: fetched, Withdrawn: g.Withdrawn != "",
	}
	if g.CVE != "" {
		a.Aliases = []string{g.CVE}
	}
	for _, e := range g.EPSS {
		a.EPSS = max(a.EPSS, e.Percentage)
	}
	for _, v := range g.Vulnerabilities {
		af := Affected{Ecosystem: v.Package.Ecosystem, Package: v.Package.Name}
		r, exact, ok := ParseRangeExpr(v.Range)
		if !ok {
			continue // unparseable range: no guessed finding
		}
		if exact != "" {
			af.Versions = []string{exact}
		} else {
			if fp := firstPatched(v.FirstPatched); fp != "" && r.Fixed == "" && r.LastAffected == "" {
				r.Fixed = fp
			}
			af.Ranges = []Range{r}
		}
		a.Affect = append(a.Affect, af)
	}
	return a.Normalize()
}

var linkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// Fetch implements Feed.
func (f *GitHubFeed) Fetch(ctx context.Context, st FeedState, _ []Component) (FetchResult, error) {
	if f.BaseURL == "" {
		return FetchResult{}, fmt.Errorf("%w: GitHub base URL not configured", ErrFeedFormat)
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	q := url.Values{"type": {"reviewed"}, "per_page": {"100"}, "sort": {"updated"}, "direction": {"desc"}}
	if !st.Cursor.IsZero() {
		q.Set("updated", ">="+st.Cursor.UTC().Format(time.RFC3339))
	}
	next := strings.TrimRight(f.BaseURL, "/") + "/advisories?" + q.Encode()
	base, _ := url.Parse(f.BaseURL)
	headers := map[string]string{"X-GitHub-Api-Version": "2022-11-28", "Accept": "application/vnd.github+json"}
	if !f.Token.IsZero() {
		headers["Authorization"] = "Bearer " + f.Token.Reveal()
	}
	pages := f.MaxPages
	if pages <= 0 {
		pages = 10
	}
	h := feedHTTP{Doer: f.HTTP, MaxBytes: f.MaxBytes}
	out := FetchResult{State: st}
	cursor := st.Cursor
	for i := 0; i < pages && next != ""; i++ {
		var page []ghAdvisory
		// Only the first page is conditional; ETags are per URL.
		var cond *FeedState
		if i == 0 {
			cond = &out.State
		}
		hdr, notMod, err := h.do(ctx, http.MethodGet, next, nil, headers, cond, &page)
		if err != nil {
			return FetchResult{}, err
		}
		if notMod {
			out.NotModified = true
			return out, nil
		}
		for _, g := range page {
			a := g.advisory(now().UTC())
			if a.Modified.After(cursor) {
				cursor = a.Modified
			}
			out.Advisories = append(out.Advisories, a)
		}
		next = ""
		if m := linkNextRe.FindStringSubmatch(hdr.Get("Link")); m != nil {
			// Follow only links on the configured host: a Link header
			// must not redirect the token elsewhere.
			if u, err := url.Parse(m[1]); err == nil && base != nil && u.Host == base.Host && u.Scheme == base.Scheme {
				next = u.String()
			}
		}
	}
	out.State.Cursor = cursor
	return out, nil
}
