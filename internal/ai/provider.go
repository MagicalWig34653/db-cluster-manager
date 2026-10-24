// SPDX-License-Identifier: AGPL-3.0-only

// Package ai is the optional AI provider layer of the vulnerability
// response (X13, ADR 0015, proposal). One interface covers five provider
// kinds: the Claude API, the OpenAI API, OpenAI-compatible endpoints (LM
// Studio, Ollama and similar, usually local), and the Claude Code and Codex
// CLI logins, which run as subprocesses through a restricted CommandRunner
// (no shell, absolute binary path, minimal environment, empty working
// directory, timeout, output caps). CLI logins are experimental and need an
// explicit acknowledgement (ADR 0015 open decisions 1 and 4).
//
// Safety: a provider is unusable until an admin consents to its data
// egress. Credentials are SecretStore references; values are wrapped in
// Secret and never logged, returned or put into prompts. Prompts carry
// only allow-listed, redacted context with untrusted text in a delimited
// data block (prompt.go). Output is parsed against a strict schema
// (proposal.go) and is only ever a proposal; nothing here executes it.
//
// Status: tested with httptest servers and a fake runner only. No real
// provider was called.
package ai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Kind is a provider kind.
type Kind string

const (
	KindClaudeAPI        Kind = "claude_api"
	KindOpenAIAPI        Kind = "openai_api"
	KindOpenAICompatible Kind = "openai_compatible"
	KindClaudeCodeCLI    Kind = "claude_code_cli"
	KindCodexCLI         Kind = "codex_cli"
)

// Kinds lists the provider kinds.
func Kinds() []Kind {
	return []Kind{KindClaudeAPI, KindOpenAIAPI, KindOpenAICompatible, KindClaudeCodeCLI, KindCodexCLI}
}

// CLI reports whether the kind runs a local CLI login.
func (k Kind) CLI() bool { return k == KindClaudeCodeCLI || k == KindCodexCLI }

// Errors.
var (
	ErrInvalidConfig     = errors.New("ai: invalid provider configuration")
	ErrNoConsent         = errors.New("ai: data egress for this provider has not been consented")
	ErrUnknownModel      = errors.New("ai: model not allowed for this provider")
	ErrProviderStatus    = errors.New("ai: provider answered with an error status")
	ErrResponseTooLarge  = errors.New("ai: provider response too large")
	ErrResponseMalformed = errors.New("ai: provider response malformed")
	ErrBudgetExceeded    = errors.New("ai: provider usage limit reached")
)

// Consent records an admin's agreement to send the stated data to a
// provider. Without it the provider cannot be opened.
type Consent struct {
	By        string    `json:"by"`
	At        time.Time `json:"at"`
	Statement string    `json:"statement"`
}

// Limits bound usage per provider and UTC day. Zero values select the
// defaults (50 requests, 200000 tokens).
type Limits struct {
	RequestsPerDay int `json:"requestsPerDay,omitempty"`
	TokensPerDay   int `json:"tokensPerDay,omitempty"`
}

// ProviderConfig is the stored, non-secret configuration of one provider.
type ProviderConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// BaseURL is the API root. Defaults: https://api.anthropic.com
	// (Claude), https://api.openai.com/v1 (OpenAI). Required for
	// OpenAI-compatible endpoints, for example http://127.0.0.1:1234/v1.
	BaseURL string `json:"baseUrl,omitempty"`
	// Models is the admin's allow-list of selectable models; empty allows
	// any syntactically valid model name. DefaultModel must be in it.
	Models       []string `json:"models,omitempty"`
	DefaultModel string   `json:"defaultModel,omitempty"`
	// CredentialRef names the SecretStore entry of the API key.
	CredentialRef string `json:"credentialRef,omitempty"`
	// Binary is the absolute path of the CLI (CLI kinds only); Home is the
	// directory holding its login state, passed as HOME.
	Binary string `json:"binary,omitempty"`
	Home   string `json:"home,omitempty"`
	// Experimental must be true for CLI kinds: the admin acknowledges that
	// unattended use of a personal login is unresolved (ADR 0015).
	Experimental    bool      `json:"experimental,omitempty"`
	Consent         *Consent  `json:"consent,omitempty"`
	Limits          Limits    `json:"limits"`
	MaxOutputTokens int       `json:"maxOutputTokens,omitempty"`
	TimeoutSeconds  int       `json:"timeoutSeconds,omitempty"`
	CreatedBy       string    `json:"createdBy,omitempty"`
	CreatedAt       time.Time `json:"createdAt,omitzero"`
}

var (
	idRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)
)

// DefaultBaseURL returns the default API root of a kind ("" when none).
func DefaultBaseURL(k Kind) string {
	switch k {
	case KindClaudeAPI:
		return "https://api.anthropic.com"
	case KindOpenAIAPI:
		return "https://api.openai.com/v1"
	}
	return ""
}

// Endpoint returns the effective base URL.
func (c ProviderConfig) Endpoint() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL(c.Kind)
}

// Timeout returns the request timeout (default 120 s, at most 600 s).
func (c ProviderConfig) Timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return 120 * time.Second
	}
	return time.Duration(min(c.TimeoutSeconds, 600)) * time.Second
}

// OutputTokens returns the output token cap (default 2048).
func (c ProviderConfig) OutputTokens() int {
	if c.MaxOutputTokens <= 0 {
		return 2048
	}
	return min(c.MaxOutputTokens, 16384)
}

