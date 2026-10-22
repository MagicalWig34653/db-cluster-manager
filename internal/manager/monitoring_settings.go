// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"errors"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

// MonitoringSettings are the replicated monitoring settings (KONZEPT U03-U06).
// Secrets are never stored here: webhook and SMTP credentials are read from
// files on the manager host (SecretFile, PasswordFile), so the replicated
// settings document carries no secret.
type MonitoringSettings struct {
	// RawRetentionDays is how long detailed values are kept (default 7).
	RawRetentionDays int `json:"rawRetentionDays,omitempty"`
	// AggRetentionDays is how long aggregated values are kept (default 90,
	// at least RawRetentionDays).
	AggRetentionDays int `json:"aggRetentionDays,omitempty"`
	// MaxBytes is the disk budget of the metric store (default 2 GiB,
	// minimum 64 MiB).
	MaxBytes int64 `json:"maxBytes,omitempty"`
	// CollectIntervalSeconds is the scrape interval (default 15, minimum 5).
	CollectIntervalSeconds int `json:"collectIntervalSeconds,omitempty"`
	// DisabledRules lists default alert rule IDs that are switched off.
	DisabledRules []string `json:"disabledRules,omitempty"`
	// Notifications configures the delivery channels.
	Notifications NotificationSettings `json:"notifications,omitzero"`
}

// NotificationSettings configures delivery. The UI inbox is always on.
type NotificationSettings struct {
	// MinSeverity is the lowest severity sent to webhook and email: "info",
	// "warning" (default), "error" or "critical". Unknown-state events are
	// treated as warning.
	MinSeverity string           `json:"minSeverity,omitempty"`
	Webhook     *WebhookSettings `json:"webhook,omitempty"`
	Email       *EmailSettings   `json:"email,omitempty"`
}

// WebhookSettings configures the signed webhook channel.
type WebhookSettings struct {
	URL string `json:"url"`
	// SecretFile holds the HMAC key (read at start; surrounding whitespace
	// is trimmed).
	SecretFile   string `json:"secretFile"`
	AllowPrivate bool   `json:"allowPrivate,omitempty"`
	MaxRetries   int    `json:"maxRetries,omitempty"`
}

// EmailSettings configures the SMTP channel (STARTTLS required unless
// AllowInsecure).
type EmailSettings struct {
	Addr          string   `json:"addr"`
	From          string   `json:"from"`
	To            []string `json:"to"`
	Username      string   `json:"username,omitempty"`
	PasswordFile  string   `json:"passwordFile,omitempty"`
	AllowInsecure bool     `json:"allowInsecure,omitempty"`
}

// Defaults of MonitoringSettings.
const (
	DefaultRawRetentionDays   = 7
	DefaultAggRetentionDays   = 90
	DefaultMetricsMaxBytes    = int64(2 << 30)
	MinMetricsMaxBytes        = int64(64 << 20)
	DefaultCollectIntervalSec = 15
	MinCollectIntervalSec     = 5
)

// Effective returns the settings with defaults applied.
func (m MonitoringSettings) Effective() MonitoringSettings {
	if m.RawRetentionDays == 0 {
		m.RawRetentionDays = DefaultRawRetentionDays
	}
	if m.AggRetentionDays == 0 {
		m.AggRetentionDays = max(DefaultAggRetentionDays, m.RawRetentionDays)
	}
	if m.MaxBytes == 0 {
		m.MaxBytes = DefaultMetricsMaxBytes
	}
	if m.CollectIntervalSeconds == 0 {
		m.CollectIntervalSeconds = DefaultCollectIntervalSec
	}
	if m.Notifications.MinSeverity == "" {
		m.Notifications.MinSeverity = "warning"
	}
	return m
}

// Validate checks the settings after defaults are applied.
func (m MonitoringSettings) Validate() error {
	e := m.Effective()
	var errs []error
	if e.RawRetentionDays < 1 {
		errs = append(errs, errors.New("rawRetentionDays must be at least 1"))
	}
	if e.AggRetentionDays < e.RawRetentionDays {
		errs = append(errs, errors.New("aggRetentionDays must not be below rawRetentionDays"))
	}
	if e.MaxBytes < MinMetricsMaxBytes {
		errs = append(errs, fmt.Errorf("maxBytes must be at least %d", MinMetricsMaxBytes))
	}
	if e.CollectIntervalSeconds < MinCollectIntervalSec {
		errs = append(errs, fmt.Errorf("collectIntervalSeconds must be at least %d", MinCollectIntervalSec))
	}
	if _, err := parseMinSeverity(e.Notifications.MinSeverity); err != nil {
		errs = append(errs, err)
	}
	if w := e.Notifications.Webhook; w != nil && (w.URL == "" || w.SecretFile == "") {
		errs = append(errs, errors.New("webhook needs url and secretFile"))
	}
	if x := e.Notifications.Email; x != nil && (x.Addr == "" || x.From == "" || len(x.To) == 0) {
		errs = append(errs, errors.New("email needs addr, from and at least one recipient"))
	}
	return errors.Join(errs...)
}

func parseMinSeverity(s string) (notify.Severity, error) {
	switch s {
	case "info":
		return notify.SeverityInfo, nil
	case "warning":
		return notify.SeverityWarning, nil
	case "error":
		return notify.SeverityError, nil
	case "critical":
		return notify.SeverityCritical, nil
	}
	return 0, fmt.Errorf("unknown minSeverity %q", s)
}

func (m MonitoringSettings) rawRetention() time.Duration {
	return time.Duration(m.RawRetentionDays) * 24 * time.Hour
}
func (m MonitoringSettings) aggRetention() time.Duration {
	return time.Duration(m.AggRetentionDays) * 24 * time.Hour
}
