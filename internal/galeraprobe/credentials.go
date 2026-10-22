// SPDX-License-Identifier: AGPL-3.0-only

package galeraprobe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Credentials is a monitoring account. Its formatting verbs, GoString and
// slog value never reveal the password.
type Credentials struct {
	User     string
	Password string
}

// String returns "user:***".
func (c Credentials) String() string { return c.User + ":***" }

// GoString implements fmt.GoStringer (%#v).
func (c Credentials) GoString() string {
	return fmt.Sprintf("galeraprobe.Credentials{User:%q}", c.User)
}

// LogValue implements slog.LogValuer.
func (c Credentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

// CredentialSource supplies the monitoring credentials for one probe. It is
// consulted on every probe so rotated secrets take effect without a restart.
type CredentialSource interface {
	Credentials(ctx context.Context) (Credentials, error)
}

// StaticCredentials is a fixed CredentialSource (tests, embedding).
type StaticCredentials Credentials

// Credentials returns the fixed account.
func (s StaticCredentials) Credentials(context.Context) (Credentials, error) {
	return Credentials(s), nil
}

// FileCredentials reads the password from a file on every probe (a Docker or
// Swarm secret, for example). One trailing newline is trimmed.
type FileCredentials struct {
	User string
	Path string
}

// Credentials reads the password file.
func (f FileCredentials) Credentials(context.Context) (Credentials, error) {
	if f.User == "" {
		return Credentials{}, errors.New("monitoring user is empty")
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		// os errors name the path, not the content.
		return Credentials{}, fmt.Errorf("reading monitoring password file: %w", err)
	}
	pw := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if pw == "" {
		return Credentials{}, fmt.Errorf("monitoring password file %s is empty", f.Path)
	}
	return Credentials{User: f.User, Password: pw}, nil
}

// EnvCredentials reads the password from an environment variable.
type EnvCredentials struct {
	User string
	Var  string
}

// Credentials reads the environment variable.
func (e EnvCredentials) Credentials(context.Context) (Credentials, error) {
	if e.User == "" {
		return Credentials{}, errors.New("monitoring user is empty")
	}
	pw := os.Getenv(e.Var)
	if pw == "" {
		return Credentials{}, fmt.Errorf("environment variable %s is empty or unset", e.Var)
	}
	return Credentials{User: e.User, Password: pw}, nil
}
