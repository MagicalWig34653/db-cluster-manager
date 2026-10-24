// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
)

// NVDFeed queries the NVD CVE API 2.0 (GET {BaseURL}/rest/json/cves/2.0)
// for CVEs modified since the cursor. NVD limits one query to a 120-day
// window; a first poll without cursor starts InitialWindow back (default
// 7 days). CPE matches become components of ecosystem "cpe" named
// "vendor:product". cisaExploitAdd sets KEV.
type NVDFeed struct {
	BaseURL string
	HTTP    HTTPDoer
	// APIKey is optional (higher rate limit); sent in the apiKey header.
	APIKey        ai.Secret
	InitialWindow time.Duration
	MaxPages      int
	MaxBytes      int64
	Now           func() time.Time
}

// Name implements Feed.
func (f *NVDFeed) Name() string { return "nvd" }

type nvdResponse struct {
	ResultsPerPage  int `json:"resultsPerPage"`
	StartIndex      int `json:"startIndex"`
	TotalResults    int `json:"totalResults"`
	Vulnerabilities []struct {
		CVE nvdCVE `json:"cve"`
	} `json:"vulnerabilities"`
}

type nvdCVE struct {
	ID             string `json:"id"`
	Published      string `json:"published"`
	LastModified   string `json:"lastModified"`
	VulnStatus     string `json:"vulnStatus"`
	CISAExploitAdd string `json:"cisaExploitAdd"`
	Descriptions   []struct {
		Lang  string `json:"lang"`
		Value string `json:"value"`
	} `json:"descriptions"`
	Metrics struct {
		V31 []nvdMetric `json:"cvssMetricV31"`
		V30 []nvdMetric `json:"cvssMetricV30"`
	} `json:"metrics"`
	Configurations []struct {
		Nodes []struct {
			CPEMatch []struct {
				Vulnerable            bool   `json:"vulnerable"`
				Criteria              string `json:"criteria"`
				VersionStartIncluding string `json:"versionStartIncluding"`
				VersionStartExcluding string `json:"versionStartExcluding"`
				VersionEndIncluding   string `json:"versionEndIncluding"`
				VersionEndExcluding   string `json:"versionEndExcluding"`
			} `json:"cpeMatch"`
		} `json:"nodes"`
	} `json:"configurations"`
	References []struct {
		URL string `json:"url"`
	} `json:"references"`
}

type nvdMetric struct {
	CVSSData struct {
		BaseScore    float64 `json:"baseScore"`
		BaseSeverity string  `json:"baseSeverity"`
	} `json:"cvssData"`
}

func (c nvdCVE) advisory(fetched time.Time) Advisory {
	a := Advisory{
		ID: c.ID, Sources: []string{"nvd"}, Published: parseTime(c.Published), Modified: parseTime(c.LastModified),
		FetchedAt: fetched, KEV: c.CISAExploitAdd != "", Withdrawn: strings.EqualFold(c.VulnStatus, "Rejected"),
	}
	for _, d := range c.Descriptions {
		if d.Lang == "en" {
			a.Details = d.Value
			break
		}
	}
	for _, m := range append(c.Metrics.V31, c.Metrics.V30...) {
		if m.CVSSData.BaseScore > a.CVSS {
			a.CVSS = m.CVSSData.BaseScore
			a.Severity = ParseSeverity(m.CVSSData.BaseSeverity)
		}
	}
	for _, r := range c.References {
		a.References = append(a.References, r.URL)
	}
	for _, cfg := range c.Configurations {
		for _, n := range cfg.Nodes {
			for _, m := range n.CPEMatch {
				if !m.Vulnerable {
					continue
				}
				// cpe:2.3:part:vendor:product:version:...
				parts := strings.Split(m.Criteria, ":")
				if len(parts) < 6 || parts[0] != "cpe" {
					continue
				}
				af := Affected{Ecosystem: "cpe", Package: parts[3] + ":" + parts[4]}
				ver := parts[5]
				if ver != "*" && ver != "-" && ver != "" {
					af.Versions = []string{ver}
				} else {
					// versionStartExcluding has no exact equivalent; it is
					// treated as inclusive, which can only add findings
					// (a documented false-positive source), never hide one.
					r := Range{Introduced: m.VersionStartIncluding, Fixed: m.VersionEndExcluding, LastAffected: m.VersionEndIncluding}
					if r.Introduced == "" {
						r.Introduced = m.VersionStartExcluding
					}
					af.Ranges = []Range{r}
				}
				a.Affect = append(a.Affect, af)
			}
		}
	}
	return a.Normalize()
}

