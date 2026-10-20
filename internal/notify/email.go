// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"sort"
	"strings"
	"time"
)

// EmailConfig configures an Email channel.
type EmailConfig struct {
	Name     string // channel name, default "email"
	Addr     string // host:port of the SMTP server
	From     string
	To       []string
	Username string // optional; PLAIN auth is only attempted after STARTTLS
	Password string
	// TLSConfig is used for STARTTLS. ServerName defaults to the Addr host.
	// Certificate verification is never disabled by this package.
	TLSConfig *tls.Config
	// AllowInsecure permits delivery to servers without STARTTLS. Default is
	// to refuse. Credentials are never sent over a plaintext connection.
	AllowInsecure bool
	Timeout       time.Duration // whole session, default 30s
	Now           Clock
}

// Email delivers events via SMTP with STARTTLS required by default.
type Email struct {
	cfg  EmailConfig
	host string
}

// NewEmail validates cfg and creates an Email channel.
func NewEmail(cfg EmailConfig) (*Email, error) {
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || host == "" {
		return nil, errors.New("email addr must be host:port")
	}
	if _, err := mail.ParseAddress(cfg.From); err != nil {
		return nil, fmt.Errorf("email from: %w", err)
	}
	if len(cfg.To) == 0 {
		return nil, errors.New("email needs at least one recipient")
	}
	for _, t := range cfg.To {
		if strings.ContainsAny(t, "\r\n") {
			return nil, errors.New("email recipient contains control characters")
		}
		if _, err := mail.ParseAddress(t); err != nil {
			return nil, fmt.Errorf("email recipient: %w", err)
		}
	}
	if cfg.Name == "" {
		cfg.Name = "email"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Email{cfg: cfg, host: host}, nil
}

// Name implements Channel.
func (m *Email) Name() string { return m.cfg.Name }

func sanitizeHeader(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r < 0x20 {
			return ' '
		}
		return r
	}, s)
}

func (m *Email) message(e Event) []byte {
	subject := fmt.Sprintf("[%s] %s: %s", e.Severity, sanitizeHeader(e.Kind), sanitizeHeader(e.Resource))
	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", m.cfg.From)
	h("To", strings.Join(m.cfg.To, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", subject))
	h("Date", m.cfg.Now().UTC().Format(time.RFC1123Z))
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	body := fmt.Sprintf("%s\n\nSeverity: %s\nKind: %s\nResource: %s\nTime: %s\nEvent ID: %s\n",
		e.Message, e.Severity, e.Kind, e.Resource, e.Time.UTC().Format(time.RFC3339), e.ID)
	keys := make([]string, 0, len(e.Details))
	for k := range e.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		body += fmt.Sprintf("%s: %s\n", k, e.Details[k])
	}
	// Normalise line endings and dot-stuff is handled by net/smtp's DATA writer.
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

// Send implements Channel.
func (m *Email) Send(ctx context.Context, e Event) error {
	dctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", m.cfg.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := dctx.Deadline()
	_ = conn.SetDeadline(deadline)
	// Unblock I/O when the context is cancelled.
	stop := context.AfterFunc(dctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return err
	}
	secure := false
	if ok, _ := c.Extension("STARTTLS"); ok {
		tc := &tls.Config{}
		if m.cfg.TLSConfig != nil {
			tc = m.cfg.TLSConfig.Clone()
		}
		if tc.ServerName == "" {
			tc.ServerName = m.host
		}
		if err := c.StartTLS(tc); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
		secure = true
	} else if !m.cfg.AllowInsecure {
		return errors.New("smtp server does not offer STARTTLS; refusing plaintext delivery")
	}
	if m.cfg.Username != "" {
		if !secure {
			return errors.New("refusing to send credentials over a plaintext connection")
		}
		if err := c.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	from, _ := mail.ParseAddress(m.cfg.From)
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	for _, t := range m.cfg.To {
		a, _ := mail.ParseAddress(t)
		if err := c.Rcpt(a.Address); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(m.message(e)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
