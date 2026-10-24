// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Feed polling.
//
// A Feed fetches advisories (or, for the KEV catalog, known-exploited IDs)
// through an HTTPDoer. Requests are conditional where the feed supports it
// (ETag / Last-Modified), response bodies are size-capped, rate-limit
// answers (429, 503 with Retry-After) back off, and every fetch updates a
// FeedState that makes a stale feed visible. The adapters were written from
// the public API documentation of OSV, the GitHub global advisory API, the
// NVD CVE API 2.0 and the CISA KEV catalog and are tested against httptest
// fixtures in that shape; none was run against the real service.

// HTTPDoer is the HTTP client seen by the feeds; *http.Client implements
// it. Tests pass a client bound to an httptest server.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// FeedState is the persisted polling state of one feed.
type FeedState struct {
	Name         string `json:"name"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	// Cursor is the feed's incremental position (the last modification
	// time seen), used by feeds that query "changed since".
	Cursor      time.Time `json:"cursor,omitzero"`
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
	// LastError is a coarse error class, never response text.
	LastError   string    `json:"lastError,omitempty"`
	Failures    int       `json:"failures,omitempty"`
	NextAttempt time.Time `json:"nextAttempt,omitzero"`
	Stale       bool      `json:"stale"`
	Advisories  int       `json:"advisories,omitempty"`
}

// FetchResult is the outcome of one fetch.
type FetchResult struct {
	Advisories []Advisory
	// KEV lists CVE IDs from a known-exploited catalog.
	KEV []string
	// NotModified reports a 304 answer: nothing changed since the last
	// fetch.
	NotModified bool
	// State is the updated state (ETag, cursor); the poller adds timing.
	State FeedState
}

// Feed is one advisory source.
type Feed interface {
	Name() string
	// Fetch fetches what changed since st. components is the current
	// inventory for feeds that query per package (OSV); others ignore it.
	Fetch(ctx context.Context, st FeedState, components []Component) (FetchResult, error)
}

// Feed errors.
var (
	ErrRateLimited = errors.New("vuln: feed rate limited")
	ErrFeedStatus  = errors.New("vuln: feed answered with an error status")
	ErrFeedTooBig  = errors.New("vuln: feed response too large")
	ErrFeedFormat  = errors.New("vuln: feed response malformed")
)

// RateLimitError carries the server's Retry-After.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("vuln: feed rate limited (retry after %s)", e.RetryAfter)
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// DefaultMaxFeedBytes caps one response body.
const DefaultMaxFeedBytes = 32 << 20

// feedHTTP is the shared request helper.
type feedHTTP struct {
	Doer     HTTPDoer
	MaxBytes int64
	// UserAgent identifies the manager to the feed operator.
	UserAgent string
}

func (h feedHTTP) maxBytes() int64 {
	if h.MaxBytes > 0 {
		return h.MaxBytes
	}
	return DefaultMaxFeedBytes
}

// do performs a request and decodes a JSON answer into out. With
// conditional set it sends If-None-Match / If-Modified-Since from st and
// reports notModified on 304. It returns the response headers.
func (h feedHTTP) do(ctx context.Context, method, url string, body any, headers map[string]string, st *FeedState, out any) (http.Header, bool, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, false, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	ua := h.UserAgent
	if ua == "" {
		ua = "db-cluster-manager-vuln/0"
	}
	req.Header.Set("User-Agent", ua)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if st != nil {
		if st.ETag != "" {
			req.Header.Set("If-None-Match", st.ETag)
		}
		if st.LastModified != "" {
			req.Header.Set("If-Modified-Since", st.LastModified)
		}
	}
	resp, err := h.Doer.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return resp.Header, true, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("Retry-After") != ""):
		return resp.Header, false, &RateLimitError{RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return resp.Header, false, fmt.Errorf("%w: %d", ErrFeedStatus, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, h.maxBytes()+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > h.maxBytes() {
		return nil, false, ErrFeedTooBig
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrFeedFormat, err)
	}
	if st != nil {
		st.ETag = resp.Header.Get("ETag")
		st.LastModified = resp.Header.Get("Last-Modified")
	}
	return resp.Header, false, nil
}

func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