// Validate checks the configuration.
func (c ProviderConfig) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidConfig, msg) }
	if !idRe.MatchString(c.ID) || c.Name == "" || len(c.Name) > 100 {
		return bad("id or name malformed")
	}
	if !slices.Contains(Kinds(), c.Kind) {
		return bad("unknown kind")
	}
	for _, m := range c.Models {
		if !modelRe.MatchString(m) {
			return bad("model name malformed")
		}
	}
	if c.DefaultModel != "" && (!modelRe.MatchString(c.DefaultModel) || (len(c.Models) > 0 && !slices.Contains(c.Models, c.DefaultModel))) {
		return bad("default model not in the model list")
	}
	if c.Kind.CLI() {
		switch {
		case !c.Experimental:
			return bad("CLI logins are experimental and need the explicit acknowledgement")
		case !strings.HasPrefix(c.Binary, "/") || strings.ContainsAny(c.Binary, "\x00\n") || strings.Contains(c.Binary, ".."):
			return bad("CLI binary must be an absolute path")
		case c.Home != "" && (!strings.HasPrefix(c.Home, "/") || strings.Contains(c.Home, "..")):
			return bad("CLI home must be an absolute path")
		case c.BaseURL != "" || c.CredentialRef != "":
			return bad("CLI logins take neither a base URL nor an API key")
		}
		return nil
	}
	if c.Binary != "" || c.Home != "" {
		return bad("binary and home apply to CLI kinds only")
	}
	if c.Kind == KindOpenAICompatible && c.BaseURL == "" {
		return bad("an OpenAI-compatible endpoint needs a base URL")
	}
	u, err := url.Parse(c.Endpoint())
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return bad("base URL malformed")
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Plain HTTP only to the local network: an API key must not cross
		// an untrusted network in clear text.
		if !LocalHost(u.Hostname()) {
			return bad("plain http is allowed only for loopback or private addresses")
		}
	default:
		return bad("base URL must be http or https")
	}
	if (c.Kind == KindClaudeAPI || c.Kind == KindOpenAIAPI) && c.CredentialRef == "" {
		return bad("an API key is required")
	}
	return nil
}

// LocalHost reports whether host is a loopback or private address or
// "localhost". Name resolution is not performed.
func LocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// Local reports whether data sent to this provider stays in the host
// network: OpenAI-compatible endpoints on local addresses. CLI logins
// talk to their vendor's service and are not local.
func (c ProviderConfig) Local() bool {
	if c.Kind != KindOpenAICompatible {
		return false
	}
	u, err := url.Parse(c.Endpoint())
	return err == nil && LocalHost(u.Hostname())
}

// EgressStatement is the text the admin consents to.
func (c ProviderConfig) EgressStatement() string {
	dest := c.Endpoint()
	switch c.Kind {
	case KindClaudeCodeCLI:
		dest = "Anthropic via the Claude Code CLI login at " + c.Binary
	case KindCodexCLI:
		dest = "OpenAI via the Codex CLI login at " + c.Binary
	}
	local := ""
	if c.Local() {
		local = " The endpoint is on a local address; data stays in the host network."
	}
	return "For each analysed finding, the manager sends to " + dest + ": package names and versions, the image name and tag " +
		"(without registry host), the advisory ID, summary and details, the stated fixed versions and the names of allow-listed " +
		"settings. Secrets, credentials, hostnames, IP addresses, database contents, logs and audit records are never sent; " +
		"all text passes redaction first." + local
}

// ModelAllowed reports whether model may be used with this provider.
func (c ProviderConfig) ModelAllowed(model string) bool {
	if !modelRe.MatchString(model) {
		return false
	}
	return len(c.Models) == 0 || slices.Contains(c.Models, model)
}

// Request is one completion request. System holds the fixed instructions;
// User holds the prompt with the delimited data block.
type Request struct {
	Model     string
	System    string
	User      string
	MaxTokens int
}

// Response is the provider's answer. Text is untrusted.
type Response struct {
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
}

// Provider is one configured AI provider.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
	// Models lists the provider's models where it has an endpoint for it;
	// CLI providers return the configured list.
	Models(ctx context.Context) ([]string, error)
}

// Deps are the side-effecting dependencies of providers.
type Deps struct {
	HTTP   HTTPDoer
	Runner CommandRunner
}

// Open validates the configuration, requires consent, resolves the
// credential and returns the provider.
func Open(ctx context.Context, c ProviderConfig, secrets SecretStore, d Deps) (Provider, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Consent == nil || c.Consent.By == "" {
		return nil, ErrNoConsent
	}
	var key Secret
	if c.CredentialRef != "" {
		if secrets == nil {
			return nil, ErrSecretNotFound
		}
		k, err := secrets.Get(ctx, c.CredentialRef)
		if err != nil {
			return nil, err
		}
		key = k
	}
	switch c.Kind {
	case KindClaudeAPI:
		return &anthropicProvider{cfg: c, key: key, http: guardHTTP(d.HTTP)}, nil
	case KindOpenAIAPI, KindOpenAICompatible:
		return &openAIProvider{cfg: c, key: key, http: guardHTTP(d.HTTP)}, nil
	case KindClaudeCodeCLI, KindCodexCLI:
		if d.Runner == nil {
			return nil, fmt.Errorf("%w: no command runner", ErrInvalidConfig)
		}
		return &cliProvider{cfg: c, runner: d.Runner}, nil
	}
	return nil, ErrInvalidConfig
}
