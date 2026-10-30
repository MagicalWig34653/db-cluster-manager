// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// vulnConfig is the "manager.vuln" section (X13, ADR 0015). Presence
// enables the vulnerability response; it needs auth.enabled. Nothing is
// contacted unless a feed with an explicit URL is configured here, and the
// policy stays notify_only until an administrator changes it in the UI.
type vulnConfig struct {
	// AllowCLIProviders explicitly permits CLI-login providers. These CLIs
	// inherit the manager process rights and must not run in the stock stack.
	AllowCLIProviders bool `json:"allowCLIProviders"`
	// PollIntervalSeconds is the feed polling interval of the leader
	// (default 3600, minimum 300).
	PollIntervalSeconds int `json:"pollIntervalSeconds"`
	// Feeds are the advisory sources; each needs an explicit URL (there are
	// no defaults). Every kind may appear once.
	Feeds []vulnFeedConfig `json:"feeds"`
	// DefaultMode seeds the policy mode when none is stored yet:
	// "notify_only" (default) or "approval_required". auto_for_policy can
	// only be enabled through the API with its allow-list and an explicit
	// opt-in; the stored policy is never overwritten by this setting.
	DefaultMode string `json:"defaultMode"`
	// Providers are AI providers registered when no provider with the ID
	// exists. They stay unusable until an administrator consents to the data
	// egress statement. CLI providers need AllowCLIProviders.
	Providers []vulnProviderConfig `json:"providers"`
}

// vulnFeedConfig configures one feed. Kind "osv", "github", "nvd" and "kev"
// need URL; "osvFile" reads a local OSV mirror file (Path). APIKeyFile
// names a file with the optional token or API key (never the key itself).
type vulnFeedConfig struct {
	Kind       string `json:"kind"`
	URL        string `json:"url"`
	Path       string `json:"path"`
	APIKeyFile string `json:"apiKeyFile"`
}

// vulnProviderConfig is a pre-registered AI provider. APIKeyFile names a
// file holding the API key (a Docker secret, for example); it is sealed
// into the metastore when the provider is registered.
type vulnProviderConfig struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Kind            ai.Kind   `json:"kind"`
	BaseURL         string    `json:"baseUrl"`
	Models          []string  `json:"models"`
	DefaultModel    string    `json:"defaultModel"`
	APIKeyFile      string    `json:"apiKeyFile"`
	Limits          ai.Limits `json:"limits"`
	MaxOutputTokens int       `json:"maxOutputTokens"`
	TimeoutSeconds  int       `json:"timeoutSeconds"`
	Binary          string    `json:"binary,omitempty"`
	Home            string    `json:"home,omitempty"`
	Experimental    bool      `json:"experimental,omitempty"`
}

func (c *vulnConfig) pollInterval() time.Duration {
	if c.PollIntervalSeconds > 0 {
		return time.Duration(c.PollIntervalSeconds) * time.Second
	}
	return time.Hour
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func validFeedURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("must be an absolute URL without credentials")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return errors.New("plain http is only accepted for loopback hosts")
		}
	default:
		return errors.New("scheme must be https")
	}
	return nil
}

func (c *vulnConfig) validate() error {
	if c.PollIntervalSeconds != 0 && c.PollIntervalSeconds < 300 {
		return errors.New("manager.vuln.pollIntervalSeconds must be at least 300")
	}
	switch c.DefaultMode {
	case "", string(vuln.ModeNotifyOnly), string(vuln.ModeApprovalRequired):
	default:
		return errors.New("manager.vuln.defaultMode must be notify_only or approval_required")
	}
	seen := map[string]bool{}
	for i, f := range c.Feeds {
		at := fmt.Sprintf("manager.vuln.feeds[%d]", i)
		if seen[f.Kind] {
			return fmt.Errorf("%s: kind %q is configured twice", at, f.Kind)
		}
		seen[f.Kind] = true
		switch f.Kind {
		case "osv", "github", "nvd", "kev":
			if err := validFeedURL(f.URL); err != nil {
				return fmt.Errorf("%s.url: %w", at, err)
			}
			if f.Path != "" {
				return fmt.Errorf("%s: path is only for osvFile", at)
			}
			if f.APIKeyFile != "" && !filepath.IsAbs(f.APIKeyFile) {
				return fmt.Errorf("%s.apiKeyFile must be an absolute path", at)
			}
			if f.APIKeyFile != "" && (f.Kind == "osv" || f.Kind == "kev") {
				return fmt.Errorf("%s: %s takes no API key", at, f.Kind)
			}
		case "osvFile":
			if !filepath.IsAbs(f.Path) {
				return fmt.Errorf("%s.path must be an absolute path", at)
			}
			if f.URL != "" || f.APIKeyFile != "" {
				return fmt.Errorf("%s: osvFile takes only path", at)
			}
		default:
			return fmt.Errorf("%s.kind %q is unknown (osv, github, nvd, kev, osvFile)", at, f.Kind)
		}
	}
	ids := map[string]bool{}
	for i, p := range c.Providers {
		at := fmt.Sprintf("manager.vuln.providers[%d]", i)
		if ids[p.ID] {
			return fmt.Errorf("%s: id %q is configured twice", at, p.ID)
		}
		ids[p.ID] = true
		if p.Kind.CLI() {
			if !c.AllowCLIProviders {
				return fmt.Errorf("%s: CLI providers require manager.vuln.allowCLIProviders=true", at)
			}
			if p.APIKeyFile != "" {
				return fmt.Errorf("%s: CLI providers take no API key", at)
			}
		}
		if p.APIKeyFile != "" && !filepath.IsAbs(p.APIKeyFile) {
			return fmt.Errorf("%s.apiKeyFile must be an absolute path", at)
		}
		pc := p.providerConfig()
		if p.APIKeyFile != "" {
			pc.CredentialRef = "x"
		}
		if err := pc.Validate(); err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
	}
	return nil
}