const nvdTimeLayout = "2006-01-02T15:04:05.000Z"

// Fetch implements Feed.
func (f *NVDFeed) Fetch(ctx context.Context, st FeedState, _ []Component) (FetchResult, error) {
	if f.BaseURL == "" {
		return FetchResult{}, fmt.Errorf("%w: NVD base URL not configured", ErrFeedFormat)
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	end := now().UTC()
	start := st.Cursor
	if start.IsZero() {
		w := f.InitialWindow
		if w <= 0 {
			w = 7 * 24 * time.Hour
		}
		start = end.Add(-w)
	}
	if end.Sub(start) > 120*24*time.Hour {
		start = end.Add(-120 * 24 * time.Hour)
	}
	headers := map[string]string{}
	if !f.APIKey.IsZero() {
		headers["apiKey"] = f.APIKey.Reveal()
	}
	pages := f.MaxPages
	if pages <= 0 {
		pages = 10
	}
	h := feedHTTP{Doer: f.HTTP, MaxBytes: f.MaxBytes}
	out := FetchResult{State: st}
	idx := 0
	for i := 0; i < pages; i++ {
		q := url.Values{
			"lastModStartDate": {start.Format(nvdTimeLayout)}, "lastModEndDate": {end.Format(nvdTimeLayout)},
			"startIndex": {strconv.Itoa(idx)}, "resultsPerPage": {"2000"},
		}
		var resp nvdResponse
		if _, _, err := h.do(ctx, http.MethodGet, strings.TrimRight(f.BaseURL, "/")+"/rest/json/cves/2.0?"+q.Encode(), nil, headers, nil, &resp); err != nil {
			return FetchResult{}, err
		}
		for _, v := range resp.Vulnerabilities {
			out.Advisories = append(out.Advisories, v.CVE.advisory(end))
		}
		idx = resp.StartIndex + len(resp.Vulnerabilities)
		if len(resp.Vulnerabilities) == 0 || idx >= resp.TotalResults {
			out.State.Cursor = end
			return out, nil
		}
	}
	// Page limit reached: keep the cursor so the next poll continues the
	// same window instead of skipping CVEs.
	return out, nil
}

// KEVFeed fetches the CISA known-exploited vulnerabilities catalog (one
// JSON document, conditional by ETag) and reports its CVE IDs.
type KEVFeed struct {
	URL      string
	HTTP     HTTPDoer
	MaxBytes int64
}

// Name implements Feed.
func (f *KEVFeed) Name() string { return "kev" }

// Fetch implements Feed.
func (f *KEVFeed) Fetch(ctx context.Context, st FeedState, _ []Component) (FetchResult, error) {
	if f.URL == "" {
		return FetchResult{}, fmt.Errorf("%w: KEV URL not configured", ErrFeedFormat)
	}
	var doc struct {
		Vulnerabilities []struct {
			CVE string `json:"cveID"`
		} `json:"vulnerabilities"`
	}
	out := FetchResult{State: st}
	_, notMod, err := feedHTTP{Doer: f.HTTP, MaxBytes: f.MaxBytes}.do(ctx, http.MethodGet, f.URL, nil, nil, &out.State, &doc)
	if err != nil {
		return FetchResult{}, err
	}
	if notMod {
		out.NotModified = true
		return out, nil
	}
	for _, v := range doc.Vulnerabilities {
		if isCVE(v.CVE) {
			out.KEV = append(out.KEV, strings.ToUpper(v.CVE))
		}
	}
	return out, nil
}
