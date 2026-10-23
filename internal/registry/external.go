// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind classifies a registry.
type Kind string

const (
	KindGHCR      Kind = "ghcr"
	KindDockerHub Kind = "dockerhub"
	KindGeneric   Kind = "generic"
	// KindManaged is the manager-operated registry (see ManagedSpec).
	KindManaged Kind = "managed"
)

// ExternalConfig configures an external registry client.
type ExternalConfig struct {
	Kind Kind
	// Endpoint is the base URL (https://host[:port]). It is fixed for ghcr
	// and dockerhub and required for generic. Plain http is accepted only
	// for loopback hosts (tests, local registries).
	Endpoint string
	Username string
	// Password is a password or access token. It is only ever sent to the
	// registry endpoint and to its token realm (see allowedRealm), and is
	// never logged or put in errors.
	Password   Secret
	HTTPClient *http.Client
}

// External is a Registry backed by the OCI distribution API.
type External struct {
	cfg  ExternalConfig
	base *url.URL
	hc   *http.Client

	mu     sync.Mutex
	tokens map[string]string // scope -> bearer token
	basic  bool              // the server asked for basic auth
}

var _ Registry = (*External)(nil)

// DefaultEndpoint returns the fixed endpoint of a well-known kind.
func DefaultEndpoint(k Kind) string {
	switch k {
	case KindGHCR:
		return "https://ghcr.io"
	case KindDockerHub:
		return "https://registry-1.docker.io"
	}
	return ""
}

// ValidateEndpoint checks a registry endpoint URL.
func ValidateEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: endpoint must be a plain https://host[:port] URL", ErrInvalidConfig)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("%w: plain http is only accepted for loopback hosts", ErrInvalidConfig)
		}
	default:
		return nil, fmt.Errorf("%w: endpoint scheme must be https", ErrInvalidConfig)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewExternal creates a client; it does not contact the registry.
func NewExternal(cfg ExternalConfig) (*External, error) {
	ep := cfg.Endpoint
	switch cfg.Kind {
	case KindGHCR, KindDockerHub:
		if ep == "" {
			ep = DefaultEndpoint(cfg.Kind)
		}
	case KindGeneric:
		if ep == "" {
			return nil, fmt.Errorf("%w: generic registry needs an endpoint", ErrInvalidConfig)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported kind %q", ErrInvalidConfig, cfg.Kind)
	}
	base, err := ValidateEndpoint(ep)
	if err != nil {
		return nil, err
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &External{cfg: cfg, base: base, hc: hc, tokens: map[string]string{}}, nil
}

// allowedRealm decides whether the bearer token endpoint may receive the
// credentials: the registry's own host, or the known auth host of Docker Hub.
func (c *External) allowedRealm(u *url.URL) bool {
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return false
	}
	if u.Host == c.base.Host {
		return true
	}
	return c.cfg.Kind == KindDockerHub && u.Host == "auth.docker.io"
}

const manifestAccept = "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json"

var challengeParamRe = regexp.MustCompile(`([A-Za-z_]+)="([^"]*)"`)

// request performs one authenticated call, answering one auth challenge.
// body must be replayable.
func (c *External) request(ctx context.Context, op, method, target, scope string, body []byte, hdr map[string]string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rd)
		if err != nil {
			return nil, fmt.Errorf("registry: %s: %w", op, ErrInvalidName)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		c.mu.Lock()
		tok, haveTok := c.tokens[scope]
		basic := c.basic
		c.mu.Unlock()
		switch {
		case haveTok:
			req.Header.Set("Authorization", "Bearer "+tok)
		case basic && c.cfg.Username != "":
			req.SetBasicAuth(c.cfg.Username, c.cfg.Password.Reveal())
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry: %s: request failed", op) // the cause may embed the URL
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt == 1 {
			return resp, nil
		}
		ch := resp.Header.Get("WWW-Authenticate")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if err := c.answerChallenge(ctx, op, ch, scope); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("registry: %s: %w", op, ErrUnauthorized)
}

func (c *External) answerChallenge(ctx context.Context, op, header, scope string) error {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if c.cfg.Username == "" {
			return fmt.Errorf("registry: %s: %w", op, ErrUnauthorized)
		}
		c.mu.Lock()
		c.basic = true
		c.mu.Unlock()
		return nil
	case "bearer":
		params := map[string]string{}
		for _, m := range challengeParamRe.FindAllStringSubmatch(rest, -1) {
			params[strings.ToLower(m[1])] = m[2]
		}
		realm, err := url.Parse(params["realm"])
		if err != nil || !c.allowedRealm(realm) {
			return fmt.Errorf("registry: %s: token endpoint not permitted", op)
		}
		q := realm.Query()
		if s := params["service"]; s != "" {
			q.Set("service", s)
		}
		if scope != "" {
			q.Set("scope", scope)
		}
		realm.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
		if err != nil {
			return fmt.Errorf("registry: %s: %w", op, ErrUnauthorized)
		}
		if c.cfg.Username != "" {
			req.SetBasicAuth(c.cfg.Username, c.cfg.Password.Reveal())
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return fmt.Errorf("registry: %s: token request failed", op)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			return &Error{Op: op + " (token)", Status: resp.StatusCode}
		}
		var tr struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
			return fmt.Errorf("registry: %s: malformed token response", op)
		}
		tok := tr.Token
		if tok == "" {
			tok = tr.AccessToken
		}
		if tok == "" {
			return fmt.Errorf("registry: %s: %w", op, ErrUnauthorized)
		}
		c.mu.Lock()
		c.tokens[scope] = tok
		c.mu.Unlock()
		return nil
	}
	return fmt.Errorf("registry: %s: %w", op, ErrUnauthorized)
}