func (p vulnProviderConfig) providerConfig() ai.ProviderConfig {
	return ai.ProviderConfig{ID: p.ID, Name: p.Name, Kind: p.Kind, BaseURL: p.BaseURL, Models: p.Models, DefaultModel: p.DefaultModel,
		Limits: p.Limits, MaxOutputTokens: p.MaxOutputTokens, TimeoutSeconds: p.TimeoutSeconds,
		Binary: p.Binary, Home: p.Home, Experimental: p.Experimental}
}

func readKeyFile(path string) (ai.Secret, error) {
	if path == "" {
		return ai.Secret{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ai.Secret{}, fmt.Errorf("read key file: %w", err)
	}
	return ai.NewSecret(strings.TrimSpace(string(b))), nil
}

// vulnHTTP is the HTTP client of feeds and AI providers. It follows no
// redirects, so a feed or provider cannot move a request (and its
// credentials) to another host.
func vulnHTTP() *http.Client {
	return &http.Client{
		Timeout:       60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// buildFeeds creates the configured feeds. Key files are read here; a
// failure is returned without naming the file content.
func (c *vulnConfig) buildFeeds(h vuln.HTTPDoer) ([]vuln.Feed, error) {
	var feeds []vuln.Feed
	for _, f := range c.Feeds {
		key, err := readKeyFile(f.APIKeyFile)
		if err != nil {
			return nil, fmt.Errorf("manager.vuln feed %s: %w", f.Kind, err)
		}
		switch f.Kind {
		case "osv":
			feeds = append(feeds, &vuln.OSVFeed{BaseURL: f.URL, HTTP: h})
		case "github":
			feeds = append(feeds, &vuln.GitHubFeed{BaseURL: f.URL, HTTP: h, Token: key})
		case "nvd":
			feeds = append(feeds, &vuln.NVDFeed{BaseURL: f.URL, HTTP: h, APIKey: key})
		case "kev":
			feeds = append(feeds, &vuln.KEVFeed{URL: f.URL, HTTP: h})
		case "osvFile":
			path := f.Path
			feeds = append(feeds, &vuln.OfflineFeed{Label: filepath.Base(path), Load: func() ([]byte, error) { return os.ReadFile(path) }})
		}
	}
	return feeds, nil
}

// newVulnService builds the vulnerability service of one leadership.
func (rt *managedRuntime) newVulnService(images *api.ImageBuildBackend) (*vuln.Service, error) {
	vc := rt.vulnCfg
	httpc := vulnHTTP()
	feeds, err := vc.buildFeeds(httpc)
	if err != nil {
		return nil, err
	}
	kv := vuln.MetastoreKV{Node: rt.svc.Store().Node()}
	svc := &vuln.Service{
		Store:             &vuln.Store{KV: kv},
		Inventory:         manager.VulnInventory{Store: rt.svc.Store(), Jobs: manager.ImageJobStore{Store: rt.svc.Store()}, Registries: images.Regs},
		Feeds:             feeds,
		IsLeader:          rt.svc.IsLeader,
		Secrets:           manager.SealedSecretStore{Store: rt.svc.Store(), Keys: rt.keys},
		AIDeps:            ai.Deps{HTTP: httpc, Runner: ai.ExecRunner{}, AllowCLIProviders: vc.AllowCLIProviders},
		Budget:            &ai.Budget{Backing: vuln.BudgetStore{KV: kv}},
		AllowCLIProviders: vc.AllowCLIProviders,
		RedactNames:       manager.VulnRedactNames(rt.svc.Store(), images.Regs),
		Catalog:           imagebuild.DefaultCatalog(),
		Notifier: manager.VulnNotifier{Send: func(ctx context.Context, e notify.Event) bool {
			if rt.monitoring == nil {
				return false
			}
			return rt.monitoring.Notify(ctx, e)
		}},
	}
	if images.Builds != nil {
		svc.Executor = vuln.ImageBuildExecutor{Builds: images.Builds}
	}
	svc.Budget.Reload()
	svc.ResetSeq()
	return svc, nil
}

// seedVuln applies the configured defaults once: the policy mode when no
// policy is stored and providers that do not exist yet. It never changes
// stored state and never grants consent.
func (rt *managedRuntime) seedVuln(ctx context.Context, svc *vuln.Service, log *slog.Logger) {
	vc := rt.vulnCfg
	if vc.DefaultMode == string(vuln.ModeApprovalRequired) {
		if p, ver, err := svc.Policy(); err == nil && ver == 0 {
			p.Mode = vuln.ModeApprovalRequired
			if _, err := svc.SetPolicy(ctx, "config", p, 0); err != nil {
				log.Warn("manager: vulnerability default mode not applied", "err", err)
			}
		}
	}
	for _, p := range vc.Providers {
		key, err := readKeyFile(p.APIKeyFile)
		if err != nil {
			log.Warn("manager: vulnerability provider not registered", "provider", p.ID, "err", err)
			continue
		}
		if _, err := svc.AddProvider(ctx, "config", p.providerConfig(), key); err != nil && !errors.Is(err, vuln.ErrConflict) {
			log.Warn("manager: vulnerability provider not registered", "provider", p.ID, "err", err)
		}
	}
}

// pollVuln is the scheduler's poll of the current leadership's service.
func (rt *managedRuntime) pollVuln(ctx context.Context) error {
	svc := rt.vuln.Load()
	if svc == nil || !rt.on("vuln") {
		return nil
	}
	_, err := svc.Poll(ctx, "manager")
	return err
}
