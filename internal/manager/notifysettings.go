// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

// ErrInvalidSettings: settings that fail validation.
var ErrInvalidSettings = errors.New("manager: invalid settings")

var settingsRes = authz.Resource{Type: authz.ResourceSettings, Scope: authz.Global}

// SettingsService implements api.Settings: the notification channels of
// the monitoring settings. Secrets set through the API are sealed with the
// data key in CollNotifySecrets and never returned; the replicated
// settings document only records that one is stored.
type SettingsService struct {
	Svc        *Service
	Authorizer *authz.Authorizer
	Keys       KeyProvider
	// Monitoring is reloaded after a change and builds the channels for a
	// test delivery; nil makes tests unavailable.
	Monitoring *Monitoring
}

var _ api.Settings = (*SettingsService)(nil)

func (s *SettingsService) current(opts ...ReadOption) (Settings, uint64, error) {
	v, _, err := Get[Settings](s.Svc.store, CollSettings, SettingsKey, opts...)
	return v.Value, v.Version, err
}

func notificationView(m MonitoringSettings) api.NotificationSettings {
	e := m.Effective()
	out := api.NotificationSettings{MinSeverity: e.Notifications.MinSeverity, Email: api.EmailSettingsView{Recipients: []string{}}}
	if w := e.Notifications.Webhook; w != nil {
		out.Webhook = api.WebhookSettingsView{Enabled: !w.Disabled, URL: w.URL, SecretSet: w.SecretFile != "" || w.SecretStored,
			AllowPrivate: w.AllowPrivate}
	}
	if x := e.Notifications.Email; x != nil {
		out.Email = api.EmailSettingsView{Enabled: !x.Disabled, Recipients: slices.Clone(x.To), Addr: x.Addr, From: x.From,
			Username: x.Username, PasswordSet: x.PasswordFile != "" || x.PasswordStored}
		if out.Email.Recipients == nil {
			out.Email.Recipients = []string{}
		}
	}
	return out
}

// NotificationSettings implements api.Settings.
func (s *SettingsService) NotificationSettings(context.Context) (api.NotificationSettings, error) {
	st, _, err := s.current()
	if err != nil {
		return api.NotificationSettings{}, MapCommandError(err)
	}
	return notificationView(st.Monitoring), nil
}

// SetNotificationSettings implements api.Settings. It needs the settings
// update permission (audited).
func (s *SettingsService) SetNotificationSettings(_ context.Context, actor authz.Subject, in api.NotificationSettingsInput) (api.NotificationSettings, error) {
	out, err := s.set(actor, in)
	return out, MapCommandError(err)
}

