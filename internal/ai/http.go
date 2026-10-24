// SPDX-License-Identifier: AGPL-3.0-only

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HTTPDoer is the HTTP client seen by the API providers.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// MaxResponseBytes caps one provider response body.
const MaxResponseBytes = 1 << 20

var errRedirect = errors.New("ai: provider redirects are not followed")

// guardHTTP returns a client that never follows redirects, so an API key
// header cannot be forwarded to another host. A non-*http.Client doer
// (tests) is used as is.
func guardHTTP(d HTTPDoer) HTTPDoer {
	c, ok := d.(*http.Client)
	if d == nil {
		c, ok = &http.Client{}, true
	}
	if !ok {
		return d
	}
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &cp
}

// postJSON sends body and decodes the answer. Error bodies are discarded:
// they may echo the prompt and are never forwarded or logged.
func postJSON(ctx context.Context, h HTTPDoer, method, url string, headers map[string]string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.Do(req)
	if err != nil {
		// Do not wrap the URL error text verbatim into logs upstream; it
		// carries no header values, but keep it coarse anyway.
		return fmt.Errorf("ai: request failed: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
		return fmt.Errorf("%w: %d", ErrProviderStatus, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > MaxResponseBytes {
		return ErrResponseTooLarge
	}
	if err := json.Unmarshal(data, out); err != nil {
		return ErrResponseMalformed
	}
	return nil
}

func unwrapURLError(err error) error {
	if errors.Is(err, errRedirect) {
		return errRedirect
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New("transport error")
}

// anthropicProvider calls the Claude Messages API.
type anthropicProvider struct {
	cfg  ProviderConfig
	key  Secret
	http HTTPDoer
}

func (p *anthropicProvider) headers() map[string]string {
	return map[string]string{"x-api-key": p.key.Reveal(), "anthropic-version": "2023-06-01"}
}

// Complete implements Provider.
func (p *anthropicProvider) Complete(ctx context.Context, r Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout())
	defer cancel()
	body := map[string]any{
		"model": r.Model, "max_tokens": r.MaxTokens, "system": r.System,
		"messages": []map[string]any{{"role": "user", "content": r.User}},
	}
	var out struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := postJSON(ctx, p.http, http.MethodPost, p.cfg.Endpoint()+"/v1/messages", p.headers(), body, &out); err != nil {
		return Response{}, err
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return Response{Text: b.String(), Model: out.Model, InputTokens: out.Usage.In, OutputTokens: out.Usage.Out}, nil
}

// Models implements Provider.
func (p *anthropicProvider) Models(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout())
	defer cancel()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := postJSON(ctx, p.http, http.MethodGet, p.cfg.Endpoint()+"/v1/models", p.headers(), nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

// openAIProvider calls a Chat Completions endpoint (OpenAI or compatible).
type openAIProvider struct {
	cfg  ProviderConfig
	key  Secret
	http HTTPDoer
}

func (p *openAIProvider) headers() map[string]string {
	if p.key.IsZero() {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + p.key.Reveal()}
}

// Complete implements Provider.
func (p *openAIProvider) Complete(ctx context.Context, r Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout())
	defer cancel()
	body := map[string]any{
		"model": r.Model, "max_tokens": r.MaxTokens,
		"messages": []map[string]any{{"role": "system", "content": r.System}, {"role": "user", "content": r.User}},
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			In  int `json:"prompt_tokens"`
			Out int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := postJSON(ctx, p.http, http.MethodPost, p.cfg.Endpoint()+"/chat/completions", p.headers(), body, &out); err != nil {
		return Response{}, err
	}
	if len(out.Choices) == 0 {
		return Response{}, ErrResponseMalformed
	}
	return Response{Text: out.Choices[0].Message.Content, Model: out.Model, InputTokens: out.Usage.In, OutputTokens: out.Usage.Out}, nil
}

// Models implements Provider.
func (p *openAIProvider) Models(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout())
	defer cancel()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := postJSON(ctx, p.http, http.MethodGet, p.cfg.Endpoint()+"/models", p.headers(), nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}