// failure turns a non-success response into an *Error, reading only the OCI
// error code from the body.
func failure(op string, resp *http.Response) error {
	defer resp.Body.Close()
	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	e := &Error{Op: op, Status: resp.StatusCode}
	if len(body.Errors) > 0 {
		e.Code = body.Errors[0].Code
	}
	return e
}

func (c *External) url(path string) string { return c.base.String() + path }

// sameHost resolves a Location/Link reference and refuses other hosts, so a
// hostile registry cannot make the client send credentials elsewhere.
func (c *External) sameHost(from string, ref string) (*url.URL, error) {
	b, err := url.Parse(from)
	if err != nil {
		return nil, err
	}
	u, err := b.Parse(ref)
	if err != nil || u.Host != c.base.Host {
		return nil, fmt.Errorf("registry: server referred to a different host")
	}
	return u, nil
}

// Ping checks that the endpoint speaks the distribution API and accepts the
// credentials.
func (c *External) Ping(ctx context.Context) error {
	resp, err := c.request(ctx, "ping", http.MethodGet, c.url("/v2/"), "", nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return failure("ping", resp)
	}
	resp.Body.Close()
	return nil
}

// Push implements Registry.
func (c *External) Push(ctx context.Context, repo, tag string, img Image) (string, error) {
	if !ValidRepository(repo) || !tagNameRe.MatchString(tag) {
		return "", ErrInvalidName
	}
	if img.MediaType == "" || len(img.Manifest) == 0 {
		return "", fmt.Errorf("%w: image needs a manifest and media type", ErrInvalidConfig)
	}
	for _, b := range img.Blobs {
		if b.Digest != DigestOf(b.Data) {
			return "", ErrDigestMismatch
		}
	}
	scope := "repository:" + repo + ":pull,push"
	for _, b := range img.Blobs {
		if err := c.pushBlob(ctx, repo, scope, b); err != nil {
			return "", err
		}
	}
	resp, err := c.request(ctx, "push manifest", http.MethodPut, c.url("/v2/"+repo+"/manifests/"+tag), scope,
		img.Manifest, map[string]string{"Content-Type": img.MediaType})
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", failure("push manifest", resp)
	}
	resp.Body.Close()
	digest := DigestOf(img.Manifest)
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" && d != digest {
		return "", ErrDigestMismatch
	}
	return digest, nil
}