func (s *SettingsService) set(actor authz.Subject, in api.NotificationSettingsInput) (api.NotificationSettings, error) {
	if l := s.Svc.Current(); l == nil || !l.Valid() {
		return api.NotificationSettings{}, ErrNotLeader
	}
	if d := s.Authorizer.Authorize(actor, authz.ActionUpdate, settingsRes); !d.Allowed {
		return api.NotificationSettings{}, &authz.DeniedError{Decision: d}
	}
	st, version, err := s.current(Consistent())
	if err != nil {
		return api.NotificationSettings{}, err
	}
	n := st.Monitoring.Notifications
	if in.MinSeverity != "" {
		n.MinSeverity = in.MinSeverity
	}
	setWebhookSecret, setEmailPassword := in.Webhook.Secret, in.Email.Password

	// Webhook: keep the stored configuration, apply the request.
	w := WebhookSettings{}
	if n.Webhook != nil {
		w = *n.Webhook
	}
	w.Disabled, w.URL = !in.Webhook.Enabled, strings.TrimSpace(in.Webhook.URL)
	if in.Webhook.AllowPrivate != nil {
		w.AllowPrivate = *in.Webhook.AllowPrivate
	}
	if setWebhookSecret != nil {
		w.SecretStored, w.SecretFile = *setWebhookSecret != "", ""
	}
	if w.URL == "" && !in.Webhook.Enabled && !w.SecretStored && w.SecretFile == "" {
		n.Webhook = nil
	} else {
		n.Webhook = &w
	}
	if in.Webhook.Enabled {
		if _, err := notify.NewWebhook(notify.WebhookConfig{URL: w.URL, Secret: []byte("validation"), AllowPrivate: w.AllowPrivate}); err != nil {
			return api.NotificationSettings{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
		}
	}

	x := EmailSettings{}
	if n.Email != nil {
		x = *n.Email
	}
	x.Disabled = !in.Email.Enabled
	x.To = nil
	for _, r := range in.Email.Recipients {
		if r = strings.TrimSpace(r); r != "" {
			x.To = append(x.To, r)
		}
	}
	if in.Email.Addr != nil {
		x.Addr = strings.TrimSpace(*in.Email.Addr)
	}
	if in.Email.From != nil {
		x.From = strings.TrimSpace(*in.Email.From)
	}
	if in.Email.Username != nil {
		x.Username = strings.TrimSpace(*in.Email.Username)
	}
	if setEmailPassword != nil {
		x.PasswordStored, x.PasswordFile = *setEmailPassword != "", ""
	}
	if !in.Email.Enabled && x.Addr == "" && x.From == "" && len(x.To) == 0 && !x.PasswordStored && x.PasswordFile == "" {
		n.Email = nil
	} else {
		n.Email = &x
	}
	if in.Email.Enabled {
		if _, err := notify.NewEmail(notify.EmailConfig{Addr: x.Addr, From: x.From, To: x.To, Username: x.Username, AllowInsecure: x.AllowInsecure}); err != nil {
			return api.NotificationSettings{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
		}
	}

	next := st
	next.Monitoring.Notifications = n
	if err := next.Monitoring.Validate(); err != nil {
		return api.NotificationSettings{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	// Secrets first: the settings document must never claim a stored secret
	// that is not there.
	for key, v := range map[string]*string{"webhook": setWebhookSecret, "email": setEmailPassword} {
		if v == nil {
			continue
		}
		if *v == "" {
			if err := Delete(s.Svc.store, CollNotifySecrets, key, nil); err != nil && !errors.Is(err, ErrNotFound) {
				return api.NotificationSettings{}, err
			}
			continue
		}
		if s.Keys == nil {
			return api.NotificationSettings{}, ErrNoKey
		}
		if err := PutSealed(s.Svc.store, s.Keys, CollNotifySecrets, key, *v); err != nil {
			return api.NotificationSettings{}, err
		}
	}
	if _, err := Put(s.Svc.store, CollSettings, SettingsKey, next, IfVersion(version)); err != nil {
		return api.NotificationSettings{}, err
	}
	s.Svc.recordEvent("", "", api.SeverityInfo, fmt.Sprintf("%s changed the notification settings", actor.ID))
	if s.Monitoring != nil {
		s.Monitoring.Reload()
	}
	return notificationView(next.Monitoring), nil
}

// TestNotification implements api.Settings. It needs the settings operate
// permission (audited). The delivery outcome carries no error text.
func (s *SettingsService) TestNotification(ctx context.Context, actor authz.Subject, channel string) (api.NotificationTest, error) {
	if l := s.Svc.Current(); l == nil || !l.Valid() {
		return api.NotificationTest{}, MapCommandError(ErrNotLeader)
	}
	if d := s.Authorizer.Authorize(actor, authz.ActionOperate, settingsRes); !d.Allowed {
		return api.NotificationTest{}, MapCommandError(&authz.DeniedError{Decision: d})
	}
	if s.Monitoring == nil {
		return api.NotificationTest{}, api.ErrUnavailable
	}
	st, _, err := s.current(Consistent())
	if err != nil {
		return api.NotificationTest{}, MapCommandError(err)
	}
	n := st.Monitoring.Notifications
	var ch notify.Channel
	switch channel {
	case "webhook":
		if n.Webhook == nil || n.Webhook.Disabled {
			return api.NotificationTest{}, MapCommandError(fmt.Errorf("%w: webhook not enabled", ErrSettingsIncomplete))
		}
		ch, err = s.Monitoring.webhookChannel(n.Webhook)
	case "email":
		if n.Email == nil || n.Email.Disabled {
			return api.NotificationTest{}, MapCommandError(fmt.Errorf("%w: email not enabled", ErrSettingsIncomplete))
		}
		ch, err = s.Monitoring.emailChannel(n.Email)
	default:
		return api.NotificationTest{}, api.ErrInvalidInput
	}
	if err != nil {
		s.Svc.log.Warn("manager: test notification channel not built", "channel", channel, "err", err)
		return api.NotificationTest{Channel: channel}, nil
	}
	e := notify.Event{ID: newID("test"), Time: s.Svc.cfg.Now(), Severity: notify.SeverityInfo, Kind: notify.KindPrefix + "test",
		Resource: "manager", Message: "Test notification from the DB cluster manager (requested by " + actor.ID + ")"}
	if err := ch.Send(ctx, e); err != nil {
		s.Svc.log.Warn("manager: test notification not delivered", "channel", channel, "err", err)
		return api.NotificationTest{Channel: channel}, nil
	}
	return api.NotificationTest{Channel: channel, Delivered: true}, nil
}
