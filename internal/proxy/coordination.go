// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// WriterHint is the replicated manager decision. Empty Writer means a
// transition freeze; no proxy may accept a write connection in that phase.
type WriterHint struct {
	Epoch   uint64 `json:"epoch"`
	Writer  string `json:"writer"`
	Pending string `json:"pending,omitempty"`
}

// WriterSource fetches the leader's barrier-protected decision and confirms
// that this proxy has closed all write connections during a freeze.
type WriterSource interface {
	Fetch(context.Context, CoordinationConfig) (WriterHint, error)
	Ack(context.Context, CoordinationConfig, uint64) error
}

type HTTPWriterSource struct{ Client *http.Client }

func (s HTTPWriterSource) request(ctx context.Context, c CoordinationConfig, method, suffix string, body []byte) ([]byte, error) {
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return nil, err
	}
	t := strings.TrimSpace(string(token))
	if t == "" {
		return nil, errors.New("empty coordination token")
	}
	client := s.Client
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	var last error
	for _, base := range c.ManagerURLs {
		endpoint := base + "/api/v1/proxy-writers/" + url.PathEscape(c.ClusterID) + suffix
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+t)
		req.Header.Set("X-Proxy-ID", c.ProxyID)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if readErr != nil {
			last = readErr
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return b, nil
		}
		last = fmt.Errorf("manager %s: HTTP %d", base, resp.StatusCode)
	}
	if last == nil {
		last = errors.New("no manager URLs")
	}
	return nil, last
}

func (s HTTPWriterSource) Fetch(ctx context.Context, c CoordinationConfig) (WriterHint, error) {
	b, err := s.request(ctx, c, http.MethodGet, "", nil)
	if err != nil {
		return WriterHint{}, err
	}
	var h WriterHint
	if err := json.Unmarshal(b, &h); err != nil {
		return h, err
	}
	if h.Epoch == 0 || (h.Writer == "") == (h.Pending == "") {
		return WriterHint{}, errors.New("invalid manager writer choice")
	}
	return h, nil
}

func (s HTTPWriterSource) Ack(ctx context.Context, c CoordinationConfig, epoch uint64) error {
	b, err := json.Marshal(struct {
		ProxyID string `json:"proxy_id"`
		Epoch   uint64 `json:"epoch"`
	}{c.ProxyID, epoch})
	if err != nil {
		return err
	}
	_, err = s.request(ctx, c, http.MethodPost, "/ack", b)
	return err
}