func (c *External) pushBlob(ctx context.Context, repo, scope string, b Blob) error {
	resp, err := c.request(ctx, "check blob", http.MethodHead, c.url("/v2/"+repo+"/blobs/"+b.Digest), scope, nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	resp, err = c.request(ctx, "start upload", http.MethodPost, c.url("/v2/"+repo+"/blobs/uploads/"), scope, []byte{}, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		return failure("start upload", resp)
	}
	resp.Body.Close()
	loc, err := c.sameHost(c.url("/v2/"+repo+"/blobs/uploads/"), resp.Header.Get("Location"))
	if err != nil {
		return err
	}
	q := loc.Query()
	q.Set("digest", b.Digest)
	loc.RawQuery = q.Encode()
	resp, err = c.request(ctx, "upload blob", http.MethodPut, loc.String(), scope, b.Data,
		map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return failure("upload blob", resp)
	}
	resp.Body.Close()
	return nil
}

// Resolve implements Registry.
func (c *External) Resolve(ctx context.Context, repo, reference string) (string, error) {
	if !ValidRepository(repo) || !ValidReference(reference) {
		return "", ErrInvalidName
	}
	scope := "repository:" + repo + ":pull"
	resp, err := c.request(ctx, "resolve", http.MethodHead, c.url("/v2/"+repo+"/manifests/"+reference), scope, nil,
		map[string]string{"Accept": manifestAccept})
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", failure("resolve", resp)
	}
	resp.Body.Close()
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		// Fall back to hashing the manifest.
		body, _, err := c.getManifest(ctx, repo, reference)
		if err != nil {
			return "", err
		}
		d = DigestOf(body)
	}
	if !ValidDigest(d) || (ValidDigest(reference) && d != reference) {
		return "", ErrDigestMismatch
	}
	return d, nil
}

func (c *External) getManifest(ctx context.Context, repo, reference string) ([]byte, string, error) {
	resp, err := c.request(ctx, "get manifest", http.MethodGet, c.url("/v2/"+repo+"/manifests/"+reference),
		"repository:"+repo+":pull", nil, map[string]string{"Accept": manifestAccept})
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", failure("get manifest", resp)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", fmt.Errorf("registry: get manifest: read failed")
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// Tag implements Registry.
func (c *External) Tag(ctx context.Context, repo, from, to string) error {
	if !ValidRepository(repo) || !ValidReference(from) || !tagNameRe.MatchString(to) {
		return ErrInvalidName
	}
	body, mt, err := c.getManifest(ctx, repo, from)
	if err != nil {
		return err
	}
	if ValidDigest(from) && DigestOf(body) != from {
		return ErrDigestMismatch
	}
	resp, err := c.request(ctx, "tag", http.MethodPut, c.url("/v2/"+repo+"/manifests/"+to),
		"repository:"+repo+":pull,push", body, map[string]string{"Content-Type": mt})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return failure("tag", resp)
	}
	resp.Body.Close()
	return nil
}

// Delete implements Registry. The registry must have deletion enabled.
func (c *External) Delete(ctx context.Context, repo, digest string) error {
	if !ValidRepository(repo) || !ValidDigest(digest) {
		return ErrInvalidName
	}
	resp, err := c.request(ctx, "delete", http.MethodDelete, c.url("/v2/"+repo+"/manifests/"+digest),
		"repository:"+repo+":pull,delete", nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return failure("delete", resp)
	}
	resp.Body.Close()
	return nil
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// maxTagPages bounds pagination against a misbehaving server.
const maxTagPages = 100

// List implements Registry.
func (c *External) List(ctx context.Context, repo string) ([]string, error) {
	if !ValidRepository(repo) {
		return nil, ErrInvalidName
	}
	scope := "repository:" + repo + ":pull"
	next := c.url("/v2/" + repo + "/tags/list?n=100")
	var tags []string
	for page := 0; next != "" && page < maxTagPages; page++ {
		resp, err := c.request(ctx, "list", http.MethodGet, next, scope, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, failure("list", resp)
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("registry: list: malformed response")
		}
		tags = append(tags, body.Tags...)
		next = ""
		if m := nextLinkRe.FindStringSubmatch(link); m != nil {
			u, err := c.sameHost(c.url("/"), m[1])
			if err != nil {
				return nil, err
			}
			next = u.String()
		}
	}
	sort.Strings(tags)
	return tags, nil
}
